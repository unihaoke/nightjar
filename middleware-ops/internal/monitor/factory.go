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
	// jobPrefix 与 prometheus.exporter_job_prefix 一致，用于在降级时仍能回传
	// 真实的选择器（用户排障时需要看到"平台到底查了什么"）。
	jobPrefix string
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
		primary:   NewPrometheusClient(cfg, store, log),
		backup:    NewSimulator(),
		log:       log,
		jobPrefix: cfg.Prometheus.ExporterJobPrefix,
	}
}

// Kind 返回实现类型。
func (f *fallbackClient) Kind() string { return f.primary.Kind() }

// Healthy 报告主客户端可用性。
func (f *fallbackClient) Healthy(ctx context.Context) bool { return f.primary.Healthy(ctx) }

// Snapshot 采集指标，失败时回退模拟器。
//
// 只有 primary 报错（上游不可达/协议错误）才降级；primary 正常返回但
// 选择器一条时序都没匹配到时，如实返回空快照并带上诊断 Note。
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
	if f.jobPrefix != "" {
		fallback.Selector = SelectorFor(target, f.jobPrefix)
	}
	return fallback, nil
}

// History 查询历史趋势，仅在查询失败时回退模拟器。
//
// 关键：**空结果不回退**。选择器写错时 Prometheus 会正常返回空序列，若在这里
// 用模拟器补齐，前端就会画出一条看起来很正常的曲线，把「实例其实没接上」
// 彻底掩盖掉——这正是早期版本"新增实例后看不到真实监控"的根因。
func (f *fallbackClient) History(ctx context.Context, target Target, metric string, r TimeRange) ([]Sample, error) {
	samples, err := f.primary.History(ctx, target, metric, r)
	if err == nil {
		return samples, nil
	}
	f.log.Warn("Prometheus 历史查询失败，回退内置模拟器",
		zap.String("instance", target.Name), zap.String("metric", metric), zap.Error(err))
	return f.backup.History(ctx, target, metric, r)
}

// Compare 多实例同指标对比，仅在查询失败时回退模拟器（理由同 History）。
func (f *fallbackClient) Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error) {
	out, err := f.primary.Compare(ctx, targets, metric)
	if err == nil {
		return out, nil
	}
	return f.backup.Compare(ctx, targets, metric)
}

// Selector 暴露底层 Prometheus 客户端的标签匹配串（实现 SelectorReporter）。
func (f *fallbackClient) Selector(target Target) string {
	if reporter, ok := f.primary.(SelectorReporter); ok {
		return reporter.Selector(target)
	}
	return ""
}
