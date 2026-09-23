package config

import "testing"

// 本文件钉住 IM 卡片回跳地址的取值。
//
// 为什么值得测：默认值 server.host 是 0.0.0.0（容器内监听），直接拿它拼出来的链接
// 是 http://0.0.0.0:8080/... ——值班同学在飞书里点开就是"打不开"，
// 而这种问题在开发环境（本机 127.0.0.1）永远复现不出来。

func TestPublicBaseURLPrefersExplicit(t *testing.T) {
	cfg := ServerConfig{Host: "0.0.0.0", Port: 8080, PublicURL: "https://platform.example.com/"}
	if got := cfg.PublicBaseURL(); got != "https://platform.example.com" {
		t.Fatalf("应优先用对外地址并去掉尾部斜杠：%q", got)
	}
}

func TestPublicBaseURLReplacesWildcardHost(t *testing.T) {
	cases := []struct{ host, want string }{
		{"0.0.0.0", "http://127.0.0.1:8080"},
		{"::", "http://127.0.0.1:8080"},
		{"[::]", "http://127.0.0.1:8080"},
		{"", "http://127.0.0.1:8080"},
		{"10.0.0.5", "http://10.0.0.5:8080"},
	}
	for _, c := range cases {
		cfg := ServerConfig{Host: c.host, Port: 8080}
		if got := cfg.PublicBaseURL(); got != c.want {
			t.Fatalf("host=%q → %q，期望 %q", c.host, got, c.want)
		}
	}
}

// TestPublicBaseURLNeverWildcard 通配监听地址绝不能出现在回跳链接里。
func TestPublicBaseURLNeverWildcard(t *testing.T) {
	for _, cfg := range []ServerConfig{
		{Host: "0.0.0.0", Port: 8080},
		{Host: "::", Port: 8000},
	} {
		if got := cfg.PublicBaseURL(); got == "http://0.0.0.0:8080" || got == "http://:::8000" {
			t.Fatalf("回跳地址不应是通配监听地址：%q", got)
		}
	}
}
