package service

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// MiddlewareService 提供中间件纳管能力（4.1）。
type MiddlewareService struct {
	repo    *repository.InstanceRepository
	cipher  *utils.Cipher
	monitor monitor.Client
	audit   *AuditService
	log     *zap.Logger
}

// NewMiddlewareService 构造纳管服务。
func NewMiddlewareService(repo *repository.InstanceRepository, cipher *utils.Cipher, mon monitor.Client, audit *AuditService, log *zap.Logger) *MiddlewareService {
	return &MiddlewareService{repo: repo, cipher: cipher, monitor: mon, audit: audit, log: log}
}

// MiddlewareInput 是新增/更新实例的入参。
type MiddlewareInput struct {
	Name         string         `json:"name" binding:"required,min=1,max=128"`
	MWType       string         `json:"mw_type" binding:"required"`
	Host         string         `json:"host" binding:"required"`
	Port         int            `json:"port" binding:"required,min=1,max=65535"`
	Username     string         `json:"username"`
	Password     string         `json:"password"`
	Environment  string         `json:"environment"`
	GroupName    string         `json:"group_name"`
	Tags         []string       `json:"tags"`
	Config       map[string]any `json:"config"`
	PromJob      string         `json:"prom_job"`
	PromInstance string         `json:"prom_instance"`
}

// Scope 是数据权限过滤条件。
//
// Owner 为可选的会话归属信息，用于审计与只读工具的数据权限标注（5.5）。
type Scope struct {
	EnvScope   []string
	GroupScope []string
	Owner      *Session
}

// supportedTypes 是一期承诺纳管的中间件类型（与能力矩阵一致）。
var supportedTypes = map[string]bool{
	model.MWTypeRedis: true,
	model.MWTypeKafka: true,
	model.MWTypeMySQL: true,
	model.MWTypePG:    true,
	model.MWTypeES:    true,
	model.MWTypeNginx: true,
	model.MWTypeRMQ:   true,
}

// defaultPorts 是各中间件默认端口（连接测试与自动发现使用）。
var defaultPorts = map[string]int{
	model.MWTypeRedis: 6379,
	model.MWTypeKafka: 9092,
	model.MWTypeMySQL: 3306,
	model.MWTypePG:    5432,
	model.MWTypeES:    9200,
	model.MWTypeNginx: 80,
	model.MWTypeRMQ:   5672,
}

// TestResult 是连接测试结果。
type TestResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Latency  int64  `json:"latency_ms"`
	Endpoint string `json:"endpoint"`
}

// MiddlewareDomainTypes 返回属于「中间件/主机纳管与监控域」的类型白名单。
//
// 判定依据是**有没有指标画像**，而不是在每个调用方硬编码"排除 log"：
//   - redis/mysql/pg/kafka/es/nginx/node：都有指标画像 → 属于该域（统一监控里能选指标）；
//   - 日志集成（mw_type=log）：没有画像、也没有实例端口 → **不属于该域**。
//
// 为什么必须收口成一函数：中间件纳管列表、统一监控的实例下拉、指标告警的实例选择、
// 大盘的实例统计、健康探测……都在查同一张表。漏掉任何一处，
// 日志集成就会以"一个实例"的身份冒出来（真实反馈：纳管列表与统一监控下拉里混进了 logs，
// 而且健康探测会把它标成离线——它根本没有实例端口）。
//
// 手工纳管的类型也要保留（如二期的 rabbitmq 没有集成模板，但能手填），
// 因此白名单 = 手工支持的类型 ∪ 有画像的集成类型。
func MiddlewareDomainTypes() []string {
	set := make(map[string]bool, len(supportedTypes)+2)
	for mwType := range supportedTypes {
		set[mwType] = true
	}
	for _, tplType := range integration.SupportedTypes() {
		if profile := monitor.ProfileOf(tplType); len(profile.Metrics) > 0 {
			set[tplType] = true
		}
	}
	out := make([]string, 0, len(set))
	for mwType := range set {
		out = append(out, mwType)
	}
	sort.Strings(out)
	return out
}

