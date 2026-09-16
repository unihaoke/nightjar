package monitor

import (
	"context"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/pkg/cache"
)

// fallbackClient 在主客户端失败时回退到模拟器，避免监控链路整体不可用。
type fallbackClient struct {
	primary Client
	backup  Client
	log     *zap.Logger
}

// New 依据配置创建监控客户端。
//
// prometheus.base_url 为空 → 直接使用模拟器（离线可运行）；
// 否则使用 Prometheus，并在查询失败时回退模拟器（可用性优先，见 10.）。
func New(cfg *config.Config, store cache.Store, log *zap.Logger) Client {
	if cfg.Prometheus.BaseURL == "" {
		log.Warn("未配置 prometheus.base_url，监控数据使用内置模拟器")
		return NewSimulator()
	}
	return &fallbackClient{
		primary: NewPrometheusClient(cfg, store, log),
		backup:  NewSimulator(),
		log:     log,
	}
}

// Kind 返回实现类型。
func (f *fallbackClient) Kind() string { return f.primary.Kind() }

// Healthy 报告主客户端可用性。
func (f *fallbackClient) Healthy(ctx context.Context) bool { return f.primary.Healthy(ctx) }

// Snapshot 采集指标，失败时回退模拟器。
func (f *fallbackClient) Snapshot(ctx context.Context, target Target) (*Snapshot, error) {
	snapshot, err := f.primary.Snapshot(ctx, target)
	if err == nil {
		return snapshot, nil
	}
	f.log.Warn("Prometheus 查询失败，回退内置模拟器", zap.String("instance", target.Name), zap.Error(err))
	fallback, fallbackErr := f.backup.Snapshot(ctx, target)
	if fallbackErr != nil {
		return nil, err
	}
	fallback.Note = "Prometheus 查询失败（" + err.Error() + "），已回退内置模拟数据"
	return fallback, nil
}

// History 查询历史，失败时回退模拟器。
func (f *fallbackClient) History(ctx context.Context, target Target, metric string, r TimeRange) ([]Sample, error) {
	samples, err := f.primary.History(ctx, target, metric, r)
	if err == nil && len(samples) > 0 {
		return samples, nil
	}
	return f.backup.History(ctx, target, metric, r)
}

// Compare 多实例对比，失败时回退模拟器。
func (f *fallbackClient) Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error) {
	out, err := f.primary.Compare(ctx, targets, metric)
	if err == nil && len(out) > 0 {
		return out, nil
	}
	return f.backup.Compare(ctx, targets, metric)
}
