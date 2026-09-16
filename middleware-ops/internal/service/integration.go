package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/docker"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// IntegrationService 实现「集成中心」：在页面上选组件、填参数，平台自动完成
// Exporter 暴露 → Prometheus 抓取 → 实例纳管 → 告警规则四件事。
//
// 与云厂商控制台「数据采集 → 集成中心」的对应关系：
//
//	控制台字段        本平台字段              落地位置
//	集成名称          name                    纳管实例名 + Prometheus instance_name 标签
//	地址              address                 实例 host:port（TCP 探测）+ Exporter 目标
//	用户名 / 密码     username / password     Exporter 环境变量（AES-256-GCM 加密存储）
//	标签              labels                  写入 file_sd 的自定义指标标签
//	环境变量/配置     options                 Exporter 的 env 或命令行开关（模板白名单）
type IntegrationService struct {
	cfg        *config.Config
	instances  *repository.InstanceRepository
	cipher     cipherCodec
	alerts     *AlertService
	audit      *AuditService
	log        *zap.Logger
	docker     *docker.Client
	dockerNote string
}

// cipherCodec 是集成中心需要的加解密能力（口令加密存储 + 部署时读回明文）。
type cipherCodec interface {
	Encrypt(plain string) (string, error)
	Decrypt(encoded string) (string, error)
}

// NewIntegrationService 构造集成中心服务。
func NewIntegrationService(
	cfg *config.Config,
	instances *repository.InstanceRepository,
	cipher cipherCodec,
	alerts *AlertService,
	audit *AuditService,
	log *zap.Logger,
) *IntegrationService {
	svc := &IntegrationService{
		cfg: cfg, instances: instances, cipher: cipher, alerts: alerts, audit: audit, log: log,
	}
	if cfg.Integration.DockerEnabled {
		client, err := docker.New(cfg.Integration.DockerHost)
		if err != nil {
			svc.dockerNote = "Docker 客户端初始化失败：" + err.Error()
			log.Warn("集成中心：Docker 客户端初始化失败，一键部署不可用", zap.Error(err))
		} else {
			svc.docker = client
		}
	} else {
		svc.dockerNote = "未启用一键部署（integration.docker_enabled=false）：平台只渲染配置，容器需人工启动"
	}
	return svc
}

// ---------------------------------------------------------------------------
// 入参 / 出参
// ---------------------------------------------------------------------------

// IntegrationInput 是集成表单入参。
type IntegrationInput struct {
	// Name 为集成名称（唯一，同时作为纳管实例名与 instance_name 标签）。
	Name string `json:"name" binding:"required,min=1,max=63"`
	// MWType 为组件类型：redis / mysql / pg / kafka / es / nginx。
	MWType string `json:"mw_type" binding:"required"`
	// Address 为被管实例地址（host:port，URL 型组件可带 scheme 与 path）。
	Address  string `json:"address" binding:"required"`
	Username string `json:"username"`
	Password string `json:"password"`
	// Labels 为自定义指标标签。
	Labels map[string]string `json:"labels"`
	// Options 为 Exporter 参数（环境变量或命令行开关，键取自模板声明）。
	Options     map[string]string `json:"options"`
	Environment string            `json:"environment"`
	GroupName   string            `json:"group_name"`
	Tags        []string          `json:"tags"`
	// Deploy 表示本次保存是否尝试一键拉起 Exporter 容器。
	Deploy *bool `json:"deploy"`
	// AutoRules 表示是否自动创建推荐告警规则（缺省取配置 integration.auto_rules）。
	AutoRules *bool `json:"auto_rules"`
}