// List 分页查询实例。
//
// 只列**纳管域**内的实例（见 MiddlewareDomainTypes）：日志集成不属于这里。
func (s *MiddlewareService) List(ctx context.Context, keyword, mwType, environment, group string, scope Scope, limit, offset int) ([]model.MiddlewareInstance, int64, error) {
	items, total, err := s.repo.List(ctx, repository.InstanceFilter{
		Keyword:     keyword,
		MWType:      mwType,
		MWTypes:     MiddlewareDomainTypes(),
		Environment: environment,
		GroupName:   group,
		EnvScope:    scope.EnvScope,
		GroupScope:  scope.GroupScope,
	}, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	for i := range items {
		items[i].HasPassword = items[i].PasswordEncrypted != ""
	}
	return items, total, nil
}

// Get 查询实例详情。
func (s *MiddlewareService) Get(ctx context.Context, id int64, scope Scope) (*model.MiddlewareInstance, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !inScope(item, scope) {
		return nil, apperr.New(apperr.CodeScopeDenied, "该实例不在你的数据权限范围内")
	}
	item.HasPassword = item.PasswordEncrypted != ""
	return item, nil
}

// Create 新增实例。
func (s *MiddlewareService) Create(ctx context.Context, in MiddlewareInput, operator Operator) (*model.MiddlewareInstance, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}
	encrypted, err := s.cipher.Encrypt(in.Password)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	item := &model.MiddlewareInstance{
		Name:              strings.TrimSpace(in.Name),
		MWType:            in.MWType,
		Host:              strings.TrimSpace(in.Host),
		Port:              in.Port,
		Username:          in.Username,
		PasswordEncrypted: encrypted,
		Environment:       defaultString(in.Environment, model.EnvDev),
		GroupName:         in.GroupName,
		Tags:              model.JSONStringSlice(in.Tags),
		Config:            model.JSONMap(in.Config),
		PromJob:           in.PromJob,
		PromInstance:      in.PromInstance,
		Status:            1,
	}
	if err := s.repo.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, item.ID, "middleware_create", map[string]any{
		"name": item.Name, "mw_type": item.MWType, "environment": item.Environment,
		"host": item.Host, "port": item.Port,
	})
	item.HasPassword = item.PasswordEncrypted != ""
	return item, nil
}

// Update 更新实例。
func (s *MiddlewareService) Update(ctx context.Context, id int64, in MiddlewareInput, operator Operator) (*model.MiddlewareInstance, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := validateInput(in); err != nil {
		return nil, err
	}
	item.Name = strings.TrimSpace(in.Name)
	item.MWType = in.MWType
	item.Host = strings.TrimSpace(in.Host)
	item.Port = in.Port
	item.Username = in.Username
	if strings.TrimSpace(in.Password) != "" {
		encrypted, encErr := s.cipher.Encrypt(in.Password)
		if encErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, encErr)
		}
		item.PasswordEncrypted = encrypted
	}
	if in.Environment != "" {
		item.Environment = in.Environment
	}
	item.GroupName = in.GroupName
	item.Tags = model.JSONStringSlice(in.Tags)
	// 表单未提交 config 时保持原值。
	//
	// 不能无条件覆盖：纳管页面并不提交 config，一次"编辑"就会把实例的配置备注清空；
	// 对「集成中心」创建的实例更严重——Config.integration 被清掉后，该实例会从
	// 集成列表里消失（只剩一个纳管实例），排查时表现为"集成莫名不见了"。
	if in.Config != nil {
		item.Config = model.JSONMap(in.Config)
	}
	item.PromJob = in.PromJob
	item.PromInstance = in.PromInstance

	if err := s.repo.Update(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, item.ID, "middleware_update", map[string]any{
		"name": item.Name, "environment": item.Environment, "password_changed": in.Password != "",
	})
	item.HasPassword = item.PasswordEncrypted != ""
	return item, nil
}

// Delete 删除实例。
//
// 调用方需先完成操作级别校验：prod 环境删除属于 L2，必须走审批（4.6）。
func (s *MiddlewareService) Delete(ctx context.Context, id int64, operator Operator) error {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, id, "middleware_delete", map[string]any{
		"name": item.Name, "environment": item.Environment,
	})
	return nil
}

