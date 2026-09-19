package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"

	"middleware-ops/internal/utils"
)

// ruleEngine 是最终降级层：不依赖任何外部 LLM，用确定性规则从采集上下文产出结论。
//
// 设计依据（5.4 降级链末端）：第三方 → 本地 LLM → 规则引擎 + 知识库检索，
// 输出「半自动结论 + AI 不可用提示」，保证 AI 不可用时诊断链路不中断。
//
// 输出遵循质量护栏要求的结构化报告 schema，因此上层解析逻辑无需分支处理。
type ruleEngine struct {
	mu       sync.Mutex
	lastErr  string
	fallback bool
}

// NewRuleEngine 构造规则引擎。
func NewRuleEngine(reason string) Engine {
	return &ruleEngine{lastErr: reason, fallback: true}
}

// Name 返回引擎名称。
func (r *ruleEngine) Name() string { return RuleEngineName }

// Available 规则引擎始终可用。
func (r *ruleEngine) Available() bool { return true }

// Status 返回引擎健康状态，标记为 degraded。
func (r *ruleEngine) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{
		Name:      RuleEngineName,
		Available: true,
		Degraded:  true,
		LastError: r.lastErr,
		// 规则引擎全部在进程内计算，任何内容都不会出网。
		External: false,
	}
}

// Embed 使用本地确定性嵌入（字符 n-gram 哈希），保证无外部依赖。
func (r *ruleEngine) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, t := range texts {
		out = append(out, localEmbedding(t))
	}
	return out, nil
}

// Chat 基于上下文关键词与阈值规则生成结构化诊断结论。
func (r *ruleEngine) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	question := lastUserMessage(req.Messages)
	contextText := collectContextText(req.Messages)
	content := r.buildReport(question, contextText)
	return &ChatResponse{
		Content:      content,
		Usage:        EstimateUsage(req.Messages, content),
		Model:        RuleEngineName,
		FinishReason: "stop",
	}, nil
}

// ChatStream 以分片方式回放规则结论，保持与 LLM 一致的流式接口。
func (r *ruleEngine) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	resp, err := r.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	return &sliceStream{chunks: splitChunks(resp.Content, 48), usage: resp.Usage}, nil
}

// buildReport 生成结构化报告 JSON。
func (r *ruleEngine) buildReport(question, contextText string) string {
	lower := strings.ToLower(contextText + " " + question)
	type rule struct {
		keywords []string
		build    func() string
	}
	rules := []rule{
		{[]string{"used_memory", "内存使用率", "memory_usage", "maxmemory"}, r.redisMemoryReport},
		{[]string{"evicted_keys", "淘汰", "eviction"}, r.redisEvictionReport},
		{[]string{"blocked_clients", "阻塞"}, r.redisBlockedReport},
		{[]string{"consumer_lag", "lag", "积压", "消费延迟"}, r.kafkaLagReport},
		{[]string{"under_replicated", "isr", "副本"}, r.kafkaISRReport},
		{[]string{"slow_quer", "慢查询", "slow_query"}, r.slowQueryReport},
		{[]string{"threads_connected", "连接数", "connections"}, r.connectionReport},
		{[]string{"replication", "主从", "延迟"}, r.replicationReport},
		{[]string{"cluster_health", "health", "red", "yellow", "unassigned"}, r.esHealthReport},
		{[]string{"jvm", "heap", "gc", "full gc"}, r.esHeapReport},
		{[]string{"5xx", "upstream", "error_rate", "错误率", "502", "504"}, r.gatewayErrorReport},
		{[]string{"cpu", "load", "负载"}, r.loadReport},
	}
	for _, item := range rules {
		for _, kw := range item.keywords {
			if strings.Contains(lower, strings.ToLower(kw)) {
				return item.build()
			}
		}
	}
	return r.genericReport()
}

