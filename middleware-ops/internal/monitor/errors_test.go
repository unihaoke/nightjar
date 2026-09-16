package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件锁定「错误翻译」的映射：跨栈接入时最常见的失败是容器网络挂错，
// 平台必须把它翻译成使用者的语言，而不是抛出 Go 的原始错误。
//
// 原始错误形态（真实案例）：dial tcp: lookup jd-prometheus on 127.0.0.11:53: server misbehaving

func TestDescribeErrorMapsDockerDNSFailure(t *testing.T) {
	raw := errors.New(`Get "http://jd-prometheus:9090/api/v1/query?query=up": dial tcp: lookup jd-prometheus on 127.0.0.11:53: server misbehaving`)
	got := DescribeError(raw, "http://jd-prometheus:9090")
	if !strings.Contains(got, "解析不了 jd-prometheus") {
		t.Fatalf("应指出解析失败的主机名，实际：%s", got)
	}
	if !strings.Contains(got, "compose.jd-link.yml") || !strings.Contains(got, "jd-nightjar") {
		t.Fatalf("应给出跨栈网络的具体修复方向，实际：%s", got)
	}
}

func TestDescribeErrorMapsConnectionRefused(t *testing.T) {
	got := DescribeError(errors.New(`Get "http://prometheus:9090/-/healthy": dial tcp 127.0.0.1:9090: connect: connection refused`), "http://prometheus:9090")
	if !strings.Contains(got, "拒绝连接") {
		t.Fatalf("应映射为端口拒绝，实际：%s", got)
	}
}

func TestDescribeErrorMapsTimeout(t *testing.T) {
	got := DescribeError(errors.New(`Get "http://10.0.0.9:9090/api/v1/query": context deadline exceeded`), "http://10.0.0.9:9090")
	if !strings.Contains(got, "超时") {
		t.Fatalf("应映射为超时，实际：%s", got)
	}
}

func TestDescribeErrorFallsBackToRaw(t *testing.T) {
	got := DescribeError(errors.New("some unexpected failure"), "")
	if !strings.Contains(got, "some unexpected failure") {
		t.Fatalf("未知错误应保留原文，实际：%s", got)
	}
	if !strings.Contains(got, "查询地址") {
		t.Fatalf("应带上查询地址便于与 .env 对照，实际：%s", got)
	}
}

func TestHostOfParsesBaseURL(t *testing.T) {
	cases := map[string]string{
		"http://jd-prometheus:9090": "jd-prometheus",
		"https://prom.example.com":  "prom.example.com",
		"10.0.0.9:9090":             "10.0.0.9",
		"":                          "",
	}
	for input, want := range cases {
		if got := HostOf(input); got != want {
			t.Fatalf("HostOf(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestLookupHostCtxSkipsIPLiteral(t *testing.T) {
	// IP 字面量不做 DNS：自检里这一步不应把 IP 场景误判成"网络问题"。
	if err := LookupHostCtx(context.Background(), "127.0.0.1"); err != nil {
		t.Fatalf("IP 字面量应直接通过，实际：%v", err)
	}
	if err := LookupHostCtx(context.Background(), ""); err == nil {
		t.Fatal("空主机名应报错")
	}
}

func TestLookupHostCtxFailsForUnknownName(t *testing.T) {
	// 用 RFC 2606 保留域名，保证在任何环境都解析不到。
	err := LookupHostCtx(context.Background(), "definitely-not-exist.invalid")
	if err == nil {
		t.Skip("当前 DNS 环境把 .invalid 解析成功了，跳过该断言")
	}
}
