package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"middleware-ops/internal/engine"
)

// 本文件锁定「平台自管设置」里最容易出错、也最容易泄露密钥的那几个判定：
//  1. 密钥掩码绝不能等于原值；
//  2. 「空串＝保持不变 / clear=true＝清空 / 非空＝覆盖」的三态语义（界面不回显明文，
//     这条语义搞错就会在"只改模型名"时把 api_key 悄悄清掉）；
//  3. 用量折线图的日期补齐与剩余额度计算（缺一天会被画成一条假直线；
//     不限额用 -1 而不是 0，否则界面读成"今天已用完"）。
//
// 全部为纯函数测试：这些逻辑一旦依赖 DB 就会变成"只能在真库上验证"，得不偿失。

// TestMaskSecret 校验掩码规则：前 3 位 + **** + 后 4 位，长度不足则全星号。
func TestMaskSecret(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		want  string
		short bool
	}{
		{"常规 key 保留前 3 后 4", "sk-1234567890cdef", "sk-****cdef", false},
		{"恰好能容纳前后缀的最短长度", "sk-cdef", "sk-****cdef", false},
		{"短一位就整体打星（否则前后缀重叠会暴露更多字符）", "sk-cde", "****", true},
		{"4 位短 key 全星号", "abcd", "****", true},
		{"空 key 返回空串", "", "", true},
	}
	for _, c := range cases {
		got := maskSecret(c.key)
		if got != c.want {
			t.Fatalf("%s: maskSecret(%q) = %q，期望 %q", c.name, c.key, got, c.want)
		}
		if c.key != "" && got == c.key {
			t.Fatalf("%s: 掩码结果不能等于原密钥", c.name)
		}
		// 掩码里不能出现密钥的"中段"，否则等于把密钥泄露了。
		if len(c.key) >= 9 && strings.Contains(got, c.key[3:len(c.key)-4]) {
			t.Fatalf("%s: 掩码泄露了密钥中段: %q", c.name, got)
		}
		if !c.short && !strings.Contains(got, "****") {
			t.Fatalf("%s: 掩码缺少星号: %q", c.name, got)
		}
	}
}

// TestMergeSecretThreeStates 校验密钥三态语义。
func TestMergeSecretThreeStates(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		in       string
		clear    bool
		want     string
	}{
		{"空串 = 保持不变（用户没动输入框）", "sk-old", "", false, "sk-old"},
		{"纯空白 = 保持不变", "sk-old", "   ", false, "sk-old"},
		{"非空 = 覆盖", "sk-old", "sk-new", false, "sk-new"},
		{"原值为空且入参为空 = 仍为空", "", "", false, ""},
		{"clear = 清空", "sk-old", "", true, ""},
		{"clear 优先于非空（前端可能带上残留值）", "sk-old", "sk-new", true, ""},
		{"clear 但原本就没配 = 仍为空", "", "", true, ""},
	}
	for _, c := range cases {
		if got := mergeSecret(c.existing, c.in, c.clear); got != c.want {
			t.Fatalf("%s: mergeSecret(%q, %q, %v) = %q，期望 %q",
				c.name, c.existing, c.in, c.clear, got, c.want)
		}
	}
}

// TestSecretChange 校验审计用的「是否换过密钥」判定（只记布尔，不记密钥）。
func TestSecretChange(t *testing.T) {
	cases := []struct {
		name             string
		existing         string
		in               string
		clear            bool
		changed, cleared bool
	}{
		{"换了新值 → changed", "sk-old", "sk-new", false, true, false},
		{"填了同样的值 → 不算变更", "sk-old", "sk-old", false, false, false},
		{"空串 → 未变更", "sk-old", "", false, false, false},
		{"清空已配置的密钥 → changed + cleared", "sk-old", "", true, true, true},
		{"清空本来就没配的密钥 → 只记 cleared", "", "", true, false, true},
	}
	for _, c := range cases {
		changed, cleared := secretChange(c.existing, c.in, c.clear)
		if changed != c.changed || cleared != c.cleared {
			t.Fatalf("%s: secretChange = (%v, %v)，期望 (%v, %v)",
				c.name, changed, cleared, c.changed, c.cleared)
		}
	}
}

