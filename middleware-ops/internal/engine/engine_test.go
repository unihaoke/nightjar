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

// TestStatusDeclaresExternalProvider 钉住"第三方引擎必须自报 external"（INC-030 的判定基础）。
//
// 合规闸门问的是引擎自己（`Status().External`）而不是读配置字符串：工厂可能因为
// 缺 base_url / 未启用把第三方降级成规则引擎，那种情况下确实不会出网，按配置判会误拦。
func TestStatusDeclaresExternalProvider(t *testing.T) {
	thirdParty := func() *config.Config {
		cfg := &config.Config{}
		cfg.AIEngine.Strategy = string(StrategyThirdParty)
		cfg.AIEngine.ThirdParty.Enabled = true
		cfg.AIEngine.ThirdParty.Kind = "openai"
		cfg.AIEngine.ThirdParty.BaseURL = "https://api.example.com/v1"
		cfg.AIEngine.ThirdParty.APIKey = "sk-test"
		return cfg
	}

	if got := NewFactory(FactoryOptions{Config: thirdParty()}).Status(); !got.External {
		t.Fatalf("third_party 策略下应声明 External=true，实际 %+v", got)
	}

	// 自建（内网）提供方不是外部：即便填了地址也不该被当成第三方。
	selfCfg := &config.Config{}
	selfCfg.AIEngine.Strategy = string(StrategySelfHosted)
	selfCfg.AIEngine.SelfHosted.Enabled = true
	selfCfg.AIEngine.SelfHosted.Kind = "openai"
	selfCfg.AIEngine.SelfHosted.BaseURL = "http://vllm.internal:8000/v1"
	if got := NewFactory(FactoryOptions{Config: selfCfg}).Status(); got.External {
		t.Fatalf("self_hosted 不应被声明为 External，实际 %+v", got)
	}

	// 混合链：只要链上含第三方就必须保守地按"外部"处理（降级可能落到那一层）。
	hybridCfg := thirdParty()
	hybridCfg.AIEngine.Strategy = string(StrategyHybrid)
	hybridCfg.AIEngine.SelfHosted.Enabled = true
	hybridCfg.AIEngine.SelfHosted.Kind = "openai"
	hybridCfg.AIEngine.SelfHosted.BaseURL = "http://vllm.internal:8000/v1"
	if got := NewFactory(FactoryOptions{Config: hybridCfg}).Status(); !got.External {
		t.Fatalf("混合链含第三方时应声明 External=true，实际 %+v", got)
	}

	// 第三方配了地址却没启用/被降级成规则引擎 → 不会出网。
	ruleCfg := &config.Config{}
	ruleCfg.AIEngine.Strategy = string(StrategyThirdParty)
	ruleCfg.AIEngine.ThirdParty.Enabled = true
	ruleCfg.AIEngine.ThirdParty.Kind = "openai"
	ruleCfg.AIEngine.ThirdParty.BaseURL = "" // 缺地址 → 规则引擎
	if got := NewFactory(FactoryOptions{Config: ruleCfg}).Status(); got.External {
		t.Fatalf("降级成规则引擎后不该声明为 External，实际 %+v", got)
	}
	// 纯规则引擎同理。
	if got := NewRuleEngine("x").Status(); got.External {
		t.Fatalf("规则引擎永不出网，实际 %+v", got)
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

// TestFactoryReloadSwitchesEngine 校验「工厂自身实现 Engine 接口」这一热加载前提。
//
// 场景：各 service 在装配期就把 engine.Engine 存进自己的字段。管理员在「AI 设置」里改完
// 配置后只会调一次 Reload，因此**已经持有工厂指针的调用方必须立刻看到新引擎**——
// 如果工厂只提供 Engine() 快照，改了配置也只有工厂自己知道，诊断链路仍在用旧引擎。
func TestFactoryReloadSwitchesEngine(t *testing.T) {
	cfg := &config.Config{}
	cfg.AIEngine.Strategy = string(StrategyHybrid)
	cfg.AIEngine.ThirdParty.Enabled = false
	cfg.AIEngine.SelfHosted.Enabled = false

	factory := NewFactory(FactoryOptions{Config: cfg})
	// 模拟 service 侧持有的引用（接口类型，指向工厂本身）。
	holder := Engine(factory)
	if holder.Name() != RuleEngineName {
		t.Fatalf("无提供方时应为规则引擎，实际 %s", holder.Name())
	}
	if !holder.Available() {
		t.Fatal("规则引擎必须始终可用")
	}
	before := len(factory.Notes())
	if before == 0 {
		t.Fatal("应记录降级说明")
	}

	// 改配置并 Reload：同一个 holder 必须看到新引擎。
	cfg.AIEngine.ThirdParty.Enabled = true
	cfg.AIEngine.ThirdParty.Kind = "mock"
	factory.Reload()
	if holder.Name() != string(StrategyHybrid) {
		t.Fatalf("Reload 后应为 hybrid，实际 %s", holder.Name())
	}
	if len(factory.Notes()) == 0 {
		t.Fatal("Reload 后 Notes 应随之更新")
	}
	if !strings.Contains(factory.Notes()[0], "mock") {
		t.Fatalf("Notes 应描述本次构建的提供方，实际 %v", factory.Notes())
	}

	// 再降回去：行为必须可逆（保存非法配置后仍能回退到规则引擎）。
	cfg.AIEngine.ThirdParty.Enabled = false
	factory.Reload()
	if holder.Name() != RuleEngineName {
		t.Fatalf("Reload 回退后应为规则引擎，实际 %s", holder.Name())
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
