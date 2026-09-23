package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
)

// 本文件钉住「AI 代码分析」的连通性探测。
//
// 为什么值得测：探测的判定口径是有取舍的（404/422 算"通"、只走同步端点不带回调），
// 而它又是设置页上唯一能提前暴露"地址错 / 密钥错 / 协议错"的入口。
// 一旦判定写反，管理员看到的就不是原因而是误导。

// TestProbeUsesSyncEndpoint 探测必须走同步端点，且不带回调地址。
//
// 开放接口的异步端点要求 callbackUrl 必填，而探测时回调地址往往还没配好——
// 为了测连通性先逼着配回调是本末倒置。
func TestProbeUsesSyncEndpoint(t *testing.T) {
	var (
		gotPath string
		gotKey  string
		gotAuth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"runId":"r1","taskId":"t1","status":"succeeded"}`))
	}))
	defer srv.Close()

	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k-1",
		Protocol: ProtocolOpenAPIV1, AuthHeader: "X-API-Key",
	}, zap.NewNop())

	res, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("探测失败: %v", err)
	}
	if gotPath != "/api/v1/openapi/analyze" {
		t.Fatalf("探测应走同步端点：%q", gotPath)
	}
	if gotKey != "k-1" {
		t.Fatalf("探测未带 X-API-Key：%q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("探测不该再发 Authorization: Bearer：%q", gotAuth)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("状态码异常：%d", res.StatusCode)
	}
	if res.Message != modelStatusSucceeded {
		t.Fatalf("应带回任务状态：%q", res.Message)
	}
}

// TestProbeReportsStatusInsteadOfFailing 4xx 也是"打通了"的证据，不能当成连接失败。
func TestProbeReportsStatusInsteadOfFailing(t *testing.T) {
	for _, status := range []int{401, 403, 404, 422, 200, 202} {
		want := status
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(want)
		}))
		client := NewAIAnalysisClient(config.AIAnalysisConfig{
			Enabled: true, BaseURL: srv.URL, Protocol: ProtocolOpenAPIV1,
		}, zap.NewNop())
		res, err := client.Probe(context.Background())
		if err != nil {
			t.Fatalf("HTTP %d 不该被当成连接失败：%v", want, err)
		}
		if res.StatusCode != want {
			t.Fatalf("状态码应原样带回：期望 %d，实际 %d", want, res.StatusCode)
		}
		srv.Close()
	}
}

// TestProbeFailsOnUnreachable 地址不可达才算连接失败。
func TestProbeFailsOnUnreachable(t *testing.T) {
	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled: true, BaseURL: "http://127.0.0.1:1", Protocol: ProtocolOpenAPIV1,
	}, zap.NewNop())
	if _, err := client.Probe(context.Background()); err == nil {
		t.Fatal("地址不可达时应返回错误")
	}
}

// TestProbeRefusesWhenNotConfigured 没配地址就不该发任何请求。
func TestProbeRefusesWhenNotConfigured(t *testing.T) {
	client := NewAIAnalysisClient(config.AIAnalysisConfig{Enabled: false}, zap.NewNop())
	if _, err := client.Probe(context.Background()); err == nil {
		t.Fatal("未配置时探测应报错")
	}
}

// TestProtocolLabel 测试结果标题里的协议名（页面展示用）。
func TestProtocolLabel(t *testing.T) {
	if got := protocolLabel(ProtocolOpenAPIV1); got != "开放接口 v1" {
		t.Fatalf("协议名异常：%q", got)
	}
	if got := protocolLabel(""); got != "通用协议" {
		t.Fatalf("空协议应回落通用协议：%q", got)
	}
	if got := protocolLabel("whatever"); got != "通用协议" {
		t.Fatalf("非法协议应回落通用协议：%q", got)
	}
}
