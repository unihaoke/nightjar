package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"middleware-ops/internal/config"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// ContainerOptions 是服务容器构造参数。
type ContainerOptions struct {
	Config        *config.Config
	DB            *gorm.DB
	Log           *zap.Logger
	Cache         cache.Store
	Queue         cache.Queue
	Cipher        *utils.Cipher
	Tokens        *utils.TokenManager
	EngineFactory *engine.Factory
	Monitor       monitor.Client
	// AppURL 为 IM 卡片回跳地址前缀。
	AppURL string
}

// NewContainer 装配全部服务并返回容器。
//
// 装配顺序：仓储 → 护栏 → 基础设施服务（审计/通知）→ 领域服务 → 大盘与调度。
func NewContainer(opt ContainerOptions) (*Deps, error) {
	cfg := opt.Config
	deps := &Deps{
		Config:        cfg,
		DB:            opt.DB,
		Log:           opt.Log,
		Cache:         opt.Cache,
		Queue:         opt.Queue,
		Engine:        opt.EngineFactory.Engine(),
		EngineFactory: opt.EngineFactory,
		Monitor:       opt.Monitor,
		Cipher:        opt.Cipher,
		Tokens:        opt.Tokens,
		Guardrails:    guardrail.NewDefaults(cfg),
		StartedAt:     time.Now().Unix(),
	}

	// 仓储层
	deps.Instances = repository.NewInstanceRepository(opt.DB)
	deps.Users = repository.NewUserRepository(opt.DB)
	deps.Roles = repository.NewRoleRepository(opt.DB)
	deps.Diagnoses = repository.NewDiagnosisRepository(opt.DB)
	deps.Rules = repository.NewAlertRuleRepository(opt.DB)
	deps.Alerts = repository.NewAlertRepository(opt.DB)
	deps.AlertVec = repository.NewAlertEmbeddingRepository(opt.DB)
	deps.Knowledge = repository.NewKnowledgeRepository(opt.DB)
	deps.Audits = repository.NewAuditRepository(opt.DB)
	deps.Approvals = repository.NewApprovalRepository(opt.DB)
	deps.Fixes = repository.NewFixRepository(opt.DB)
	deps.Servers = repository.NewServerRepository(opt.DB)
	deps.CodeRepos = repository.NewCodeRepoRepository(opt.DB)
	deps.LogEvents = repository.NewLogEventRepository(opt.DB)
	deps.CodeAnalyses = repository.NewCodeAnalysisRepository(opt.DB)
	deps.Notifies = repository.NewNotificationLogRepository(opt.DB)

	// 六道护栏实例
	g := deps.Guardrails
	deps.Budget = guardrail.NewBudget(g.InputTokenBudget, g.OutputTokenBudget)
	toolNames := []string{"metrics_snapshot", "log_events", "config_read", "knowledge_search"}
	deps.LoopGuard = guardrail.NewLoopGuard(g.MaxSteps, g.LoopRepeatThreshold, toolNames)
	// 工具超时与整任务超时分别取配置值（5.4）。
	toolTimeout := cfg.AIEngine.ThirdParty.Timeout.ToolCall
	taskDeadline := cfg.AIEngine.ThirdParty.Timeout.TaskDeadline
	if cfg.AIEngine.SelfHosted.Timeout.ToolCall > toolTimeout {
		toolTimeout = cfg.AIEngine.SelfHosted.Timeout.ToolCall
	}
	if cfg.AIEngine.SelfHosted.Timeout.TaskDeadline > taskDeadline {
		taskDeadline = cfg.AIEngine.SelfHosted.Timeout.TaskDeadline
	}
	deps.Timeout = guardrail.NewTimeout(toolTimeout, taskDeadline, g.MaxConcurrency)
	deps.Quality = guardrail.NewQuality(0.5)
	deps.Cost = guardrail.NewCost(g.DailyTokenQuota, g.PerUserDailyTokenQuota)
	deps.SQLGuard = guardrail.NewSQLGuard(g.SQLDefaultLimit, g.SQLMaxLimit, g.SQLTableAllowlist)
	deps.Registry = guardrail.NewToolRegistry()

	// 基础设施服务
	snapshotDir := "./data/audit-snapshots"
	deps.Audit = NewAuditService(deps.Audits, snapshotDir, opt.Log)
	deps.Notifier = NewNotifierService(&cfg.Notify, opt.AppURL, opt.Cache, deps.Notifies, opt.Log)

	// 领域服务
	deps.Auth = NewAuthService(cfg, deps.Users, deps.Roles, opt.Tokens, opt.Log)
	deps.Middleware = NewMiddlewareService(deps.Instances, opt.Cipher, opt.Monitor, deps.Audit, opt.Log)
	deps.Metrics = NewMetricsService(deps.Instances, opt.Monitor, opt.Log)
	deps.AlertSvc = NewAlertService(deps.Rules, deps.Alerts, deps.AlertVec, deps.Instances, opt.Cache,
		opt.Monitor, deps.Engine, deps.Notifier, deps.Audit, opt.Log)
	deps.KnowledgeSvc = NewKnowledgeService(deps.Knowledge, deps.Diagnoses, deps.Audit, opt.Log)
	deps.Approval = NewApprovalService(deps.Approvals, deps.Notifier, deps.Audit, opt.Log)
	deps.Fix = NewFixService(deps.Instances, deps.Fixes, deps.Approval, deps.Audit, deps.SQLGuard, deps.Registry, dryRunExecutor{}, opt.Log)
	deps.LogAlert = NewLogAlertService(deps.Servers, deps.LogEvents, deps.CodeRepos, deps.Audit, opt.Log)
	// 集成中心：把 Exporter 暴露 + Prometheus 抓取 + 实例纳管 + 告警规则串成一次点击。
	deps.Integration = NewIntegrationService(cfg, deps.Instances, deps.Servers, opt.Cipher,
		deps.AlertSvc, deps.Audit, deps.Approval, opt.Monitor, opt.Log)
	// 启动时对齐 file_sd：清理已删除集成残留的目标，并保证文件存在
	// （Prometheus 的 file_sd_configs 指向它，文件缺失只会在日志里刷错误）。
	if cfg.Integration.Enabled {
		if err := deps.Integration.SyncFileSD(context.Background()); err != nil {
			opt.Log.Warn("集成中心：初始化 file_sd 失败", zap.Error(err))
		}
	}
	redactor := NewRedactor(&cfg.Security)
	deps.CodeAnalysis = NewCodeAnalysisService(cfg, deps.Engine, deps.LogEvents, deps.CodeRepos,
		deps.CodeAnalyses, redactor, deps.Audit, deps.Cost, opt.Log)

	deps.Diagnoser = NewDiagnoseService(DiagnoseDeps{
		Instances: deps.Instances, Diagnoses: deps.Diagnoses, Knowledge: deps.Knowledge,
		LogEvents: deps.LogEvents, Engine: deps.Engine, Factory: opt.EngineFactory, Monitor: opt.Monitor,
		Store: opt.Cache, Audit: deps.Audit, Notifier: deps.Notifier,
		Budget: deps.Budget, Loop: deps.LoopGuard, Timeout: deps.Timeout, Quality: deps.Quality,
		Cost: deps.Cost, Registry: deps.Registry, Log: opt.Log,
		Settings: DiagnoseSettings{
			InputBudget:     g.InputTokenBudget,
			OutputBudget:    g.OutputTokenBudget,
			MaxConcurrency:  g.MaxConcurrency,
			CacheTTL:        g.CacheTTL,
			VectorThreshold: g.VectorReferenceThreshold,
			LLMRetry:        g.LLMRetry,
		},
	})
	deps.Dashboard = NewDashboardService(DashboardDeps{
		Instances: deps.Instances, Alerts: deps.Alerts, Diagnoses: deps.Diagnoses,
		Audits: deps.Audits, Approvals: deps.Approvals, Events: deps.LogEvents,
		Knowledge: deps.Knowledge, Monitor: opt.Monitor, Engine: deps.Engine,
		Notifier: deps.Notifier, Cost: deps.Cost, CacheKind: opt.Cache.Kind(),
		QueueLen: func(ctx context.Context) (int64, error) {
			if opt.Queue == nil {
				return 0, nil
			}
			return opt.Queue.Len(ctx)
		},
		StartedAt: deps.StartedAt, Log: opt.Log,
	})

	// 只读工具注册（5.5：AI 只挂载只读工具集）。
	if err := mountReadOnlyTools(deps); err != nil {
		return nil, err
	}
	// 工具调用审计接入护栏。
	toolCtxRecorder := &toolAuditRecorder{audit: deps.Audit}
	_ = toolCtxRecorder
	return deps, nil
}