// TestAIProviderMergeKeepsSecret 校验「只改模型名不会清空 api_key」。
func TestAIProviderMergeKeepsSecret(t *testing.T) {
	old := aiProviderPayload{
		Enabled: true, Kind: "openai", BaseURL: "https://api.deepseek.com/v1",
		APIKey: "sk-secret-value", Model: "deepseek-chat", MaxTokens: 2048, PricePerKToken: 0.001,
	}
	in := ProviderSettingsInput{
		Enabled: true, Kind: "openai", BaseURL: "https://api.deepseek.com/v1",
		Model: "deepseek-reasoner", MaxTokens: 4096, PricePerKToken: 0.002,
	}
	got := mergeAIProvider(old, in)
	if got.APIKey != old.APIKey {
		t.Fatalf("空串应保持原密钥，实际 %q", got.APIKey)
	}
	if got.Model != "deepseek-reasoner" || got.MaxTokens != 4096 {
		t.Fatalf("非密钥字段应整体覆盖，实际 model=%q max_tokens=%d", got.Model, got.MaxTokens)
	}

	// clear_api_key=true 才清空。
	in.ClearAPIKey = true
	if got := mergeAIProvider(old, in); got.APIKey != "" {
		t.Fatalf("clear_api_key=true 应清空，实际 %q", got.APIKey)
	}

	// max_tokens<=0 视为未填：HTTP 提供方内部会兜成 2048，原样落库会让界面长期显示 0。
	in.MaxTokens = 0
	if got := mergeAIProvider(old, in); got.MaxTokens != old.MaxTokens {
		t.Fatalf("max_tokens=0 应沿用原值 %d，实际 %d", old.MaxTokens, got.MaxTokens)
	}
}

// TestNotifyChannelMergeAndView 校验渠道合并的三态语义与展示形态。
func TestNotifyChannelMergeAndView(t *testing.T) {
	old := notifyChannelPayload{
		Enabled: true, Webhook: "https://open.feishu.cn/open-apis/bot/v2/hook/abcdefgh",
		Secret: "sign-secret", Mentions: []string{"@all"},
	}
	in := NotifyChannelInput{Enabled: true, Mentions: []string{"", "  ", "ops"}}
	got := mergeNotifyChannel(old, in)
	if got.Webhook != old.Webhook || got.Secret != old.Secret {
		t.Fatalf("空串应保持原 webhook/secret，实际 %q / %q", got.Webhook, got.Secret)
	}
	if len(got.Mentions) != 1 || got.Mentions[0] != "ops" {
		t.Fatalf("mentions 应去空白且只保留有效项，实际 %v", got.Mentions)
	}

	view := got.view()
	if !view.WebhookSet || view.WebhookMasked == "" || view.WebhookMasked == got.Webhook {
		t.Fatalf("webhook 应只回掩码，实际 set=%v masked=%q", view.WebhookSet, view.WebhookMasked)
	}
	if !view.SecretSet {
		t.Fatal("secret 应只回是否存在")
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, secret := range []string{got.Webhook, got.Secret, "open-apis", "sign-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("响应里出现了明文渠道密钥 %q: %s", secret, raw)
		}
	}

	// clear_webhook / clear_secret 才清空。
	cleared := mergeNotifyChannel(old, NotifyChannelInput{ClearWebhook: true, ClearSecret: true})
	if cleared.Webhook != "" || cleared.Secret != "" {
		t.Fatalf("clear_*=true 应清空，实际 %q / %q", cleared.Webhook, cleared.Secret)
	}
}

// TestEmailChannelMerge 校验邮件口令三态与端口兜底。
func TestEmailChannelMerge(t *testing.T) {
	old := emailPayload{Enabled: true, Host: "smtp.example.com", Port: 465, Username: "ops", Password: "smtp-pass", From: "ops@example.com", To: []string{"oncall@example.com"}, UseTLS: true}

	got := mergeEmailChannel(old, EmailChannelInput{Enabled: true, Host: "smtp.example.com", Username: "ops", From: "ops@example.com", To: []string{"oncall@example.com"}, UseTLS: true})
	if got.Password != "smtp-pass" {
		t.Fatalf("空口令应保持不变，实际 %q", got.Password)
	}
	if got.Port != 465 {
		t.Fatalf("端口未填应沿用原值 465，实际 %d", got.Port)
	}

	got = mergeEmailChannel(old, EmailChannelInput{ClearPassword: true})
	if got.Password != "" {
		t.Fatalf("clear_password=true 应清空口令，实际 %q", got.Password)
	}
	got = mergeEmailChannel(emailPayload{}, EmailChannelInput{Host: "smtp.example.com"})
	if got.Port != 465 {
		t.Fatalf("原值也为空时应兜底 465，实际 %d", got.Port)
	}
}

