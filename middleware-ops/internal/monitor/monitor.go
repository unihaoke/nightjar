// Package monitor 封装 Prometheus 查询能力。
//
// 设计依据（设计文档 4.2）：指标统一由 Prometheus + 官方 Exporter 采集，
// 平台通过 PromQL 查询，**不建自有指标表**；历史趋势由 Prometheus 保留策略控制。
//
// 当 prometheus.base_url 为空时使用内置指标模拟器（simulator.go），
// 保证离线环境下监控页面、告警规则评估与 AI 诊断链路仍然可用。
package monitor

import (
	"context"
	"time"
)

// Sample 是单个指标采样点。
type Sample struct {
	Timestamp time.Time `json:"timestamp"`
	Value     float64   `json:"value"`
}

// Metric 是一个指标序列及其摘要。
type Metric struct {
	Name string `json:"name"`
	// DisplayName 为中文展示名。
	DisplayName string `json:"display_name"`
	Unit        string `json:"unit"`
	// Category 用于前端分组：resource / performance / reliability。
	Category string `json:"category"`
	// Latest 为最新值（当前快照）。
	Latest float64 `json:"latest"`
	// Status 为语义化健康判定：ok / warning / critical / unknown。
	Status string `json:"status"`
	// Threshold 为告警参考阈值（仅用于前端展示）。
	WarningThreshold  float64 `json:"warning_threshold"`
	CriticalThreshold float64 `json:"critical_threshold"`
	// Series 为历史采样（降采样后）。
	Series []Sample `json:"series,omitempty"`
	// Expr 为实际执行的 PromQL（可追溯性）。
	Expr string `json:"expr,omitempty"`
}

// Snapshot 是一次实例当前指标采集结果。
type Snapshot struct {
	InstanceID int64     `json:"instance_id"`
	MWType     string    `json:"mw_type"`
	Collected  time.Time `json:"collected_at"`
	Metrics    []Metric  `json:"metrics"`
	// Source 取值 prometheus / simulator。
	Source string `json:"source"`
	// Degraded 为 true 表示 Prometheus 不可用，已降级为模拟数据。
	Degraded bool   `json:"degraded"`
	Note     string `json:"note"`
	// Selector 为本次查询实际使用的 PromQL 标签匹配串（接入自检与排障用）。
	//
	// 它是「平台能不能找到这个实例的指标」的唯一依据：选择器错一个字符，
	// 所有指标都会静默变成 unknown/0，因此必须随快照回传，便于前端与自检接口展示。
	Selector string `json:"selector"`
	// Matched 为成功取到数值的指标个数；Total 为画像中的指标总数。
	Matched int `json:"matched"`
	Total   int `json:"total"`
	// JobUp 为 up{job="<选择器里的 job>"} 的取值（nil 表示该 job 在 Prometheus 中不存在）。
	//
	// 用途：区分「job 根本没被 Prometheus 抓取」与「job 有抓取但标签对不上」，
	// 这两类故障的处理方式完全不同，靠一个空快照无法分辨。
	JobUp *float64 `json:"job_up"`
}

// Get 按名称取指标。
func (s *Snapshot) Get(name string) (Metric, bool) {
	for _, m := range s.Metrics {
		if m.Name == name {
			return m, true
		}
	}
	return Metric{}, false
}

// Value 返回指标最新值，不存在时返回 0 与 false。
func (s *Snapshot) Value(name string) (float64, bool) {
	m, ok := s.Get(name)
	if !ok {
		return 0, false
	}
	return m.Latest, true
}

// SummaryMap 把快照压缩为「指标名 → 最新值」映射，供 AI 上下文使用。
func (s *Snapshot) SummaryMap() map[string]float64 {
	out := make(map[string]float64, len(s.Metrics))
	for _, m := range s.Metrics {
		out[m.Name] = m.Latest
	}
	return out
}

// TimeRange 是查询时间范围。
type TimeRange struct {
	Start time.Time
	End   time.Time
	// Step 为查询步长。
	Step time.Duration
}

// Query 是 PromQL 查询请求。
type Query struct {
	// Expr 为 PromQL 表达式。
	Expr  string
	Range TimeRange
	// Instant 为 true 时只取瞬时值。
	Instant bool
}

// QueryResult 是查询结果。
type QueryResult struct {
	// Series 为「标签集合 → 采样序列」。
	Series map[string][]Sample `json:"series"`
	Source string              `json:"source"`
}

// Target 描述被查询的实例在 Prometheus 中的标签匹配依据。
type Target struct {
	InstanceID int64
	Name       string
	MWType     string
	// Job / Instance 对应 Prometheus 标签 job / instance。
	Job      string
	Instance string
}

// SelectorReporter 由能暴露 PromQL 选择器的客户端实现，供接入自检接口使用。
//
// 单独定义而不是并入 Client：模拟器没有「选择器」概念，不应被迫实现。
type SelectorReporter interface {
	// Selector 返回该实例在 Prometheus 中的标签匹配串。
	Selector(target Target) string
}

// LabelReporter 由能查询标签取值的客户端实现，用于把「Prometheus 里实际有什么」
// 回给使用者。
//
// 典型场景：纳管时把容器名（jd-redis-exporter）当成 job 名填进 prom_job，
// 于是选择器永远为空。此时最有用的不是"查不到"，而是"Prometheus 里的 job 是这几个、
// 该 job 下的实例标签是这几个"。
type LabelReporter interface {
	// LabelValues 返回某标签的全部取值；matchers 为可选的序列选择器（match[]）。
	LabelValues(ctx context.Context, label string, matchers ...string) ([]string, error)
}

// EndpointReporter 由能暴露上游查询地址的客户端实现（自检用于判定容器网络/DNS）。
//
// 有了它，自检才能确定地说"平台容器解析不了 jd-prometheus"，
// 而不是笼统地提示"请检查网络"。
type EndpointReporter interface {
	// Endpoint 返回 prometheus.base_url（模拟器返回空串）。
	Endpoint() string
}

// Client 是监控查询接口。
type Client interface {
	// Snapshot 采集某实例的当前指标。
	Snapshot(ctx context.Context, target Target) (*Snapshot, error)
	// History 采集某实例某指标的历史趋势。
	History(ctx context.Context, target Target, metric string, r TimeRange) ([]Sample, error)
	// Compare 多实例同指标对比。
	Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error)
	// Kind 返回实现类型（prometheus / simulator）。
	Kind() string
	// Healthy 报告上游可用性。
	Healthy(ctx context.Context) bool
}
