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
)

// 本文件钉住《AI 代码分析接口文档 v1》的对接形状。
//
// 为什么单独测：这套协议与 generic 的字段名几乎不重叠（X-API-Key / repoLocator /
// stacktrace / callbackUrl / runId / state / rootCause），任何一处拼错都会表现为
// "提交即 400"或"分析成功但没有结论"——后者在页面上和"AI 服务慢"长得一模一样，
// 靠人眼看日志是分不出来的。

// TestOpenAPISubmitAsync 钉住异步端点的路径、鉴权头与正文字段。
func TestOpenAPISubmitAsync(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotHeader string
		gotBody   map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotHeader = r.Header.Get("X-API-Key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"runId":"run_1","taskId":"task_1","status":"queued","acceptTime":"2026-09-23T10:00:00Z"}`))
	}))
	defer srv.Close()

	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k-1",
		Protocol: ProtocolOpenAPIV1, AuthHeader: "X-API-Key",
	}, zap.NewNop())

	res, err := client.Submit(context.Background(), SubmitInput{
		TaskID:         "mwo-1-1",
		Service:        "order-svc",
		Stacktrace:     "java.lang.NullPointerException\n\tat com.acme.OrderService.create(OrderService.java:42)",
		Message:        "NPE",
		Environment:    "prod",
		IdempotencyKey: "mwo-1-abc",
		CallbackURL:    "https://platform/api/ai/analysis/callback",
		EventID:        1,
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if gotPath != "/api/v1/openapi/tasks" {
		t.Fatalf("异步端点错误：%q", gotPath)
	}
	if gotHeader != "k-1" {
		t.Fatalf("X-API-Key 未带上：%q", gotHeader)
	}
	if gotAuth != "" {
		t.Fatalf("开放接口不该再发 Authorization: Bearer：%q", gotAuth)
	}
	if _, ok := gotBody["stacktrace"]; !ok {
		t.Fatalf("缺少必填字段 stacktrace：%v", gotBody)
	}
	if gotBody["callbackUrl"] != "https://platform/api/ai/analysis/callback" {
		t.Fatalf("callbackUrl 字段名或值不对（应为驼峰）：%v", gotBody)
	}
	if gotBody["idempotencyKey"] != "mwo-1-abc" {
		t.Fatalf("idempotencyKey 不对：%v", gotBody)
	}
	if gotBody["environment"] != "prod" {
		t.Fatalf("environment 不对：%v", gotBody)
	}
	locator, ok := gotBody["repoLocator"].(map[string]any)
	if !ok || locator["host"] != "order-svc" {
		t.Fatalf("repoLocator 应带 host=服务名：%v", gotBody)
	}
	if res.RunID != "run_1" || res.TaskID != "task_1" {
		t.Fatalf("结果异常：%+v", res)
	}
	// queued 是"还在处理"：平台侧统一记为 submitted（等回调或轮询）。
	if res.Status != modelStatusSubmitted {
		t.Fatalf("queued 不该被判成终态：%q", res.Status)
	}
}

// TestOpenAPISubmitSyncUsesAnalyzeEndpoint 同步模式必须换到 /analyze 端点。
func TestOpenAPISubmitSyncUsesAnalyzeEndpoint(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"runId":"run_2","taskId":"task_2","status":"succeeded","rootCause":{"summary":"未判空","confidence":0.9}}`))
	}))
	defer srv.Close()

	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: srv.URL, Protocol: ProtocolOpenAPIV1,
	}, zap.NewNop())

	res, err := client.SubmitWithTimeout(context.Background(), SubmitInput{
		TaskID: "mwo-2-1", Service: "order-svc", Stacktrace: "NPE", Sync: true,
	}, 0)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if gotPath != "/api/v1/openapi/analyze" {
		t.Fatalf("同步端点错误：%q", gotPath)
	}
	if res.Status != modelStatusSucceeded {
		t.Fatalf("状态应为 succeeded：%q", res.Status)
	}
	if !strings.Contains(res.Answer, "未判空") {
		t.Fatalf("结论里应带上根因摘要：%q", res.Answer)
	}
}

// TestOpenAPIParseResult 钉住终态映射与结论合成。
func TestOpenAPIParseResult(t *testing.T) {
	body := []byte(`{
		"runId":"run_3","taskId":"task_3","status":"needs_review","severity":"critical",
		"rootCause":{"summary":"OrderService.create 未判空","category":"null_pointer","confidence":0.92,
		             "detail":"并发下入参可能为 null","blastRadius":["下单链路"]},
		"patches":[{"repoKey":"order-service","filePath":"service/order.go","action":"modify","rationale":"增加空值保护"}],
		"reportId":"rep_3","reportUrl":"https://console.x/r/rep_3","markdown":"# 报告"
	}`)
	res := parseOpenAPIResult(body)
	if res.Status != modelStatusSucceeded {
		t.Fatalf("needs_review 是终态，应按有结论处理：%q", res.Status)
	}
	if res.TaskID != "task_3" || res.RunID != "run_3" {
		t.Fatalf("任务号解析异常：%+v", res)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(res.Answer), &decoded); err != nil {
		t.Fatalf("结论应是平台内部约定的 JSON：%v", err)
	}
	if decoded["root_cause"] != "OrderService.create 未判空\n\n并发下入参可能为 null" {
		t.Fatalf("root_cause 合成异常：%v", decoded["root_cause"])
	}
	if decoded["located_file"] != "service/order.go" {
		t.Fatalf("located_file 应取首个补丁的文件：%v", decoded["located_file"])
	}
	if decoded["confidence"] != 0.92 {
		t.Fatalf("confidence 应透传：%v", decoded["confidence"])
	}
	if decoded["report_url"] != "https://console.x/r/rep_3" {
		t.Fatalf("report_url 应保留：%v", decoded["report_url"])
	}

	// degraded / cancelled 也要有明确归宿：前者按有结论，后者按失败。
	if got := parseOpenAPIResult([]byte(`{"status":"degraded","runId":"r4"}`)).Status; got != modelStatusSucceeded {
		t.Fatalf("degraded 应按有结论处理：%q", got)
	}
	if got := parseOpenAPIResult([]byte(`{"status":"cancelled","runId":"r5"}`)).Status; got != modelStatusFailed {
		t.Fatalf("cancelled 应按失败处理：%q", got)
	}
}

// TestOpenAPICallback 钉住回调报文（字段名与同步响应完全不同）。
func TestOpenAPICallback(t *testing.T) {
	body := []byte(`{
		"runId":"run_6","state":"succeeded","reportId":"rep_6","severity":"high",
		"summary":"OrderService.create 未判空解引用","tenantId":"t-demo","taskId":"task_6","attempt":1,
		"rootCause":{"summary":"未判空","confidence":0.8},
		"patches":[{"filePath":"service/order.go","rationale":"增加空值保护"}],
		"reportUrl":"https://console.x/r/rep_6"
	}`)
	taskID, _, status, answer, errMsg, err := ParseCallbackWithProtocol(body, ProtocolOpenAPIV1)
	if err != nil {
		t.Fatalf("解析回调失败: %v", err)
	}
	if taskID != "task_6" {
		t.Fatalf("回调对号应用 taskId：%q", taskID)
	}
	if status != "succeeded" {
		t.Fatalf("状态解析异常：%q", status)
	}
	if !strings.Contains(answer, "未判空") || !strings.Contains(answer, "service/order.go") {
		t.Fatalf("回调结论不应为空（否则会被判成'成功但没内容'）：%q", answer)
	}
	if errMsg != "" {
		t.Fatalf("成功回调不该带错误：%q", errMsg)
	}

	// 只有 summary 没有 rootCause 时也不能丢结论。
	_, _, _, answer2, _, err := ParseCallbackWithProtocol(
		[]byte(`{"taskId":"task_7","state":"succeeded","summary":"连接池耗尽"}`), ProtocolOpenAPIV1)
	if err != nil {
		t.Fatalf("解析回调失败: %v", err)
	}
	if !strings.Contains(answer2, "连接池耗尽") {
		t.Fatalf("summary 应作为结论兜底：%q", answer2)
	}

	// 失败终态要带出原因。
	if _, _, st, _, msg, _ := ParseCallbackWithProtocol(
		[]byte(`{"taskId":"task_8","state":"failed","error":"仓库定位失败"}`), ProtocolOpenAPIV1); st != "failed" || msg != "仓库定位失败" {
		t.Fatalf("失败回调解析异常：status=%q err=%q", st, msg)
	}
}

// TestOpenAPIQueryUsesRunID 轮询地址按 runId 组织，不能拿 taskId 去拼。
func TestOpenAPIQueryUsesRunID(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"runId":"run_9","taskId":"task_9","status":"succeeded","rootCause":{"summary":"OK"}}`))
	}))
	defer srv.Close()

	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: srv.URL, Protocol: ProtocolOpenAPIV1,
		QueryPath: "/api/v1/runs/{run_id}",
	}, zap.NewNop())
	if _, err := client.Query(context.Background(), "task_9", "run_9"); err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if gotPath != "/api/v1/runs/run_9" {
		t.Fatalf("轮询路径应替换 {run_id}：%q", gotPath)
	}
}

