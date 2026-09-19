package repo

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestRedactURL 表格用例（硬性要求 7）：userinfo、?token=、无凭据 URL、非 URL 字符串。
func TestRedactURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"userinfo 与 token 同时出现", "https://user:token@host/x.git?token=abc&foo=1", "https://***@host/x.git?token=***&foo=1"},
		{"仅 userinfo", "https://user:token@host/x.git", "https://***@host/x.git"},
		{"仅用户名", "https://user@host/x.git", "https://***@host/x.git"},
		{"用户名含百分号转义", "https://user:p%40ss@host/x.git", "https://***@host/x.git"},
		{"仅 token 查询参数", "https://host/x.git?token=abc", "https://host/x.git?token=***"},
		{"access_token 且保留锚点", "https://host/x.git?access_token=abc#frag", "https://host/x.git?access_token=***#frag"},
		{"private_token 与其余参数共存", "https://h/x.git?foo=1&private_token=s3cret", "https://h/x.git?foo=1&private_token=***"},
		{"password 参数", "https://h/x.git?password=pw&a=b", "https://h/x.git?password=***&a=b"},
		{"无凭据 URL 原样返回", "https://host/x.git", "https://host/x.git"},
		{"保留端口", "https://host:8443/x.git", "https://host:8443/x.git"},
		{"ssh 风格带 scheme 的 userinfo", "ssh://git@host:2222/org/repo.git", "ssh://***@host:2222/org/repo.git"},
		{"scp 风格无 scheme 且无口令", "git@github.com:org/repo.git", "git@github.com:org/repo.git"},
		{"无 scheme 的 用户:口令@", "user:pass@host/org/repo.git", "***@host/org/repo.git"},
		{"非 URL 字符串", "not a url", "not a url"},
		{"Windows 路径", `D:\repos\app\.git`, `D:\repos\app\.git`},
		{"空串", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactURL(tc.in)
			if got != tc.want {
				t.Fatalf("RedactURL(%q) = %q, 期望 %q", tc.in, got, tc.want)
			}
			// 幂等：重复脱敏不应继续变化（日志/审计会反复处理同一个值）。
			if again := RedactURL(got); again != got {
				t.Fatalf("RedactURL 不幂等: %q → %q", got, again)
			}
		})
	}
}