// TestConnection 执行连接测试（不落库，仅返回探测结果）。
func (s *MiddlewareService) TestConnection(ctx context.Context, id int64, scope Scope) (*TestResult, error) {
	item, err := s.Get(ctx, id, scope)
	if err != nil {
		return nil, err
	}
	return s.probe(ctx, item)
}

// TestEndpoint 对未保存的连接参数执行探测（新增表单即时校验）。
func (s *MiddlewareService) TestEndpoint(ctx context.Context, in MiddlewareInput) (*TestResult, error) {
	if err := validateInput(in); err != nil {
		return nil, err
	}
	item := &model.MiddlewareInstance{Name: in.Name, MWType: in.MWType, Host: in.Host, Port: in.Port}
	return s.probe(ctx, item)
}

// probe 执行 TCP 连通性探测（端口可达 + 耗时）。
func (s *MiddlewareService) probe(ctx context.Context, item *model.MiddlewareInstance) (*TestResult, error) {
	endpoint := net.JoinHostPort(item.Host, fmt.Sprintf("%d", item.Port))
	// 远程集成的地址是目标机视角：回环地址在平台侧探测没有意义（会打到平台自己）。
	if reason, skip := platformSideProbeSkipReason(*item); skip {
		return &TestResult{Endpoint: endpoint, Success: true, Message: reason}, nil
	}
	start := time.Now()
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	latency := time.Since(start).Milliseconds()
	result := &TestResult{Endpoint: endpoint, Latency: latency}
	if err != nil {
		result.Success = false
		result.Message = "连接失败：" + err.Error()
		return result, nil
	}
	_ = conn.Close()
	result.Success = true
	result.Message = fmt.Sprintf("TCP 连通正常（%s，耗时 %dms）", endpoint, latency)
	return result, nil
}

// HealthCheck 触发即时健康探测并落库状态。
func (s *MiddlewareService) HealthCheck(ctx context.Context, id int64, operator Operator) (*TestResult, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	result, err := s.probe(ctx, item)
	if err != nil {
		return nil, err
	}
	status := int16(0)
	if result.Success {
		status = 1
	}
	if err := s.repo.UpdateStatus(ctx, id, status, result.Message); err != nil {
		s.log.Warn("更新实例健康状态失败", zap.Int64("id", id), zap.Error(err))
	}
	s.record(ctx, operator, id, "middleware_health", map[string]any{
		"success": result.Success, "endpoint": result.Endpoint, "latency_ms": result.Latency,
	})
	return result, nil
}

// ProbeAll 对所有实例执行健康探测（定时任务调用）。
//
// 返回在线数量与探测总数，便于调度日志观测。
func (s *MiddlewareService) ProbeAll(ctx context.Context) (online, total int, err error) {
	// 只探纳管域内的实例：日志集成没有实例端口，探它只会必然失败、
	// 把它标成"离线"，并在大盘上多出一个假的离线实例。
	items, err := s.repo.All(ctx, repository.InstanceFilter{MWTypes: MiddlewareDomainTypes()})
	if err != nil {
		return 0, 0, err
	}
	for i := range items {
		item := items[i]
		result, probeErr := s.probe(ctx, &item)
		status := int16(0)
		message := "探测失败"
		if probeErr == nil && result.Success {
			status = 1
			online++
			message = result.Message
		} else if probeErr == nil {
			message = result.Message
		} else {
			message = probeErr.Error()
		}
		if updateErr := s.repo.UpdateStatus(ctx, item.ID, status, message); updateErr != nil {
			s.log.Warn("更新实例健康状态失败", zap.Int64("id", item.ID), zap.Error(updateErr))
		}
		total++
	}
	return online, total, nil
}

