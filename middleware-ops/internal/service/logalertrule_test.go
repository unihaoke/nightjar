package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"middleware-ops/internal/model"
	"middleware-ops/internal/pkg/cache"
)

// 本文件锁定「日志告警规则」与「冷却抑制」的判定语义。
//
// 为什么这两块最值得测：它们决定"多久打扰人一次"和"这条告警到底会不会通知/分析"。
// 判错的后果是使用者完全感知不到的静默失效——规则配了不生效、或者该通知的全被冷却吞掉，
// 页面上看不出任何异常。

// ---------------------------------------------------------------------------
// 规则匹配
// ---------------------------------------------------------------------------

// TestMatchLogAlertRule 校验匹配的四个维度与优先级顺序。
func TestMatchLogAlertRule(t *testing.T) {
	rules := []model.LogAlertRule{
		{Base: model.Base{ID: 1}, Name: "通用", Priority: 100, Enabled: true, MinSeverity: "WARN"},
		{Base: model.Base{ID: 2}, Name: "订单特例", Priority: 10, Enabled: true, ServiceName: "order-api"},
		{Base: model.Base{ID: 3}, Name: "停用规则", Priority: 5, Enabled: false},
		{Base: model.Base{ID: 4}, Name: "支付错误", Priority: 20, Enabled: true,
			ServiceName: "pay-api", SignaturePattern: "/timeout|refused/"},
	}
	cases := []struct {
		name    string
		service string
		message string
		level   string
		wantID  int64
		wantHit bool
	}{
		{"具体服务优先于通用（priority 小者胜）", "order-api", "boom", "ERROR", 2, true},
		{"正则命中的服务规则", "pay-api", "connection timeout after 3s", "ERROR", 4, true},
		{"正则不命中时回落到通用规则", "pay-api", "some other error", "ERROR", 1, true},
		// 级别过滤是**按规则**生效的：#2 没写 min_severity（不限级别）且优先级更高，
		// 所以 INFO 事件仍然命中 #2，而不是被"跳过 #2 后命中 #1（WARN 门槛）"。
		{"规则自身的级别门槛只过滤它自己", "order-api", "boom", "INFO", 2, true},
		{"没有可用规则时不命中", "unknown-svc", "boom", "INFO", 0, false},
		{"停用的规则永不命中", "pay-api", "timeout", "FATAL", 4, true},
	}
	for _, c := range cases {
		got, hit := MatchLogAlertRule(rules, c.service, c.message, c.level)
		if hit != c.wantHit {
			t.Fatalf("%s: 命中=%v，期望 %v", c.name, hit, c.wantHit)
		}
		if hit && got.ID != c.wantID {
			t.Fatalf("%s: 命中规则 #%d，期望 #%d", c.name, got.ID, c.wantID)
		}
	}
}

// TestMatchLogAlertRuleUsesDefaultLevelRank 校验未知级别按 ERROR 处理。
//
// 为什么重要：Filebeat 未解析出 log.level 时平台按内容推断，可能出现奇怪的值；
// 若被当成 INFO，配了 min_severity=ERROR 的规则就会静默失效。
func TestMatchLogAlertRuleUsesDefaultLevelRank(t *testing.T) {
	rules := []model.LogAlertRule{
		{Base: model.Base{ID: 1}, Enabled: true, MinSeverity: "ERROR"},
	}
	if _, hit := MatchLogAlertRule(rules, "svc", "boom", "weird-level"); !hit {
		t.Fatal("未知级别应按 ERROR 处理，从而命中 min_severity=ERROR 的规则")
	}
}

// TestMessageMatches 校验消息匹配的三种写法与容错。
func TestMessageMatches(t *testing.T) {
	cases := []struct {
		pattern string
		message string
		want    bool
	}{
		{"", "anything", true},
		{"timeout", "connection timeout after 3s", true},
		{"TIMEOUT", "connection timeout after 3s", true}, // 大小写不敏感
		{"oom", "connection timeout", false},
		{"/timeout|refused/", "dial refused", true},
		{"/^NullPointer/", "NullPointerException at A.f", true},
		// 正则写错时不 panic，退化为子串匹配（规则写错不该让日志链路挂掉）
		{"/[unclosed/", "boom [unclosed bracket", true},
	}
	for _, c := range cases {
		if got := messageMatches(c.pattern, c.message); got != c.want {
			t.Fatalf("messageMatches(%q, %q)=%v，期望 %v", c.pattern, c.message, got, c.want)
		}
	}
}

