package guardrail

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Quality 是护栏⑤：质量护栏（防幻觉）。
//
// 规则（5.6）：
//   - 输出强制结构化：根因 / 证据引用 / 置信度 / 建议 / 影响 / 待确认项；
//   - 每条结论必须引用证据（指标名 / 日志行 / 代码位置），无证据标注「推测」；
//   - 内置典型故障评测集，引擎/模型切换后回归；
//   - 用户「有用/没用」反馈 + 采纳率统计作为质量基线。
type Quality struct {
	// minConfidence 为置信度下限，低于该值会在报告中标注「低置信度」。
	minConfidence float64
}

// NewQuality 构造质量护栏。
func NewQuality(minConfidence float64) *Quality {
	if minConfidence <= 0 {
		minConfidence = 0.5
	}
	return &Quality{minConfidence: minConfidence}
}

// Evidence 是一条证据引用。
type Evidence struct {
	// Source 取值 metric / log / config / code / knowledge / user_input。
	Source string `json:"source"`
	// Ref 为具体引用位置（指标名、文件名:行号、日志时间点等）。
	Ref string `json:"ref"`
	// Detail 为证据内容摘要。
	Detail string `json:"detail"`
	// Speculative 为 true 表示该结论无硬证据，属于推测。
	Speculative bool `json:"speculative"`
}

// Suggestion 是一条修复建议。
type Suggestion struct {
	Action string `json:"action"`
	// Horizon 取值 immediate / short_term / long_term。
	Horizon string `json:"horizon"`
	// Risk 为风险等级（低/中/高）。
	Risk string `json:"risk"`
	// Level 为操作级别（L0/L1/L2），决定是否需要审批（4.6）。
	Level string `json:"level"`
	// EvidenceRefs 指向支撑该建议的证据序号。
	EvidenceRefs []int `json:"evidence_refs,omitempty"`
}

// Report 是结构化诊断报告（质量护栏的强制 schema）。
type Report struct {
	RootCause      string       `json:"root_cause"`
	Confidence     float64      `json:"confidence"`
	Evidence       []Evidence   `json:"evidence"`
	Suggestions    []Suggestion `json:"suggestions"`
	ImpactScope    string       `json:"impact_scope"`
	PendingConfirm []string     `json:"pending_confirm"`
	// Speculative 为 true 表示整体结论属于推测（无证据支撑）。
	Speculative bool `json:"speculative"`
	// LowConfidence 标注低置信度结论。
	LowConfidence bool `json:"low_confidence"`
	// MissingDimensions 记录未采集到的上下文维度。
	MissingDimensions []string `json:"missing_dimensions,omitempty"`
	// AIAvailable 为 false 表示当前为降级结论（规则引擎/半自动）。
	AIAvailable bool `json:"ai_available"`
	// EngineNote 记录引擎说明与截断提示。
	EngineNote string `json:"engine_note,omitempty"`
	// GroundedRatio 为有证据支撑的结论占比（评测与采纳率之外的量化指标）。
	GroundedRatio float64 `json:"grounded_ratio"`
}