// Targets 返回可用于监控查询的目标列表（受数据权限约束）。
func (s *MiddlewareService) Targets(ctx context.Context, scope Scope, ids []int64) ([]monitor.Target, error) {
	items, err := s.repo.All(ctx, repository.InstanceFilter{
		MWTypes:  MiddlewareDomainTypes(),
		EnvScope: scope.EnvScope, GroupScope: scope.GroupScope,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	out := make([]monitor.Target, 0, len(items))
	for _, item := range items {
		if len(idSet) > 0 && !idSet[item.ID] {
			continue
		}
		out = append(out, ToTarget(item))
	}
	return out, nil
}

// ToTarget 把实例转换为监控查询目标。
func ToTarget(item model.MiddlewareInstance) monitor.Target {
	return monitor.Target{
		InstanceID: item.ID,
		Name:       item.Name,
		MWType:     item.MWType,
		Job:        item.PromJob,
		Instance:   item.PromInstance,
	}
}

// GuardScope 构造只读工具的数据权限范围（5.5）。
//
// 数据权限由服务端强制施加，不依赖 Prompt 约束：环境与分组过滤条件直接落到
// repository 查询上，AI 看不到超出权限的实例。
func GuardScope(session *Session, auth *AuthService) (scope Scope, scopeDesc string) {
	if session == nil || session.User == nil {
		return Scope{}, "匿名"
	}
	envs, groups, allowAll := auth.ScopeOf(session)
	if allowAll {
		return Scope{Owner: session}, fmt.Sprintf("角色=%s 环境=全部 分组=全部", session.User.RoleCode)
	}
	return Scope{EnvScope: envs, GroupScope: groups, Owner: session},
		fmt.Sprintf("角色=%s 环境=%s 分组=%s", session.User.RoleCode, joinOr(envs, "全部"), joinOr(groups, "全部"))
}

// inScope 判断实例是否在数据权限范围内。
func inScope(item *model.MiddlewareInstance, scope Scope) bool {
	if len(scope.EnvScope) > 0 && !contains(scope.EnvScope, item.Environment) {
		return false
	}
	if len(scope.GroupScope) > 0 && !contains(scope.GroupScope, item.GroupName) {
		return false
	}
	return true
}

// contains 判断切片是否包含目标值。
func contains(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// joinOr 连接字符串，空集合返回默认值。
func joinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	sorted := append([]string(nil), items...)
	sort.Strings(sorted)
	return strings.Join(sorted, "/")
}

// defaultString 返回首个非空字符串。
func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// validateInput 校验入参。
func validateInput(in MiddlewareInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return apperr.New(apperr.CodeInvalidParam, "实例名称不能为空")
	}
	if !supportedTypes[in.MWType] {
		return apperr.Newf(apperr.CodeInvalidParam, "不支持的中间件类型 %q（一期支持 redis/kafka/mysql/pg/es/nginx/rabbitmq）", in.MWType)
	}
	if strings.TrimSpace(in.Host) == "" {
		return apperr.New(apperr.CodeInvalidParam, "连接地址不能为空")
	}
	if in.Port <= 0 || in.Port > 65535 {
		return apperr.New(apperr.CodeInvalidParam, "端口必须在 1-65535 之间")
	}
	if in.Environment != "" && in.Environment != model.EnvDev &&
		in.Environment != model.EnvStaging && in.Environment != model.EnvProd {
		return apperr.Newf(apperr.CodeInvalidParam, "环境 %q 非法（可选 dev/staging/prod）", in.Environment)
	}
	return nil
}

// Operator 描述操作者身份，用于审计。
type Operator struct {
	UserID   int64
	Username string
	IP       string
	Agent    string
}

// record 写入审计日志。
func (s *MiddlewareService) record(ctx context.Context, op Operator, instanceID int64, action string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID:     op.UserID,
		Username:   op.Username,
		InstanceID: instanceID,
		ActionType: action,
		Level:      LevelLow,
		IPAddress:  op.IP,
		UserAgent:  op.Agent,
		Detail:     detail,
	})
}

// DefaultPort 返回中间件默认端口。
func DefaultPort(mwType string) int { return defaultPorts[mwType] }

// WatchEngineStatus 暴露引擎状态（供实例详情页展示 AI 可用性）。
func WatchEngineStatus(e engine.Engine) map[string]any {
	if e == nil {
		return map[string]any{"available": false, "name": "none"}
	}
	st := e.Status()
	return map[string]any{
		"name": st.Name, "available": st.Available, "degraded": st.Degraded,
		"circuit_open": st.CircuitOpen, "consecutive_fails": st.ConsecutiveFails,
		"last_error": st.LastError,
	}
}