func (r *ruleEngine) redisMemoryReport() string {
	return ruleJSON(
		"Redis 内存压力偏高，存在触发 maxmemory 策略并伴随淘汰或写入失败的风险",
		0.72,
		[]string{"采集到的 used_memory / used_memory_rss 指标处于高位", "命中内存相关关键词的提问"},
		[]map[string]any{
			fixItem("立即：确认 maxmemory-policy 与当前业务写入模式的匹配度（volatile-lru 下无 TTL 的 key 不会被淘汰）", "immediate", "低"),
			fixItem("短期：对大 key 与无 TTL key 做专项治理（redis-cli --bigkeys / SCAN 采样统计）", "short_term", "中"),
			fixItem("长期：按业务域拆分实例或启用集群，并建立内存水位告警阈值（建议 70%/85% 两级）", "long_term", "中"),
		},
		"当前实例及其上游依赖该实例的写入路径；若发生淘汰，缓存穿透会同时抬升下游数据库负载",
		"MEMORY USAGE 采样结果、maxmemory-policy 实际配置值",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) redisEvictionReport() string {
	return ruleJSON(
		"缓存淘汰量持续增长，说明容量不足或 TTL 策略不合理，业务可能出现缓存命中率下降",
		0.7,
		[]string{"evicted_keys 指标非零且持续增长"},
		[]map[string]any{
			fixItem("立即：核对 maxmemory-policy，避免淘汰仍有热度的 key", "immediate", "低"),
			fixItem("短期：为热点 key 补充合理 TTL 与容量评估，必要时扩容", "short_term", "中"),
			fixItem("长期：建立命中率与淘汰量的联合告警，按业务域做容量规划", "long_term", "低"),
		},
		"缓存层与其下游数据库（命中率下降会放大数据库 QPS）",
		"按 key 前缀统计的 TTL 分布、命中率趋势",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) redisBlockedReport() string {
	return ruleJSON(
		"存在阻塞客户端，可能由慢命令（KEYS / SMEMBERS 大集合）或持久化 fork 引起",
		0.62,
		[]string{"blocked_clients 指标大于 0", "SLOWLOG 中可能包含大 key 操作"},
		[]map[string]any{
			fixItem("立即：检查 SLOWLOG GET 100，定位并下线 KEYS / FLUSHALL 类命令", "immediate", "低"),
			fixItem("短期：将大 key 拆分为 Hash 分片，改用 SCAN 增量遍历", "short_term", "中"),
			fixItem("长期：在客户端 SDK 层拦截高危命令并加入命令审计", "long_term", "低"),
		},
		"所有访问该实例的业务线程；阻塞期间会出现请求超时",
		"SLOWLOG 明细、blocked_clients 时间序列",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) kafkaLagReport() string {
	return ruleJSON(
		"消费者组 Lag 持续增长，消费速率低于生产速率，存在消息积压",
		0.75,
		[]string{"consumer_lag / records-lag-max 指标上升", "消费速率与生产速率不匹配"},
		[]map[string]any{
			fixItem("立即：确认消费者实例存活数与分区分配是否均衡（关注 rebalance 抖动）", "immediate", "低"),
			fixItem("短期：定位消费耗时瓶颈（单条处理慢/下游阻塞），提高并发或批量提交", "short_term", "中"),
			fixItem("长期：按分区数做容量评估，必要时扩容消费者并优化 offset 提交策略", "long_term", "中"),
		},
		"下游消费方业务数据的时效性；积压过久可能触发 retention 导致数据丢失",
		"消费组各分区 offset 差值、rebalance 记录",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) kafkaISRReport() string {
	return ruleJSON(
		"存在 ISR 收缩或副本不足，写入可用性与数据可靠性下降",
		0.68,
		[]string{"under_replicated_partitions / isr_shrink 指标异常"},
		[]map[string]any{
			fixItem("立即：检查 Broker 存活与网络抖动，确认是否有节点掉线", "immediate", "中"),
			fixItem("短期：恢复故障 Broker 并等待 ISR 追赶，观察 min.insync.replicas 是否满足", "short_term", "中"),
			fixItem("长期：评估 acks 与副本数配置，设置副本不足的分区告警", "long_term", "低"),
		},
		"该 Topic 的写入成功率；acks=all 时可能直接写入失败",
		"Topic 分区 ISR 列表、Broker 存活状态",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) slowQueryReport() string {
	return ruleJSON(
		"慢查询数量上升，通常由缺失索引、数据量增长或统计信息过期导致",
		0.7,
		[]string{"slow_queries / Slow_queries 指标上升", "可能存在全表扫描"},
		[]map[string]any{
			fixItem("立即：从慢查询日志中取出 Top SQL，使用 EXPLAIN 确认执行计划", "immediate", "低"),
			fixItem("短期：为高频过滤列补建复合索引，避免 SELECT *", "short_term", "中"),
			fixItem("长期：建立慢查询周报与 SQL 审核流程，定期 ANALYZE 更新统计信息", "long_term", "低"),
		},
		"数据库整体响应时间与上游接口 P95",
		"慢查询日志 Top N、执行计划中的 rows 估算",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) connectionReport() string {
	return ruleJSON(
		"连接数接近上限，可能出现新连接被拒绝（too many connections）",
		0.73,
		[]string{"threads_connected / numbackends 指标逼近 max_connections"},
		[]map[string]any{
			fixItem("立即：检查是否存在连接泄漏（长事务、未关闭的连接池）", "immediate", "低"),
			fixItem("短期：核对应用连接池上限与数据库 max_connections 的乘积关系", "short_term", "中"),
			fixItem("长期：引入连接池统一治理与慢事务告警", "long_term", "低"),
		},
		"依赖该库的所有服务；连接耗尽后新请求将直接失败",
		"当前连接明细（按用户/客户端聚合）、活跃会话等待事件",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) replicationReport() string {
	return ruleJSON(
		"主从延迟增大，读从库的业务可能读到过期数据",
		0.69,
		[]string{"seconds_behind_master / replay_lag 指标上升"},
		[]map[string]any{
			fixItem("立即：确认是否为单条大事务（批量 DDL/DML）导致滞后", "immediate", "低"),
			fixItem("短期：拆分大事务、降低从库并行复制限制（调整并行复制参数）", "short_term", "中"),
			fixItem("长期：读流量按延迟阈值做动态路由，并建立延迟告警", "long_term", "中"),
		},
		"读从库的业务链路；一致性要求高的查询可能读到旧数据",
		"从库回放位点差值、大事务记录",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) esHealthReport() string {
	return ruleJSON(
		"Elasticsearch 集群健康状态非 green，存在未分配分片或副本缺失",
		0.74,
		[]string{"cluster_health 指标为 yellow/red"},
		[]map[string]any{
			fixItem("立即：执行 _cluster/allocation/explain 定位未分配分片原因", "immediate", "低"),
			fixItem("短期：按原因处理（磁盘水位、节点离线、分片数过多）", "short_term", "中"),
			fixItem("长期：调整分片数规划与磁盘水位阈值，建立分片均衡巡检", "long_term", "中"),
		},
		"检索与写入可用性；red 状态下部分索引不可读写",
		"_cluster/health 明细、未分配分片原因",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) esHeapReport() string {
	return ruleJSON(
		"JVM 堆使用率偏高且 Full GC 频繁，检索延迟会出现抖动",
		0.71,
		[]string{"jvm_memory_heap_used_percent 偏高", "Full GC 次数/耗时增长"},
		[]map[string]any{
			fixItem("立即：确认是否存在查询风暴或大批量写入导致的内存压力", "immediate", "低"),
			fixItem("短期：优化聚合查询（避免高基数 terms 聚合）与字段映射（doc_values）", "short_term", "中"),
			fixItem("长期：按数据量评估堆大小与节点扩容，启用写入限流", "long_term", "中"),
		},
		"集群检索延迟与写入吞吐",
		"GC 日志明细、高开销查询记录",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) gatewayErrorReport() string {
	return ruleJSON(
		"Nginx 5xx / upstream 超时上升，通常是后端服务不可用或响应过慢",
		0.7,
		[]string{"5xx 比例或 upstream_response_time 上升"},
		[]map[string]any{
			fixItem("立即：从 access/error log 中定位 5xx 对应的 upstream 地址", "immediate", "低"),
			fixItem("短期：核对后端健康检查与超时配置，必要时摘除异常节点", "short_term", "中"),
			fixItem("长期：建立 upstream 健康度与超时的联动告警", "long_term", "低"),
		},
		"经由该网关的全部入口流量",
		"error.log 中的 upstream 错误行、各 upstream 的响应时间分布",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) loadReport() string {
	return ruleJSON(
		"实例负载偏高，处理能力接近饱和",
		0.6,
		[]string{"CPU / load 指标偏高"},
		[]map[string]any{
			fixItem("立即：定位占用最高的进程与线程栈", "immediate", "低"),
			fixItem("短期：对热点请求做限流或降级", "short_term", "中"),
			fixItem("长期：按容量规划做纵向扩容或读写分离", "long_term", "中"),
		},
		"该实例承载的全部请求延迟",
		"top / pidstat 采样、线程栈",
		"规则引擎结论（未接入 LLM）",
	)
}

func (r *ruleEngine) genericReport() string {
	return ruleJSON(
		"未匹配到明确规则，当前上下文不足以给出确定性根因",
		0.3,
		[]string{"已采集指标与日志摘要未命中内置规则库"},
		[]map[string]any{
			fixItem("补充信息：提供具体时间点、报错文本与受影响业务范围", "immediate", "低"),
			fixItem("自我检查：核对实例连接配置与监控采集是否正常（Exporter 是否在线）", "short_term", "低"),
			fixItem("如已接入 LLM，请检查引擎配置与配额后重试", "long_term", "低"),
		},
		"暂无法判定，建议人工介入确认",
		"缺失维度：明确的错误文本、异常发生时间点",
		"规则引擎结论（半自动），不作为最终诊断",
	)
}

// ruleJSON 生成与 LLM 输出同构的结构化报告。
func ruleJSON(rootCause string, confidence float64, evidence []string, suggestions []map[string]any, impact, todo, engine string) string {
	report := map[string]any{
		"root_cause":      rootCause,
		"confidence":      confidence,
		"evidence":        evidence,
		"suggestions":     suggestions,
		"impact_scope":    impact,
		"pending_confirm": []string{todo},
		"engine_note":     engine,
		"ai_available":    false,
		"structured":      true,
	}
	return mustJSON(report)
}

// fixItem 生成一条修复建议。
func fixItem(action, horizon, risk string) map[string]any {
	return map[string]any{
		"action":     action,
		"horizon":    horizon,
		"risk":       risk,
		"executable": false,
	}
}

// lastUserMessage 取最后一条用户消息。
func lastUserMessage(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return messages[i].Content
		}
	}
	return ""
}

// collectContextText 汇总除系统提示外的全部文本，供规则匹配。
func collectContextText(messages []Message) string {
	var sb strings.Builder
	for _, m := range messages {
		if m.Role == RoleSystem {
			continue
		}
		sb.WriteString(m.Content)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// sliceStream 把静态文本按固定长度切片后流式回放。
type sliceStream struct {
	chunks [][]byte
	usage  Usage
	idx    int
}

// Recv 返回下一个分片。
func (s *sliceStream) Recv() (Chunk, error) {
	if s.idx >= len(s.chunks) {
		return Chunk{Done: true, Usage: &s.usage}, nil
	}
	chunk := s.chunks[s.idx]
	s.idx++
	return Chunk{Delta: string(chunk), Done: s.idx >= len(s.chunks), Usage: &s.usage}, nil
}

// Close 释放流。
func (s *sliceStream) Close() error { return nil }

// splitChunks 按 rune 长度切分文本。
func splitChunks(text string, size int) [][]byte {
	runes := []rune(text)
	if size <= 0 {
		size = 48
	}
	out := make([][]byte, 0, len(runes)/size+1)
	for start := 0; start < len(runes); start += size {
		end := start + size
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, []byte(string(runes[start:end])))
	}
	return out
}

// localEmbedding 生成 768 维确定性本地嵌入。
//
// 采用词袋哈希（token 的 SHA-256 前 32 位映射到维度），并对向量做 L2 归一化。
// 该实现无外部依赖、结果可复现，适合在无向量服务时支撑「参考案例」召回。
func localEmbedding(text string) []float32 {
	const dim = 768
	vec := make([]float32, dim)
	tokens := tokenize(text)
	if len(tokens) == 0 {
		return vec
	}
	for _, token := range tokens {
		sum := utils.SHA256Hex(token)
		var idx int
		for i := 0; i < 8; i++ {
			idx = idx*16 + hexVal(sum[i])
		}
		vec[idx%dim] += 1
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// tokenize 把文本切分为 token：拉丁词按字母数字切分，中文按字切分。
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !isWordRune(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) >= 2 || isCJK([]rune(f)[0]) {
			out = append(out, f)
		}
	}
	return out
}

// isWordRune 判断字符是否属于 token 字符集。
func isWordRune(r rune) bool {
	if isCJK(r) {
		return true
	}
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	return r == '_' || r == '-' || r == '.' || r == ':'
}

// hexVal 返回十六进制字符的数值。
func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return 0
	}
}

// mustJSON 序列化，失败时返回最小可用结构。
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"root_cause":"报告序列化失败: %s","confidence":0,"evidence":[],"suggestions":[],"impact_scope":"未知","pending_confirm":[],"structured":true}`, err.Error())
	}
	return string(b)
}