// TestRedactText 自由文本（git stderr）脱敏。
func TestRedactText(t *testing.T) {
	in := "fatal: unable to access 'https://alice:s3cr3t@git.example.com/org/repo.git/?token=tok123': The requested URL returned error: 403"
	got := redactText(in)
	for _, secret := range []string{"s3cr3t", "tok123", "alice"} {
		if strings.Contains(got, secret) {
			t.Fatalf("脱敏后仍含敏感值 %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "***") {
		t.Fatalf("脱敏后应出现占位符: %s", got)
	}
	// 无凭据的文本内容不应被破坏。
	plain := "fatal: Remote branch foo not found in upstream origin"
	if redactText(plain) != plain {
		t.Fatalf("无凭据文本被改动: %s", redactText(plain))
	}
}

// TestRedactArgs 命令行参数（含 RepoURL）脱敏，供 debug 日志使用。
func TestRedactArgs(t *testing.T) {
	args := []string{"clone", "--branch", "main", "--", "https://u:p@h/x.git?token=t", `C:\cache\x`}
	got := strings.Join(redactArgs(args), " ")
	if strings.Contains(got, "u:p@") || strings.Contains(got, "token=t") {
		t.Fatalf("参数未脱敏: %s", got)
	}
	if !strings.Contains(got, `C:\cache\x`) {
		t.Fatalf("普通参数被改动: %s", got)
	}
}

// TestTranslateGitErrorAdvice 硬性要求 8：把 git stderr 翻译成可操作的中文结论。
func TestTranslateGitErrorAdvice(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   string
	}{
		{"HTTPS 认证失败", "fatal: Authentication failed for 'https://github.com/org/repo.git/'", "认证失败"},
		{"无法读取用户名（交互被禁用）", "fatal: could not read Username for 'https://github.com': terminal prompts disabled", "认证失败"},
		{"SSH 公钥被拒", "git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.", "认证失败"},
		{"401", "fatal: unable to access 'https://h/x.git/': The requested URL returned error: 401", "认证失败"},
		{"仓库不存在", "remote: Repository not found.\nfatal: repository 'https://github.com/org/x.git/' not found", "仓库不存在"},
		{"403 无权限", "fatal: unable to access 'https://h/x.git/': The requested URL returned error: 403", "403"},
		{"分支不存在", "warning: Could not find remote branch foo to clone.\nfatal: Remote branch foo not found in upstream origin", "分支或引用不存在"},
		{"远端 ref 不存在", "fatal: couldn't find remote ref feature/x", "分支或引用不存在"},
		{"磁盘写满", "fatal: write error: No space left on device", "磁盘空间不足"},
		{"缓存目录不是仓库", "fatal: not a git repository (or any of the parent directories): .git", "缓存目录"},
		{"TLS 证书问题", "fatal: unable to access 'https://h/x.git/': SSL certificate problem: unable to get local issuer certificate", "证书"},
		{"DNS 解析失败", "fatal: unable to access 'https://h/x.git/': Could not resolve host: h", "白名单"},
		{"连接超时", "fatal: unable to access 'https://h/x.git/': Failed to connect to h port 443: Connection timed out", "白名单"},
		{"无法快进", "fatal: Not possible to fast-forward, aborting.", "快进"},
		{"未能分类", "fatal: something strange happened", "未能自动分类"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cause := errors.New("exit status 128")
			err := translateGitError("fetch", "https://h/x.git", tc.stderr, cause)
			if err == nil {
				t.Fatal("期望报错")
			}
			msg := err.Error()
			if !strings.Contains(msg, tc.want) {
				t.Fatalf("错误信息 %q 未包含 %q", msg, tc.want)
			}
			// 必须带上脱敏后的 git stderr 片段，而不是只有 exit status。
			firstLine := strings.SplitN(strings.TrimSpace(tc.stderr), "\n", 2)[0]
			if !strings.Contains(msg, strings.TrimSpace(strings.TrimPrefix(firstLine, "fatal: "))) &&
				!strings.Contains(msg, "git 输出：") {
				t.Fatalf("错误信息缺少 git 输出片段: %q", msg)
			}
			// 保留 errors.Is 语义。
			if !errors.Is(err, cause) {
				t.Fatalf("错误未包裹原始 err: %v", err)
			}
		})
	}
}

// TestTranslateGitErrorRedactsCredentials 错误信息里绝不能出现凭据。
func TestTranslateGitErrorRedactsCredentials(t *testing.T) {
	stderr := "fatal: unable to access 'https://alice:s3cr3t@git.example.com/org/repo.git/?token=tok123': " +
		"The requested URL returned error: 403"
	rawURL := "https://alice:s3cr3t@git.example.com/org/repo.git?token=tok123"

	err := translateGitError("clone", rawURL, stderr, errors.New("exit status 128"))
	msg := err.Error()
	for _, secret := range []string{"s3cr3t", "tok123", "alice"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("错误信息泄漏凭据 %q: %s", secret, msg)
		}
	}
	if !strings.Contains(msg, "***") {
		t.Fatalf("错误信息里应出现脱敏占位符: %s", msg)
	}
}

// TestTranslateGitErrorMissingBinary 可执行文件缺失时给出可操作结论。
func TestTranslateGitErrorMissingBinary(t *testing.T) {
	err := translateGitError("clone", "https://h/x.git", "", &exec.Error{Name: "git", Err: exec.ErrNotFound})
	if err == nil {
		t.Fatal("期望报错")
	}
	if !strings.Contains(err.Error(), "GitBinary") {
		t.Fatalf("错误应提示 Options.GitBinary: %v", err)
	}
}

// TestGitErrorDetailTruncated 超长 stderr 会被压成单行并限长。
func TestGitErrorDetailTruncated(t *testing.T) {
	long := strings.Repeat("x", maxSnippet*2)
	got := gitErrorDetail("fatal: " + long + "\nsecond line")
	if !strings.Contains(got, "已截断") {
		t.Fatalf("超长输出应被截断: %d", len(got))
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("stderr 应被压成单行: %q", got)
	}
	if len([]rune(got)) > maxSnippet+16 {
		t.Fatalf("截断后长度异常: %d", len([]rune(got)))
	}
}