// TestAISettingsViewNeverLeaksAPIKey 校验接口响应里不存在明文 api_key。
func TestAISettingsViewNeverLeaksAPIKey(t *testing.T) {
	const key = "sk-1234567890abcdefghijklmn"
	payload := aiSettingsPayload{
		Strategy:        "hybrid",
		ThirdParty:      aiProviderPayload{Enabled: true, Kind: "openai", BaseURL: "https://api.deepseek.com/v1", APIKey: key, Model: "deepseek-chat", MaxTokens: 2048, PricePerKToken: 0.001},
		SelfHosted:      aiProviderPayload{Enabled: false, Kind: "ollama"},
		DailyTokenQuota: 2000000,
		PerUserQuota:    200000,
	}
	view := AISettingsView{
		Strategy:          payload.Strategy,
		ThirdParty:        payload.ThirdParty.view(),
		SelfHosted:        payload.SelfHosted.view(),
		DailyTokenQuota:   payload.DailyTokenQuota,
		PerUserDailyQuota: payload.PerUserQuota,
		ProvidersActive:   payload.providersActive(),
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatalf("响应里出现了明文 api_key: %s", raw)
	}
	if !view.ThirdParty.APIKeySet {
		t.Fatal("已配置的 key 应报告 api_key_set=true")
	}
	if view.ThirdParty.APIKeyMasked != "sk-****klmn" {
		t.Fatalf("掩码不符合契约，实际 %q", view.ThirdParty.APIKeyMasked)
	}
	if !strings.Contains(string(raw), `"api_key_masked":"sk-****klmn"`) {
		t.Fatalf("响应字段名应符合契约: %s", raw)
	}
	// providers_active 复用时序标签格式，界面据此提示"配了但没生效"。
	if len(view.ProvidersActive) != 1 || view.ProvidersActive[0] != "third_party(openai/https://api.deepseek.com/v1)" {
		t.Fatalf("providers_active 不符预期: %v", view.ProvidersActive)
	}

	// 没有密钥的提供方不会出现在 providers_active 里。
	payload.ThirdParty.APIKey = ""
	if active := payload.providersActive(); len(active) != 1 || active[0] != engine.RuleEngineName {
		t.Fatalf("无密钥时应只剩规则引擎，实际 %v", active)
	}
}

// TestBuildUsageSeriesFillsMissingDays 校验日期补齐、跨来源累加与窗口边界。
func TestBuildUsageSeriesFillsMissingDays(t *testing.T) {
	now := time.Date(2026, 9, 18, 15, 4, 5, 0, time.UTC)
	rows := []usageAggRow{
		{Source: usageSourceDiagnosis, DayKey: "2026-09-16", Tokens: 30, Calls: 1},
		{Source: usageSourceDiagnosis, DayKey: "2026-09-18", Tokens: 100, Calls: 2},
		{Source: usageSourceCodeAnalysis, DayKey: "2026-09-18", Tokens: 50, Calls: 1},
		// 窗口外的历史数据不该出现在序列里（SQL 已过滤，这里再兜一层）。
		{Source: usageSourceDiagnosis, DayKey: "2026-09-15", Tokens: 999, Calls: 9},
	}
	points := buildUsageSeries(rows, 2, now)

	want := []AIUsagePoint{
		{Date: "2026-09-16", Tokens: 30, Calls: 1},
		{Date: "2026-09-17", Tokens: 0, Calls: 0},
		{Date: "2026-09-18", Tokens: 150, Calls: 3},
	}
	if len(points) != len(want) {
		t.Fatalf("序列长度应为 days+1=%d，实际 %d（%+v）", len(want), len(points), points)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Fatalf("第 %d 个点应为 %+v，实际 %+v", i, want[i], points[i])
		}
	}

	// 窗口口径：days=30 时序列长度为 31，最后一天必须是「今天」。
	long := buildUsageSeries(nil, defaultUsageDays, now)
	if len(long) != defaultUsageDays+1 {
		t.Fatalf("默认窗口应补出 %d 个点，实际 %d", defaultUsageDays+1, len(long))
	}
	if last := long[len(long)-1]; last.Date != "2026-09-18" || last.Tokens != 0 {
		t.Fatalf("序列最后一天应是今天且补零，实际 %+v", last)
	}
}

