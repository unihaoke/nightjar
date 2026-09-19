package repo

import (
	"strings"
	"testing"
)

// 本文件钉住「仓库地址 ↔ 凭据」的拆分与拼回（INC-031）。
//
// 之所以要这么多边界用例：这是"令牌会不会又躺回数据库/页面"的唯一关口，
// 拆错一种形态（例如 `https://<token>@host` 被当成用户名），后果是
// "保存成功但拉不下来"或"令牌继续明文入库"，两者都不会有任何报错。

// TestSplitCredentials 覆盖各种地址形态。
func TestSplitCredentials(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantClean  string
		wantCred   string
		wantSecret bool
	}{
		{
			name: "标准 user:token 形式", in: "https://oauth2:glpat-abc@gitlab.internal/g/r.git",
			wantClean: "https://gitlab.internal/g/r.git", wantCred: "oauth2:glpat-abc", wantSecret: true,
		},
		{
			name: "只有一段（按 oauth2 用户名处理）", in: "https://glpat-abc@gitlab.internal/g/r.git",
			wantClean: "https://gitlab.internal/g/r.git", wantCred: "oauth2:glpat-abc", wantSecret: true,
		},
		{
			name: "带端口", in: "http://user:pw@gitea.internal:3000/g/r.git",
			wantClean: "http://gitea.internal:3000/g/r.git", wantCred: "user:pw", wantSecret: true,
		},
		{
			name: "查询参数 token", in: "https://git.internal/g/r.git?token=abc123",
			wantClean: "https://git.internal/g/r.git", wantCred: "?token=abc123", wantSecret: true,
		},
		{
			name: "查询参数 private_token 且带其它参数",
			in:   "https://git.internal/g/r.git?ref=main&private_token=xyz",
			// 只摘凭据参数，非凭据参数保持原样（否则会改变仓库地址语义）。
			wantClean: "https://git.internal/g/r.git?ref=main", wantCred: "?private_token=xyz", wantSecret: true,
		},
		{
			name: "scp 风格不拆（git@ 是用户名不是令牌）",
			in:   "git@github.com:org/repo.git",
			wantClean: "git@github.com:org/repo.git", wantCred: "", wantSecret: false,
		},
		{
			name: "无凭据的 https 原样返回",
			in:   "https://git.internal/g/r.git",
			wantClean: "https://git.internal/g/r.git", wantCred: "", wantSecret: false,
		},
		{name: "空串", in: "", wantClean: "", wantCred: "", wantSecret: false},
		{name: "前后空白要吃掉", in: "  https://git.internal/g/r.git  ",
			wantClean: "https://git.internal/g/r.git", wantCred: "", wantSecret: false},
	}
	for _, c := range cases {
		clean, cred := SplitCredentials(c.in)
		if clean != c.wantClean {
			t.Fatalf("%s：clean = %q，期望 %q", c.name, clean, c.wantClean)
		}
		if cred != c.wantCred {
			t.Fatalf("%s：credential = %q，期望 %q", c.name, cred, c.wantCred)
		}
		if got := CredentialHasSecret(cred); got != c.wantSecret {
			t.Fatalf("%s：CredentialHasSecret = %v，期望 %v", c.name, got, c.wantSecret)
		}
	}
}

// TestCredentialRoundTrip 钉住"拆出来再拼回去 = 原地址"。
//
// 这条是"迁移旧数据"的正确性前提：迁移后仍要能用同一个地址拉到代码。
func TestCredentialRoundTrip(t *testing.T) {
	for _, raw := range []string{
		"https://oauth2:glpat-abc@gitlab.internal/g/r.git",
		"https://glpat-abc@gitlab.internal/g/r.git",
		"http://user:pw@gitea.internal:3000/g/r.git",
		"https://git.internal/g/r.git?token=abc123",
		"https://git.internal/g/r.git",
	} {
		clean, cred := SplitCredentials(raw)
		got := WithCredential(clean, cred)
		// `https://<token>@host` 会被规范成 `oauth2:<token>@host`（这是刻意的：
		// 只有一段的 userinfo 在 GitLab/GitHub 上会被当成用户名而认证失败）。
		want := raw
		if raw == "https://glpat-abc@gitlab.internal/g/r.git" {
			want = "https://oauth2:glpat-abc@gitlab.internal/g/r.git"
		}
		if got != want {
			t.Fatalf("往返后地址不一致：in=%q got=%q want=%q", raw, got, want)
		}
	}
}

// TestSanitizeURLEmptyURL 边界：空地址、只有 scheme 的地址不能 panic 或产出畸形结果。
//
// 刻意用「位置数组」而不是带键的 map：键长度不一时 gofmt 会要求整列对齐，
// 手写很容易差一个空格（而且这种差异与测试意图无关，纯粹是格式噪音）。
func TestSanitizeURLEmptyURL(t *testing.T) {
	cases := [][2]string{
		{"", ""},
		{"   ", ""},
		{"https://u:p@host/x.git", "https://host/x.git"},
		{"git@host:org/r.git", "git@host:org/r.git"},
	}
	for _, c := range cases {
		if got := SanitizeURL(c[0]); got != c[1] {
			t.Fatalf("SanitizeURL(%q) = %q，期望 %q", c[0], got, c[1])
		}
	}
	// SanitizeURL 对任何输入都不该 panic，也不该留下明文令牌。
	for _, raw := range []string{
		"https://oauth2:SuperSecret@gitlab.internal/g/r.git",
		"https://git.internal/g/r.git?access_token=SuperSecret",
	} {
		if got := SanitizeURL(raw); strings.Contains(got, "SuperSecret") {
			t.Fatalf("SanitizeURL(%q) 仍含凭据：%q", raw, got)
		}
	}
}

// TestCredentialHasSecret 钉住"只有用户名没有口令"不算配了凭据。
func TestCredentialHasSecret(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{in: "", want: false},
		{in: "oauth2:", want: false},
		{in: "oauth2:token", want: true},
		{in: "?token=abc", want: true},
		{in: "?", want: false},
		{in: "user:pass:wor", want: true},
	}
	for _, c := range cases {
		if got := CredentialHasSecret(c.in); got != c.want {
			t.Fatalf("CredentialHasSecret(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}
