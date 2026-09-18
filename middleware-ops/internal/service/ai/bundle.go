// Package ai 提供 AI 能力的业务实现：上下文组装、Prompt 模板、知识沉淀与代码分析。
//
// 引擎抽象与六道护栏位于 internal/engine，本包只编排「采集 → 组装 → 推理 → 落库」。
package ai

import (
	"fmt"
	"sort"
	"strings"

	"middleware-ops/internal/monitor"
)

// MetricSummary 是指标摘要（降采样后），不携带原始序列（5.2）。
type MetricSummary struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Unit        string   `json:"unit"`
	Latest      float64  `json:"latest"`
	Status      string   `json:"status"`
	Avg         float64  `json:"avg"`
	P95         float64  `json:"p95"`
	Min         float64  `json:"min"`
	Max         float64  `json:"max"`
	Slope       float64  `json:"slope"`
	Turning     int      `json:"turning_points"`
	Anomalies   []string `json:"anomaly_segments,omitempty"`
	// Trend 为人类可读趋势描述（上升/下降/平稳）。
	Trend string `json:"trend"`
}

// LogEvidence 是一条日志证据。
type LogEvidence struct {
	Source    string `json:"source"`
	Signature string `json:"signature"`
	Count     int    `json:"count"`
	LastSeen  string `json:"last_seen"`
	// Lines 为错误前后 N 行（已按预算裁剪）。
	Lines []string `json:"lines"`
}

// KnowledgeReference 是「参考案例」（5.7：相似命中不作为最终诊断）。
type KnowledgeReference struct {
	ID         int64   `json:"id"`
	Title      string  `json:"title"`
	MWType     string  `json:"mw_type"`
	Similarity float64 `json:"similarity"`
	Excerpt    string  `json:"excerpt"`
	AdoptCount int     `json:"adopt_count"`
}

// CodeEvidence 是代码位置证据（代码分析链路复用）。
type CodeEvidence struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Snippet string `json:"snippet"`
}

// Bundle 是一次诊断的全部采集上下文。
type Bundle struct {
	// InstanceName / MWType / Environment 为诊断对象元信息。
	InstanceName string `json:"instance_name"`
	MWType       string `json:"mw_type"`
	Environment  string `json:"environment"`
	GroupName    string `json:"group_name"`
	// Snapshot 为当前指标快照。
	Snapshot *monitor.Snapshot `json:"snapshot,omitempty"`
	// Summaries 为指标摘要（按异常优先级排序）。
	Summaries []MetricSummary `json:"summaries"`
	Logs      []LogEvidence   `json:"logs"`
	// Config 为只读配置片段（如 maxmemory-policy）。
	Config map[string]string `json:"config"`
	// References 为知识库参考案例。
	References []KnowledgeReference `json:"references"`
	// Missing 为未采集到的维度（工具超时/失败）。
	Missing []string `json:"missing"`
	// ToolSteps 记录工具调用决策卡。
	ToolSteps []string `json:"tool_steps"`
	// Truncated 为被预算截断的维度。
	Truncated []string `json:"truncated"`
	// DataSource 为指标来源（prometheus / disabled；平台不使用模拟数据）。
	DataSource string `json:"data_source"`
}

// AnomalyFirst 返回按「异常优先」排序的指标摘要。
//
// 排序规则：critical → warning → unknown → ok，同级按偏离程度降序，
// 保证上下文预算不足时优先保留最有诊断价值的指标（5.2）。
func (b *Bundle) AnomalyFirst() []MetricSummary {
	out := append([]MetricSummary(nil), b.Summaries...)
	rank := map[string]int{"critical": 0, "warning": 1, "unknown": 2, "ok": 3}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank[out[i].Status], rank[out[j].Status]
		if ri != rj {
			return ri < rj
		}
		return out[i].Latest > out[j].Latest
	})
	return out
}

// MetricsSection 生成指标相关的上下文分段文本。
func (b *Bundle) MetricsSection() string {
	if len(b.Summaries) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("实例：%s（类型 %s，环境 %s，分组 %s）\n", b.InstanceName, b.MWType, b.Environment, b.GroupName))
	sb.WriteString(fmt.Sprintf("指标来源：%s，采集时间：%s\n\n", b.DataSource, b.SnapshotTime()))
	sb.WriteString("指标摘要（降采样，非原始序列）：\n")
	for _, item := range b.AnomalyFirst() {
		sb.WriteString(fmt.Sprintf("- %s(%s) 当前=%.4f%s 状态=%s 均值=%.4f P95=%.4f 最小=%.4f 最大=%.4f 趋势=%s 斜率=%.6f 拐点=%d\n",
			item.DisplayName, item.Name, item.Latest, item.Unit, item.Status,
			item.Avg, item.P95, item.Min, item.Max, item.Trend, item.Slope, item.Turning))
		for _, seg := range item.Anomalies {
			sb.WriteString("    - 异常片段：" + seg + "\n")
		}
	}
	return sb.String()
}

// SnapshotTime 返回快照采集时间。
func (b *Bundle) SnapshotTime() string {
	if b.Snapshot == nil {
		return "未知"
	}
	return b.Snapshot.Collected.Format("2006-01-02 15:04:05")
}

// LogsSection 生成日志相关的上下文分段文本。
func (b *Bundle) LogsSection() string {
	if len(b.Logs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("近期错误日志（已按指纹聚合）：\n")
	for _, item := range b.Logs {
		sb.WriteString(fmt.Sprintf("- 来源=%s 指纹=%s 次数=%d 最近=%s\n", item.Source, item.Signature, item.Count, item.LastSeen))
		for _, line := range item.Lines {
			sb.WriteString("    | " + line + "\n")
		}
	}
	return sb.String()
}

// ConfigSection 生成配置相关的上下文分段文本。
func (b *Bundle) ConfigSection() string {
	if len(b.Config) == 0 {
		return ""
	}
	keys := make([]string, 0, len(b.Config))
	for k := range b.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("关键只读配置：\n")
	for _, k := range keys {
		sb.WriteString(fmt.Sprintf("- %s = %s\n", k, b.Config[k]))
	}
	return sb.String()
}

// ReferencesSection 生成「参考案例」分段文本。
//
// 明确标注为参考、不构成结论，避免相似命中被当成最终诊断（5.7）。
func (b *Bundle) ReferencesSection() string {
	if len(b.References) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("历史相似案例（仅作参考，不得直接作为结论）：\n")
	for _, item := range b.References {
		sb.WriteString(fmt.Sprintf("- [相似度 %.2f] %s（%s，采纳 %d 次）：%s\n",
			item.Similarity, item.Title, item.MWType, item.AdoptCount, item.Excerpt))
	}
	return sb.String()
}

// MissingSection 列出缺失维度，要求模型在「待确认项」中体现。
func (b *Bundle) MissingSection() string {
	if len(b.Missing) == 0 {
		return ""
	}
	return "缺失维度（采集失败或超时，相关结论必须标注为推测）：" + strings.Join(b.Missing, "、") + "\n"
}
