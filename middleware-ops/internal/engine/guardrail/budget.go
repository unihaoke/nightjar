package guardrail

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"middleware-ops/internal/engine"
	"middleware-ops/internal/utils"
)

// boolToInt 把截断标记转换为数量（用于 Truncation.Dropped 的粗略计数）。
func boolToInt(flag bool) int {
	if flag {
		return 1
	}
	return 0
}

// Budget 是护栏①：上下文预算。
//
// 规则（5.2）：
//   - 单次诊断输入预算 8K tokens、输出 2K（可配置）；
//   - 超预算时截断，并**明确告知用户被截断的维度**；
//   - 时序指标降采样为摘要（均值/P95/斜率/拐点），不塞原始序列。
type Budget struct {
	maxInput  int
	maxOutput int
}

// NewBudget 构造上下文预算护栏。
func NewBudget(maxInput, maxOutput int) *Budget {
	if maxInput <= 0 {
		maxInput = 8192
	}
	if maxOutput <= 0 {
		maxOutput = 2048
	}
	return &Budget{maxInput: maxInput, maxOutput: maxOutput}
}

// MaxInput 返回输入预算。
func (b *Budget) MaxInput() int { return b.maxInput }

// MaxOutput 返回输出预算。
func (b *Budget) MaxOutput() int { return b.maxOutput }

// Truncation 描述一个被截断的维度。
type Truncation struct {
	// Dimension 取值如 metrics.history / logs.source / code.files。
	Dimension string `json:"dimension"`
	// Reason 说明截断原因（预算不足 / 条数上限 / 长度上限）。
	Reason string `json:"reason"`
	// Kept / Dropped 为保留与丢弃的数量（长度类截断以行数计）。
	Kept    int `json:"kept"`
	Dropped int `json:"dropped"`
}

// FitResult 是预算适配结果。
type FitResult struct {
	Messages []engine.Message
	Used     int
	Limit    int
	// Truncations 需要透出给用户，前端以「已截断维度」提示。
	Truncations []Truncation
}

// Truncated 报告是否发生了截断。
func (r FitResult) Truncated() bool { return len(r.Truncations) > 0 }

// Dimensions 返回被截断的维度名列表。
func (r FitResult) Dimensions() []string {
	out := make([]string, 0, len(r.Truncations))
	for _, t := range r.Truncations {
		out = append(out, t.Dimension)
	}
	return out
}