// IntegrationView 是集成中心列表/详情的对外结构。
type IntegrationView struct {
	InstanceID  int64             `json:"instance_id"`
	Name        string            `json:"name"`
	MWType      string            `json:"mw_type"`
	Component   string            `json:"component"`
	Address     string            `json:"address"`
	Host        string            `json:"host"`
	Port        int               `json:"port"`
	Username    string            `json:"username"`
	Environment string            `json:"environment"`
	GroupName   string            `json:"group_name"`
	Labels      map[string]string `json:"labels"`
	Options     map[string]string `json:"options"`
	JobName     string            `json:"job_name"`
	Container   string            `json:"container"`
	Image       string            `json:"image"`
	// ContainerStatus 为 Exporter 容器状态（未启用一键部署时为空）。
	ContainerStatus string `json:"container_status"`
	Selector        string `json:"selector"`
	AppliedAt       string `json:"applied_at"`
	LastError       string `json:"last_error"`
	HasPassword     bool   `json:"has_password"`
}

// IntegrationOverview 是集成中心的概览（用于卡片上的角标）。
type IntegrationOverview struct {
	Total     int            `json:"total"`
	ByType    map[string]int `json:"by_type"`
	Templates []TemplateView `json:"templates"`
	// FileSDPath 为 file_sd 文件在平台侧的实际路径。
	FileSDPath string `json:"file_sd_path"`
	// DockerNote 说明一键部署能力当前是否可用。
	DockerNote string `json:"docker_note"`
	DockerOK   bool   `json:"docker_ok"`
}

