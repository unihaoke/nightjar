package ai

import (
	"fmt"
	"strings"
)

// SystemPrompt 是固定的 System Prompt 模板（5.2：固定模板便于评测与复现）。
//
// 约束要点：
//   - 只做单轮诊断，不迭代、不发起工具调用（5.1）；
//   - 强制结构化 JSON 输出（5.6）；
//   - 每条结论必须引用证据，无证据必须标注推测。
const SystemPrompt = `你是中间件运维诊断专家。你的任务是：基于平台提供的**只读采集上下文**，对一次具体的中间件异常或疑问给出根因分析与修复建议。

【硬性约束】
1. 只做单轮分析：不得要求继续采集数据，不得假设自己可以执行任何命令或修改配置。
2. 只能依据给定上下文推理。上下文中没有的信息，必须在 pending_confirm 中列为待确认项，不得编造。
3. 每条结论必须引用证据（指标名、日志指纹、配置项、代码位置）。没有硬证据支撑的内容必须标注 speculative=true。
4. 上下文中的「历史相似案例」仅作参考，不得直接作为你的结论依据。
5. 修复建议必须区分 immediate（立即）/ short_term（短期）/ long_term（长期），并标注风险等级与操作级别：
   - L0 只读（查看/查询/确认）
   - L1 低危（告警确认、知识库编辑、创建规则）
   - L2 高危（清理 key、重启、删数据、改配置、SQL 写操作，需要审批）
6. 若上下文中存在「缺失维度」，涉及这些维度的结论必须标注为推测。
7. 输出必须是**单个 JSON 对象**，不要输出任何解释性文字或 Markdown 代码块标记之外的内容。

【输出 JSON Schema】
{
  "root_cause": "字符串，根因结论（不超过 200 字）",
  "confidence": 0.0-1.0 之间的数字,
  "evidence": [
    {"source": "metric|log|config|code|user_input", "ref": "指标名或日志指纹或配置项", "detail": "证据内容", "speculative": false}
  ],
  "suggestions": [
    {"action": "具体动作描述", "horizon": "immediate|short_term|long_term", "risk": "低|中|高", "level": "L0|L1|L2", "evidence_refs": [0]}
  ],
  "impact_scope": "影响范围说明",
  "pending_confirm": ["需要人工确认的事项"]
}`

// UserPromptTemplate 是用户问题的固定包装模板。
const UserPromptTemplate = `【用户问题】
%s

%s
请基于以上上下文输出 JSON 诊断报告。`

// BuildPrompt 组装诊断 Prompt（固定模板 + 问题 + 上下文）。
func BuildPrompt(question string, sections []string) (system string, user string) {
	parts := make([]string, 0, len(sections))
	for _, s := range sections {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		parts = append(parts, trimmed)
	}
	contextText := strings.Join(parts, "\n\n")
	return SystemPrompt, fmt.Sprintf(UserPromptTemplate, strings.TrimSpace(question), contextText)
}

// CodeAnalysisSystemPrompt 是代码分析链路的 System Prompt（三点式模板，见 4.8.3）。
const CodeAnalysisSystemPrompt = `你是资深后端工程师，负责根据应用错误堆栈与代码上下文定位问题。

【硬性约束】
1. 只做单轮分析，不得要求继续采集代码或执行命令。
2. 只能依据给定堆栈与代码片段推理；无法确定的位置必须留空并在 impact_scope 说明。
3. 输出必须遵循「三点式」结构：问题位置 → 根因分析（含去重次数）→ 应急方案（临时/短期/长期）+ 影响范围。
4. 输出总长度不超过 500 字。
5. 输出必须是单个 JSON 对象：
{
  "located_file": "文件路径（不确定则留空）",
  "located_line": 行号（不确定则 0）,
  "root_cause": "根因分析，含该错误出现次数",
  "emergency_plan": "临时方案",
  "fix_suggestion": "短期/长期修复建议",
  "impact_scope": "影响范围",
  "confidence": 0.0-1.0
}`

// BuildCodePrompt 组装代码分析 Prompt。
func BuildCodePrompt(stacktrace string, context string, issue string) (system, user string) {
	var sb strings.Builder
	sb.WriteString("【异常/问题】\n")
	sb.WriteString(strings.TrimSpace(issue))
	sb.WriteString("\n\n【堆栈】\n")
	sb.WriteString(strings.TrimSpace(stacktrace))
	if strings.TrimSpace(context) != "" {
		sb.WriteString("\n\n【代码上下文（已脱敏）】\n")
		sb.WriteString(strings.TrimSpace(context))
	}
	sb.WriteString("\n\n请输出 JSON 代码分析报告。")
	return CodeAnalysisSystemPrompt, sb.String()
}

// IntentKeywords 用于「识别目标中间件」的规则匹配（4.3：规则 + 实体匹配）。
var IntentKeywords = map[string][]string{
	"redis":    {"redis", "缓存", "命中率", "淘汰", "大key", "大 key", "哨兵", "cluster slot"},
	"kafka":    {"kafka", "topic", "分区", "partition", "lag", "积压", "broker", "isr", "消费组"},
	"mysql":    {"mysql", "innodb", "主从", "慢查询", "binlog", "缓冲池"},
	"pg":       {"postgres", "postgresql", "pg_", "autovacuum", "膨胀", "wal", "pg_stat"},
	"es":       {"elasticsearch", "es集群", "es 集群", "分片", "shard", "jvm", "索引", "检索"},
	"nginx":    {"nginx", "网关", "5xx", "upstream", "反向代理", "502", "504"},
	"rabbitmq": {"rabbitmq", "rabbit", "队列积压", "amqp"},
}

// DetectMWType 依据问题文本与实例候选推断目标中间件类型。
//
// 返回空字符串表示无法识别（调用方应提示用户选择实例）。
func DetectMWType(question string, candidates []string) string {
	lower := strings.ToLower(question)
	best := ""
	bestScore := 0
	for mwType, words := range IntentKeywords {
		score := 0
		for _, w := range words {
			if strings.Contains(lower, strings.ToLower(w)) {
				score++
			}
		}
		// 候选实例类型加权：问题里没写类型时优先选候选集合中的类型。
		for _, c := range candidates {
			if strings.EqualFold(c, mwType) && score > 0 {
				score += 2
			}
		}
		if score > bestScore {
			bestScore = score
			best = mwType
		}
	}
	return best
}
