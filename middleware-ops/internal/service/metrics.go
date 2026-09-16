package service

import (
	"context"
	"sort"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/repository"
)

// MetricsService 提供统一监控查询能力（4.2）。
//
// 指标全部来自 Prometheus（或内置模拟器），平台不落自有指标表。
type MetricsService struct {
	instances *repository.InstanceRepository
	monitor   monitor.Client
	log       *zap.Logger
}

// NewMetricsService 构造监控服务。
func NewMetricsService(instances *repository.InstanceRepository, mon monitor.Client, log *zap.Logger) *MetricsService {
	return &MetricsService{instances: instances, monitor: mon, log: log}
}

// Snapshot 查询实例当前指标。
func (s *MetricsService) Snapshot(ctx context.Context, instanceID int64, scope Scope) (*monitor.Snapshot, error) {
	item, err := s.instance(ctx, instanceID, scope)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.monitor.Snapshot(ctx, ToTarget(*item))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	return snapshot, nil
}

// History 查询指标历史趋势。
func (s *MetricsService) History(ctx context.Context, instanceID int64, metric string, r monitor.TimeRange, scope Scope) ([]monitor.Sample, error) {
	item, err := s.instance(ctx, instanceID, scope)
	if err != nil {
		return nil, err
	}
	if metric == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须指定指标名")
	}
	if r.End.IsZero() {
		r.End = time.Now().UTC()
	}
	if r.Start.IsZero() {
		r.Start = r.End.Add(-6 * time.Hour)
	}
	if r.Step <= 0 {
		r.Step = 5 * time.Minute
	}
	samples, err := s.monitor.History(ctx, ToTarget(*item), metric, r)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	return samples, nil
}

// CompareItem 是多实例对比结果项。
type CompareItem struct {
	InstanceID   int64   `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	MWType       string  `json:"mw_type"`
	Environment  string  `json:"environment"`
	GroupName    string  `json:"group_name"`
	Value        float64 `json:"value"`
	Unit         string  `json:"unit"`
}

// Compare 执行多实例对比。
func (s *MetricsService) Compare(ctx context.Context, ids []int64, metric string, scope Scope) ([]CompareItem, error) {
	if metric == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须指定对比指标")
	}
	items, err := s.instances.All(ctx, repository.InstanceFilter{
		EnvScope: scope.EnvScope, GroupScope: scope.GroupScope,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	selected := make([]model.MiddlewareInstance, 0, len(items))
	for _, item := range items {
		if len(idSet) > 0 && !idSet[item.ID] {
			continue
		}
		selected = append(selected, item)
	}
	if len(selected) == 0 {
		return nil, apperr.New(apperr.CodeNotFound, "没有可对比的实例")
	}
	out := make([]CompareItem, 0, len(selected))
	for _, item := range selected {
		history, histErr := s.monitor.History(ctx, ToTarget(item), metric, monitor.TimeRange{
			Start: time.Now().Add(-30 * time.Minute), End: time.Now(), Step: time.Minute,
		})
		value := 0.0
		if histErr == nil && len(history) > 0 {
			value = history[len(history)-1].Value
		}
		unit := ""
		if profile := monitor.ProfileOf(item.MWType); profile.MWType != "" {
			for _, spec := range profile.Metrics {
				if spec.Name == metric {
					unit = spec.Unit
					break
				}
			}
		}
		out = append(out, CompareItem{
			InstanceID: item.ID, InstanceName: item.Name, MWType: item.MWType,
			Environment: item.Environment, GroupName: item.GroupName, Value: value, Unit: unit,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out, nil
}

// Catalog 返回指定中间件类型的指标目录（供前端筛选与规则配置）。
func (s *MetricsService) Catalog(mwType string) map[string]any {
	profile := monitor.ProfileOf(mwType)
	metrics := make([]map[string]any, 0, len(profile.Metrics))
	for _, spec := range profile.Metrics {
		metrics = append(metrics, map[string]any{
			"name": spec.Name, "display_name": spec.DisplayName, "unit": spec.Unit,
			"category": spec.Category, "warning_threshold": spec.WarningThreshold,
			"critical_threshold": spec.CriticalThreshold,
			// threshold_mode 决定前端如何解读阈值方向：
			//   higher_worse 越高越差 / lower_worse 越低越差 / bool_down 正常-异常 / 空 表示纯观测
			"threshold_mode": string(spec.Mode),
		})
	}
	return map[string]any{
		"mw_type": mwType,
		"metrics": metrics,
		"types":   monitor.SupportedTypes(),
	}
}

// instance 查询实例并校验数据权限。
func (s *MetricsService) instance(ctx context.Context, id int64, scope Scope) (*model.MiddlewareInstance, error) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !inScope(item, scope) {
		return nil, apperr.New(apperr.CodeScopeDenied, "该实例不在你的数据权限范围内")
	}
	return item, nil
}

// MonitorKind 返回监控数据来源类型。
func (s *MetricsService) MonitorKind() string {
	if s.monitor == nil {
		return "none"
	}
	return s.monitor.Kind()
}