// TemplateView 是模板 + 当前环境默认值（前端渲染表单用）。
type TemplateView struct {
	integration.Template
	// JobName 为 file_sd 抓取任务名。
	JobName string `json:"job_name"`
	// DefaultEnvironment 为新建集成的默认环境。
	DefaultEnvironment string `json:"default_environment"`
	// Integrated 为该组件已集成数量。
	Integrated int `json:"integrated"`
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// Overview 返回集成中心概览。
func (s *IntegrationService) Overview(ctx context.Context) (*IntegrationOverview, error) {
	items, err := s.listIntegrations(ctx, repository.InstanceFilter{})
	if err != nil {
		return nil, err
	}
	byType := make(map[string]int, len(items))
	for _, item := range items {
		meta, ok := IntegrationMetaOf(item)
		if !ok {
			continue
		}
		byType[meta.Template]++
	}
	views := make([]TemplateView, 0, len(integration.Templates()))
	for _, tpl := range integration.Templates() {
		views = append(views, TemplateView{
			Template: tpl, JobName: s.jobName(),
			DefaultEnvironment: s.defaultEnvironment(), Integrated: byType[tpl.Type],
		})
	}
	return &IntegrationOverview{
		Total: len(items), ByType: byType, Templates: views,
		FileSDPath: s.fileSDPath(),
		DockerNote: s.dockerNote, DockerOK: s.docker != nil,
	}, nil
}

// Templates 返回组件模板。
func (s *IntegrationService) Templates() []integration.Template { return integration.Templates() }

// List 返回全部集成（受数据权限约束）。
func (s *IntegrationService) List(ctx context.Context, scope Scope) ([]IntegrationView, error) {
	items, err := s.listIntegrations(ctx, repository.InstanceFilter{
		EnvScope: scope.EnvScope, GroupScope: scope.GroupScope,
	})
	if err != nil {
		return nil, err
	}
	out := make([]IntegrationView, 0, len(items))
	for _, item := range items {
		out = append(out, s.toView(ctx, item))
	}
	return out, nil
}

// Get 返回单个集成。
func (s *IntegrationService) Get(ctx context.Context, id int64, scope Scope) (*IntegrationView, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if !inScope(item, scope) {
		return nil, apperr.New(apperr.CodeScopeDenied, "该集成不在你的数据权限范围内")
	}
	view := s.toView(ctx, *item)
	return &view, nil
}

// ---------------------------------------------------------------------------
// 预览与保存
// ---------------------------------------------------------------------------

// Preview 只做校验与渲染，不落库、不部署（供表单一键预览生成的配置）。
func (s *IntegrationService) Preview(in IntegrationInput) (*integration.Artifacts, error) {
	tpl, instance, err := s.build(in)
	if err != nil {
		return nil, err
	}
	artifacts, err := integration.Render(tpl, instance, s.jobName(), s.fileSDPath(), s.exporterNetwork())
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	return &artifacts, nil
}

// Create 新建集成：纳管实例 + file_sd 落盘 + 可选一键部署 + 可选告警规则。
func (s *IntegrationService) Create(ctx context.Context, in IntegrationInput, operator Operator) (*IntegrationView, error) {
	if !s.cfg.Integration.Enabled {
		return nil, apperr.New(apperr.CodeForbidden, "集成中心未启用（integration.enabled=false）")
	}
	tpl, instance, err := s.build(in)
	if err != nil {
		return nil, err
	}
	// 名称唯一性：file_sd 的 instance_name 与容器名都由它派生。
	if existing, err := s.findByName(ctx, instance.Name); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "集成名称 %q 已存在（集成名称需全局唯一）", instance.Name)
	}

	encrypted, err := s.cipher.Encrypt(instance.Password)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	tags := append([]string{"integration", instance.MWType}, in.Tags...)
	item := &model.MiddlewareInstance{
		Name:              instance.Name,
		MWType:            tpl.Type,
		Host:              instance.Address.Host,
		Port:              instance.Address.Port,
		Username:          instance.Username,
		PasswordEncrypted: encrypted,
		Environment:       instance.Environment,
		GroupName:         instance.GroupName,
		Tags:              model.JSONStringSlice(tags),
		Config:            model.JSONMap{},
		PromJob:           s.jobName(),
		PromInstance:      "",
		Status:            1,
	}
	meta := IntegrationMeta{
		Template: tpl.Type, Address: instance.Address.Raw, Labels: instance.Labels,
		Options: instance.Options, Job: s.jobName(), Image: tpl.Image,
		Container: integration.ContainerName(instance.Name), ExporterPort: tpl.ExporterPort,
		CreatedBy: operator.Username, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	item.Config["integration"] = meta.toMap()
	if err := s.instances.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	// 落盘 file_sd：写入失败不回滚实例，但把原因回传给调用方（可重试"重新应用"）。
	syncErr := s.SyncFileSD(ctx)
	deployErr := s.deploy(ctx, item, tpl, instance)

	if s.shouldCreateRules(in) {
		if err := s.createRecommendedRules(ctx, item, tpl, operator); err != nil {
			s.log.Warn("集成：自动创建告警规则失败", zap.String("integration", item.Name), zap.Error(err))
		}
	}
	s.record(ctx, operator, item.ID, "integration_create", map[string]any{
		"name": item.Name, "mw_type": tpl.Type, "address": instance.Address.Raw,
		"deploy": s.deployEnabled(in), "labels": instance.Labels,
	})

	if err := firstErr(syncErr, deployErr); err != nil {
		s.markError(ctx, item.ID, err.Error())
		view := s.toView(ctx, *item)
		view.LastError = err.Error()
		return &view, nil
	}
	s.markApplied(ctx, item.ID)
	view := s.toView(ctx, *item)
	return &view, nil
}