// config2 类型别名已移除：诊断服务的配置切片由 DiagnoseSettings 显式承载。

// readOnlyTool 实现 guardrail.ReadOnlyTool。
type readOnlyTool struct {
	name     string
	decision string
	fn       func(tc *guardrail.ToolContext, args map[string]any) (any, error)
}

// Name 返回工具名。
func (t *readOnlyTool) Name() string { return t.name }

// Decision 返回决策卡说明。
func (t *readOnlyTool) Decision() string { return t.decision }

// ReadOnly 声明工具只读。
func (t *readOnlyTool) ReadOnly() bool { return true }

// Run 执行只读采集。
func (t *readOnlyTool) Run(tc *guardrail.ToolContext, args map[string]any) (any, error) {
	return t.fn(tc, args)
}

// mountReadOnlyTools 注册 AI 可挂载的只读工具。
func mountReadOnlyTools(deps *Deps) error {
	tools := []guardrail.ReadOnlyTool{
		&readOnlyTool{
			name:     "metrics_snapshot",
			decision: "判断是否存在资源瓶颈、性能拐点或可用性下降",
			fn: func(tc *guardrail.ToolContext, _ map[string]any) (any, error) {
				item, err := deps.Instances.Get(tc.Context, tc.Instance.ID)
				if err != nil {
					return nil, err
				}
				return deps.Monitor.Snapshot(tc.Context, ToTarget(*item))
			},
		},
		&readOnlyTool{
			name:     "log_events",
			decision: "确认是否存在应用侧报错或堆栈线索",
			fn: func(tc *guardrail.ToolContext, args map[string]any) (any, error) {
				service := tc.Instance.Name
				if v, ok := args["service"].(string); ok && v != "" {
					service = v
				}
				items, _, err := deps.LogEvents.List(tc.Context, repository.LogEventFilter{
					ServiceName: service, From: ptrTime(time.Now().Add(-24 * time.Hour)),
				}, 20, 0)
				if err != nil {
					return nil, err
				}
				return items, nil
			},
		},
		&readOnlyTool{
			name:     "config_read",
			decision: "核对配置项与当前现象是否匹配",
			fn: func(tc *guardrail.ToolContext, _ map[string]any) (any, error) {
				item, err := deps.Instances.Get(tc.Context, tc.Instance.ID)
				if err != nil {
					return nil, err
				}
				return item.Config, nil
			},
		},
		&readOnlyTool{
			name:     "knowledge_search",
			decision: "查找历史相似案例作为参考（不作为结论）",
			fn: func(tc *guardrail.ToolContext, args map[string]any) (any, error) {
				keyword, _ := args["keyword"].(string)
				items, _, err := deps.Knowledge.List(tc.Context, repository.KnowledgeFilter{
					Keyword: keyword, Status: "published",
				}, 5, 0)
				if err != nil {
					return nil, err
				}
				return items, nil
			},
		},
	}
	for _, tool := range tools {
		if err := deps.Registry.Mount(tool); err != nil {
			return fmt.Errorf("挂载只读工具失败: %w", err)
		}
	}
	return nil
}

// ptrTime 返回时间指针。
func ptrTime(t time.Time) *time.Time { return &t }
