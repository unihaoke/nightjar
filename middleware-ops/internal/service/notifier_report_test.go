package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
)

// 本文件钉住「AI 结论要能被顺着点到完整报告」这件事。
//
// 为什么值得测：IM 卡片只能放摘要，而开放接口把补丁、验证结果与完整 Markdown
// 都放在它自己的报告页上。少一个可点的入口，值班同学就得先在平台上找到这条告警、
// 再从库里翻报告——链路一断，"AI 分析了"就变成了"AI 分析过"。

// TestBuildBriefCarriesReportURLAndMarkdown 结论摘要要带上报告地址与正文。
func TestBuildBriefCarriesReportURLAndMarkdown(t *testing.T) {
	raw := []byte(`{
		"runId":"run_1","taskId":"task_1","status":"succeeded",
		"rootCause":{"summary":"未判空","confidence":0.9},
		"reportUrl":"https://console.x/r/rep_1","markdown":"# 分析报告\n根因是未判空"
	}`)
	answer := parseOpenAPIResult(raw).Answer
	brief := (&CodeAnalysisService{}).buildBrief(&model.AIAnalysisTask{TaskID: "t-1"}, answer)
	if brief.ReportURL != "https://console.x/r/rep_1" {
		t.Fatalf("报告地址未带出：%q", brief.ReportURL)
	}
	if !strings.Contains(brief.Markdown, "根因是未判空") {
		t.Fatalf("报告 Markdown 未带出：%q", brief.Markdown)
	}
	if brief.RootCause != "未判空" {
		t.Fatalf("根因解析异常：%q", brief.RootCause)
	}
}

// TestNotifyLogEventCarriesReportButton 飞书卡片要给出「查看完整报告」按钮。
func TestNotifyLogEventCarriesReportButton(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()

	cfg := config.NotifyConfig{
		Enabled: true,
		Feishu:  config.WebhookChannelConfig{Enabled: true, Webhook: srv.URL},
	}
	notifier := NewNotifierService(&cfg, "http://platform", nil, nil, zap.NewNop())

	brief := &LogAnalysisBrief{
		RootCause:     "OrderService.create 未判空",
		FixSuggestion: "增加空值保护",
		Confidence:    0.9,
		ReportURL:     "https://console.x/r/rep_1",
		Markdown:      "# 分析报告\n根因是未判空",
	}
	err := notifier.NotifyLogEvent(context.Background(),
		model.LogAlertEvent{ServiceName: "order-svc", ErrorMessage: "NPE", Severity: "critical"},
		model.LogAlertRule{Name: "订单异常", AIEnabled: true},
		[]string{"feishu"}, brief)
	if err != nil {
		t.Fatalf("发送失败: %v", err)
	}

	card, ok := payload["card"].(map[string]any)
	if !ok {
		t.Fatalf("响应不是飞书卡片：%v", payload)
	}
	elements, _ := card["elements"].([]any)

	// ① 按钮：必须多出一个指向报告的按钮。
	var (
		reportURL string
		labels    []string
		detailURL string
	)
	for _, el := range elements {
		block, _ := el.(map[string]any)
		if block["tag"] != "action" {
			continue
		}
		actions, _ := block["actions"].([]any)
		for _, a := range actions {
			btn, _ := a.(map[string]any)
			txt, _ := btn["text"].(map[string]any)
			label, _ := txt["content"].(string)
			labels = append(labels, label)
			switch label {
			case "查看完整报告":
				reportURL, _ = btn["url"].(string)
			case "查看详情":
				detailURL, _ = btn["url"].(string)
			}
		}
	}
	if reportURL != "https://console.x/r/rep_1" {
		t.Fatalf("缺少「查看完整报告」按钮或地址不对：%q", reportURL)
	}

	// ② 主按钮文案：日志告警在 IM 上没有"确认"动作，不能沿用指标告警的「查看详情 / 确认」。
	if detailURL == "" {
		t.Fatalf("主按钮文案应为「查看详情」，实际按钮：%v", labels)
	}
	for _, label := range labels {
		if strings.Contains(label, "确认") {
			t.Fatalf("日志告警不该出现「确认」字样（会让人以为点一下就算确认）：%v", labels)
		}
	}
	// 落点是列表页并带上事件号，前端据此自动展开该条详情。
	if !strings.Contains(detailURL, "/log-alerts?event_id=") {
		t.Fatalf("详情地址应落到日志告警列表并带 event_id：%q", detailURL)
	}

	// ② 报告正文：截断后作为独立段落出现，README 里那种"只有结论没有细节"要避免。
	joined := ""
	for _, el := range elements {
		block, _ := el.(map[string]any)
		if text, ok := block["text"].(map[string]any); ok {
			joined += "\n" + stringOf(text["content"])
		}
	}
	if !strings.Contains(joined, "根因是未判空") {
		t.Fatalf("卡片未带上报告正文：%q", joined)
	}
}

// TestRenderTextCarriesReportLink 无卡片渠道（企微/钉钉/邮件）退化为可点的链接。
func TestRenderTextCarriesReportLink(t *testing.T) {
	n := AlertNotification{
		Content:   "**错误信息**：NPE",
		Fields:    []NotificationField{{Key: "服务", Value: "order-svc"}},
		ReportURL: "https://console.x/r/rep_1",
	}
	got := n.renderText()
	if !strings.Contains(got, "完整报告：https://console.x/r/rep_1") {
		t.Fatalf("文本渠道未带上报告链接：%q", got)
	}
	if strings.Contains((AlertNotification{Content: "x"}).renderText(), "完整报告") {
		t.Fatal("没有报告地址时不该出现空的报告行")
	}
}