// Update 更新集成。
func (s *IntegrationService) Update(ctx context.Context, id int64, in IntegrationInput, operator Operator) (*IntegrationView, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	tpl, instance, err := s.build(in)
	if err != nil {
		return nil, err
	}
	if tpl.Type != item.MWType {
		return nil, apperr.New(apperr.CodeInvalidParam, "集成创建后不允许更换组件类型（请删除后重新集成）")
	}
	// 改名会改变 instance_name 标签与 file_sd 内容，需要同样保证唯一性。
	if instance.Name != item.Name {
		if existing, findErr := s.findByName(ctx, instance.Name); findErr != nil {
			return nil, findErr
		} else if existing != nil && existing.ID != item.ID {
			return nil, apperr.Newf(apperr.CodeInvalidParam, "集成名称 %q 已被占用", instance.Name)
		}
	}
	item.Name = instance.Name
	item.Host = instance.Address.Host
	item.Port = instance.Address.Port
	item.Username = instance.Username
	if strings.TrimSpace(in.Password) != "" {
		encrypted, encErr := s.cipher.Encrypt(in.Password)
		if encErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, encErr)
		}
		item.PasswordEncrypted = encrypted
	}
	if instance.Environment != "" {
		item.Environment = instance.Environment
	}
	item.GroupName = instance.GroupName
	if len(in.Tags) > 0 {
		item.Tags = model.JSONStringSlice(append([]string{"integration", instance.MWType}, in.Tags...))
	}
	meta, _ := IntegrationMetaOf(*item)
	meta.Template = tpl.Type
	meta.Address = instance.Address.Raw
	meta.Labels = instance.Labels
	meta.Options = instance.Options
	meta.Job = s.jobName()
	meta.Image = tpl.Image
	meta.Container = integration.ContainerName(instance.Name)
	meta.ExporterPort = tpl.ExporterPort
	meta.UpdatedBy = operator.Username
	meta.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if item.Config == nil {
		item.Config = model.JSONMap{}
	}
	item.Config["integration"] = meta.toMap()
	// prom_job 必须与 file_sd 的抓取任务一致，否则平台查不到指标。
	item.PromJob = s.jobName()
	item.PromInstance = ""

	if err := s.instances.Update(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	syncErr := s.SyncFileSD(ctx)
	deployErr := s.deploy(ctx, item, tpl, instance)
	s.record(ctx, operator, item.ID, "integration_update", map[string]any{
		"name": item.Name, "mw_type": tpl.Type, "address": instance.Address.Raw,
		"password_changed": in.Password != "",
	})
	view := s.toView(ctx, *item)
	if err := firstErr(syncErr, deployErr); err != nil {
		s.markError(ctx, item.ID, err.Error())
		view.LastError = err.Error()
		return &view, nil
	}
	s.markApplied(ctx, item.ID)
	return &view, nil
}

// Apply 重新应用：重渲染 file_sd 并按需重建 Exporter 容器。
//
// 用于「改了地址/口令后指标不生效」「容器被误删」等场景的一键修复。
func (s *IntegrationService) Apply(ctx context.Context, id int64, operator Operator) (*IntegrationView, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidParam, "该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "组件模板 %q 不存在", meta.Template)
	}
	password, err := s.decrypt(item.PasswordEncrypted)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	address, err := integration.ParseAddress(meta.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	instance := integration.Instance{
		Name: item.Name, MWType: tpl.Type, Address: address,
		Username: item.Username, Password: password,
		Labels: meta.Labels, Options: meta.Options,
		Environment: item.Environment, GroupName: item.GroupName,
	}
	syncErr := s.SyncFileSD(ctx)
	deployErr := s.deploy(ctx, item, tpl, instance)
	s.record(ctx, operator, item.ID, "integration_apply", map[string]any{"name": item.Name})
	if err := firstErr(syncErr, deployErr); err != nil {
		s.markError(ctx, item.ID, err.Error())
		view := s.toView(ctx, *item)
		view.LastError = err.Error()
		return &view, nil
	}
	s.markApplied(ctx, item.ID)
	return s.Get(ctx, id, Scope{})
}

// Delete 删除集成：停止并移除 Exporter 容器 + 重写 file_sd。
//
// 注意：告警规则与历史告警保留（审计与复盘需要），实例记录按纳管规则删除。
func (s *IntegrationService) Delete(ctx context.Context, id int64, operator Operator) error {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return err
	}
	meta, _ := IntegrationMetaOf(*item)
	if meta.Container != "" && s.docker != nil {
		if removeErr := s.docker.Remove(ctx, meta.Container); removeErr != nil {
			s.log.Warn("集成：移除 Exporter 容器失败", zap.String("container", meta.Container), zap.Error(removeErr))
		}
	}
	if err := s.instances.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.SyncFileSD(ctx); err != nil {
		s.log.Warn("集成：删除后重写 file_sd 失败", zap.Error(err))
	}
	s.record(ctx, operator, id, "integration_delete", map[string]any{"name": item.Name, "mw_type": item.MWType})
	return nil
}

