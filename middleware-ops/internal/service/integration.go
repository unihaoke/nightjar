package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/docker"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
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
	servers    *repository.ServerRepository
	cipher     cipherCodec
	alerts     *AlertService
	audit      *AuditService
	approval   *ApprovalService
	monitor    monitor.Client
	log        *zap.Logger
	docker     *docker.Client
	dockerNote string
	// dockerOK 为启动期探活结果：socket 挂上了但没权限时，这里为 false，
	// 前端据此禁用按钮并展示 dockerNote 里的修复步骤。
	dockerOK bool
	// selfImageName 缓存平台自身镜像名（采集容器复用它）。
	selfImageName string
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
	servers *repository.ServerRepository,
	cipher cipherCodec,
	alerts *AlertService,
	audit *AuditService,
	approval *ApprovalService,
	monitorClient monitor.Client,
	log *zap.Logger,
) *IntegrationService {
	svc := &IntegrationService{
		cfg: cfg, instances: instances, servers: servers, cipher: cipher,
		alerts: alerts, audit: audit, approval: approval,
		monitor: monitorClient, log: log,
	}
	if cfg.Integration.DockerEnabled {
		client, err := docker.New(cfg.Integration.DockerHost)
		if err != nil {
			svc.dockerNote = "Docker 客户端初始化失败：" + err.Error()
			log.Warn("集成中心：Docker 客户端初始化失败，一键部署不可用", zap.Error(err))
		} else {
			svc.docker = client
			// 启动时真探一次：socket 挂上了但"没权限"是最常见的形态
			// （平台以非 root 用户运行，宿主 socket 是 root:docker 0660）。
			// 这里把结论直接写进 docker_note —— 集成中心页面顶部会显示它，
			// 不至于等使用者点保存才看到一句 permission denied。
			pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			pingErr := client.Ping(pingCtx)
			cancel()
			if pingErr == nil {
				svc.dockerOK = true
			} else {
				svc.dockerNote = describeDockerChannel(pingErr)
				log.Warn("集成中心：Docker 通道不可用，一键集成将不可用",
					zap.String("host", cfg.Integration.DockerHost), zap.Error(pingErr))
			}
		}
	} else {
		svc.dockerNote = "未启用一键部署（integration.docker_enabled=false）：平台只渲染配置，容器需人工启动"
	}
	return svc
}

// describeDockerChannel 把"启动期探活失败"翻译成可直接照做的修复步骤。
func describeDockerChannel(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		return "Docker 通道不可用：平台能读到 /var/run/docker.sock，但容器内用户没有权限。" +
			"修复：取宿主 docker 组的 GID（`stat -c '%g' /var/run/docker.sock` 或 `getent group docker | cut -d: -f3`），" +
			"在平台 .env 里设 DOCKER_GID=<该 GID>，然后 `docker compose up -d --force-recreate backend`。" +
			"（更严格的做法是用 docker-socket-proxy 只放行必要接口，见 docs/INTEGRATION.md 的安全边界）"
	case strings.Contains(msg, "no such file"):
		return "Docker 通道不可用：平台容器内没有 /var/run/docker.sock。" +
			"修复：确认 docker-compose.yml 里 backend 挂载了该 socket，然后 `docker compose up -d --force-recreate backend`；" +
			"rootless Docker 请把 INTEGRATION_DOCKER_HOST 指向 /run/user/<uid>/docker.sock 并挂载同一路径"
	case strings.Contains(msg, "connection refused"):
		return "Docker 通道不可用：socket 路径不对或 daemon 未运行（connection refused）。" +
			"核对 INTEGRATION_DOCKER_HOST 与宿主上实际路径"
	default:
		return "Docker 通道不可用：" + msg
	}
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
	// BootstrapAccount 表示由平台创建/更新只读监控账号（需要管理凭据）。
	BootstrapAccount *bool `json:"bootstrap_account"`
	// JoinPlatformNetwork 表示**把目标容器接入平台网络**（反向接网），默认关闭。
	//
	// 默认方向是平台把自己的 Exporter 接进目标网络（不改被管项目）。
	// 这个开关是给"平台接不进去"的场景留的人工兜底：例如目标在网络命名空间上受限、
	// 或运维明确要求所有被管容器都挂在平台网络上。
	// 代价：目标容器会因此获得平台网络的可达性（若它原本只在 internal 网络里，
	// 等于多了一条出网路径），所以必须由用户显式勾选。
	JoinPlatformNetwork *bool `json:"join_platform_network"`
	// AdminUsername / AdminPassword 为被管实例的管理凭据，仅用于执行固定模板 SQL。
	//
	// 安全约定：只在本次请求内存中使用，绝不落库、绝不写审计、绝不回显；
	// 平台只执行内置模板 SQL，不接受任意语句（见 monitoringAccountSQL）。
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
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
	// DeployNote 记录平台"为你做了什么"：一键部署结果，以及在哪个网络上发现了目标容器。
	DeployNote string `json:"deploy_note"`
	// JoinPlatformNetwork 表示该集成是否勾选了「把目标容器接入平台网络」。
	JoinPlatformNetwork bool   `json:"join_platform_network"`
	Selector            string `json:"selector"`
	AppliedAt           string `json:"applied_at"`
	LastError           string `json:"last_error"`
	HasPassword         bool   `json:"has_password"`
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
		DockerNote: s.dockerNote, DockerOK: s.dockerOK,
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

// asyncBudget 是后台"重活"的时间预算。
//
// 为什么需要后台：创建 Exporter / 建号都要经过 Docker，**首次还要拉镜像**
//（mysqld-exporter、mysql 客户端镜像动辄上百 MB）。这些同步做完会超过
// 前端 60s 的请求超时，表现为"点击集成→请求超时，然后 target up=0"。
// 因此写库与产物落盘照旧同步完成，重活交给后台，接口立刻返回并给出进度说明。
const asyncBudget = 10 * time.Minute

// runAsync 在后台执行重活，并把结果写回集成的备注/状态。
func (s *IntegrationService) runAsync(instanceID int64, name, what string, fn func(ctx context.Context) error) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), asyncBudget)
		defer cancel()
		if err := fn(ctx); err != nil {
			s.log.Warn("集成：后台任务失败",
				zap.String("integration", name), zap.String("task", what), zap.Error(err))
			s.setDeployNote(context.Background(), instanceID, what+"失败："+err.Error())
			s.markError(context.Background(), instanceID, what+"失败："+err.Error())
			return
		}
		s.log.Info("集成：后台任务完成", zap.String("integration", name), zap.String("task", what))
	}()
}

// pendingNote 是在后台任务完成前给使用者看的进度说明。
func pendingNote(what string) string {
	return "⏳ 已开始" + what + "（后台执行，首次会拉取镜像，通常 10–60 秒；完成后此处显示结果，可点「刷新」查看）"
}

