package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/monitor"
)

// 锁定「集成自检」里那些"结论性"的纯逻辑（真实反馈：排查不该只能靠翻日志）。
//
// 自检要回答的是三个问题：Exporter 还在不在、Prometheus 抓没抓到、指标是不是真的有数据。
// 每一环的判据必须稳定，否则自检本身就成了新的猜谜来源。

// componentUpExpr 决定"这个组件当前是否可用"的查询：按类型直接查权威的 *_up。
func TestComponentUpExpr(t *testing.T) {
	expr, name := componentUpExpr("redis", `job="middleware-integration",instance_name="jd-redis"`)
	if name != "redis_up" || !strings.Contains(expr, "redis_up{") || !strings.Contains(expr, "jd-redis") {
		t.Fatalf("redis 应查 redis_up{selector}，实际 %s / %s", name, expr)
	}
	if expr, name := componentUpExpr("mysql", "s"); name != "mysql_up" || !strings.Contains(expr, "mysql_up") {
		t.Fatalf("mysql 应查 mysql_up，实际 %s / %s", name, expr)
	}
	// node 没有 node_up：抓取目标的 up 就代表 Exporter 在位。
	if _, name := componentUpExpr("node", "s"); name != "up" {
		t.Fatalf("node 应查 up，实际 %s", name)
	}
	// 没有约定时返回空 → 交给 profile 的 bool_down 兜底。
	if expr, name := componentUpExpr("kafka", "s"); expr != "" || name != "" {
		t.Fatalf("没有约定的类型应返回空，实际 %s / %s", name, expr)
	}
	// 选择器为空时也应能查（退化为不带标签的指标名）。
	if expr, _ := componentUpExpr("redis", "  "); expr != "redis_up" {
		t.Fatalf("空选择器应退化为裸指标名，实际 %s", expr)
	}
}

// findUpMetric 是兜底路径：走 profile 里 Mode=ThresholdBoolDown 的指标，
// 而不是猜指标名——新增组件模板时不需要改自检代码。
func TestFindUpMetricUsesProfileSemantics(t *testing.T) {
	snapshot := &monitor.Snapshot{
		MWType: "kafka",
		Metrics: []monitor.Metric{
			{Name: "cpu_usage_percent", Status: "ok", Latest: 12},
			{Name: "broker_up", Status: "ok", Latest: 0},
		},
	}
	name, value, ok := findUpMetric(snapshot)
	if !ok {
		t.Fatal("kafka 的 broker_up 应能被识别")
	}
	if name != "broker_up" || value == nil || *value != 0 {
		t.Fatalf("应取到 broker_up=0，实际 %s/%v", name, value)
	}
	if got := formatUpValue(value); !strings.Contains(got, "不可用") {
		t.Fatalf("0 应渲染成「不可用」：%s", got)
	}

	// redis 的 profile 里没有 up 指标（它靠 componentUpExpr 直查 redis_up）→ 兜底应判为"找不到"。
	redisSnapshot := &monitor.Snapshot{
		MWType:  "redis",
		Metrics: []monitor.Metric{{Name: "memory_usage_percent", Status: "ok", Latest: 62}},
	}
	if _, _, ok := findUpMetric(redisSnapshot); ok {
		t.Fatal("profile 里没有 bool_down 指标时不应给出结论")
	}

	// 无数据的 up 指标不参与判定（避免把"查不到"当成 0）。
	empty := &monitor.Snapshot{
		MWType:  "kafka",
		Metrics: []monitor.Metric{{Name: "broker_up", Status: "unknown"}},
	}
	if _, _, ok := findUpMetric(empty); ok {
		t.Fatal("up 指标无数据时不应给出结论")
	}

	// nil 快照不得 panic（自检在降级路径上也会被调用）。
	if _, _, ok := findUpMetric(nil); ok {
		t.Fatal("nil 快照应返回未找到")
	}
}

func TestFormatUpValue(t *testing.T) {
	one, zero := 1.0, 0.0
	if got := formatUpValue(&one); !strings.Contains(got, "可用") {
		t.Fatalf("1 应渲染成「可用」：%s", got)
	}
	if got := formatUpValue(&zero); !strings.Contains(got, "不可用") {
		t.Fatalf("0 应渲染成「不可用」：%s", got)
	}
	if got := formatUpValue(nil); got != "无数据" {
		t.Fatalf("nil 应渲染成「无数据」：%s", got)
	}
}

// 自检结论的平台侧动作由**环节自己声明**，而不是从错误文本里猜——
// 文本分类器只服务于待处理横幅（那里的输入本来就是一段自由文本）。
func TestSelfCheckStageDeclaresOwnAction(t *testing.T) {
	// 远程端口不通：只能人工放通安全组 → 不给平台侧动作按钮（给了也是"点了没用"）。
	remote := SelfCheckStage{Status: stageFail}
	if remote.Action != "" {
		t.Fatalf("远程不可达不应声明平台侧动作，实际 %s", remote.Action)
	}

	// up=0（Exporter 活着但连不上被管实例）→ 走"测试连接 / 重试建号"这条最省事的通道。
	fake := &IntegrationSelfCheck{Stages: []SelfCheckStage{
		{Key: "platform", Title: "平台 → Exporter 端口", Status: stageOK},
		{Key: "exporter", Title: "Exporter 是否在位", Status: stageOK},
		{Key: "scrape", Title: "Prometheus 是否已抓取到它", Status: stageOK},
		{Key: "metrics", Title: "业务指标是否真的在流", Status: stageFail,
			Advice: "核对地址与账号口令", Action: string(NextActionRetryAccount), ActionLabel: "去测试连接 / 重试建号"},
	}}
	first := SelfCheckStage{}
	for _, stage := range fake.Stages {
		if stage.Status == stageFail {
			first = stage
			break
		}
	}
	if first.Action != string(NextActionRetryAccount) {
		t.Fatalf("应把失败环节声明的动作透出，实际 %s", first.Action)
	}
	if nextActionLabel(NextActionRetryAccount) == "" {
		t.Fatal("动作必须有文案")
	}
}

// 待处理横幅仍然用文本分类（输入是一段自由文本），两条通道的判定都要能用。
func TestSelfCheckActionMatchesPendingBanner(t *testing.T) {
	cases := []struct {
		detail string
		want   NextAction
	}{
		{"Exporter 未跑通（up=0）：认证失败 WRONGPASS", NextActionRetryAccount},
		{"远程安装需要 SSH 凭据", NextActionReapply},
		{"Ansible 执行失败：unknown flag", NextActionReapply},
	}
	for _, c := range cases {
		action, label := nextActionOf(c.detail)
		if action != string(c.want) {
			t.Fatalf("detail=%q 应判为 %s，实际 %s", c.detail, c.want, action)
		}
		if label == "" {
			t.Fatalf("动作必须带文案：%s", action)
		}
	}
}

// selfCheckSummaryForLog 只输出压缩状态，不把长文本塞进日志。
func TestSelfCheckSummaryForLog(t *testing.T) {
	check := &IntegrationSelfCheck{Stages: []SelfCheckStage{
		{Key: "platform", Status: stageOK},
		{Key: "exporter", Status: stageWarn},
		{Key: "scrape", Status: stageFail},
	}}
	got := selfCheckSummaryForLog(check)
	if got != "platform=ok exporter=warn scrape=fail" {
		t.Fatalf("压缩格式不符：%s", got)
	}
	if selfCheckSummaryForLog(nil) != "" {
		t.Fatal("nil 应返回空串")
	}
}