// ---------------------------------------------------------------------------
// 服务发现（Prometheus 侧）
// ---------------------------------------------------------------------------

// ServiceDiscovery 返回 Prometheus http_sd_configs 需要的目标文档。
//
// 这是集成中心与 Prometheus 之间的**主通道**：平台把全部集成渲染成
// file_sd 格式的 JSON（targets + labels.instance_name），Prometheus 每 30s 拉一次。
// 相比共享卷方案，它没有文件属主/挂载顺序问题（后端以非 root 运行），
// 支持跨主机，且天然是"拉一次拿全量"。
//
// 文档内容只有地址与标签，不含任何口令，因此默认不鉴权；
// 需要收紧时设置 integration.sd_token，接口按 ?token= 或 X-SD-Token 校验。
func (s *IntegrationService) ServiceDiscovery(ctx context.Context) (string, error) {
	items, err := s.listIntegrations(ctx, repository.InstanceFilter{})
	if err != nil {
		return "", err
	}
	entries := make([]integration.FileSDEntry, 0, len(items))
	for _, item := range items {
		meta, ok := IntegrationMetaOf(item)
		if !ok {
			continue
		}
		tpl, ok := integration.TemplateOf(meta.Template)
		if !ok {
			continue
		}
		address, parseErr := integration.ParseAddress(meta.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
		if parseErr != nil {
			s.log.Warn("集成：地址无法解析，已跳过服务发现项",
				zap.String("integration", item.Name), zap.String("address", meta.Address), zap.Error(parseErr))
			continue
		}
		entries = append(entries, integration.EntryFor(integration.Instance{
			Name: item.Name, MWType: tpl.Type, Address: address,
			Labels: meta.Labels, Environment: item.Environment, GroupName: item.GroupName,
		}))
	}
	return integration.RenderFileSD(entries)
}

// SDToken 返回服务发现接口的令牌（为空表示不鉴权）。
func (s *IntegrationService) SDToken() string { return s.cfg.Integration.SDToken }

// SyncFileSD 把全部集成重写进 file_sd 文件（原子替换 + 0644）。
//
// 平台自带的 Prometheus 走 HTTP 服务发现，本文件是**副产物**：
// 供人工核对、审计留档，或交给无法访问平台接口的外部 Prometheus。
// 原子替换（临时文件 + rename）避免读取方拿到写了一半的 JSON。
func (s *IntegrationService) SyncFileSD(ctx context.Context) error {
	payload, err := s.ServiceDiscovery(ctx)
	if err != nil {
		return err
	}
	return writeFileAtomic(s.fileSDPath(), payload)
}

// fileSDPath 返回 file_sd 文件路径。
func (s *IntegrationService) fileSDPath() string {
	name := s.cfg.Integration.FileSDName
	if strings.TrimSpace(name) == "" {
		name = "integrations.json"
	}
	return filepath.Join(s.cfg.Integration.OutputDir, name)
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// IntegrationMeta 是持久化在实例 Config 里的集成元信息。
type IntegrationMeta struct {
	Template     string            `json:"template"`
	Address      string            `json:"address"`
	Labels       map[string]string `json:"labels"`
	Options      map[string]string `json:"options"`
	Job          string            `json:"job"`
	Image        string            `json:"image"`
	Container    string            `json:"container"`
	ExporterPort int               `json:"exporter_port"`
	AppliedAt    string            `json:"applied_at"`
	LastError    string            `json:"last_error"`
	CreatedBy    string            `json:"created_by"`
	CreatedAt    string            `json:"created_at"`
	UpdatedBy    string            `json:"updated_by"`
	UpdatedAt    string            `json:"updated_at"`
	DeployNote   string            `json:"deploy_note"`
}

func (m IntegrationMeta) toMap() map[string]any {
	return map[string]any{
		"template": m.Template, "address": m.Address, "labels": m.Labels, "options": m.Options,
		"job": m.Job, "image": m.Image, "container": m.Container, "exporter_port": m.ExporterPort,
		"applied_at": m.AppliedAt, "last_error": m.LastError,
		"created_by": m.CreatedBy, "created_at": m.CreatedAt,
		"updated_by": m.UpdatedBy, "updated_at": m.UpdatedAt, "deploy_note": m.DeployNote,
	}
}

// IntegrationMetaOf 从实例 Config 中取出集成元信息。
func IntegrationMetaOf(item model.MiddlewareInstance) (IntegrationMeta, bool) {
	if item.Config == nil {
		return IntegrationMeta{}, false
	}
	raw, ok := item.Config["integration"]
	if !ok {
		return IntegrationMeta{}, false
	}
	// Config 是 JSON 往返过的 map，这里统一用 JSON 反序列化，避免手写类型断言。
	payload, err := json.Marshal(raw)
	if err != nil {
		return IntegrationMeta{}, false
	}
	var meta IntegrationMeta
	if err := json.Unmarshal(payload, &meta); err != nil {
		return IntegrationMeta{}, false
	}
	if meta.Template == "" {
		return IntegrationMeta{}, false
	}
	return meta, true
}

// listIntegrations 列出通过集成中心创建的实例。
func (s *IntegrationService) listIntegrations(ctx context.Context, filter repository.InstanceFilter) ([]model.MiddlewareInstance, error) {
	items, err := s.instances.All(ctx, filter)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	out := make([]model.MiddlewareInstance, 0, len(items))
	for _, item := range items {
		if _, ok := IntegrationMetaOf(item); ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// integrationInstance 取实例并确认它来自集成中心。
func (s *IntegrationService) integrationInstance(ctx context.Context, id int64) (*model.MiddlewareInstance, error) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "集成不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if _, ok := IntegrationMetaOf(*item); !ok {
		return nil, apperr.New(apperr.CodeNotFound, "该实例不是通过集成中心创建的")
	}
	return item, nil
}

// findByName 按实例名查询（集成名称要求全局唯一）。
func (s *IntegrationService) findByName(ctx context.Context, name string) (*model.MiddlewareInstance, error) {
	items, err := s.instances.All(ctx, repository.InstanceFilter{Keyword: name})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	for i := range items {
		if items[i].Name == name {
			return &items[i], nil
		}
	}
	return nil, nil
}

// build 校验入参并构造渲染输入。
func (s *IntegrationService) build(in IntegrationInput) (integration.Template, integration.Instance, error) {
	tpl, ok := integration.TemplateOf(in.MWType)
	if !ok {
		return integration.Template{}, integration.Instance{}, apperr.Newf(
			apperr.CodeInvalidParam, "不支持的组件类型 %q（可选 %s）",
			in.MWType, strings.Join(integration.SupportedTypes(), "/"))
	}
	address, err := integration.ParseAddress(in.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		return integration.Template{}, integration.Instance{}, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	environment := strings.TrimSpace(in.Environment)
	if environment == "" {
		environment = s.defaultEnvironment()
	}
	switch environment {
	case model.EnvDev, model.EnvStaging, model.EnvProd:
	default:
		return integration.Template{}, integration.Instance{}, apperr.Newf(
			apperr.CodeInvalidParam, "环境 %q 非法（可选 dev/staging/prod）", environment)
	}
	instance := integration.Instance{
		Name: strings.TrimSpace(in.Name), MWType: tpl.Type, Address: address,
		Username: strings.TrimSpace(in.Username), Password: in.Password,
		Labels: in.Labels, Options: in.Options,
		Environment: environment, GroupName: in.GroupName,
	}
	// 更新场景允许口令留空（表示沿用已存口令），因此这里按"空口令"再校验一次：
	// 模板只需要账号非空，口令是否必填由 Validate 内部按组件类型判断。
	if err := tpl.Validate(instance); err != nil {
		return integration.Template{}, integration.Instance{}, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	return tpl, instance, nil
}

// deploy 按配置尝试一键拉起 Exporter 容器。
func (s *IntegrationService) deploy(ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template, instance integration.Instance) error {
	if s.docker == nil {
		s.setDeployNote(ctx, item.ID, s.dockerNote)
		return nil
	}
	env := tpl.RenderEnv(instance)
	args := tpl.RenderArgs(instance)
	spec := docker.ContainerSpec{
		Name:     integration.ContainerName(instance.Name),
		Image:    tpl.Image,
		Env:      envPairs(env),
		Cmd:      args,
		Networks: s.exporterNetworks(),
		Labels: map[string]string{
			"mwops.integration": instance.Name,
			"mwops.mw_type":     tpl.Type,
		},
	}
	action, id, err := s.docker.Ensure(ctx, spec)
	if err != nil {
		s.setDeployNote(ctx, item.ID, "一键部署失败："+err.Error())
		return fmt.Errorf("一键部署 Exporter 失败：%w", err)
	}
	s.setDeployNote(ctx, item.ID, fmt.Sprintf("Exporter 容器已%s（%s，ID %s）", actionLabel(action), spec.Name, shortID(id)))
	return nil
}

// syncErr 保留错误顺序，便于把首个失败原因回传。
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// envPairs 把 map 转为 KEY=VALUE 列表（排序，保证重建幂等）。
func envPairs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+integration.SanitizeEnvValue(env[key]))
	}
	return out
}

// actionLabel 把 Docker 动作翻成中文。
func actionLabel(action string) string {
	switch action {
	case "created":
		return "创建"
	case "recreated":
		return "重建"
	default:
		return action
	}
}

// shortID 截断容器 ID。
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// shouldCreateRules 判断是否创建推荐告警规则。
func (s *IntegrationService) shouldCreateRules(in IntegrationInput) bool {
	if in.AutoRules != nil {
		return *in.AutoRules
	}
	return s.cfg.Integration.AutoRules
}

// deployEnabled 判断本次是否请求一键部署。
func (s *IntegrationService) deployEnabled(in IntegrationInput) bool {
	if in.Deploy != nil {
		return *in.Deploy
	}
	return s.docker != nil
}

// createRecommendedRules 按模板创建推荐告警规则（已存在同名规则则跳过）。
func (s *IntegrationService) createRecommendedRules(ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template, operator Operator) error {
	if len(tpl.Alerts) == 0 || s.alerts == nil {
		return nil
	}
	existing, _, err := s.alerts.ListRules(ctx, item.ID, 100, 0)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(existing))
	for _, rule := range existing {
		known[rule.MetricName+"|"+rule.Operator] = true
	}
	for _, tplAlert := range tpl.Alerts {
		if known[tplAlert.MetricName+"|"+tplAlert.Operator] {
			continue
		}
		if _, err := s.alerts.CreateRule(ctx, RuleInput{
			Name:        item.Name + " · " + tplAlert.Name,
			InstanceID:  item.ID,
			MWType:      tpl.Type,
			MetricName:  tplAlert.MetricName,
			Operator:    tplAlert.Operator,
			Threshold:   tplAlert.Threshold,
			Level:       tplAlert.Level,
			TimeWindow:  tplAlert.TimeWindow,
			Cooldown:    tplAlert.Cooldown,
			Description: tplAlert.Description,
		}, operator); err != nil {
			return err
		}
	}
	return nil
}

