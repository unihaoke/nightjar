package service

import (
	"context"

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

// Deps 是全部服务的依赖集合与应用服务容器。
//
// 采用「显式依赖 + 统一容器」的方式，避免全局单例与隐式耦合；
// handler 只依赖 *service.Deps。
type Deps struct {
	Config *config.Config
	DB     *gorm.DB
	Log    *zap.Logger
	Cache  cache.Store
	Queue  cache.Queue
	Engine engine.Engine
	// EngineFactory 用于在配置变化后重建引擎并读取降级说明。
	EngineFactory *engine.Factory
	Monitor       monitor.Client
	Cipher        *utils.Cipher
	Tokens        *utils.TokenManager
	Guardrails    guardrail.Defaults
	StartedAt     int64

	// 仓储
	Instances     *repository.InstanceRepository
	Users         *repository.UserRepository
	Roles         *repository.RoleRepository
	Diagnoses     *repository.DiagnosisRepository
	Rules         *repository.AlertRuleRepository
	Alerts        *repository.AlertRepository
	AlertVec      *repository.AlertEmbeddingRepository
	Knowledge     *repository.KnowledgeRepository
	Audits        *repository.AuditRepository
	Approvals     *repository.ApprovalRepository
	Fixes         *repository.FixRepository
	Servers       *repository.ServerRepository
	LogEvents     *repository.LogEventRepository
	// AIAnalysisTasks 为异步 AI 分析任务仓储（提交—回调—超时）。
	AIAnalysisTasks *repository.AIAnalysisTaskRepository
	LogAlertRules *repository.LogAlertRuleRepository
	// LogAlertExclusions 为日志告警屏蔽项仓储（"这类错误不告警"）。
	LogAlertExclusions *repository.LogAlertExclusionRepository
	CodeAnalyses  *repository.CodeAnalysisRepository
	Notifies      *repository.NotificationLogRepository

	// 领域服务
	Auth         *AuthService
	Middleware   *MiddlewareService
	Metrics      *MetricsService
	AlertSvc     *AlertService
	Diagnoser    *DiagnoseService
	KnowledgeSvc *KnowledgeService
	Fix          *FixService
	Approval     *ApprovalService
	Audit        *AuditService
	LogAlert     *LogAlertService
	CodeAnalysis *CodeAnalysisService
	Integration  *IntegrationService
	Dashboard    *DashboardService
	Notifier     *NotifierService
	// Settings 为平台自管设置（AI 提供方 / 通知渠道），密钥加密落库、保存即生效。
	Settings *SettingService
	// LogPipeline 为「日志集成」的平台侧接收链路（Kafka 消费 → 日志事件）。
	LogPipeline *LogPipeline
	// LogAlertWorker 为日志告警的后处理（通知 + AI 代码分析），由定时任务驱动。
	LogAlertWorker *LogAlertWorker

	// 六道护栏实例（进程级共享，承载配额与熔断状态）
	Budget    *guardrail.Budget
	LoopGuard *guardrail.LoopGuard
	Timeout   *guardrail.Timeout
	Quality   *guardrail.Quality
	Cost      *guardrail.Cost
	SQLGuard  *guardrail.SQLGuard
	Registry  *guardrail.ToolRegistry
}

// registerToolRecorder 由 diagnose 服务实现，用于把工具调用审计接入护栏。
type toolAuditRecorder struct {
	audit *AuditService
}

// RecordToolCall 实现 guardrail.AuditRecorder（5.5：AI 每次工具调用独立审计）。
func (t *toolAuditRecorder) RecordToolCall(userID int64, username, tool string, detail map[string]any, result string) {
	if t.audit == nil {
		return
	}
	t.audit.RecordAsync(context.Background(), AuditEntry{
		UserID:     userID,
		Username:   username,
		ActionType: "ai_tool_call",
		Level:      "L0",
		Result:     result,
		Detail:     mergeDetail(detail, map[string]any{"tool": tool}),
	})
}

// mergeDetail 合并详情字段。
func mergeDetail(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		if _, exists := out[k]; !exists {
			out[k] = v
		}
	}
	return out
}