// TestOpenAPIStacktraceFallback 没有堆栈时用错误信息顶上（stacktrace 是必填字段）。
func TestOpenAPIStacktraceFallback(t *testing.T) {
	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: "http://x", Protocol: ProtocolOpenAPIV1,
	}, zap.NewNop())
	p := client.openAPIPayload(context.Background(), SubmitInput{Service: "order-svc", Message: "下单失败"})
	if p.Stacktrace != "下单失败" {
		t.Fatalf("stacktrace 为空时应退回错误信息：%q", p.Stacktrace)
	}
	// 未命中任何映射时，回落为 host=服务名（依赖 AI 侧 matchRules）。
	p = client.openAPIPayload(context.Background(), SubmitInput{Service: "order-svc", Stacktrace: "NPE"})
	if p.RepoLocator == nil || p.RepoLocator.Host != "order-svc" {
		t.Fatalf("未命中映射应回落 host=服务名：%+v", p.RepoLocator)
	}
	// 服务名 → git 地址映射命中时，直接带 gitUrl 定位（不再依赖全局单仓配置）。
	client.cfg.ServiceRepoMap = map[string]string{"order-svc": "https://git.x/order.git"}
	p = client.openAPIPayload(context.Background(), SubmitInput{Service: "order-svc", Stacktrace: "NPE"})
	if p.RepoLocator == nil || p.RepoLocator.GitURL != "https://git.x/order.git" {
		t.Fatalf("服务名→git 地址映射定位异常：%+v", p.RepoLocator)
	}
}