// toView 组装对外结构。
func (s *IntegrationService) toView(ctx context.Context, item model.MiddlewareInstance) IntegrationView {
	meta, _ := IntegrationMetaOf(item)
	tpl, _ := integration.TemplateOf(meta.Template)
	view := IntegrationView{
		InstanceID: item.ID, Name: item.Name, MWType: item.MWType, Component: tpl.Component,
		Address: meta.Address, Host: item.Host, Port: item.Port, Username: item.Username,
		Environment: item.Environment, GroupName: item.GroupName,
		Labels: meta.Labels, Options: meta.Options, JobName: meta.Job,
		Container: meta.Container, Image: meta.Image,
		Selector:  integration.SelectorFor(integration.Instance{Name: item.Name, MWType: item.MWType}, meta.Job),
		AppliedAt: meta.AppliedAt, LastError: meta.LastError,
		HasPassword: item.PasswordEncrypted != "",
	}
	if s.docker != nil && meta.Container != "" {
		if state, err := s.docker.Inspect(ctx, meta.Container); err == nil && state != nil {
			view.ContainerStatus = state.Status
		} else if err != nil {
			s.log.Debug("集成：查询容器状态失败", zap.String("container", meta.Container), zap.Error(err))
		}
	}
	return view
}

// markApplied 记录最近一次成功应用时间。
func (s *IntegrationService) markApplied(ctx context.Context, id int64) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		return
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return
	}
	meta.AppliedAt = time.Now().UTC().Format(time.RFC3339)
	meta.LastError = ""
	if item.Config == nil {
		item.Config = model.JSONMap{}
	}
	item.Config["integration"] = meta.toMap()
	if err := s.instances.Update(ctx, item); err != nil {
		s.log.Warn("集成：写入应用时间失败", zap.Int64("id", id), zap.Error(err))
	}
}