// Preview 只做校验与渲染，不落库、不部署（供表单一键预览生成的配置）。
//
// 会带上**自动发现到的目标网络**：手工执行这段 compose 时，Exporter 必须能解析
// 被管实例的主机名，而平台网络里通常没有这个名字——只列平台网络的产物是跑不通的。
func (s *IntegrationService) Preview(ctx context.Context, in IntegrationInput) (*integration.Artifacts, error) {
	tpl, instance, err := s.build(in)
	if err != nil {
		return nil, err
	}
	networks := append([]string{}, s.exporterNetworks()...)
	discoveredNote := ""
	if s.docker != nil {
		res, note := s.resolveTarget(ctx, instance.Address.Host)
		if res != nil && res.Container != "" {
			for _, name := range res.Networks {
				networks = appendUnique(networks, name)
			}
			discoveredNote = note
		}
	}
	artifacts, err := integration.Render(tpl, instance, s.jobName(), s.fileSDPath(), strings.Join(networks, ","))
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	artifacts.NetworkNote = discoveredNote
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

	// 平台托管账号：需要账号的组件默认由平台代建（见 shouldBootstrapAccount）。
	// 口令留空时由平台生成——十六进制随机串天然不含需要转义的字符，
	// 使用者因此**不需要提前建号、也不需要自己想口令**。
	bootstrap := s.shouldBootstrapAccount(in, tpl.Type) && hasAdminCreds(in)
	if bootstrap && strings.TrimSpace(instance.Password) == "" {
		instance.Password = randomHexPassword(24)
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
		JoinPlatformNetwork: s.shouldJoinPlatformNetwork(in),
		CreatedBy:           operator.Username, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	item.Config["integration"] = meta.toMap()
	if err := s.instances.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	// 落盘 file_sd：写入失败不回滚实例，但把原因回传给调用方（可重试"重新应用"）。
	syncErr := s.SyncFileSD(ctx)

	// 重活（建号 → 拉 Exporter）放后台：见 asyncBudget 的说明。
	// 顺序很重要：先建只读账号，再拉起 Exporter；反过来的话 Exporter 会因认证失败反复重启。
	if s.deployEnabled(in) || s.shouldBootstrapAccount(in, tpl.Type) {
		s.setDeployNote(ctx, item.ID, pendingNote("创建只读账号并拉起 Exporter"))
		s.runAsync(item.ID, item.Name, "创建只读账号并拉起 Exporter", func(bgCtx context.Context) error {
			bootstrapNote, bootstrapErr, _ := s.bootstrapAccount(bgCtx, item, tpl, instance, in, operator)
			if bootstrapNote != "" {
				s.setDeployNote(context.Background(), item.ID, bootstrapNote)
			}
			deployErr := s.deploy(bgCtx, item, tpl, instance)
			if err := firstErr(bootstrapErr, deployErr); err != nil {
				return err
			}
			s.markApplied(context.Background(), item.ID)
			s.scheduleVerify(item.ID, item.Name, s.jobName())
			return nil
		})
	}

	if s.shouldCreateRules(in) {
		if err := s.createRecommendedRules(ctx, item, tpl, operator); err != nil {
			s.log.Warn("集成：自动创建告警规则失败", zap.String("integration", item.Name), zap.Error(err))
		}
	}
	s.record(ctx, operator, item.ID, "integration_create", map[string]any{
		"name": item.Name, "mw_type": tpl.Type, "address": instance.Address.Raw,
		"deploy": s.deployEnabled(in), "labels": instance.Labels,
		// 只记录"是否代为建号"与账号名，**绝不记录口令**
		"bootstrap_account": s.shouldBootstrapAccount(in, tpl.Type), "monitor_user": instance.Username,
	})

	if syncErr != nil {
		s.markError(ctx, item.ID, syncErr.Error())
		view := s.toView(ctx, *item)
		view.LastError = syncErr.Error()
		return &view, nil
	}
	// 保存即"声明成功"是不够的：Exporter 起没起来、Prometheus 抓没抓到，
	// 只有核验过才知道。异步核验失败会把原因写回 LastError，前端直接可见。
	// （核验在后台任务完成时触发，见上面的 runAsync。）
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
	// 编辑时若前端没带该字段（nil）则沿用原值，避免"编辑一次就把勾选丢掉"。
	if in.JoinPlatformNetwork != nil {
		meta.JoinPlatformNetwork = *in.JoinPlatformNetwork
	}
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
	// 重活放后台（与 Create 一致）：编辑保存同样会重建 Exporter/建号。
	if s.deployEnabled(in) || s.shouldBootstrapAccount(in, tpl.Type) {
		s.setDeployNote(ctx, item.ID, pendingNote("重建账号与 Exporter"))
		s.runAsync(item.ID, item.Name, "重建账号与 Exporter", func(bgCtx context.Context) error {
			bootstrapNote, bootstrapErr, _ := s.bootstrapAccount(bgCtx, item, tpl, instance, in, operator)
			if bootstrapNote != "" {
				s.setDeployNote(context.Background(), item.ID, bootstrapNote)
			}
			deployErr := s.deploy(bgCtx, item, tpl, instance)
			if err := firstErr(bootstrapErr, deployErr); err != nil {
				return err
			}
			s.markApplied(context.Background(), item.ID)
			s.scheduleVerify(item.ID, item.Name, s.jobName())
			return nil
		})
	}
	s.record(ctx, operator, item.ID, "integration_update", map[string]any{
		"name": item.Name, "mw_type": tpl.Type, "address": instance.Address.Raw,
		"password_changed": in.Password != "",
	})
	view := s.toView(ctx, *item)
	if syncErr != nil {
		s.markError(ctx, item.ID, syncErr.Error())
		view.LastError = syncErr.Error()
		return &view, nil
	}
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
	// 「重新应用」没有管理凭据，建不了号；这里把"还差什么"直接写进备注，
	// 让使用者知道该去「监控账号」点「重试建号」，而不是反复点重新应用。
	if !meta.AccountManaged && tpl.MonitorUser != "" {
		s.setDeployNote(ctx, item.ID,
			"该实例的只读监控账号尚未由平台创建：到「监控账号」点「重试建号」并填一次管理员凭据即可")
	} else {
		s.setDeployNote(ctx, item.ID, pendingNote("重建 Exporter"))
	}
	// 重建 Exporter 同样要经过 Docker（可能还要拉镜像）→ 放后台，避免请求超时。
	s.runAsync(item.ID, item.Name, "重建 Exporter", func(bgCtx context.Context) error {
		if err := s.deploy(bgCtx, item, tpl, instance); err != nil {
			return err
		}
		s.markApplied(context.Background(), item.ID)
		s.scheduleVerify(item.ID, item.Name, s.jobName())
		return nil
	})
	s.record(ctx, operator, item.ID, "integration_apply", map[string]any{"name": item.Name})
	if syncErr != nil {
		s.markError(ctx, item.ID, syncErr.Error())
		view := s.toView(ctx, *item)
		view.LastError = syncErr.Error()
		return &view, nil
	}
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
		entry := integration.EntryFor(integration.Instance{
			Name: item.Name, MWType: tpl.Type, Address: address,
			Labels: meta.Labels, Environment: item.Environment, GroupName: item.GroupName,
		})
		// 抓取目标指向平台自己的 Exporter 容器，而不是被管实例：
		// MySQL / Redis 自身没有 /metrics，抓实例地址只会得到 up=0。
		entry.Targets = []string{s.scrapeTarget(meta, address)}
		entries = append(entries, entry)
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

// scheduleVerify 异步核验集成是否真的"跑起来"（Exporter 被 Prometheus 抓到）。
//
// 为什么异步而不是同步等待：抓取目标经 http_sd 下发，refresh_interval 默认 30s，
// 同步等待会把一次 HTTP 请求拖到 40s 以上；后台核验则把结论写回实例，
// 前端刷新即可看到「待处理：<Prometheus 记录的失败原因>」，
// 使用者不必再去 Prometheus 的 /targets 页面翻 lastError。
func (s *IntegrationService) scheduleVerify(instanceID int64, name, job string) {
	if s.monitor == nil {
		return
	}
	if _, ok := s.monitor.(monitor.TargetReporter); !ok {
		return // 模拟器等不支持目标查询，跳过核验（不影响集成本身）
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		delay := 35 * time.Second // 等 http_sd 刷新 + 首次抓取
		for attempt := 1; attempt <= 3; attempt++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			reason := s.probeIntegration(ctx, job, name)
			if reason == "" {
				s.markApplied(context.Background(), instanceID)
				s.log.Info("集成核验通过", zap.String("integration", name), zap.String("job", job))
				return
			}
			if attempt == 3 {
				s.markError(context.Background(), instanceID, reason)
				s.log.Warn("集成核验未通过，已把原因写回集成",
					zap.String("integration", name), zap.String("reason", reason))
				return
			}
			delay = 25 * time.Second
		}
	}()
}

// probeIntegration 核验某个集成的抓取目标是否已 up。
//
// 返回空串表示通过；否则返回可读的失败原因（已翻译 lastError）。
func (s *IntegrationService) probeIntegration(ctx context.Context, job, name string) string {
	reporter, ok := s.monitor.(monitor.TargetReporter)
	if !ok {
		return ""
	}
	targets, err := reporter.Targets(ctx, job)
	if err != nil {
		// 查询本身失败（如 Prometheus 暂时不可达）不该判定为"集成失败"，
		// 这类问题由接入自检负责呈现。
		s.log.Debug("集成核验：查询 Prometheus 目标失败", zap.Error(err))
		return ""
	}
	found := false
	for _, target := range targets {
		// 同一 job（middleware-integration）下会有多个集成，按 instance_name 区分。
		if target.Labels["instance_name"] != "" && target.Labels["instance_name"] != name {
			continue
		}
		found = true
		if target.Health != "up" {
			return "Exporter 未跑通（up=0）：" + monitor.DescribeTargetError(target.LastError)
		}
	}
	if !found {
		return "Prometheus 中还没有该集成对应的抓取目标：确认抓取配置里有 middleware-integration 任务（http_sd 默认 30s 刷新），" +
			"必要时执行「重新应用」"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 只读监控账号托管（平台代为创建）
// ---------------------------------------------------------------------------

// monitoringAccountSQL 返回"创建只读监控账号"的固定模板 SQL。
//
// 设计约束（与平台的 fail-safe 护栏一致）：
//   - 只接受内置模板，不接受使用者传入任意 SQL；
//   - 幂等：CREATE USER IF NOT EXISTS + ALTER USER，可重复执行；
//   - 最小权限：只授监控必需的只读权限；
//   - MySQL 额外限制 MAX_USER_CONNECTIONS，避免高频抓取压垮实例。
//
// 口令由平台生成（十六进制随机串），因此不存在 SQL/DSN 转义问题。
func monitoringAccountSQL(mwType, username, password string) ([]string, error) {
	switch mwType {
	case integration.TypeMySQL:
		return []string{
			fmt.Sprintf("CREATE USER IF NOT EXISTS '%s'@'%%' IDENTIFIED WITH mysql_native_password BY '%s' WITH MAX_USER_CONNECTIONS 3", username, password),
			fmt.Sprintf("ALTER USER '%s'@'%%' IDENTIFIED WITH mysql_native_password BY '%s'", username, password),
			fmt.Sprintf("GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO '%s'@'%%'", username),
			"FLUSH PRIVILEGES",
		}, nil
	case integration.TypePG:
		return []string{
			fmt.Sprintf("DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN CREATE ROLE %s LOGIN PASSWORD '%s'; ELSE ALTER ROLE %s LOGIN PASSWORD '%s'; END IF; END $$", username, username, password, username, password),
			fmt.Sprintf("GRANT pg_monitor TO %s", username),
		}, nil
	default:
		return nil, fmt.Errorf("%s 不需要只读监控账号（口令由目标自身的鉴权配置决定）", mwType)
	}
}

// rotateAccountSQL 返回「账号改自己口令」的模板 SQL。
//
// 关键点：不需要管理员权限——MySQL 允许 ALTER USER USER()、PostgreSQL 允许
// ALTER ROLE CURRENT_USER，因此平台可以自助轮换（不要求使用者再填管理员凭据）。
func rotateAccountSQL(mwType, newPassword string) ([]string, error) {
	switch mwType {
	case integration.TypeMySQL:
		return []string{
			fmt.Sprintf("ALTER USER USER() IDENTIFIED WITH mysql_native_password BY '%s'", newPassword),
		}, nil
	case integration.TypePG:
		return []string{
			fmt.Sprintf("ALTER ROLE CURRENT_USER PASSWORD '%s'", newPassword),
		}, nil
	default:
		return nil, fmt.Errorf("%s 没有平台托管的只读账号，无需轮换", mwType)
	}
}

// dropAccountSQL 返回删除监控账号的模板 SQL（破坏性，需管理凭据）。
func dropAccountSQL(mwType, username string) ([]string, error) {
	switch mwType {
	case integration.TypeMySQL:
		return []string{fmt.Sprintf("DROP USER IF EXISTS '%s'@'%%'", username)}, nil
	case integration.TypePG:
		return []string{fmt.Sprintf("DROP ROLE IF EXISTS %s", username)}, nil
	default:
		return nil, fmt.Errorf("%s 没有平台托管的只读账号，无需删除", mwType)
	}
}

// bootstrapClientImage 是执行模板 SQL 用的一次性客户端镜像。
//
// 选官方客户端镜像而不是引入 mysql/postgres 驱动：平台二进制保持无数据库驱动依赖，
// 且"执行 SQL"这件事与"起容器"共用同一条已验证的 Docker 通道。
func bootstrapClientImage(mwType string) string {
	if mwType == integration.TypePG {
		return "postgres:15-alpine"
	}
	return "mysql:8.0"
}

// bootstrapCommand 组装一次性容器的执行命令。
//
// 口令通过**环境变量**传入（MYSQL_PWD / PGPASSWORD），不出现在命令行里，
// 因此既不会留在容器配置里，也不会出现在 `docker ps` 的输出中。
func bootstrapCommand(mwType string, address integration.Address, adminUser, adminPassword string, statements []string) ([]string, []string) {
	if mwType == integration.TypePG {
		// psql 用 PGPASSWORD 环境变量，SQL 通过 -c 逐条执行
		args := []string{"psql", "-h", address.Host, "-p", strconv.Itoa(address.Port),
			"-U", adminUser, "-d", "postgres", "-v", "ON_ERROR_STOP=1"}
		for _, stmt := range statements {
			args = append(args, "-c", stmt)
		}
		return args, []string{"PGPASSWORD=" + adminPassword}
	}
	// mysql 客户端：口令走 MYSQL_PWD 环境变量
	args := []string{"mysql", "-h", address.Host, "-P", strconv.Itoa(address.Port),
		"-u", adminUser, "--protocol=TCP"}
	for _, stmt := range statements {
		args = append(args, "-e", stmt)
	}
	return args, []string{"MYSQL_PWD=" + adminPassword}
}

// bootstrapAccount 执行「由平台创建只读监控账号」，返回说明、错误与是否真的执行了写操作。
//
// 三条路径共用（新建 / 编辑 / 重新应用），行为一致：
//   - 组件不需要账号（如 Redis）→ 什么都不做；
//   - 需要账号但本次没给管理凭据 → **不报错**，只提示"填凭据后点重新应用"，
//     避免使用者因为没填凭据而存不下集成；
//   - 生产环境 → 只创建审批工单（写被管库属 L2），不执行；
//   - 其余 → 用一次性客户端容器执行内置模板 SQL。
func (s *IntegrationService) bootstrapAccount(
	ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template,
	instance integration.Instance, in IntegrationInput, operator Operator,
) (note string, err error, performed bool) {
	if tpl.MonitorUser == "" {
		return "", nil, false // 该组件不需要平台建号（Redis 等口令由目标自身鉴权决定）
	}
	if !hasAdminCreds(in) {
		return "已跳过自动建号：本次未提供管理凭据（只需填一次管理员账号口令并点「重新应用」，平台即可自动建号）", nil, false
	}
	if instance.Environment == model.EnvProd && s.approval != nil {
		ticket, ticketErr := s.approval.Create(ctx, ApprovalRequest{
			InstanceID:  item.ID,
			Environment: instance.Environment,
			ActionType:  "integration_bootstrap",
			ActionDetail: map[string]any{
				"name": item.Name, "mw_type": tpl.Type,
				"address": instance.Address.Raw, "monitor_user": instance.Username,
				"sql": monitoringAccountSQLForDisplay(tpl.Type, instance.Username),
			},
			Reason: "生产环境由平台创建只读监控账号（L2）",
		}, operator)
		if ticketErr != nil {
			return "", fmt.Errorf("创建审批工单失败：%w", ticketErr), false
		}
		return "生产环境需审批：已创建工单 " + ticket.TicketID +
			"（工单内含将执行的 SQL）；审批通过后请点「重新应用」由平台建号", nil, false
	}
	note, err = s.ensureMonitoringAccount(ctx, instance, tpl, in.AdminUsername, in.AdminPassword)
	if err != nil {
		s.log.Warn("集成：创建只读监控账号失败",
			zap.String("integration", item.Name), zap.Error(err))
		return "", err, false
	}
	// 记下"该账号由平台代管"，供账号管理页展示与后续轮换/删除使用。
	s.writeMeta(ctx, item, func(meta *IntegrationMeta) { meta.AccountManaged = true })
	return note, nil, true
}

// AccountStatus 描述一个集成的监控账号现状（供「监控账号」管理界面）。
type AccountStatus struct {
	IntegrationID int64  `json:"integration_id"`
	Name          string `json:"name"`
	MWType        string `json:"mw_type"`
	Component     string `json:"component"`
	Username      string `json:"username"`
	Address       string `json:"address"`
	// Managed 表示该账号由平台创建（平台会记在集成元信息里）。
	Managed bool `json:"managed"`
	// HasPassword 表示平台持有该账号的口令（加密存储），因此可以自助轮换。
	HasPassword   bool   `json:"has_password"`
	RotatedAt     string `json:"rotated_at"`
	Grants        string `json:"grants"`
	SupportsMngmt bool   `json:"supports_management"`
	// LastError 取自集成核验结论（Exporter up / 认证失败等）。
	LastError string `json:"last_error"`
}

// ListAccounts 汇总所有集成的监控账号现状。
func (s *IntegrationService) ListAccounts(ctx context.Context, scope Scope) ([]AccountStatus, error) {
	items, err := s.List(ctx, scope)
	if err != nil {
		return nil, err
	}
	out := make([]AccountStatus, 0, len(items))
	for _, view := range items {
		item, getErr := s.instances.Get(ctx, view.InstanceID)
		if getErr != nil {
			continue
		}
		meta, _ := IntegrationMetaOf(*item)
		tpl, _ := integration.TemplateOf(meta.Template)
		out = append(out, AccountStatus{
			IntegrationID: view.InstanceID, Name: view.Name, MWType: view.MWType,
			Component: view.Component, Username: view.Username, Address: view.Address,
			Managed: meta.AccountManaged, HasPassword: view.HasPassword,
			RotatedAt: meta.AccountRotatedAt, Grants: grantSummary(tpl.Type),
			SupportsMngmt: tpl.MonitorUser != "",
			LastError:     view.LastError,
		})
	}
	return out, nil
}

// AccountRotateResult 是轮换口令的结果。
//
// NewPassword **只在这一次响应里出现**：平台不提供"查看已存口令"的接口（安全约定），
// 但使用者需要它来手工执行 Exporter 的 compose/docker run，所以轮换时给一次明文。
type AccountRotateResult struct {
	View *IntegrationView `json:"view"`
	// NewPassword 为本次轮换后的新口令，仅此一次返回；页面提示复制保存。
	NewPassword string `json:"new_password"`
}

// RotateAccountPassword 轮换平台托管的监控账号口令。
//
// 为什么不需要管理凭据：MySQL / PostgreSQL 都允许**账号修改自己的口令**，
// 而平台加密保存着该账号的口令，因此可以自助完成轮换——
// 轮换后立即重建 Exporter，使它用新口令抓取。
func (s *IntegrationService) RotateAccountPassword(ctx context.Context, id int64, operator Operator) (*AccountRotateResult, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidParam, "该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok || tpl.MonitorUser == "" {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "%s 不支持平台代管账号", item.MWType)
	}
	if s.docker == nil {
		return nil, apperr.New(apperr.CodeForbidden,
			"轮换口令需要平台能访问 Docker（integration.docker_enabled=true 且挂载 docker.sock）")
	}
	oldPassword, err := s.decrypt(item.PasswordEncrypted)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	address, err := integration.ParseAddress(meta.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	newPassword := randomHexPassword(24)
	instance := integration.Instance{
		Name: item.Name, MWType: tpl.Type, Address: address,
		Username: item.Username, Password: newPassword,
		Labels: meta.Labels, Options: meta.Options,
		Environment: item.Environment, GroupName: item.GroupName,
	}
	statements, err := rotateAccountSQL(tpl.Type, newPassword)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	// 关键：用**账号自己**的旧口令登录后执行 ALTER，因此不需要管理员凭据。
	if _, err := s.runClientSQL(ctx, instance, tpl, item.Username, oldPassword, statements); err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream,
			fmt.Errorf("轮换口令失败（可能账号已被删除，请重新保存并勾选自动建号）：%w", err))
	}
	encrypted, err := s.cipher.Encrypt(newPassword)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	item.PasswordEncrypted = encrypted
	if err := s.instances.Update(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	instance.Password = newPassword
	deployErr := s.deploy(ctx, item, tpl, instance)
	if deployErr != nil {
		s.log.Warn("集成：轮换后重建 Exporter 失败", zap.String("integration", item.Name), zap.Error(deployErr))
	}
	s.writeMeta(ctx, item, func(m *IntegrationMeta) {
		m.AccountRotatedAt = time.Now().UTC().Format(time.RFC3339)
	})
	s.record(ctx, operator, id, "integration_account_rotate", map[string]any{
		"name": item.Name, "monitor_user": item.Username,
	})
	s.scheduleVerify(item.ID, item.Name, s.jobName())
	// 明文口令只在这一个响应里返回（审计里仍然只有账号名）。
	return &AccountRotateResult{View: s.viewOf(ctx, *item), NewPassword: newPassword}, nil
}

// DropAccountInput 是删除监控账号的入参（需要管理凭据）。
type DropAccountInput struct {
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
}

// DropAccount 删除由平台创建的只读监控账号。
//
// 这是**破坏性写操作**（被管库里少一个账号）：必须提供管理凭据；
// 生产环境只创建审批工单，不直接执行。
func (s *IntegrationService) DropAccount(
	ctx context.Context, id int64, in DropAccountInput, operator Operator,
) (*IntegrationView, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidParam, "该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok || tpl.MonitorUser == "" {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "%s 不支持平台代管账号", item.MWType)
	}
	if s.docker == nil {
		return nil, apperr.New(apperr.CodeForbidden,
			"删除账号需要平台能访问 Docker（integration.docker_enabled=true 且挂载 docker.sock）")
	}
	if strings.TrimSpace(in.AdminUsername) == "" || strings.TrimSpace(in.AdminPassword) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "删除账号需要提供被管实例的管理账号与口令")
	}
	address, err := integration.ParseAddress(meta.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	instance := integration.Instance{
		Name: item.Name, MWType: tpl.Type, Address: address,
		Username: item.Username, Labels: meta.Labels, Options: meta.Options,
		Environment: item.Environment, GroupName: item.GroupName,
	}
	statements, err := dropAccountSQL(tpl.Type, item.Username)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	if item.Environment == model.EnvProd && s.approval != nil {
		ticket, ticketErr := s.approval.Create(ctx, ApprovalRequest{
			InstanceID: item.ID, Environment: item.Environment,
			ActionType: "integration_account_drop",
			ActionDetail: map[string]any{
				"name": item.Name, "mw_type": tpl.Type, "monitor_user": item.Username,
				"sql": statements,
			},
			Reason: "生产环境由平台删除只读监控账号（L2）",
		}, operator)
		if ticketErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Errorf("创建审批工单失败：%w", ticketErr))
		}
		view := s.toView(ctx, *item)
		view.DeployNote = "生产环境需审批：已创建工单 " + ticket.TicketID + "（删除账号 " + item.Username + "）"
		return &view, nil
	}
	if _, err := s.runClientSQL(ctx, instance, tpl, in.AdminUsername, in.AdminPassword, statements); err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, fmt.Errorf("删除监控账号失败：%w", err))
	}
	s.writeMeta(ctx, item, func(m *IntegrationMeta) {
		m.AccountManaged = false
		m.AccountRotatedAt = ""
	})
	s.record(ctx, operator, id, "integration_account_drop", map[string]any{
		"name": item.Name, "monitor_user": item.Username,
	})
	view := s.toView(ctx, *item)
	view.DeployNote = "已删除只读监控账号 " + item.Username + "；如需恢复请重新保存并勾选自动建号"
	return &view, nil
}