// TestProtocolSwitchRewritesPaths 切到开放接口时，没改过的路径要自动换掉。
func TestProtocolSwitchRewritesPaths(t *testing.T) {
	old := aiAnalysisPayload{SubmitPath: defaultSubmitPath, QueryPath: defaultQueryPath}
	got := mergeAIAnalysis(old, AIAnalysisInput{Protocol: ProtocolOpenAPIV1})
	if got.Protocol != ProtocolOpenAPIV1 {
		t.Fatalf("协议未生效：%q", got.Protocol)
	}
	if got.SubmitPath != defaultOpenAPISubmitPath || got.QueryPath != defaultOpenAPIQueryPath {
		t.Fatalf("路径未随协议改写：%q / %q", got.SubmitPath, got.QueryPath)
	}
	if got.AuthHeader != defaultAuthHeaderOpenAPI {
		t.Fatalf("鉴权头未随协议改写：%q", got.AuthHeader)
	}
	// 用户自己填过的路径要保留。
	custom := mergeAIAnalysis(aiAnalysisPayload{SubmitPath: "/custom"}, AIAnalysisInput{Protocol: ProtocolOpenAPIV1})
	if custom.SubmitPath != "/custom" {
		t.Fatalf("自定义路径不该被覆盖：%q", custom.SubmitPath)
	}
	// 前端漏传 protocol 时不该把已有协议改回 generic。
	keep := mergeAIAnalysis(aiAnalysisPayload{Protocol: ProtocolOpenAPIV1}, AIAnalysisInput{})
	if keep.Protocol != ProtocolOpenAPIV1 {
		t.Fatalf("空值不该把协议重置：%q", keep.Protocol)
	}
}