// TestMatchLogAlertRuleMatchesRawMessageNotFingerprint 钉住"规则按日志原文匹配"这件事。
//
// 为什么要专门钉一条：signature 是归一化后的哈希，若拿哈希去比 pattern，
// 使用者照着日志里那句话配出来的规则永远不命中，而页面上看不出任何异常——
// 只会表现为"日志进来了但没有告警"，是最难查的一类静默失效。
func TestMatchLogAlertRuleMatchesRawMessageNotFingerprint(t *testing.T) {
	rules := []model.LogAlertRule{
		{Base: model.Base{ID: 1}, Name: "框架噪音", Enabled: true,
			SignaturePattern: "Request method 'GET' is not supported"},
	}
	message := "Resolved [org.springframework.web.HttpRequestMethodNotSupportedException: Request method 'GET' is not supported]"
	fingerprint := ErrorSignature(message, "")

	if _, hit := MatchLogAlertRule(rules, "jd-logs", message, "ERROR"); !hit {
		t.Fatal("按日志原文应命中规则")
	}
	if _, hit := MatchLogAlertRule(rules, "jd-logs", fingerprint, "ERROR"); hit {
		t.Fatalf("拿指纹（%s）去匹配不应命中——那说明又退回成按哈希比了", fingerprint)
	}
}

// TestSeverityRank 校验级别排序（FATAL > ERROR > WARN > INFO）。
func TestSeverityRank(t *testing.T) {
	if !(severityRank("FATAL") > severityRank("ERROR") &&
		severityRank("ERROR") > severityRank("WARN") &&
		severityRank("WARN") > severityRank("INFO")) {
		t.Fatal("级别排序必须严格递减：FATAL > ERROR > WARN > INFO")
	}
	if severityRank("error") != severityRank("ERROR") {
		t.Fatal("级别比较应忽略大小写")
	}
}

// TestBuildLogAlertRuleValidation 校验规则入参的校验与默认值。
func TestBuildLogAlertRuleValidation(t *testing.T) {
	svc := &LogAlertService{}
	base := model.LogAlertRule{DedupWindow: 5, Cooldown: 10, AIEnabled: true, Enabled: true}
	ai, enabled := true, true

	// 正常：只填名字，其余走默认值。
	rule, err := svc.buildRule(base, LogAlertRuleInput{
		Name: "默认规则", AIEnabled: &ai, Enabled: &enabled,
	})
	if err != nil {
		t.Fatalf("合法入参不应报错：%v", err)
	}
	if rule.Priority != 100 {
		t.Fatalf("未填优先级时应为默认 100，实际 %d", rule.Priority)
	}
	if rule.DedupWindow != 5 || rule.Cooldown != 10 {
		t.Fatalf("未传窗口/冷却时应沿用基线值，实际 %d/%d", rule.DedupWindow, rule.Cooldown)
	}

	// 显式传 0：表示"不去重/不冷却"，必须被尊重（而不是被当成未填）。
	zero := 0
	rule, err = svc.buildRule(base, LogAlertRuleInput{Name: "关闭去重", DedupWindow: &zero, Cooldown: &zero})
	if err != nil {
		t.Fatalf("显式传 0 是合法入参：%v", err)
	}
	if rule.DedupWindow != 0 || rule.Cooldown != 0 {
		t.Fatalf("显式传 0 应被尊重，实际 %d/%d", rule.DedupWindow, rule.Cooldown)
	}

	neg := -1
	bad := []struct {
		name string
		in   LogAlertRuleInput
	}{
		{"名字为空", LogAlertRuleInput{Name: "  "}},
		{"窗口为负", LogAlertRuleInput{Name: "x", DedupWindow: &neg}},
		{"冷却为负", LogAlertRuleInput{Name: "x", Cooldown: &neg}},
		{"级别非法", LogAlertRuleInput{Name: "x", MinSeverity: "CRITICAL"}},
		{"渠道非法", LogAlertRuleInput{Name: "x", NotifyChannels: []string{"sms"}}},
		{"正则非法", LogAlertRuleInput{Name: "x", SignaturePattern: "/[unclosed/"}},
	}
	for _, c := range bad {
		if _, err := svc.buildRule(base, c.in); err == nil {
			t.Fatalf("%s：必须报错", c.name)
		}
	}
}