// requireDocker 是集成中心里所有"需要 Docker 通道"的操作的统一前置校验。
//
// 抽出来是为了让"平台没有 docker 权限"这件事始终给出一致的、可执行的提示，
// 并且可以被单元测试直接锁定（否则每条路径都要靠真实容器才能验证）。
func (s *IntegrationService) requireDocker(action string) error {
	if s.docker == nil {
		if s.dockerNote != "" {
			return apperr.Newf(apperr.CodeForbidden, "%s 不可用：%s", action, s.dockerNote)
		}
		return apperr.Newf(apperr.CodeForbidden,
			"%s 需要平台能访问 Docker：请确认 docker-compose.yml 里 backend 挂载了 "+
				"/var/run/docker.sock 且 INTEGRATION_DOCKER_ENABLED=true，"+
				"然后 docker compose up -d --force-recreate backend（或重跑 scripts/setup-jd-link.sh）", action)
	}
	// 启动期探活已经明确失败时，直接把那份"怎么修"的结论回传，
	// 而不是让使用者再撞一次 permission denied。
	if !s.dockerOK && s.dockerNote != "" {
		return apperr.Newf(apperr.CodeForbidden, "%s 不可用：%s", action, s.dockerNote)
	}
	return nil
}

// RetryAccountInput 是「重试建号/连接」的入参。
//
// 管理凭据是**可选**的：
//   - 带了凭据 → 幂等重跑建号 SQL（账号不存在就建、存在就重置口令并授权）+ 测试连接；
//   - 没带凭据 → 只测试已有监控账号能否连上，并重建 Exporter（用于"账号其实已经建好、
//     只是 Exporter 用了旧口令"这类场景）。
type RetryAccountInput struct {
	AdminUsername string `json:"admin_username"`
	AdminPassword string `json:"admin_password"`
}

