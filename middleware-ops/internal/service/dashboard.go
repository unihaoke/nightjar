package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/repository"
)

// DashboardService 汇总全局大盘数据（8.2 系统模块）。
type DashboardService struct {
	instances *repository.InstanceRepository
	alerts    *repository.AlertRepository
	diagnoses *repository.DiagnosisRepository
	audits    *repository.AuditRepository
	approvals *repository.ApprovalRepository
	events    *repository.LogEventRepository
	knowledge *repository.KnowledgeRepository
	monitor   monitor.Client
	engine    engine.Engine
	notifier  *NotifierService
	cost      engineCostSnapshot
	cacheKind string
	queueLen  func(ctx context.Context) (int64, error)
	startedAt int64
	log       *zap.Logger
}

// engineCostSnapshot 抽象成本快照，避免大盘服务直接依赖护栏实现细节。
type engineCostSnapshot interface {
	Snapshot(userID int64) map[string]any
	Tripped() bool
}

// DashboardDeps 是大盘服务依赖。
type DashboardDeps struct {
	Instances *repository.InstanceRepository
	Alerts    *repository.AlertRepository
	Diagnoses *repository.DiagnosisRepository
	Audits    *repository.AuditRepository
	Approvals *repository.ApprovalRepository
	Events    *repository.LogEventRepository
	Knowledge *repository.KnowledgeRepository
	Monitor   monitor.Client
	Engine    engine.Engine
	Notifier  *NotifierService
	Cost      engineCostSnapshot
	CacheKind string
	QueueLen  func(ctx context.Context) (int64, error)
	StartedAt int64
	Log       *zap.Logger
}

// NewDashboardService 构造大盘服务。
func NewDashboardService(d DashboardDeps) *DashboardService {
	return &DashboardService{
		instances: d.Instances, alerts: d.Alerts, diagnoses: d.Diagnoses, audits: d.Audits,
		approvals: d.Approvals, events: d.Events, knowledge: d.Knowledge, monitor: d.Monitor,
		engine: d.Engine, notifier: d.Notifier, cost: d.Cost, cacheKind: d.CacheKind,
		queueLen: d.QueueLen, startedAt: d.StartedAt, log: d.Log,
	}
}

// Overview 返回大盘总览。
func (s *DashboardService) Overview(ctx context.Context, session *Session, scope Scope) (map[string]any, error) {
	// 大盘的实例口径 = 纳管域（见 MiddlewareDomainTypes）：日志集成不是"中间件实例"，
	// 否则会出现 by_type: {log: 1} 这种让人困惑的统计。
	online, offline, err := s.instances.CountStatus(ctx, scope.EnvScope, scope.GroupScope, MiddlewareDomainTypes())
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	byType, err := s.instances.CountByType(ctx, scope.EnvScope, scope.GroupScope, MiddlewareDomainTypes())
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	since := time.Now().UTC().Add(-24 * time.Hour)
	alertLevels, err := s.alerts.CountByLevel(ctx, since)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	alertTrend, err := s.alerts.Trend(ctx, time.Now().UTC().Add(-24*time.Hour), 24)
	if err != nil {
		s.log.Warn("查询告警趋势失败", zap.Error(err))
	}
	logStats, err := s.events.CountByStatus(ctx, since)
	if err != nil {
		s.log.Warn("查询日志事件统计失败", zap.Error(err))
	}
	knowledgeCounts, err := s.knowledge.CountByStatus(ctx)
	if err != nil {
		s.log.Warn("查询知识库统计失败", zap.Error(err))
	}
	feedback, err := s.diagnoses.CountFeedback(ctx)
	if err != nil {
		s.log.Warn("查询诊断反馈统计失败", zap.Error(err))
	}
	diagTotal, avgDuration, tokens, degraded, err := s.diagnoses.Stats(ctx, time.Time{})
	if err != nil {
		s.log.Warn("查询诊断统计失败", zap.Error(err))
	}
	pendingApprovals, err := s.approvals.CountPending(ctx)
	if err != nil {
		s.log.Warn("查询待审批数量失败", zap.Error(err))
	}
	actionCounts, err := s.audits.CountByAction(ctx, since)
	if err != nil {
		s.log.Warn("查询审计统计失败", zap.Error(err))
	}

	useful := feedback["useful"] + feedback["adopted"]
	rated := useful + feedback["useless"]
	adoptionRate := 0.0
	if rated > 0 {
		adoptionRate = float64(useful) / float64(rated)
	}

	var queuePending int64
	if s.queueLen != nil {
		if n, err := s.queueLen(ctx); err == nil {
			queuePending = n
		}
	}

	return map[string]any{
		"instances": map[string]any{
			"online": online, "offline": offline, "total": online + offline, "by_type": byType,
		},
		"alerts": map[string]any{
			"by_level": alertLevels, "trend": alertTrend,
			"active": alertLevels["warning"] + alertLevels["critical"],
		},
		"log_events": logStats,
		"knowledge":  knowledgeCounts,
		"diagnosis": map[string]any{
			"total": diagTotal, "avg_duration_ms": roundFloat(avgDuration, 1),
			"total_tokens": tokens, "degraded": degraded,
			"feedback": feedback, "adoption_rate": roundFloat(adoptionRate, 3),
		},
		"approvals": map[string]any{"pending": pendingApprovals},
		"audit":     map[string]any{"by_action": actionCounts},
		"platform": map[string]any{
			"version":        "1.0.0",
			"uptime_sec":     time.Now().Unix() - s.startedAt,
			"monitor_source": s.monitorKind(),
			"cache_kind":     s.cacheKind,
			"queue_pending":  queuePending,
			"engine":         engineStatusMap(s.engine),
			"cost":           s.costSnapshot(session),
			"cost_tripped":   s.costTripped(),
			"notify":         s.notifyStatus(),
		},
	}, nil
}

// monitorKind 返回监控数据源。
func (s *DashboardService) monitorKind() string {
	if s.monitor == nil {
		return "none"
	}
	return s.monitor.Kind()
}

// costSnapshot 返回当前用户成本快照。
func (s *DashboardService) costSnapshot(session *Session) map[string]any {
	if s.cost == nil || session == nil || session.User == nil {
		return map[string]any{}
	}
	return s.cost.Snapshot(session.User.ID)
}

// costTripped 返回熔断状态。
func (s *DashboardService) costTripped() bool {
	if s.cost == nil {
		return false
	}
	return s.cost.Tripped()
}

// notifyStatus 返回通知渠道状态。
func (s *DashboardService) notifyStatus() []map[string]any {
	if s.notifier == nil {
		return nil
	}
	return s.notifier.ChannelStatus()
}

// engineStatusMap 返回引擎状态。
func engineStatusMap(e engine.Engine) map[string]any {
	if e == nil {
		return map[string]any{"name": "none", "available": false}
	}
	st := e.Status()
	return map[string]any{
		"name": st.Name, "available": st.Available, "degraded": st.Degraded,
		"circuit_open": st.CircuitOpen, "consecutive_fails": st.ConsecutiveFails,
		"last_error": st.LastError,
	}
}
