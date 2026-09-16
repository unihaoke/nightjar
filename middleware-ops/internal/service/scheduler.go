package service

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

// Scheduler 承载定时任务（3.4：定时采集、巡检、快照、日志扫描）。
type Scheduler struct {
	cron       *cron.Cron
	middleware *MiddlewareService
	alerts     *AlertService
	audit      *AuditService
	approvals  *ApprovalService
	log        *zap.Logger
	cfg        SchedulerConfig
}

// SchedulerConfig 是调度参数。
type SchedulerConfig struct {
	Enabled          bool
	HealthProbe      time.Duration
	RuleEval         time.Duration
	AlertCluster     time.Duration
	AuditSnapshot    string
	ApprovalExpire   time.Duration
	SnapshotDir      string
	ClusterThreshold float64
}

// NewScheduler 构造调度器。
func NewScheduler(cfg SchedulerConfig, middleware *MiddlewareService, alerts *AlertService, audit *AuditService, approvals *ApprovalService, log *zap.Logger) *Scheduler {
	return &Scheduler{
		cron:       cron.New(cron.WithSeconds()),
		middleware: middleware, alerts: alerts, audit: audit, approvals: approvals,
		log: log, cfg: cfg,
	}
}

// Start 注册并启动全部定时任务。
func (s *Scheduler) Start() error {
	if !s.cfg.Enabled {
		s.log.Info("定时任务已关闭（scheduler.enabled=false）")
		return nil
	}
	if s.cfg.HealthProbe <= 0 {
		s.cfg.HealthProbe = time.Minute
	}
	if s.cfg.RuleEval <= 0 {
		s.cfg.RuleEval = 30 * time.Second
	}
	if s.cfg.AlertCluster <= 0 {
		s.cfg.AlertCluster = 5 * time.Minute
	}
	if s.cfg.ApprovalExpire <= 0 {
		s.cfg.ApprovalExpire = time.Minute
	}

	// 健康巡检：探测全部实例 up/down（4.1）。
	if _, err := s.cron.AddFunc("@every "+durationSpec(s.cfg.HealthProbe), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		online, total, probeErr := s.middleware.ProbeAll(ctx)
		if probeErr != nil {
			s.log.Warn("健康巡检失败", zap.Error(probeErr))
			return
		}
		s.log.Info("健康巡检完成", zap.Int("online", online), zap.Int("total", total))
	}); err != nil {
		return fmt.Errorf("注册健康巡检任务: %w", err)
	}

	// 阈值评估：PromQL 指标 → 告警规则（4.4 主链路）。
	if _, err := s.cron.AddFunc("@every "+durationSpec(s.cfg.RuleEval), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result, evalErr := s.alerts.Evaluate(ctx)
		if evalErr != nil {
			s.log.Warn("告警规则评估失败", zap.Error(evalErr))
			return
		}
		if result.Triggered > 0 || result.Suppressed > 0 {
			s.log.Info("告警规则评估完成",
				zap.Int("evaluated", result.Evaluated), zap.Int("triggered", result.Triggered),
				zap.Int("merged", result.Merged), zap.Int("suppressed", result.Suppressed),
				zap.Duration("duration", result.Duration))
		}
	}); err != nil {
		return fmt.Errorf("注册告警评估任务: %w", err)
	}

	// 语义聚类：离线批处理，仅合并展示（4.4）。
	if _, err := s.cron.AddFunc("@every "+durationSpec(s.cfg.AlertCluster), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		result, clusterErr := s.alerts.Cluster(ctx, s.cfg.ClusterThreshold)
		if clusterErr != nil {
			s.log.Warn("告警语义聚类失败", zap.Error(clusterErr))
			return
		}
		if len(result.Clusters) > 0 {
			s.log.Info("告警语义聚类完成",
				zap.Int("processed", result.Processed), zap.Int("clusters", len(result.Clusters)))
		}
	}); err != nil {
		return fmt.Errorf("注册聚类任务: %w", err)
	}

	// 审批超时：30min 自动拒绝（4.6）。
	if _, err := s.cron.AddFunc("@every "+durationSpec(s.cfg.ApprovalExpire), func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		expired, expireErr := s.approvals.ExpireOverdue(ctx)
		if expireErr != nil {
			s.log.Warn("审批超时处理失败", zap.Error(expireErr))
			return
		}
		if len(expired) > 0 {
			s.log.Warn("审批超时自动拒绝", zap.Int("count", len(expired)))
		}
	}); err != nil {
		return fmt.Errorf("注册审批超时任务: %w", err)
	}

	// 审计快照：每日生成哈希链快照并校验（6.4）。
	spec := s.cfg.AuditSnapshot
	if spec == "" {
		spec = "0 10 0 * * *"
	}
	if _, err := s.cron.AddFunc(spec, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		snapshot, snapErr := s.audit.Snapshot(ctx)
		if snapErr != nil {
			s.log.Error("生成审计快照失败", zap.Error(snapErr))
			return
		}
		if !snapshot.Verified {
			s.log.Error("审计哈希链校验失败（疑似篡改）",
				zap.String("date", snapshot.SnapshotDate), zap.String("hash", snapshot.ChainHash))
			return
		}
		s.log.Info("审计快照生成完成",
			zap.String("date", snapshot.SnapshotDate), zap.Int64("logs", snapshot.LogCount),
			zap.String("file", snapshot.FilePath))
	}); err != nil {
		return fmt.Errorf("注册审计快照任务: %w", err)
	}

	s.cron.Start()
	s.log.Info("定时任务已启动",
		zap.Duration("health_probe", s.cfg.HealthProbe),
		zap.Duration("rule_eval", s.cfg.RuleEval),
		zap.Duration("alert_cluster", s.cfg.AlertCluster),
		zap.String("audit_snapshot", spec))
	return nil
}

// Stop 停止调度器。
func (s *Scheduler) Stop() {
	if s.cron == nil {
		return
	}
	ctx := s.cron.Stop()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		s.log.Warn("定时任务停止超时")
	}
}

// durationSpec 把时长转换为 cron 的 @every 表达式。
func durationSpec(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return d.Truncate(time.Second).String()
}