// markError 记录最近一次失败原因。
func (s *IntegrationService) markError(ctx context.Context, id int64, message string) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		return
	}
	s.writeMeta(ctx, item, func(meta *IntegrationMeta) { meta.LastError = message })
}

// setDeployNote 记录部署说明。
func (s *IntegrationService) setDeployNote(ctx context.Context, id int64, note string) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		return
	}
	s.writeMeta(ctx, item, func(meta *IntegrationMeta) { meta.DeployNote = note })
}

// writeMeta 读改写实例的集成元信息。
func (s *IntegrationService) writeMeta(ctx context.Context, item *model.MiddlewareInstance, mutate func(*IntegrationMeta)) {
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return
	}
	mutate(&meta)
	if item.Config == nil {
		item.Config = model.JSONMap{}
	}
	item.Config["integration"] = meta.toMap()
	if err := s.instances.Update(ctx, item); err != nil {
		s.log.Warn("集成：写入元信息失败", zap.Int64("id", item.ID), zap.Error(err))
	}
}

// decrypt 解密实例口令（一键部署需要明文注入容器环境变量）。
func (s *IntegrationService) decrypt(encrypted string) (string, error) {
	if strings.TrimSpace(encrypted) == "" {
		return "", nil
	}
	return s.cipher.Decrypt(encrypted)
}

