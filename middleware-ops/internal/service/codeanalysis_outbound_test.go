package service

import (
	"strings"
	"testing"

	"go.uber.org/zap"
)

// 本文件钉住两件事：
//  1. "能不能把问题发给外部 AI"这条合规判定的接线（INC-030 的回归）；
//  2. 外部 AI 服务响应的宽松解析（不同服务的字段名各不相同）。

// TestOutboundGate 钉住**闸门的接线**：
// "未许可"必须等于"不调用"，而且原因必须给出（否则使用者无从解释为什么没有结论）。
//
// 这条测试是补上来的：早先只测了判定纯函数，把调用点的判定去掉后测试照样全绿——
// 而那个"去掉"正是原始缺陷本身。
func TestOutboundGate(t *testing.T) {
	cases := []struct {
		name        string
		forceLocal  bool
		whitelisted bool
		wantAllowed bool
		wantBlocked bool
	}{
		{"许可齐全 → 放行", false, true, true, false},
		{"force_local → 阻断", true, true, false, true},
		{"不在白名单 → 阻断", false, false, false, true},
		{"force_local + 不在白名单 → 阻断", true, false, false, true},
	}
	for _, c := range cases {
		svc := &CodeAnalysisService{log: zap.NewNop()}
		allowed, blocked, _, reason := svc.outboundGate(c.forceLocal, c.whitelisted, "svc")
		if allowed != c.wantAllowed || blocked != c.wantBlocked {
			t.Fatalf("%s：allowed=%v blocked=%v，期望 allowed=%v blocked=%v",
				c.name, allowed, blocked, c.wantAllowed, c.wantBlocked)
		}
		if c.wantBlocked && strings.TrimSpace(reason) == "" {
			t.Fatalf("%s：阻断时必须给出原因", c.name)
		}
		if !c.wantBlocked && reason != "" {
			t.Fatalf("%s：放行时不该有阻断原因，实际 %q", c.name, reason)
		}
	}
}

// TestThirdPartyAllowedFor 钉住两个条件（INC-030 的判定表）。
func TestThirdPartyAllowedFor(t *testing.T) {
	cases := []struct {
		name        string
		forceLocal  bool
		whitelisted bool
		wantAllowed bool
		wantWarn    string
	}{
		{"在白名单 → 允许", false, true, true, ""},
		{"不在白名单 → 不允许，并给出说明", false, false, false, "不在出网白名单"},
		{"force_local → 不允许（本次显式要求）", true, true, false, ""},
	}
	for _, c := range cases {
		got, warn := thirdPartyAllowedFor(c.forceLocal, c.whitelisted, "svc")
		if got != c.wantAllowed {
			t.Fatalf("%s：allowed = %v，期望 %v", c.name, got, c.wantAllowed)
		}
		if c.wantWarn != "" && !strings.Contains(warn, c.wantWarn) {
			t.Fatalf("%s：告警应提到 %q，实际 %q", c.name, c.wantWarn, warn)
		}
	}
}

// TestOutboundBlockReasonIsActionable 钉住阻断原因的"可操作性"：
// 使用者看到的不能只是"被拒绝"，他得知道去改哪个配置。
func TestOutboundBlockReasonIsActionable(t *testing.T) {
	svc := &CodeAnalysisService{log: zap.NewNop()}

	reason := svc.outboundBlockReason("order-api", false)
	for _, want := range []string{"order-api", "出网白名单", "outbound_whitelist"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("阻断原因应包含 %q，实际：%s", want, reason)
		}
	}
	// force_local 时要说清是本次请求自己的要求。
	if got := svc.outboundBlockReason("order-api", true); !strings.Contains(got, "force_local") {
		t.Fatalf("应提到 force_local，实际：%s", got)
	}
}

// TestParseTaskResultNormalizesFields 钉住"宽松解析"：字段名各家不同，
// 认不出来的字段只是少一点信息，不该让整次分析失败。
func TestParseTaskResultNormalizesFields(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantTaskID string
		wantStatus string
		wantAnswer string
	}{
		{
			name:       "标准字段",
			body:       `{"task_id":"t-1","status":"succeeded","answer":"根因是空指针"}`,
			wantTaskID: "t-1", wantStatus: modelStatusSucceeded, wantAnswer: "根因是空指针",
		},
		{
			name:       "别名与 data 嵌套",
			body:       `{"id":"t-2","state":"DONE","data":{"result":"修好了","status":"completed"}}`,
			wantTaskID: "t-2", wantStatus: modelStatusSucceeded, wantAnswer: "修好了",
		},
		{
			name:       "仍在处理",
			body:       `{"task_id":"t-3","status":"running"}`,
			wantTaskID: "t-3", wantStatus: "", wantAnswer: "",
		},
		{
			name:       "纯文本响应",
			body:       `根因是连接池耗尽`,
			wantTaskID: "", wantStatus: modelStatusSucceeded, wantAnswer: "根因是连接池耗尽",
		},
	}
	for _, c := range cases {
		got := parseTaskResult([]byte(c.body))
		if got.TaskID != c.wantTaskID || got.Status != c.wantStatus || got.Answer != c.wantAnswer {
			t.Fatalf("%s：解析结果 %+v，期望 task=%q status=%q answer=%q",
				c.name, got, c.wantTaskID, c.wantStatus, c.wantAnswer)
		}
	}
}

// TestParseCallback 钉住回调的两种结局与"必须有 task_id"。
func TestParseCallback(t *testing.T) {
	taskID, status, answer, _, err := ParseCallback([]byte(`{"task_id":"t-9","answer":"结论"}`))
	if err != nil {
		t.Fatalf("解析回调失败: %v", err)
	}
	if taskID != "t-9" || status != "succeeded" || answer != "结论" {
		t.Fatalf("回调解析结果异常：%s %s %s", taskID, status, answer)
	}
	_, status, _, _, err = ParseCallback([]byte(`{"task_id":"t-9","status":"failed","error":"分析报错"}`))
	if err != nil {
		t.Fatalf("解析失败回调出错: %v", err)
	}
	if status != "failed" {
		t.Fatalf("失败回调应解析为 failed，实际 %s", status)
	}
	// 没有 task_id 的回调无法对上号，必须报错而不是"静默丢弃"。
	if _, _, _, _, err := ParseCallback([]byte(`{"answer":"结论"}`)); err == nil {
		t.Fatal("缺少 task_id 的回调应当报错")
	}
}
