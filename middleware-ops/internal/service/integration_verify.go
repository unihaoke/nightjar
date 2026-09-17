package service

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/monitor"
)

// 本文件实现「重新核验」与周期自愈。
//
// 真实反馈：Redis 已经正常被监控了，集成状态却一直是「待处理」。
// 原因是核验只在**部署之后**跑 3 次（约 35s / 60s / 85s）；若外部原因在那之后才修好
// （改了地址、放通了安全组、Exporter 自己起来了），就再也没有人去核对一次，
// 状态只能等使用者再点一次「重新应用」——而重新应用要重填 SSH 凭据、还会真的重装，
// 代价与"再看一眼"完全不成比例。
//
// 因此补两条通道：
//  1. 「重新核验」（VerifyNow）：无副作用、不需要凭据，立刻按 Prometheus 现状刷新状态；
//  2. 周期自愈（ReverifyIntegrations）：只对当前处于「待处理」的集成做**由坏变好**的纠正，
//     且只有**明确看到目标 up** 才清除错误——Prometheus 不可达或目标缺失一律保持原状。

// VerifyNow 按 Prometheus 现状重新核验一个集成并刷新状态（无副作用：不重装、不需要凭据）。
func (s *IntegrationService) VerifyNow(ctx context.Context, id int64, operator Operator) (*IntegrationView, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, fmt.Errorf("该实例不是通过集成中心创建的")
	}
	job := meta.Job
	if job == "" {
		job = s.jobName()
	}
	reason, verified := s.probeIntegration(ctx, job, item.Name)
	switch {
	case !verified:
		s.setDeployNote(ctx, item.ID, "重新核验未完成：暂时读不到 Prometheus 的目标状态（不影响集成本身），请稍后再试")
	case reason == "":
		s.markApplied(ctx, item.ID)
		s.setDeployNote(ctx, item.ID, fmt.Sprintf("重新核验通过：抓取目标 %s 已 up（%s）",
			item.Name, time.Now().UTC().Format(time.RFC3339)))
		s.log.Info("集成：重新核验通过", zap.String("integration", item.Name), zap.String("job", job))
	default:
		s.markError(ctx, item.ID, reason)
		s.setDeployNote(ctx, item.ID, "重新核验未通过："+reason)
	}
	s.record(ctx, operator, item.ID, "integration_verify", map[string]any{
		"name": item.Name, "job": job, "ok": verified && reason == "",
	})
	return s.Get(ctx, id, Scope{})
}

// ReverifyIntegrations 周期自愈：对处于「待处理」的集成重新核验，好一个清一个。
//
// 返回检查数与清除数，便于调度日志观测。语义刻意保守：
//   - 只处理 meta.LastError 非空的集成（正常的不打扰）；
//   - 只做"由坏变好"：明确 up 才 markApplied（清除错误）；
//   - Prometheus 读不到时整轮跳过，绝不凭空制造新错误。
func (s *IntegrationService) ReverifyIntegrations(ctx context.Context) (checked, cleared int, err error) {
	reporter, ok := s.monitor.(monitor.TargetReporter)
	if !ok {
		return 0, 0, nil // 模拟器等不支持目标查询
	}
	items, listErr := s.List(ctx, Scope{})
	if listErr != nil {
		return 0, 0, listErr
	}
	// 同一个 job 下的目标只查一次（集成通常共用 middlewware-integration 这一个 job）。
	targetsByJob := map[string][]monitor.TargetStatus{}
	targetsFailed := map[string]bool{}
	for _, view := range items {
		item, getErr := s.instances.Get(ctx, view.InstanceID)
		if getErr != nil {
			continue
		}
		meta, metaOK := IntegrationMetaOf(*item)
		if !metaOK || meta.LastError == "" {
			continue
		}
		checked++
		job := meta.Job
		if job == "" {
			job = s.jobName()
		}
		if targetsFailed[job] {
			continue
		}
		statuses, cached := targetsByJob[job]
		if !cached {
			fetched, fetchErr := reporter.Targets(ctx, job)
			if fetchErr != nil {
				targetsFailed[job] = true
				s.log.Debug("周期自愈：读不到 Prometheus 目标状态，本轮跳过",
					zap.String("job", job), zap.Error(fetchErr))
				continue
			}
			statuses = fetched
			targetsByJob[job] = fetched
		}
		status, found := pickTargetStatus(statuses, item.Name)
		if !found || status.Health != "up" {
			continue // 仍然没起来/没有目标：保持原样（原因由部署路径与接入自检给）
		}
		s.markApplied(ctx, item.ID)
		s.setDeployNote(ctx, item.ID, fmt.Sprintf("周期核验通过：抓取目标 %s 已 up，已自动清除待处理（%s）",
			item.Name, time.Now().UTC().Format(time.RFC3339)))
		s.log.Info("集成：周期自愈清除了待处理", zap.String("integration", item.Name), zap.String("job", job))
		cleared++
	}
	return checked, cleared, nil
}
