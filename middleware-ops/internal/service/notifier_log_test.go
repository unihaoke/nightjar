package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/model"
)

// 日志告警通知的可读性必须有单测钉住：正文发的是"哈希指纹"还是"错误消息原文"，
// 单看代码看不出差别——出错时值班同学在 IM 里只看到一串十六进制，而没有任何人报错。
func TestLogErrorBlocksPrefersMessageOverSignature(t *testing.T) {
	event := model.LogAlertEvent{
		ServiceName:    "jd-logs",
		ErrorSignature: strings.Repeat("a", 32),
		ErrorMessage:   "Request method 'GET' is not supported",
		ContextLines:   "line-1\nline-2",
		RawStacktrace:  "java.lang.NullPointerException\n\tat OrderService.process(OrderService.java:42)",
	}
	content, sections := logErrorBlocks(event)

	if !strings.Contains(content, "Request method 'GET' is not supported") {
		t.Fatalf("正文应给错误消息原文：%q", content)
	}
	if strings.Contains(content, event.ErrorSignature) {
		t.Fatalf("正文不应出现哈希指纹（人读不了）：%q", content)
	}
	if len(sections) != 2 {
		t.Fatalf("上下文与堆栈应各成一段，实际 %d 段：%v", len(sections), sections)
	}
	if !strings.Contains(sections[0], "line-2") || !strings.Contains(sections[1], "OrderService.process") {
		t.Fatalf("附加段落丢失内容：%v", sections)
	}
}

func TestLogErrorBlocksFallsBackToSignature(t *testing.T) {
	content, sections := logErrorBlocks(model.LogAlertEvent{ErrorSignature: strings.Repeat("b", 32)})

	if !strings.Contains(content, "bbbbbbbb") {
		t.Fatalf("无消息原文时应回落指纹短标签：%q", content)
	}
	if len(sections) != 0 {
		t.Fatalf("没有上下文/堆栈时不应产生附加段落：%v", sections)
	}
}

func TestTrimLogBlockLimitsLinesAndRunes(t *testing.T) {
	long := strings.Repeat("x", 600)
	got := trimLogBlock(strings.Join([]string{"1", "2", "3", "4"}, "\n"), 2, 600)
	if !strings.Contains(got, "已截断") {
		t.Fatalf("超行数应标注截断：%q", got)
	}
	if got2 := trimLogBlock(long, 8, 100); len([]rune(got2)) != 101 { // 100 + 省略号
		t.Fatalf("超长文本应按 rune 截断，实际长度 %d", len([]rune(got2)))
	}
	if trimLogBlock("   ", 8, 100) != "" {
		t.Fatal("空白文本应视为空")
	}
}

// 规则信息必须能被单测钉住："这条告警是哪条规则命中的"是使用者在群里问的第一句话。
func TestLogRuleValueCarriesNameAndConditions(t *testing.T) {
	rule := model.LogAlertRule{
		Name:             "支付服务-超时",
		ServiceName:      "pay-service",
		SignaturePattern: "TimeoutException",
		MinSeverity:      "ERROR",
	}
	rule.ID = 7

	got := logRuleValue(rule)
	for _, want := range []string{"支付服务-超时", "服务=pay-service", "消息包含 TimeoutException", "级别≥ERROR", "规则ID=7"} {
		if !strings.Contains(got, want) {
			t.Fatalf("命中规则行缺少 %q：%q", want, got)
		}
	}

	regexRule := model.LogAlertRule{Name: "GC 告警", SignaturePattern: "/full gc/"}
	if got := logRuleValue(regexRule); !strings.Contains(got, "消息正则") || !strings.Contains(got, "全部服务") {
		t.Fatalf("正则与服务范围应被正确描述：%q", got)
	}

	if got := logRuleValue(model.LogAlertRule{}); !strings.Contains(got, "历史事件") {
		t.Fatalf("没有规则时应显式说明是历史事件，而不是留空：%q", got)
	}
}

func TestLogRuleNameFallsBack(t *testing.T) {
	if got := logRuleName(model.LogAlertRule{Name: "超时告警"}); got != "超时告警" {
		t.Fatalf("规则名应原样返回：%q", got)
	}
	if got := logRuleName(model.LogAlertRule{}); got == "" {
		t.Fatal("标题里的规则名不能为空（会出现「服务｜」这种残缺标题）")
	}
}
