package engine

import (
	"context"
	"strings"
	"testing"

	"middleware-ops/internal/config"
)

// TestRuleEngineReportIsStructured 校验规则引擎输出满足质量护栏要求的 JSON 结构。
func TestRuleEngineReportIsStructured(t *testing.T) {
	eng := NewRuleEngine("test")
	resp, err := eng.Chat(context.Background(), ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: "system"},
			{Role: RoleUser, Content: "Redis 内存使用率很高，used_memory 快到 maxmemory 了，怎么办"},
		},
	})
	if err != nil {
		t.Fatalf("规则引擎调用失败: %v", err)
	}
	if !LooksLikeJSON(resp.Content) {
		t.Fatalf("规则引擎输出必须是 JSON：%s", resp.Content)
	}
	for _, field := range []string{"root_cause", "confidence", "evidence", "suggestions", "impact_scope", "pending_confirm"} {
		if !strings.Contains(resp.Content, `"`+field+`"`) {
			t.Fatalf("输出缺少字段 %s：%s", field, resp.Content)
		}
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("应估算 token 消耗")
	}
}

// TestRuleEngineFallbackNotice 校验降级结论携带「AI 不可用」提示。
func TestRuleEngineFallbackNotice(t *testing.T) {
	eng := NewRuleEngine("no provider")
	status := eng.Status()
	if !status.Degraded || !status.Available {
		t.Fatalf("规则引擎状态应为「可用但降级」：%+v", status)
	}
	if eng.Name() != RuleEngineName {
		t.Fatalf("引擎名称应为 %s，实际 %s", RuleEngineName, eng.Name())
	}
}

// TestEstimateTokens 校验 token 估算的量级合理性。
func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens(""); got != 0 {
		t.Fatalf("空文本应为 0，实际 %d", got)
	}
	cjk := EstimateTokens("Redis 内存使用率偏高")
	if cjk < 5 || cjk > 20 {
		t.Fatalf("中文估算量级异常: %d", cjk)
	}
	ascii := EstimateTokens(strings.Repeat("a", 400))
	if ascii < 80 || ascii > 120 {
		t.Fatalf("英文估算量级异常: %d", ascii)
	}
}

// TestFactoryDegradesWithoutProvider 校验「配置不可即降级」的启动语义（5.4）。
func TestFactoryDegradesWithoutProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.AIEngine.Strategy = string(StrategyHybrid)
	cfg.AIEngine.ThirdParty.Enabled = false
	cfg.AIEngine.SelfHosted.Enabled = false

	factory := NewFactory(FactoryOptions{Config: cfg})
	eng := factory.Engine()
	if eng.Name() != RuleEngineName {
		t.Fatalf("无提供方时应降级为规则引擎，实际 %s", eng.Name())
	}
	if len(factory.Notes()) == 0 {
		t.Fatal("降级原因应记录在 Notes 中")
	}
	if !eng.Available() {
		t.Fatal("规则引擎必须始终可用，否则诊断链路中断")
	}
}

// TestFactoryMockProvider 校验 mock 提供方无需外部依赖即可工作。
func TestFactoryMockProvider(t *testing.T) {
	cfg := &config.Config{}
	cfg.AIEngine.Strategy = string(StrategySelfHosted)
	cfg.AIEngine.SelfHosted.Enabled = true
	cfg.AIEngine.SelfHosted.Kind = "mock"
	cfg.AIEngine.SelfHosted.BaseURL = ""

	factory := NewFactory(FactoryOptions{Config: cfg})
	eng := factory.Engine()
	if !eng.Available() {
		t.Fatal("mock 引擎应可用")
	}
	// 混合策略下的降级链仍应返回结论。
	hybrid := NewHybrid(HybridOptions{Primary: eng, Rule: NewRuleEngine("fallback")})
	resp, err := hybrid.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: RoleUser, Content: "Kafka 消费积压"}},
	})
	if err != nil {
		t.Fatalf("混合引擎调用失败: %v", err)
	}
	if strings.TrimSpace(resp.Content) == "" {
		t.Fatal("应返回非空结论")
	}
}

// TestLocalEmbeddingDeterministic 校验本地嵌入的确定性（无外部服务时的兜底）。
func TestLocalEmbeddingDeterministic(t *testing.T) {
	eng := NewRuleEngine("test")
	vecs, err := eng.Embed(context.Background(), []string{"Redis 内存告警", "Redis 内存告警"})
	if err != nil {
		t.Fatalf("本地嵌入失败: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("嵌入数量不符: %d", len(vecs))
	}
	if len(vecs[0]) != 768 {
		t.Fatalf("向量维度应为 768，实际 %d", len(vecs[0]))
	}
	for i := range vecs[0] {
		if vecs[0][i] != vecs[1][i] {
			t.Fatal("相同文本的嵌入应完全一致")
		}
	}
	other, _ := eng.Embed(context.Background(), []string{"Nginx 5xx 错误"})
	if len(other) == 0 || len(other[0]) != 768 {
		t.Fatal("不同文本应返回同维度向量")
	}
}

// TestSplitChunksPreservesContent 校验流式分片不丢字符（中文安全）。
func TestSplitChunksPreservesContent(t *testing.T) {
	text := "Redis 内存使用率偏高，建议检查 maxmemory-policy 与无 TTL 大 key。"
	chunks := splitChunks(text, 7)
	var sb strings.Builder
	for _, c := range chunks {
		sb.Write(c)
	}
	if sb.String() != text {
		t.Fatalf("分片重组后内容不一致\n期望: %s\n实际: %s", text, sb.String())
	}
}