// jobName 返回 file_sd 抓取任务名。
func (s *IntegrationService) jobName() string {
	if strings.TrimSpace(s.cfg.Integration.JobName) != "" {
		return s.cfg.Integration.JobName
	}
	return "middleware-integration"
}

// exporterNetwork 返回 Exporter 容器加入的主网络（Prometheus 所在网络）。
func (s *IntegrationService) exporterNetwork() string { return s.cfg.Integration.ExporterNetwork }

// exporterNetworks 解析逗号分隔的网络列表。
//
// 第一个网络是「监控面」（Prometheus 能抓到 Exporter），其余是「数据面」
// （Exporter 能连上被管实例）。jd 场景下两者不同：
// exporter_network: "middleware-ops_mwops,jd-nightjar"。
func (s *IntegrationService) exporterNetworks() []string {
	raw := s.cfg.Integration.ExporterNetwork
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	return out
}

// defaultEnvironment 返回集成默认环境。
func (s *IntegrationService) defaultEnvironment() string {
	if strings.TrimSpace(s.cfg.Integration.DefaultEnvironment) != "" {
		return s.cfg.Integration.DefaultEnvironment
	}
	return model.EnvDev
}

// record 写审计。
func (s *IntegrationService) record(ctx context.Context, op Operator, instanceID int64, action string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, InstanceID: instanceID,
		ActionType: action, Level: LevelLow, IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}

// writeFileAtomic 原子写入文件（临时文件 + rename），避免 Prometheus 读到半截 JSON。
func writeFileAtomic(path, payload string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(payload), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", path, err)
	}
	return nil
}