// TestNormalizeChannelList 校验渠道去重与稳定顺序（顺序会影响 IM 群里的展示）。
func TestNormalizeChannelList(t *testing.T) {
	got := normalizeChannelList([]string{" Feishu ", "feishu", "wecom", "", "email", "WECOM"})
	want := []string{"email", "feishu", "wecom"}
	if len(got) != len(want) {
		t.Fatalf("去重后长度应为 %d，实际 %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项=%q，期望 %q（必须稳定排序）", i, got[i], want[i])
		}
	}
}

// ---------------------------------------------------------------------------
// 冷却期
// ---------------------------------------------------------------------------

// memoryStore 是测试用的内存 Store（只实现冷却追踪用到的方法）。
type memoryStore struct {
	values  map[string]string
	failGet bool
}

func newMemoryStore() *memoryStore { return &memoryStore{values: map[string]string{}} }

func (m *memoryStore) Get(_ context.Context, key string) (string, error) {
	if m.failGet {
		return "", errors.New("redis 抖动")
	}
	value, ok := m.values[key]
	if !ok {
		return "", cache.ErrNotFound
	}
	return value, nil
}

func (m *memoryStore) Set(_ context.Context, key, value string, _ time.Duration) error {
	m.values[key] = value
	return nil
}

func (m *memoryStore) Del(_ context.Context, keys ...string) error {
	for _, key := range keys {
		delete(m.values, key)
	}
	return nil
}

func (m *memoryStore) Exists(ctx context.Context, key string) (bool, error) {
	value, err := m.Get(ctx, key)
	return value != "" && err == nil, nil
}

func (m *memoryStore) IncrBy(context.Context, string, int64) (int64, error) { return 1, nil }
func (m *memoryStore) Expire(context.Context, string, time.Duration) error  { return nil }
func (m *memoryStore) Ping(context.Context) error                           { return nil }
func (m *memoryStore) Kind() string                                         { return "memory" }
func (m *memoryStore) Close() error                                         { return nil }
func (m *memoryStore) Enqueue(context.Context, cache.Task) error            { return nil }
func (m *memoryStore) Dequeue(context.Context) (cache.Task, error)          { return cache.Task{}, nil }
func (m *memoryStore) Ack(context.Context, cache.Task) error                { return nil }
func (m *memoryStore) Len(context.Context) (int64, error)                   { return 0, nil }

// TestCooldownTracker 校验"冷却期内抑制、冷却过后放行"。
func TestCooldownTracker(t *testing.T) {
	store := newMemoryStore()
	tracker := newCooldownTracker(store, "logalert:cd:")
	key := "order-api|NullPointerException"
	now := time.Now().UTC()

	if cooling, err := tracker.inCooldown(context.Background(), key, 10, now); err != nil || cooling {
		t.Fatalf("没有记录时不应处于冷却：cooling=%v err=%v", cooling, err)
	}
	if err := tracker.markSent(context.Background(), key, now, 10); err != nil {
		t.Fatalf("记录发送时间失败：%v", err)
	}
	if cooling, _ := tracker.inCooldown(context.Background(), key, 10, now.Add(5*time.Minute)); !cooling {
		t.Fatal("冷却期内（5 分钟 < 10 分钟）必须判为冷却中")
	}
	if cooling, _ := tracker.inCooldown(context.Background(), key, 10, now.Add(11*time.Minute)); cooling {
		t.Fatal("超过冷却期（11 分钟 > 10 分钟）应放行")
	}

	// 人工打破冷却：clear 之后立即放行。
	if err := tracker.clear(context.Background(), key); err != nil {
		t.Fatalf("清除冷却失败：%v", err)
	}
	if cooling, _ := tracker.inCooldown(context.Background(), key, 10, now.Add(time.Minute)); cooling {
		t.Fatal("clear 之后不应再判为冷却（「重新分析」依赖这一点）")
	}
}