// FitMessages 按预算裁剪消息集合。
//
// 策略：固定 Prompt（system）永不裁剪；上下文按「分段 + 逐段降级」处理，
// 用户问题始终保留，最后才裁剪上下文；单条消息过长时按 rune 截断。
func (b *Budget) FitMessages(messages []engine.Message, contextSections []Section) FitResult {
	result := FitResult{Limit: b.maxInput}
	system := make([]engine.Message, 0, 1)
	rest := make([]engine.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == engine.RoleSystem {
			system = append(system, m)
			continue
		}
		rest = append(rest, m)
	}

	// 固定 Prompt 与用户问题先占额度。
	used := engine.EstimateMessagesTokens(system)
	kept := make([]engine.Message, 0, len(rest))
	for _, m := range rest {
		cost := engine.EstimateTokens(m.Content) + 4
		if used+cost <= b.maxInput {
			used += cost
			kept = append(kept, m)
			continue
		}
		// 用户问题不可丢弃：按剩余预算截断正文。
		remaining := b.maxInput - used
		if m.Role == engine.RoleUser && remaining > 64 {
			trimmed, dropped := utils.Truncate(m.Content, remaining*4)
			used += engine.EstimateTokens(trimmed) + 4
			kept = append(kept, engine.Message{Role: m.Role, Content: trimmed + "\n[问题因预算限制被截断]"})
			result.Truncations = append(result.Truncations, Truncation{
				Dimension: "user_query",
				Reason:    "输入预算不足",
				Kept:      len([]rune(trimmed)),
				Dropped:   boolToInt(dropped),
			})
			continue
		}
		result.Truncations = append(result.Truncations, Truncation{
			Dimension: "message",
			Reason:    "输入预算不足，整条消息未纳入",
			Kept:      0,
			Dropped:   engine.EstimateTokens(m.Content),
		})
	}

	// 上下文分段：按优先级排序后逐段纳入，超预算的段落记入截断维度。
	sorted := append([]Section(nil), contextSections...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
	var sb strings.Builder
	for _, sec := range sorted {
		text := SectionText(sec)
		cost := engine.EstimateTokens(text) + 8
		if used+cost <= b.maxInput {
			sb.WriteString(text)
			sb.WriteString("\n\n")
			used += cost
			continue
		}
		// 尝试压缩：只保留摘要字段。
		if sec.Summary != "" {
			summary := fmt.Sprintf("### %s（已压缩为摘要）\n%s\n", sec.Title, sec.Summary)
			sc := engine.EstimateTokens(summary) + 8
			if used+sc <= b.maxInput {
				sb.WriteString(summary)
				sb.WriteString("\n")
				used += sc
				result.Truncations = append(result.Truncations, Truncation{
					Dimension: sec.Dimension,
					Reason:    "输入预算不足，降级为摘要",
					Kept:      len([]rune(sec.Summary)),
					Dropped:   len([]rune(sec.Body)),
				})
				continue
			}
		}
		result.Truncations = append(result.Truncations, Truncation{
			Dimension: sec.Dimension,
			Reason:    "输入预算不足，该维度未纳入",
			Kept:      0,
			Dropped:   len([]rune(sec.Body)),
		})
	}
	if sb.Len() > 0 {
		kept = append(kept, engine.Message{Role: engine.RoleUser, Content: "【采集上下文】\n" + sb.String()})
	}

	final := append(system, kept...)
	result.Messages = final
	result.Used = engine.EstimateMessagesTokens(final)
	return result
}

// Section 是一个可独立截断的上下文分段。
type Section struct {
	// Dimension 为维度标识，与 Truncation.Dimension 对应。
	Dimension string
	Title     string
	Body      string
	// Summary 为预算不足时的降级摘要（可为空）。
	Summary string
	// Priority 越小越先纳入（0 为最高优先级）。
	Priority int
}

// SectionText 渲染分段正文。
func SectionText(s Section) string {
	if s.Title == "" {
		return s.Body
	}
	return fmt.Sprintf("### %s\n%s", s.Title, s.Body)
}

// SeriesSummary 是时序指标的摘要表示（降采样结果）。
type SeriesSummary struct {
	Metric string  `json:"metric"`
	Unit   string  `json:"unit,omitempty"`
	Count  int     `json:"count"`
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Avg    float64 `json:"avg"`
	P95    float64 `json:"p95"`
	Last   float64 `json:"last"`
	// Slope 为线性回归斜率（单位/采样点），用于判断趋势。
	Slope float64 `json:"slope"`
	// TurningPoints 为拐点数量（一阶差分符号变化次数）。
	TurningPoints int `json:"turning_points"`
	// AnomalySegments 为异常片段（超出均值 ±2σ 的连续区间描述）。
	AnomalySegments []string `json:"anomaly_segments,omitempty"`
}

// SummarizeSeries 把原始时序降采样为摘要，避免把原始序列塞进上下文（5.2）。
//
// 入参 values 按时间升序排列。
func SummarizeSeries(metric, unit string, values []float64) SeriesSummary {
	s := SeriesSummary{Metric: metric, Unit: unit, Count: len(values)}
	if len(values) == 0 {
		return s
	}
	sum := 0.0
	s.Min, s.Max = values[0], values[0]
	for _, v := range values {
		sum += v
		if v < s.Min {
			s.Min = v
		}
		if v > s.Max {
			s.Max = v
		}
	}
	s.Avg = sum / float64(len(values))
	s.Last = values[len(values)-1]
	s.P95 = percentile(values, 0.95)
	s.Slope = linearSlope(values)
	s.TurningPoints = turningPoints(values)
	s.AnomalySegments = anomalySegments(values, s.Avg, stddev(values, s.Avg))
	return s
}

// JSON 序列化摘要。
func (s SeriesSummary) JSON() string {
	b, err := json.Marshal(s)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// String 输出单行可读摘要。
func (s SeriesSummary) String() string {
	return fmt.Sprintf("%s: 采样=%d 均值=%.4f P95=%.4f 最小=%.4f 最大=%.4f 最新=%.4f 斜率=%.6f 拐点=%d",
		s.Metric, s.Count, s.Avg, s.P95, s.Min, s.Max, s.Last, s.Slope, s.TurningPoints)
}

// percentile 计算分位数（最近秩法，无需额外依赖）。
func percentile(values []float64, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := int(p*float64(len(sorted)-1) + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// linearSlope 使用最小二乘拟合斜率。
func linearSlope(values []float64) float64 {
	n := float64(len(values))
	if n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumXX float64
	for i, v := range values {
		x := float64(i)
		sumX += x
		sumY += v
		sumXY += x * v
		sumXX += x * x
	}
	den := n*sumXX - sumX*sumX
	if den == 0 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
}

// turningPoints 统计一阶差分的符号变化次数。
func turningPoints(values []float64) int {
	if len(values) < 3 {
		return 0
	}
	count := 0
	prev := sign(values[1] - values[0])
	for i := 2; i < len(values); i++ {
		cur := sign(values[i] - values[i-1])
		if cur != 0 && prev != 0 && cur != prev {
			count++
		}
		if cur != 0 {
			prev = cur
		}
	}
	return count
}

// sign 返回数值符号。
func sign(v float64) int {
	switch {
	case v > 0:
		return 1
	case v < 0:
		return -1
	default:
		return 0
	}
}

// stddev 计算标准差。
func stddev(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}
	var sum float64
	for _, v := range values {
		d := v - mean
		sum += d * d
	}
	return math.Sqrt(sum / float64(len(values)))
}

// anomalySegments 提取超出 ±2σ 的连续异常片段，输出人类可读描述。
func anomalySegments(values []float64, mean, sd float64) []string {
	if sd == 0 || len(values) == 0 {
		return nil
	}
	high := mean + 2*sd
	low := mean - 2*sd
	out := make([]string, 0, 2)
	start := -1
	for i, v := range values {
		abnormal := v > high || v < low
		if abnormal && start < 0 {
			start = i
		}
		if !abnormal && start >= 0 {
			out = append(out, describeSegment(start, i-1, values))
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, describeSegment(start, len(values)-1, values))
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

// describeSegment 描述单个异常片段。
func describeSegment(start, end int, values []float64) string {
	if start == end {
		return fmt.Sprintf("采样点 %d 显著偏离（值=%.4f）", start, values[start])
	}
	return fmt.Sprintf("采样点 %d-%d 持续偏离（峰值=%.4f）", start, end, values[end])
}