// TestSummarizeUsage 校验窗口总量、今日消耗与分来源顺序。
func TestSummarizeUsage(t *testing.T) {
	now := time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC)
	rows := []usageAggRow{
		{Source: usageSourceDiagnosis, DayKey: "2026-09-16", Tokens: 30, Calls: 1},
		{Source: usageSourceDiagnosis, DayKey: "2026-09-18", Tokens: 100, Calls: 2},
		{Source: usageSourceCodeAnalysis, DayKey: "2026-09-18", Tokens: 50, Calls: 1},
	}
	points := buildUsageSeries(rows, 2, now)
	summary := summarizeUsage(points, rows)

	if summary.WindowTokens != 180 || summary.WindowCalls != 4 {
		t.Fatalf("窗口汇总应为 180/4，实际 %d/%d", summary.WindowTokens, summary.WindowCalls)
	}
	if summary.TodayTokens != 150 || summary.TodayCalls != 3 {
		t.Fatalf("今日应为 150/3，实际 %d/%d", summary.TodayTokens, summary.TodayCalls)
	}
	if len(summary.BySource) != 2 {
		t.Fatalf("固定两个来源，实际 %+v", summary.BySource)
	}
	if summary.BySource[0] != (AIUsageSource{Source: usageSourceDiagnosis, Tokens: 130, Calls: 3}) {
		t.Fatalf("诊断来源统计不符: %+v", summary.BySource[0])
	}
	if summary.BySource[1] != (AIUsageSource{Source: usageSourceCodeAnalysis, Tokens: 50, Calls: 1}) {
		t.Fatalf("代码分析来源统计不符: %+v", summary.BySource[1])
	}

	// 窗口内没有任何数据时也要给出两个 0 行（界面图例不该消失）。
	empty := summarizeUsage(buildUsageSeries(nil, 1, now), nil)
	if empty.TodayTokens != 0 || len(empty.BySource) != 2 {
		t.Fatalf("空数据应补出 0 值与两个来源，实际 %+v", empty)
	}
}

// TestRemainingQuota 校验今日剩余额度与使用率（含「不限额」与「已超额」）。
func TestRemainingQuota(t *testing.T) {
	cases := []struct {
		name  string
		quota int64
		used  int64
		want  int64
	}{
		{"未使用 → 满额", 2000000, 0, 2000000},
		{"用了一半 → 剩一半", 2000000, 1000000, 1000000},
		{"恰好用完 → 0", 2000000, 2000000, 0},
		{"用超了 → 0（不返回负数）", 2000000, 2500000, 0},
		{"quota=0 表示不限额 → -1", 0, 12345, -1},
		{"quota<0 也表示不限额 → -1", -5, 12345, -1},
	}
	for _, c := range cases {
		if got := remainingQuota(c.quota, c.used); got != c.want {
			t.Fatalf("%s: remainingQuota(%d, %d) = %d，期望 %d", c.name, c.quota, c.used, got, c.want)
		}
	}

	if got := usedRatio(0, 500); got != 0 {
		t.Fatalf("不限额时使用率应为 0，实际 %v", got)
	}
	if got := usedRatio(200, 100); got != 0.5 {
		t.Fatalf("使用率应为 0.5，实际 %v", got)
	}
	if got := usedRatio(200, 400); got != 2 {
		t.Fatalf("超额时使用率应 >1，实际 %v", got)
	}
}

// TestNormalizeUsageDays 校验窗口参数归一化（默认 30、上限 90）。
func TestNormalizeUsageDays(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 30}, {-3, 30}, {1, 1}, {30, 30}, {90, 90}, {365, 90},
	}
	for _, c := range cases {
		if got := normalizeUsageDays(c.in); got != c.want {
			t.Fatalf("normalizeUsageDays(%d) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

// TestValidateAIStrategy 校验策略白名单（保存成功但引擎静默降级是最难排查的一类问题）。
func TestValidateAIStrategy(t *testing.T) {
	for _, ok := range []string{"third_party", "self_hosted", "hybrid", ""} {
		if err := validateAIStrategy(ok); err != nil {
			t.Fatalf("策略 %q 应通过，实际 %v", ok, err)
		}
	}
	for _, bad := range []string{"openai", "HYBRID", "local"} {
		if err := validateAIStrategy(bad); err == nil {
			t.Fatalf("策略 %q 应被拒绝", bad)
		}
	}
}