// Normalize 校验并补全模型输出，返回规范化报告与质量提示。
//
// 这一步是「防幻觉」的落点：任何缺少证据的结论都会被显式标注为推测，
// 而不是被静默接受为事实。
func (q *Quality) Normalize(report *Report, missing []string) (*Report, []string) {
	var warnings []string
	if report == nil {
		return &Report{
			RootCause:         "模型未返回可用结论",
			Confidence:        0,
			Speculative:       true,
			LowConfidence:     true,
			AIAvailable:       false,
			MissingDimensions: missing,
			EngineNote:        "输出解析失败，已降级为待人工确认",
		}, []string{"模型输出无法解析为结构化报告"}
	}

	report.MissingDimensions = missing
	if report.Evidence == nil {
		report.Evidence = []Evidence{}
	}
	if report.Suggestions == nil {
		report.Suggestions = []Suggestion{}
	}
	if report.PendingConfirm == nil {
		report.PendingConfirm = []string{}
	}

	// 置信度裁剪到 [0,1]。
	if report.Confidence < 0 {
		report.Confidence = 0
	}
	if report.Confidence > 1 {
		report.Confidence = 1
	}

	// 证据齐备性检查。
	hardEvidence := 0
	for i := range report.Evidence {
		e := &report.Evidence[i]
		if strings.TrimSpace(e.Source) == "" {
			e.Source = "unknown"
		}
		if strings.TrimSpace(e.Ref) == "" {
			e.Speculative = true
		}
		if e.Speculative {
			continue
		}
		hardEvidence++
	}
	if hardEvidence == 0 {
		report.Speculative = true
		warnings = append(warnings, "结论未引用任何硬证据，已标注为「推测」")
	}
	if report.Confidence < q.minConfidence {
		report.LowConfidence = true
		warnings = append(warnings, fmt.Sprintf("置信度 %.2f 低于阈值 %.2f，建议人工复核", report.Confidence, q.minConfidence))
	}

	// 建议规范化：补全操作级别与风险等级。
	for i := range report.Suggestions {
		s := &report.Suggestions[i]
		switch strings.ToLower(s.Horizon) {
		case "immediate", "short_term", "long_term":
		default:
			s.Horizon = "short_term"
		}
		if s.Risk == "" {
			s.Risk = "中"
		}
		if s.Level == "" {
			s.Level = InferLevel(s.Action)
		}
	}
	if len(report.Suggestions) == 0 {
		warnings = append(warnings, "模型未给出修复建议")
		report.PendingConfirm = append(report.PendingConfirm, "需要人工补充处理方案")
	}

	total := len(report.Evidence)
	if total == 0 {
		report.GroundedRatio = 0
	} else {
		report.GroundedRatio = float64(hardEvidence) / float64(total)
	}
	return report, warnings
}

// ParseReport 从模型原始输出中解析结构化报告。
//
// 兼容三种形态：纯 JSON、```json 代码块、JSON 前后带说明文字。
func ParseReport(content string) (*Report, error) {
	payload, err := extractJSON(content)
	if err != nil {
		return nil, err
	}
	// 先按质量护栏 schema 解析。
	var report Report
	if err := json.Unmarshal(payload, &report); err == nil && report.RootCause != "" {
		return &report, nil
	}
	// 兼容宽松 schema（例如 suggestions 为字符串数组）。
	var loose struct {
		RootCause      string           `json:"root_cause"`
		Confidence     float64          `json:"confidence"`
		Evidence       []any            `json:"evidence"`
		Suggestions    []map[string]any `json:"suggestions"`
		ImpactScope    string           `json:"impact_scope"`
		PendingConfirm []string         `json:"pending_confirm"`
	}
	if err := json.Unmarshal(payload, &loose); err != nil {
		return nil, fmt.Errorf("解析结构化报告失败: %w", err)
	}
	report = Report{
		RootCause:      loose.RootCause,
		Confidence:     loose.Confidence,
		ImpactScope:    loose.ImpactScope,
		PendingConfirm: loose.PendingConfirm,
	}
	for _, item := range loose.Evidence {
		switch v := item.(type) {
		case string:
			report.Evidence = append(report.Evidence, Evidence{Source: "unknown", Ref: "", Detail: v, Speculative: true})
		case map[string]any:
			report.Evidence = append(report.Evidence, Evidence{
				Source:      str(v["source"]),
				Ref:         str(v["ref"]),
				Detail:      str(v["detail"]),
				Speculative: boolOf(v["speculative"]),
			})
		}
	}
	for _, item := range loose.Suggestions {
		report.Suggestions = append(report.Suggestions, Suggestion{
			Action:  firstNonEmpty(str(item["action"]), str(item["text"])),
			Horizon: str(item["horizon"]),
			Risk:    str(item["risk"]),
			Level:   str(item["level"]),
		})
	}
	if report.RootCause == "" {
		return nil, fmt.Errorf("解析结构化报告失败: 缺少 root_cause 字段")
	}
	return &report, nil
}

// extractJSON 从文本中提取第一个平衡的 JSON 对象。
func extractJSON(content string) ([]byte, error) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, fmt.Errorf("模型输出为空")
	}
	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return nil, fmt.Errorf("模型输出未包含 JSON 对象")
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(trimmed); i++ {
		ch := trimmed[i]
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && inString:
			escaped = true
		case ch == '"':
			inString = !inString
		case !inString && ch == '{':
			depth++
		case !inString && ch == '}':
			depth--
			if depth == 0 {
				return []byte(trimmed[start : i+1]), nil
			}
		}
	}
	return nil, fmt.Errorf("模型输出的 JSON 不完整")
}

