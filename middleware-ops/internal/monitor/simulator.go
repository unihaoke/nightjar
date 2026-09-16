package monitor

import (
	"context"
	"fmt"
	"math"
	"time"

	"middleware-ops/internal/utils"
)

// simulator 是内置指标模拟器。
//
// 用途：在未接入 Prometheus 的开发/演示环境提供**确定性**的指标数据，
// 使监控页面、告警规则评估与 AI 诊断链路可以完整跑通。
//
// 确定性保证：同一实例 + 同一指标 + 同一时间桶 → 同一数值，
// 因此趋势图、规则评估与诊断上下文三者之间始终自洽。
type simulator struct {
	// step 为采样粒度，决定时间桶大小。
	step time.Duration
}

// NewSimulator 构造内置指标模拟器。
func NewSimulator() Client { return &simulator{step: time.Minute} }

// Kind 返回实现类型。
func (s *simulator) Kind() string { return "simulator" }

// Healthy 模拟器始终可用。
func (s *simulator) Healthy(context.Context) bool { return true }

// Snapshot 生成实例当前指标快照。
func (s *simulator) Snapshot(_ context.Context, target Target) (*Snapshot, error) {
	profile := ProfileOf(target.MWType)
	now := time.Now().UTC()
	bucket := now.Truncate(s.step).Unix()
	snapshot := &Snapshot{
		InstanceID: target.InstanceID,
		MWType:     target.MWType,
		Collected:  now,
		Source:     "simulator",
		Degraded:   true,
		Note:       "未配置 prometheus.base_url，当前为内置模拟指标（用于离线演示与联调）",
		Metrics:    make([]Metric, 0, len(profile.Metrics)),
	}
	for _, spec := range profile.Metrics {
		value := sampleValue(spec, target, bucket)
		snapshot.Metrics = append(snapshot.Metrics, Metric{
			Name: spec.Name, DisplayName: spec.DisplayName, Unit: spec.Unit, Category: spec.Category,
			Latest: value, Status: evaluateStatus(spec, value),
			WarningThreshold: spec.WarningThreshold, CriticalThreshold: spec.CriticalThreshold,
			Expr: fmt.Sprintf("# 模拟指标：%s（%s）", spec.Name, target.Name),
		})
	}
	return snapshot, nil
}

// History 生成指标历史趋势。
func (s *simulator) History(_ context.Context, target Target, metric string, r TimeRange) ([]Sample, error) {
	spec, ok := SpecOf(target.MWType, metric)
	if !ok {
		return nil, fmt.Errorf("未知指标: %s", metric)
	}
	if r.Step <= 0 {
		r.Step = 5 * time.Minute
	}
	if r.End.IsZero() {
		r.End = time.Now().UTC()
	}
	if r.Start.IsZero() {
		r.Start = r.End.Add(-6 * time.Hour)
	}
	out := make([]Sample, 0, 128)
	for ts := r.Start.Truncate(s.step); !ts.After(r.End); ts = ts.Add(r.Step) {
		out = append(out, Sample{Timestamp: ts, Value: sampleValue(spec, target, ts.Unix())})
	}
	return out, nil
}

// Compare 多实例同指标对比。
func (s *simulator) Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error) {
	out := make(map[string]float64, len(targets))
	for _, target := range targets {
		samples, err := s.History(ctx, target, metric, TimeRange{})
		if err != nil || len(samples) == 0 {
			continue
		}
		out[target.Name] = samples[len(samples)-1].Value
	}
	return out, nil
}

// sampleValue 计算指定时间桶的确定性数值。
//
// 形态：base + trend × 桶序号 + amplitude × sin(相位) + noise × (hash 均匀分布)。
// 相位与噪声种子均来自「实例 + 指标」的稳定哈希，因此同一实例的趋势形态固定，
// 但不同实例之间形态不同（避免大盘上所有实例完全同形）。
func sampleValue(spec MetricSpec, target Target, unix int64) float64 {
	g := spec.Generator
	// 用 5 分钟为趋势推进单位，避免指标在短时间内剧烈发散。
	bucketIndex := float64(unix/300) - 5800000
	phase := float64(hashUnit(target.InstanceID, target.Name, spec.Name, "phase")) * 2 * math.Pi
	value := g.Base +
		g.Trend*bucketIndex +
		g.Amplitude*math.Sin(float64(unix)/900+phase) +
		g.Noise*(hashUnit(target.InstanceID, target.Name, spec.Name, fmt.Sprintf("%d", unix/60))-0.5)*2

	// 比率型指标需要保持在合理区间内。
	switch spec.Name {
	case "keyspace_hit_rate", "buffer_pool_hit_rate", "cache_hit_rate":
		value = clamp(value, 80, 100)
	case "memory_usage_percent", "jvm_heap_used_percent":
		value = clamp(value, 1, 99)
	case "error_rate_5xx":
		value = math.Max(value, 0)
	case "broker_up", "node_count", "consumers", "cluster_status":
		value = math.Round(value)
	default:
		value = math.Max(value, 0)
	}
	return round(value, 4)
}

// hashUnit 把一组键映射为 [0,1) 的稳定数值。
func hashUnit(instanceID int64, parts ...string) float64 {
	seed := utils.SHA256Hex(fmt.Sprintf("%d|%s", instanceID, joinStrings(parts)))
	var acc uint64
	for i := 0; i < 8 && i < len(seed); i++ {
		acc = acc*16 + uint64(hexDigit(seed[i]))
	}
	return float64(acc%100000) / 100000.0
}

// joinStrings 以 "|" 连接字符串。
func joinStrings(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += "|"
		}
		out += item
	}
	return out
}

// hexDigit 返回十六进制字符数值。
func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return 0
	}
}

// clamp 限制数值区间。
func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// round 四舍五入到指定小数位。
func round(v float64, digits int) float64 {
	factor := math.Pow(10, float64(digits))
	return math.Round(v*factor) / factor
}