// TestCooldownTrackerNeverSuppressesOnStoreFailure 校验 Redis 故障时不吞告警。
//
// 这是刻意的取向：读失败时"多打扰一次"远好于"静默丢掉一条告警"。
func TestCooldownTrackerNeverSuppressesOnStoreFailure(t *testing.T) {
	store := newMemoryStore()
	store.failGet = true
	tracker := newCooldownTracker(store, "logalert:cd:")

	cooling, err := tracker.inCooldown(context.Background(), "svc|sig", 10, time.Now().UTC())
	if err == nil {
		t.Fatal("存储读失败时应把错误返回给调用方（以便记录日志）")
	}
	if cooling {
		t.Fatal("存储读失败时绝不能判为冷却中——那等于把告警静默掉")
	}
	// cooldown=0 表示不冷却：连读都不该读（store 是坏的，读了一定报错）。
	cooling, err = tracker.inCooldown(context.Background(), "svc|sig", 0, time.Now().UTC())
	if err != nil || cooling {
		t.Fatalf("冷却时长为 0 时应直接放行且不访问存储：cooling=%v err=%v", cooling, err)
	}
}

// ---------------------------------------------------------------------------
// 后处理判定
// ---------------------------------------------------------------------------

// TestAnalysisDecision 校验"该不该分析、不分析时给什么原因"。
func TestAnalysisDecision(t *testing.T) {
	on := model.LogAlertRule{AIEnabled: true}
	off := model.LogAlertRule{AIEnabled: false}

	if state, _ := analysisDecision(on, "order-api", true); state != "" {
		t.Fatalf("条件齐备时应继续分析，实际 state=%q", state)
	}
	if state, reason := analysisDecision(off, "order-api", true); state != model.LogAnalysisDisabled ||
		!strings.Contains(reason, "未启用") {
		t.Fatalf("规则关闭 AI 时应判禁用并说明原因：state=%q reason=%q", state, reason)
	}
	if state, reason := analysisDecision(on, "order-api", false); state != model.LogAnalysisDisabled ||
		!strings.Contains(reason, "未装配") {
		t.Fatalf("平台未配置 AI 分析服务时应判禁用并说明原因：state=%q reason=%q", state, reason)
	}
}

// TestSuppressReasonExplainsCooldown 校验抑制原因里带上了冷却截止时间（页面直接展示）。
func TestSuppressReasonExplainsCooldown(t *testing.T) {
	until := time.Date(2026, 3, 1, 10, 30, 0, 0, time.UTC)
	reason := suppressReason(&until)
	if !strings.Contains(reason, "冷却期内抑制") || !strings.Contains(reason, "重新分析") {
		t.Fatalf("抑制原因应说明「为什么」和「怎么办」，实际 %q", reason)
	}
	if !strings.Contains(suppressReason(nil), "冷却期内抑制") {
		t.Fatal("没有截止时间时也要给出原因")
	}
}

// TestNotifySuffixKeepsFailureVisible 校验"通知失败"不会被分析成功覆盖掉。
//
// 真实后果：分析成功后页面显示「已分析」，而使用者从没收到过通知，且找不到任何解释。
func TestNotifySuffixKeepsFailureVisible(t *testing.T) {
	if got := notifySuffix(nil); got != "" {
		t.Fatalf("通知成功时不应追加说明，实际 %q", got)
	}
	got := notifySuffix(errors.New("飞书渠道未启用或未配置 webhook"))
	if !strings.Contains(got, "通知未送达") || !strings.Contains(got, "飞书") {
		t.Fatalf("通知失败时必须把原因留在状态说明里，实际 %q", got)
	}
}
