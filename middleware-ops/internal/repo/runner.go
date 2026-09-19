package repo

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// 本文件是 Runner 的默认实现：真正去执行 git（硬性要求 10）。

// gitEnv 是给 git 子进程注入的环境变量（硬性要求 10）。
//
//   - GIT_TERMINAL_PROMPT=0：平台以服务方式运行、没有终端。git 一旦试图弹出
//     用户名/密码提示就会一直等待输入，直到超时才失败，而且日志里看不出原因。
//   - GIT_ASKPASS=/bin/echo：没有终端时 git 会调用 askpass 程序取口令；
//     指到 echo 让它返回空口令，认证会立刻失败并把真实原因写进 stderr。
//
// 两者只为「失败要快、原因要明」，不影响正常凭据：URL 内嵌令牌、SSH 密钥、
// credential helper 依然生效。
func gitEnv() []string {
	return []string{"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/echo"}
}

// execRunner 用 exec.CommandContext 执行真实命令，是 Options.Runner 为空时的默认值。
type execRunner struct{}

// Run 执行一次外部命令并分别捕获 stdout/stderr。
//
// 关键细节与原因：
//   - cmd.Dir = dir：git 的 -C 参数已经指定仓库目录，dir 主要影响相对路径解析；
//     显式指定比依赖进程当前目录更可预测（并发调用不会互相影响）。
//   - env 追加到 os.Environ()：必须保留 PATH/HOME/SSH_AUTH_SOCK 等（git 依赖它们），
//     调用方传入的同名变量排在后面，按 exec 的规则优先生效。
//   - Stdout/Stderr 分开捕获：需要把 stderr 原文（脱敏后）放进错误里做翻译与取证；
//     合并成一路会让「正常输出」和「失败原因」混在一起，无法分类。
//     （Go 的 os/exec 直接用管道 + goroutine 拷贝，不受 Node 那种管道限制影响。）
//   - git 命令额外补齐 GIT_TERMINAL_PROMPT=0 / GIT_ASKPASS=/bin/echo（见 gitEnv），
//     即使调用方忘了传也能避免在无终端环境卡死。
func (execRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	fullEnv := append(os.Environ(), env...)
	if isGitCommand(name) {
		fullEnv = ensureGitEnv(fullEnv)
	}
	cmd.Env = fullEnv

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// isGitCommand 判断可执行文件是否为 git（兼容自定义 GitBinary 路径与 Windows 的 .exe）。
func isGitCommand(name string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(name)))
	return base == "git" || base == "git.exe"
}

// ensureGitEnv 补齐缺失的 git 防卡死变量；已存在的值不覆盖（调用方显式配置优先）。
func ensureGitEnv(env []string) []string {
	out := env
	for _, kv := range gitEnv() {
		key := kv[:strings.Index(kv, "=")]
		found := false
		for _, existing := range out {
			if strings.HasPrefix(strings.ToUpper(existing), strings.ToUpper(key)+"=") {
				found = true
				break
			}
		}
		if !found {
			out = append(out, kv)
		}
	}
	return out
}