// AccountRetryResult 是重试结果：既要能显示"做了什么"，也要能显示"还差什么"。
type AccountRetryResult struct {
	// Created 表示本次真的执行了建号 SQL。
	Created bool `json:"created"`
	// Connected 表示用监控账号成功连上了被管实例。
	Connected bool `json:"connected"`
	// OK 为总体是否就绪（账号可用）。
	OK bool `json:"ok"`
	// Message 为可读结论（成功说明或失败原因，已翻译成可执行的下一步）。
	Message string `json:"message"`
	// Output 为客户端容器输出的摘要（不含口令）。
	Output string `json:"output"`
	// View 为更新后的集成视图（前端直接刷新即可）。
	View *IntegrationView `json:"view"`
}

// RetryAccount 重试「建号 + 连接」：失败后不必重新编辑整个表单，在页面上点一下就能再来一次。
//
// 步骤：
//  1. （带凭据时）幂等重跑内置建号 SQL —— 账号不存在则创建，存在则重置口令并补齐授权；
//  2. 用监控账号做一次真实连接测试（SELECT 1），拿到"能连/不能连 + 为什么"；
//  3. 重建 Exporter（让它用当前口令抓取）并触发核验。
func (s *IntegrationService) RetryAccount(
	ctx context.Context, id int64, in RetryAccountInput, operator Operator,
) (*AccountRetryResult, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidParam, "该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok || tpl.MonitorUser == "" {
		return nil, apperr.Newf(apperr.CodeInvalidParam,
			"%s 不需要平台托管账号（口令由目标自身鉴权配置决定）", item.MWType)
	}
	if s.docker == nil {
		return nil, apperr.New(apperr.CodeForbidden,
			"重试建号需要平台能访问 Docker（integration.docker_enabled=true 且挂载 docker.sock）")
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

	result := &AccountRetryResult{}
	if err := s.requireDocker("重试建号"); err != nil {
		return nil, err
	}
	// 1) 建号（幂等）：口令沿用库内已存的那一个，保证与 Exporter 注入的一致。
	if hasAdminCreds(IntegrationInput{AdminUsername: in.AdminUsername, AdminPassword: in.AdminPassword}) {
		note, bootErr, performed := s.bootstrapAccount(ctx, item, tpl, instance, IntegrationInput{
			AdminUsername: in.AdminUsername, AdminPassword: in.AdminPassword,
		}, operator)
		result.Created = performed
		if bootErr != nil {
			result.Message = "建号失败：" + bootErr.Error()
			s.setDeployNote(ctx, item.ID, result.Message)
			s.markError(ctx, item.ID, result.Message)
			result.View = s.viewOf(ctx, *item)
			s.record(ctx, operator, id, "integration_account_retry", map[string]any{
				"name": item.Name, "created": performed, "connected": false, "ok": false,
			})
			return result, nil
		}
		if note != "" {
			result.Message = note
		}
	} else if !meta.AccountManaged {
		result.Message = "未提供管理凭据，且该账号不是平台创建的：只会测试连接与重建 Exporter。" +
			"要由平台建号，请填写一次管理员凭据后重试。"
	}

	// 2) 连接测试：拿真实结果，而不是让使用者去猜。
	probe, probeErr := s.runClientSQL(ctx, instance, tpl, item.Username, password, probeAccountSQL(tpl.Type))
	if probeErr == nil {
		result.Connected = true
		result.Output = truncateText(probe, 200)
	} else {
		result.Connected = false
		if result.Message != "" {
			result.Message += "；"
		}
		result.Message += "连接测试未通过：" + probeErr.Error()
	}

	// 3) 重建 Exporter + 核验（即使连接没通过也重建：这样拿到的是最新配置）。
	if deployErr := s.deploy(ctx, item, tpl, instance); deployErr != nil {
		if result.Message != "" {
			result.Message += "；"
		}
		result.Message += "重建 Exporter 失败：" + deployErr.Error()
	}
	result.OK = result.Connected
	if result.OK {
		if result.Message == "" {
			result.Message = "监控账号可用，Exporter 已按当前口令重建"
		} else if !result.Created {
			result.Message += "；连接测试通过，Exporter 已重建"
		}
		s.markApplied(ctx, item.ID)
	} else {
		s.markError(ctx, item.ID, result.Message)
	}
	s.setDeployNote(ctx, item.ID, result.Message)
	s.record(ctx, operator, id, "integration_account_retry", map[string]any{
		"name": item.Name, "created": result.Created, "connected": result.Connected, "ok": result.OK,
	})
	if result.OK {
		s.scheduleVerify(item.ID, item.Name, s.jobName())
	}
	result.View = s.viewOf(ctx, *item)
	return result, nil
}