// InferLevel 依据动作文本推断操作级别（4.6）。
//
// 判定顺序遵循「先识别低危，再识别只读，最后按最高风险兜底」：
// 「确认告警」属于 L1，而「确认配置项」属于 L0，因此低危关键词必须先匹配。
// 无法判断时归为 L2，强制走审批而非直接执行。
func InferLevel(action string) string {
	s := strings.ToLower(action)
	// L1 低危：告警确认、知识库维护、规则维护。
	lowRiskHints := []string{
		"确认告警", "告警确认", "ack", "告警", "知识库", "规则", "标注", "备注", "采纳", "发布条目",
	}
	for _, hint := range lowRiskHints {
		if strings.Contains(s, hint) {
			return "L1"
		}
	}
	// L0 只读：查看、查询、核对、定位、分析等。
	readOnlyHints := []string{
		"查看", "查询", "核对", "定位", "检查", "观察", "分析", "评估", "确认", "监控",
		"explain", "select", "show", "monitor", "inspect",
	}
	for _, hint := range readOnlyHints {
		if strings.Contains(s, hint) {
			return "L0"
		}
	}
	return "L2"
}

// str 安全地取字符串值。
func str(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case fmt.Stringer:
		return val.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}

// boolOf 安全地取布尔值。
func boolOf(v any) bool {
	switch val := v.(type) {
	case bool:
		return val
	case string:
		return strings.EqualFold(val, "true")
	default:
		return false
	}
}

// firstNonEmpty 返回首个非空字符串。
func firstNonEmpty(items ...string) string {
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			return item
		}
	}
	return ""
}

// EvalCase 是评测集用例（5.6：内置 20-50 条典型故障用例，引擎切换后回归）。
type EvalCase struct {
	ID       string `json:"id"`
	MWType   string `json:"mw_type"`
	Question string `json:"question"`
	// ExpectKeywords 为期望命中的根因关键词（任一命中即通过）。
	ExpectKeywords []string `json:"expect_keywords"`
	// ForbidKeywords 为不应出现的关键词（防幻觉黑名单）。
	ForbidKeywords []string `json:"forbid_keywords"`
}