// AccountProbeResult 是单独的"测试连接"结果。
type AccountProbeResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Output  string `json:"output"`
}

// ProbeAccount 只做连接测试（不建号、不改配置），供"账号到底能不能连"这个疑问。
func (s *IntegrationService) ProbeAccount(ctx context.Context, id int64) (*AccountProbeResult, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, apperr.New(apperr.CodeInvalidParam, "该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok || tpl.MonitorUser == "" {
		return nil, apperr.Newf(apperr.CodeInvalidParam,
			"%s 不需要平台托管账号（请直接看「统一监控」里的指标是否正常）", item.MWType)
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
	output, err := s.runClientSQL(ctx, instance, tpl, item.Username, password, probeAccountSQL(tpl.Type))
	if err != nil {
		return &AccountProbeResult{OK: false, Message: err.Error(), Output: truncateText(output, 300)}, nil
	}
	return &AccountProbeResult{
		OK: true, Message: fmt.Sprintf("监控账号 %s 连接正常", item.Username),
		Output: truncateText(output, 300),
	}, nil
}

// probeAccountSQL 是"用监控账号自证可用"的探测语句（只读，不产生副作用）。
func probeAccountSQL(mwType string) []string {
	switch mwType {
	case integration.TypeMySQL:
		return []string{"SELECT CURRENT_USER()", "SHOW GRANTS FOR CURRENT_USER()"}
	case integration.TypePG:
		return []string{"SELECT current_user"}
	default:
		return []string{"SELECT 1"}
	}
}

// viewOf 组装视图并在失败时带上 last_error（供重试结果回传）。
func (s *IntegrationService) viewOf(ctx context.Context, item model.MiddlewareInstance) *IntegrationView {
	view := s.toView(ctx, item)
	if meta, ok := IntegrationMetaOf(item); ok && meta.LastError != "" {
		view.LastError = meta.LastError
	}
	return &view
}

// runClientSQL 用一次性客户端容器执行 SQL（口令走环境变量，不出现在命令行）。
func (s *IntegrationService) runClientSQL(
	ctx context.Context, instance integration.Instance, tpl integration.Template,
	user, password string, statements []string,
) (string, error) {
	if s.docker == nil {
		return "", fmt.Errorf("未启用一键部署（integration.docker_enabled=false）")
	}
	if len(statements) == 0 {
		return "", nil
	}
	networks, resolvedHost, _ := s.targetNetworks(ctx, instance.Address.Host)
	if resolvedHost != "" {
		instance.Address.Host = resolvedHost
	}
	args, env := bootstrapCommand(tpl.Type, instance.Address, user, password, statements)
	spec := docker.ContainerSpec{
		Name:     integration.ContainerName(instance.Name) + "-sql",
		Image:    bootstrapClientImage(tpl.Type),
		Env:      env,
		Cmd:      args,
		Networks: networks,
		Labels:   map[string]string{"mwops.integration": instance.Name, "mwops.role": "sql"},
	}
	output, err := s.docker.RunOnce(ctx, spec, 60*time.Second)
	if err != nil {
		return output, fmt.Errorf("%w%s（容器输出：%s）", err, dockerHint(err), truncateText(output, 300))
	}
	return output, nil
}


// ensureMonitoringAccount 由平台创建/更新只读监控账号。
//
// 返回人可读的执行说明（写入集成元信息，供前端展示）；失败时返回错误由调用方上报。
func (s *IntegrationService) ensureMonitoringAccount(
	ctx context.Context, in integration.Instance, tpl integration.Template,
	adminUser, adminPassword string,
) (string, error) {
	if s.docker == nil {
		return "", fmt.Errorf("未启用一键部署（integration.docker_enabled=false），平台无法代为创建账号")
	}
	if strings.TrimSpace(adminUser) == "" || strings.TrimSpace(adminPassword) == "" {
		return "", fmt.Errorf("需要填写被管实例的管理账号与口令，平台才能创建只读监控账号")
	}
	statements, err := monitoringAccountSQL(tpl.Type, in.Username, in.Password)
	if err != nil {
		return "", err
	}
	// 网络自动发现：目标可能属于**另一个 compose 项目**（如 jd），
	// 使用者只需要填 "jd-mysql" / "interview-mysql" 这样的名字，平台自己找出
	// 该名字对应哪个容器、在哪张网络上，并把一次性容器接进去——
	// 这就是"同服务器不同 docker/compose 也不用改对方配置"的关键一步。
	networks, resolvedHost, note := s.targetNetworks(ctx, in.Address.Host)
	if len(networks) == 0 {
		return "", fmt.Errorf("平台无法确定目标所在网络：地址里的主机名与 docker 里的容器名/别名都不匹配 %s，"+
			"且平台没有可用的默认网络（请检查 integration.exporter_network 与 docker.sock 挂载）",
			s.targetHint(ctx, in.Address.Host))
	}
	if resolvedHost != "" && resolvedHost != in.Address.Host {
		in.Address.Host = resolvedHost
	}
	if note != "" {
		s.log.Info("集成：目标解析结果", zap.String("integration", in.Name), zap.String("detail", note))
	}

	args, env := bootstrapCommand(tpl.Type, in.Address, adminUser, adminPassword, statements)
	spec := docker.ContainerSpec{
		Name:     integration.ContainerName(in.Name) + "-bootstrap",
		Image:    bootstrapClientImage(tpl.Type),
		Env:      env,
		Cmd:      args,
		Networks: networks,
		Labels:   map[string]string{"mwops.integration": in.Name, "mwops.role": "bootstrap"},
	}
	output, err := s.docker.RunOnce(ctx, spec, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("创建只读监控账号失败：%w%s（容器输出：%s）",
			err, dockerHint(err), truncateText(output, 400))
	}
	s.log.Info("集成：只读监控账号已就绪",
		zap.String("integration", in.Name), zap.String("mw_type", tpl.Type), zap.String("user", in.Username))
	return fmt.Sprintf("平台已创建/更新只读监控账号 %s（权限：%s）", in.Username, grantSummary(tpl.Type)), nil
}

// randomHexPassword 生成十六进制随机口令。
//
// 用十六进制而不是 base64：口令会进入 SQL 与容器环境变量，
// 不含引号/反斜杠/@ 等字符，任何一层都不需要转义。
func randomHexPassword(bytesLen int) string {
	buf := make([]byte, bytesLen)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// monitoringAccountSQLForDisplay 返回供审批工单展示的 SQL（口令用占位符）。
//
// 工单会流转到审批人眼前，绝不能把口令写进去：这里只展示"平台将执行什么"。
func monitoringAccountSQLForDisplay(mwType, username string) []string {
	statements, err := monitoringAccountSQL(mwType, username, "<平台生成>")
	if err != nil {
		return nil
	}
	return statements
}

// grantSummary 返回权限摘要（供前端展示"平台做了什么"）。
func grantSummary(mwType string) string {
	if mwType == integration.TypePG {
		return "pg_monitor"
	}
	return "PROCESS, REPLICATION CLIENT, SELECT"
}

// truncateText 截断长文本（用于错误信息里的容器输出）。
func truncateText(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
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
	// JoinPlatformNetwork 记录用户是否显式要求「把目标容器接入平台网络」。
	// 存起来是为了「重新应用」时行为可复现（平台每次都会重新接网）。
	JoinPlatformNetwork bool `json:"join_platform_network"`
	// AccountManaged 表示只读监控账号由平台创建（供账号管理界面展示）。
	AccountManaged bool `json:"account_managed"`
	// AccountRotatedAt 为最近一次口令轮换时间（RFC3339）。
	AccountRotatedAt string `json:"account_rotated_at"`
}

func (m IntegrationMeta) toMap() map[string]any {
	return map[string]any{
		"template": m.Template, "address": m.Address, "labels": m.Labels, "options": m.Options,
		"job": m.Job, "image": m.Image, "container": m.Container, "exporter_port": m.ExporterPort,
		"applied_at": m.AppliedAt, "last_error": m.LastError,
		"created_by": m.CreatedBy, "created_at": m.CreatedAt,
		"updated_by": m.UpdatedBy, "updated_at": m.UpdatedAt, "deploy_note": m.DeployNote,
		"join_platform_network": m.JoinPlatformNetwork,
		"account_managed":       m.AccountManaged, "account_rotated_at": m.AccountRotatedAt,
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
	// 监控账号名留空时用模板默认值（如 mwops_exporter）：
	// 使用者因此**不需要提前建号、也不需要自己想一个账号名**。
	if instance.Username == "" && tpl.MonitorUser != "" {
		instance.Username = tpl.MonitorUser
	}
	// 更新场景允许口令留空（表示沿用已存口令），因此这里按"空口令"再校验一次：
	// 模板只需要账号非空，口令是否必填由 Validate 内部按组件类型判断。
	if err := tpl.Validate(instance); err != nil {
		return integration.Template{}, integration.Instance{}, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	return tpl, instance, nil
}

// deploy 按配置尝试一键拉起 Exporter 容器。
//
// 网络不需要任何人预先配置：平台先用 docker 反查目标容器在哪张网络上，
// 再把 Exporter 接进「监控面（平台网络）+ 目标网络」。被管项目因此
// 不需要建互联网络、不需要加别名，也不需要把 compose 文件交给平台。
func (s *IntegrationService) deploy(ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template, instance integration.Instance) error {
	if s.docker == nil {
		s.setDeployNote(ctx, item.ID, s.dockerNote)
		return nil
	}
	networks, resolvedHost, note := s.targetNetworks(ctx, instance.Address.Host)
	// 平台把用户填的名字换成"在目标网络上一定能解析"的名字（优先容器名）：
	// 用户填 `jd-redis`（别名）也不会因为别名只存在于旧网络而连不上。
	if resolvedHost != "" && resolvedHost != instance.Address.Host {
		instance.Address.Host = resolvedHost
	}
	env := tpl.RenderEnv(instance)
	args := tpl.RenderArgs(instance)
	spec := docker.ContainerSpec{
		Name:     integration.ContainerName(instance.Name),
		Image:    tpl.Image,
		Env:      envPairs(env),
		Cmd:      args,
		Networks: networks,
		Labels: map[string]string{
			"mwops.integration": instance.Name,
			"mwops.mw_type":     tpl.Type,
		},
	}
	action, id, err := s.docker.Ensure(ctx, spec)
	if err != nil {
		s.setDeployNote(ctx, item.ID, "一键部署失败："+err.Error())
		return fmt.Errorf("一键部署 Exporter 失败：%w%s", err, dockerHint(err))
	}
	// 顺带把平台自己接进目标网络，纳管实例的 TCP 健康探测才能成功——
	// 这一步同样是平台侧动作，被管项目无感。
	s.attachSelf(ctx, instance.Address.Host)
	joinNote := ""
	// 反向接网（改被管容器）是用户显式勾选的可选项，值持久化在集成元信息里，
	// 因此 Create / Update / 重新应用 三条路径行为一致。
	if meta, ok := IntegrationMetaOf(*item); ok && meta.JoinPlatformNetwork {
		joinNote = s.joinTargetToPlatformNetwork(ctx, instance.Address.Host)
	}
	if instance.Address.Host != item.Host && instance.Address.Host != "" {
		// 纳管实例的连接地址也统一成可解析名，避免"平台能抓指标、但健康探测失败"。
		item.Host = instance.Address.Host
		if err := s.instances.Update(ctx, item); err != nil {
			s.log.Debug("集成：同步解析后的地址失败", zap.Int64("instance_id", item.ID), zap.Error(err))
		}
	}

	done := fmt.Sprintf("Exporter 容器已%s（%s，ID %s）", actionLabel(action), spec.Name, shortID(id))
	if note != "" {
		done += "；" + note
	}
	if joinNote != "" {
		done += "；" + joinNote
	}
	s.setDeployNote(ctx, item.ID, done)
	return nil
}

// joinTargetToPlatformNetwork 把目标容器接入平台网络（用户显式勾选时才调用）。
//
// 返回人可读说明；失败只记日志并把原因写进备注，不影响集成本身。
func (s *IntegrationService) joinTargetToPlatformNetwork(ctx context.Context, host string) string {
	if s.docker == nil {
		return ""
	}
	res, _ := s.resolveTarget(ctx, host)
	if res == nil || res.ContainerID == "" {
		return "未找到目标容器，跳过「接入平台网络」"
	}
	platformNets := s.exporterNetworks()
	for _, network := range platformNets {
		if contains(res.Networks, network) {
			continue // 已经在平台网络上
		}
		if err := s.docker.ConnectNetwork(ctx, res.ContainerID, network); err != nil {
			s.log.Warn("集成：目标容器接入平台网络失败",
				zap.String("container", res.Container), zap.String("network", network), zap.Error(err))
			return fmt.Sprintf("目标容器 %s 接入平台网络 %s 失败：%s", res.Container, network, err)
		}
		s.log.Info("集成：已按要求把目标容器接入平台网络",
			zap.String("container", res.Container), zap.String("network", network))
	}
	return fmt.Sprintf("已按勾选把目标容器 %s 接入平台网络（%s）——该容器因此能反向看到平台网络",
		res.Container, strings.Join(platformNets, "、"))
}

// dockerHint 把 docker 调用失败翻译成可执行的下一步。
//
// 最常见的真实故障：平台容器里没有 /var/run/docker.sock（compose 里的挂载被注释着，
// 或 rootless Docker 的 socket 在别的路径），表现为
//   dial unix /var/run/docker.sock: connect: no such file or directory
// 原始信息看不出"该怎么修"，因此在这里补上。
func dockerHint(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "docker.sock") && strings.Contains(msg, "no such file"):
		return "。原因：平台容器内找不到 docker.sock，因此无法创建任何容器。" +
			"处理：在 docker-compose.yml 里取消 backend.volumes 的 " +
			"`/var/run/docker.sock:/var/run/docker.sock` 注释，然后 " +
			"`docker compose up -d --force-recreate backend`（或重跑 scripts/setup-jd-link.sh）。" +
			"若使用 rootless Docker，socket 通常在 /run/user/<uid>/docker.sock，请把 " +
			"INTEGRATION_DOCKER_HOST 指向它并挂载该路径"
	case strings.Contains(msg, "permission denied") && strings.Contains(msg, "docker.sock"):
		return "。原因：平台容器能读到 docker.sock 但无权访问（容器内用户不在宿主 docker 组）。" +
			"处理：取宿主 docker 组的 GID（`stat -c '%g' /var/run/docker.sock` 或 " +
			"`getent group docker | cut -d: -f3`），在平台 .env 里设 DOCKER_GID=<该 GID>，" +
			"然后 `docker compose up -d --force-recreate backend`；" +
			"也可以用 docker-socket-proxy 只放行必要接口（见 docs/INTEGRATION.md 的安全边界）"
	case strings.Contains(msg, "connection refused"):
		return "。原因：docker 守护进程不可达（socket 路径不对或 daemon 未运行）。" +
			"处理：核对 INTEGRATION_DOCKER_HOST 与宿主上实际路径"
	}
	return ""
}

// resolveTarget 用 docker 反查目标容器与其所在网络。
//
// 第二个返回值是人可读的说明（写进集成备注，让用户看到"平台做了什么"），
// 第三个返回值在解析失败时给出候选名，便于拼出可执行的报错。
func (s *IntegrationService) resolveTarget(ctx context.Context, host string) (res *docker.TargetResolution, note string) {
	if s.docker == nil || strings.TrimSpace(host) == "" {
		return &docker.TargetResolution{Host: host}, ""
	}
	res, err := s.docker.ResolveTarget(ctx, host)
	if err != nil {
		s.log.Debug("集成：解析目标容器失败", zap.String("host", host), zap.Error(err))
		return &docker.TargetResolution{Host: host}, ""
	}
	if res == nil {
		return &docker.TargetResolution{Host: host}, ""
	}
	if res.Container == "" {
		return res, ""
	}
	note = fmt.Sprintf("平台自动发现目标容器 %s（依据 %s），所在网络：%s",
		res.Container, res.MatchedBy, strings.Join(res.Networks, "、"))
	if !res.Running {
		note += "（注意：该容器当前未运行）"
	}
	return res, note
}

// targetNetworks 返回容器应加入的网络 = 平台网络（配置项）+ 目标容器所在网络（自动发现）。
//
// 第二个返回值是"替换后的可解析主机名"：用户填别名时，平台会换成容器名，
// 因为容器名在目标容器所在的任意网络上都能被内嵌 DNS 解析。
func (s *IntegrationService) targetNetworks(ctx context.Context, host string) ([]string, string, string) {
	networks := append([]string{}, s.exporterNetworks()...)
	res, note := s.resolveTarget(ctx, host)
	resolvedHost := host
	if res != nil {
		for _, name := range res.Networks {
			networks = appendUnique(networks, name)
		}
		if res.Host != "" {
			resolvedHost = res.Host
		}
	}
	return networks, resolvedHost, note
}

// targetHint 在解析失败时拼出"你可能想填什么"的提示。
func (s *IntegrationService) targetHint(ctx context.Context, host string) string {
	res, _ := s.resolveTarget(ctx, host)
	if res == nil || len(res.Candidates) == 0 {
		return ""
	}
	limit := res.Candidates
	if len(limit) > 8 {
		limit = limit[:8]
	}
	return "（docker 里现有的容器：" + strings.Join(limit, "、") + "）"
}

// scrapeTarget 返回 Prometheus 应当抓取的目标。
//
// 关键点：集成由平台拉起 Exporter 时，抓取目标必须是 **Exporter 容器**
// （mwops-exporter-<集成名>:<模板端口>），而不是被管实例本身——
// MySQL / Redis 自己不暴露 /metrics，抓实例地址必然是 up=0。
// 只有平台拿不到 Exporter 信息时才回落到实例地址。
func (s *IntegrationService) scrapeTarget(meta IntegrationMeta, address integration.Address) string {
	if strings.TrimSpace(meta.Container) != "" && meta.ExporterPort > 0 {
		return net.JoinHostPort(meta.Container, strconv.Itoa(meta.ExporterPort))
	}
	return address.HostPort()
}

// attachSelf 把平台自身（backend 容器）接入目标容器所在网络。
//
// 为什么需要：纳管实例保存后平台会做一次 TCP 健康探测，如果平台不在目标网络上，
// 探测必然失败，而这个失败**不是**用户的配置问题。与其要求用户去改宿主机上两个
// compose 项目的网络，不如平台自己接进去（可在挂载了 docker.sock 时完成）。
// 失败只记日志：探测失败会被降级为提示，不影响集成本身。
func (s *IntegrationService) attachSelf(ctx context.Context, host string) {
	if s.docker == nil {
		return
	}
	res, _ := s.resolveTarget(ctx, host)
	if res == nil || len(res.Networks) == 0 {
		return
	}
	networks := res.Networks
	self := strings.TrimSpace(os.Getenv("MWOPS_SELF_CONTAINER"))
	if self == "" {
		self = defaultSelfContainer
	}
	state, err := s.docker.Inspect(ctx, self)
	if err != nil || state == nil {
		s.log.Debug("集成：未找到平台自身容器，跳过网络接入", zap.String("container", self), zap.Error(err))
		return
	}
	current, err := s.docker.InspectDetail(ctx, self)
	if err != nil {
		current = nil
	}
	for _, network := range networks {
		if current != nil && contains(current.Networks, network) {
			continue
		}
		if err := s.docker.ConnectNetwork(ctx, state.ID, network); err != nil {
			s.log.Debug("集成：平台接入目标网络失败",
				zap.String("network", network), zap.Error(err))
			continue
		}
		s.log.Info("集成：平台已自动接入目标网络",
			zap.String("container", self), zap.String("network", network))
	}
}

// defaultSelfContainer 是平台自身（backend）的默认容器名。
const defaultSelfContainer = "mwops-backend"

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

// shouldJoinPlatformNetwork 判断本次是否把目标容器接入平台网络（默认否）。
func (s *IntegrationService) shouldJoinPlatformNetwork(in IntegrationInput) bool {
	if in.JoinPlatformNetwork == nil {
		return false
	}
	return *in.JoinPlatformNetwork
}

// shouldBootstrapAccount 判断本次是否由平台创建只读监控账号。
//
// 默认策略：**需要账号的组件（MySQL / PostgreSQL）默认由平台代建**——
// 使用者的预期是"我不建号，平台帮我建好"。显式传 false 才关闭。
// 未提供管理凭据时不会硬失败，而是跳过并给出提示（见 Create/Update 的调用处）。
func (s *IntegrationService) shouldBootstrapAccount(in IntegrationInput, mwType string) bool {
	if in.BootstrapAccount != nil {
		return *in.BootstrapAccount
	}
	switch mwType {
	case integration.TypeMySQL, integration.TypePG:
		return true
	default:
		return false
	}
}

// hasAdminCreds 判断本次请求是否带了可用的管理凭据。
func hasAdminCreds(in IntegrationInput) bool {
	return strings.TrimSpace(in.AdminUsername) != "" && strings.TrimSpace(in.AdminPassword) != ""
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
		Container: meta.Container, Image: meta.Image, DeployNote: meta.DeployNote,
		JoinPlatformNetwork: meta.JoinPlatformNetwork,
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
// exporter_network: "middleware-ops_mwops,my-project_default"。
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