// EvalResult 是用例回归结果。
type EvalResult struct {
	CaseID string  `json:"case_id"`
	Passed bool    `json:"passed"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

// Evaluate 对单条用例评估模型输出。
func (q *Quality) Evaluate(c EvalCase, report *Report, rawOutput string) EvalResult {
	res := EvalResult{CaseID: c.ID}
	if report == nil {
		res.Reason = "无可解析的结构化报告"
		return res
	}
	blob := strings.ToLower(report.RootCause + " " + report.ImpactScope + " " + rawOutput)
	hit := 0
	for _, kw := range c.ExpectKeywords {
		if strings.Contains(blob, strings.ToLower(kw)) {
			hit++
		}
	}
	for _, kw := range c.ForbidKeywords {
		if strings.Contains(blob, strings.ToLower(kw)) {
			res.Reason = fmt.Sprintf("出现禁止关键词 %q", kw)
			return res
		}
	}
	if len(c.ExpectKeywords) == 0 {
		res.Passed = true
		res.Score = 1
		res.Reason = "无期望关键词约束"
		return res
	}
	res.Score = float64(hit) / float64(len(c.ExpectKeywords))
	// 结构完整性加权：有证据且置信度达标才计满分。
	// GroundedRatio 由 Normalize 计算；此处兜底重算，保证评测可独立运行。
	grounded := report.GroundedRatio
	if grounded == 0 && len(report.Evidence) > 0 {
		hard := 0
		for _, e := range report.Evidence {
			if !e.Speculative && strings.TrimSpace(e.Ref) != "" {
				hard++
			}
		}
		grounded = float64(hard) / float64(len(report.Evidence))
	}
	if res.Score > 0 && grounded > 0 && report.Confidence >= 0.5 {
		res.Passed = true
		res.Reason = fmt.Sprintf("命中 %d/%d 关键词，证据覆盖率 %.2f", hit, len(c.ExpectKeywords), grounded)
		return res
	}
	if res.Score > 0 {
		res.Reason = fmt.Sprintf("命中关键词但缺少硬证据（覆盖率 %.2f，置信度 %.2f）", grounded, report.Confidence)
		return res
	}
	res.Reason = "未命中任何期望关键词"
	return res
}

// DefaultEvalSet 返回内置评测集（按中间件类型，共 24 条，满足 20-50 条要求）。
func DefaultEvalSet() []EvalCase {
	return []EvalCase{
		{ID: "redis-mem-01", MWType: "redis", Question: "Redis 最近内存涨得很快，可能会怎样？", ExpectKeywords: []string{"内存", "maxmemory", "淘汰"}},
		{ID: "redis-mem-02", MWType: "redis", Question: "缓存命中率下降怎么排查", ExpectKeywords: []string{"命中率", "淘汰", "容量"}},
		{ID: "redis-slow-01", MWType: "redis", Question: "Redis 出现阻塞客户端怎么办", ExpectKeywords: []string{"阻塞", "慢命令", "SLOWLOG"}},
		{ID: "redis-key-01", MWType: "redis", Question: "如何发现 Redis 大 key", ExpectKeywords: []string{"大 key", "SCAN", "拆分"}},
		{ID: "kafka-lag-01", MWType: "kafka", Question: "消费组积压严重怎么处理", ExpectKeywords: []string{"Lag", "积压", "消费"}},
		{ID: "kafka-lag-02", MWType: "kafka", Question: "消费速率低于生产速率的原因", ExpectKeywords: []string{"消费速率", "分区", "并发"}},
		{ID: "kafka-isr-01", MWType: "kafka", Question: "ISR 收缩告警如何处理", ExpectKeywords: []string{"ISR", "副本", "Broker"}},
		{ID: "kafka-part-01", MWType: "kafka", Question: "Topic 分区分配不均衡", ExpectKeywords: []string{"分区", "均衡", "rebalance"}, ForbidKeywords: []string{"无关结论"}},
		{ID: "mysql-slow-01", MWType: "mysql", Question: "慢查询增多是什么原因", ExpectKeywords: []string{"索引", "EXPLAIN", "慢查询"}},
		{ID: "mysql-conn-01", MWType: "mysql", Question: "连接数打满了怎么办", ExpectKeywords: []string{"连接", "max_connections", "连接池"}},
		{ID: "mysql-repl-01", MWType: "mysql", Question: "主从延迟变大如何定位", ExpectKeywords: []string{"主从", "延迟", "事务"}},
		{ID: "mysql-lock-01", MWType: "mysql", Question: "出现锁等待和死锁怎么排查", ExpectKeywords: []string{"锁", "事务", "等待"}},
		{ID: "pg-slow-01", MWType: "pg", Question: "PostgreSQL 慢查询如何优化", ExpectKeywords: []string{"索引", "执行计划", "统计信息"}},
		{ID: "pg-conn-01", MWType: "pg", Question: "连接数过多导致拒绝连接", ExpectKeywords: []string{"连接", "空闲事务", "池"}},
		{ID: "pg-vacuum-01", MWType: "pg", Question: "表膨胀和 autovacuum 问题", ExpectKeywords: []string{"膨胀", "vacuum", "死元组"}},
		{ID: "pg-lock-01", MWType: "pg", Question: "查询被锁阻塞如何找到阻塞源", ExpectKeywords: []string{"pg_stat_activity", "锁", "阻塞"}},
		{ID: "es-health-01", MWType: "es", Question: "集群状态变成 yellow 怎么处理", ExpectKeywords: []string{"分片", "副本", "未分配"}},
		{ID: "es-health-02", MWType: "es", Question: "集群 red 无法写入数据", ExpectKeywords: []string{"未分配", "分片", "主分片"}},
		{ID: "es-heap-01", MWType: "es", Question: "JVM 堆内存高并且 Full GC 频繁", ExpectKeywords: []string{"堆", "GC", "聚合"}},
		{ID: "es-query-01", MWType: "es", Question: "检索变慢是什么原因", ExpectKeywords: []string{"检索", "查询", "分片"}},
		{ID: "nginx-5xx-01", MWType: "nginx", Question: "网关 5xx 突然增多", ExpectKeywords: []string{"upstream", "5xx", "后端"}},
		{ID: "nginx-timeout-01", MWType: "nginx", Question: "请求大量超时", ExpectKeywords: []string{"超时", "upstream", "响应"}},
		{ID: "common-capacity-01", MWType: "redis", Question: "实例负载高需要扩容吗", ExpectKeywords: []string{"负载", "容量", "扩容"}},
		{ID: "common-agent-01", MWType: "mysql", Question: "监控数据缺失如何排查", ExpectKeywords: []string{"Exporter", "采集", "连接"}},
	}
}
