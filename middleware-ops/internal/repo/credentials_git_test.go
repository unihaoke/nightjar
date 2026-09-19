package repo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 本文件用**假执行器**逐字断言 git 命令行（不需要真实网络/仓库），
// 因为这里锁的是"凭据到底出现在哪条命令、有没有留在 .git/config 里"这种契约，
// 用真实 git 反而看不出命令行长什么样。

// recordingRunner 记录命令行并返回可编程结果。
type recordingRunner struct {
	mu    sync.Mutex
	calls [][]string
	// respond 按"子命令"返回 stdout（第一个匹配的参数即为子命令，例如 clone/fetch）。
	respond map[string]string
	// fail 里的子命令返回错误。
	fail map[string]bool
}

func (r *recordingRunner) Run(_ context.Context, _ string, _ []string, name string, args ...string) (string, string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	sub := subcommandOf(args)
	if r.fail[sub] {
		return "", "boom", errFakeGit
	}
	if r.respond != nil {
		if out, ok := r.respond[sub]; ok {
			return out, "", nil
		}
	}
	return "", "", nil
}

// errFakeGit 是假执行器的失败信号（不是任何真实 git 错误）。
var errFakeGit = os.ErrInvalid

// subcommandOf 取出 git 的子命令：跳过 -C <dir> 与其它前置选项。
func subcommandOf(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-C" {
			i++ // 跳过目录参数
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

// callWith 返回第一条（从后往前找）子命令匹配的命令行。
func (r *recordingRunner) lastCall(sub string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.calls) - 1; i >= 0; i-- {
		if subcommandOf(r.calls[i][1:]) == sub {
			return r.calls[i]
		}
	}
	return nil
}

func (r *recordingRunner) allCalls() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestCloneInjectsCredentialButKeepsConfigClean 钉住克隆路径的两件事：
//  1. clone 用的是**带凭据**的地址（否则私有仓库拉不下来）；
//  2. clone 之后立刻把 origin 改写成**干净地址**（令牌不留 .git/config）。
func TestCloneInjectsCredentialButKeepsConfigClean(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{}
	// get-url 返回空 → 与配置不同 → 触发 set-url。
	runner.respond = map[string]string{"remote": ""}
	f := NewFetcher(Options{RootDir: root, Runner: runner})
	req := Request{
		Service: "svc", RepoURL: "https://gitlab.internal/g/r.git",
		Branch: "main", Credential: "oauth2:glpat-secret", AllowOutbound: true,
	}
	res, err := f.Ensure(context.Background(), req)
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("Action = %q，期望 %q", res.Action, ActionCloned)
	}

	clone := runner.lastCall("clone")
	if clone == nil {
		t.Fatal("没有执行 clone")
	}
	if !hasArg(clone, "https://oauth2:glpat-secret@gitlab.internal/g/r.git") {
		t.Fatalf("clone 应使用带凭据的地址，实际 argv = %v", clone)
	}

	setURL := runner.lastCall("remote")
	if setURL == nil {
		t.Fatal("clone 之后没有改写 origin（令牌会留在 .git/config 里）")
	}
	if !hasPair(setURL, "set-url", "origin", "https://gitlab.internal/g/r.git") {
		t.Fatalf("origin 应被改写为干净地址，实际 argv = %v", setURL)
	}
	// 反证：任何一条命令里都不该出现"带凭据的地址写进 origin"这种组合。
	for _, call := range runner.allCalls() {
		if hasPair(call, "set-url", "origin", "https://oauth2:glpat-secret@gitlab.internal/g/r.git") {
			t.Fatalf("origin 不许带凭据：%v", call)
		}
	}
}

// TestUpdateFetchesWithCredentialExplicitly 钉住更新路径：
//   - fetch 用显式地址 + 显式 refspec（不依赖 .git/config 里的 origin，因此 origin 可以是干净的）；
//   - 地址变化（令牌轮换/改地址）时 origin 会被**改写**，不再需要手工删缓存。
func TestUpdateFetchesWithCredentialExplicitly(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "svc")
	if err := os.MkdirAll(filepath.Join(local, ".git"), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	runner := &recordingRunner{}
	runner.respond = map[string]string{"remote": "https://oauth2:old-token@gitlab.internal/g/r.git\n", "rev-parse": "abcdef1234567890\n"}
	f := NewFetcher(Options{RootDir: root, Runner: runner})
	req := Request{
		Service: "svc", RepoURL: "https://gitlab.internal/g/r.git",
		Branch: "main", Credential: "oauth2:new-token", AllowOutbound: true,
	}
	if _, err := f.Ensure(context.Background(), req); err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}

	fetch := runner.lastCall("fetch")
	if fetch == nil {
		t.Fatal("没有执行 fetch")
	}
	if !hasArg(fetch, "https://oauth2:new-token@gitlab.internal/g/r.git") {
		t.Fatalf("fetch 应使用带凭据的地址，实际 argv = %v", fetch)
	}
	if !hasArg(fetch, "+refs/heads/main:refs/remotes/origin/main") {
		t.Fatalf("fetch 应带显式 refspec（否则 checkout origin/main 拿不到），实际 argv = %v", fetch)
	}

	setURL := runner.lastCall("remote")
	if setURL == nil || !hasPair(setURL, "set-url", "origin", "https://gitlab.internal/g/r.git") {
		t.Fatalf("origin 应被改写成配置里的干净地址（令牌轮换后免手工清缓存），实际 argv = %v", setURL)
	}
}

// TestUpdateWithoutBranchKeepsFastForwardSemantics 钉住"没配分支"那条路：
// 仍然是 fetch + merge --ff-only（与原来的 pull --ff-only 等价，不产生合并提交/不覆盖本地）。
func TestUpdateWithoutBranchKeepsFastForwardSemantics(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "svc")
	if err := os.MkdirAll(filepath.Join(local, ".git"), 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	runner := &recordingRunner{}
	runner.respond = map[string]string{"remote": "https://git.internal/g/r.git\n", "rev-parse": "abcdef1\n"}
	f := NewFetcher(Options{RootDir: root, Runner: runner})
	req := Request{Service: "svc", RepoURL: "https://git.internal/g/r.git", AllowOutbound: true}
	if _, err := f.Ensure(context.Background(), req); err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if fetch := runner.lastCall("fetch"); fetch == nil {
		t.Fatal("没有执行 fetch")
	}
	merge := runner.lastCall("merge")
	if merge == nil || !hasArg(merge, "--ff-only") || !hasArg(merge, "FETCH_HEAD") {
		t.Fatalf("无分支时应 fetch + merge --ff-only FETCH_HEAD，实际 argv = %v", merge)
	}
}

// hasArg 判断命令行里是否含某个参数。
func hasArg(call []string, want string) bool {
	for _, a := range call {
		if a == want {
			return true
		}
	}
	return false
}

// hasPair 判断命令行里是否**连续**出现这几个参数（用于断言 `set-url origin <url>` 这种组合）。
func hasPair(call []string, want ...string) bool {
	for i := 0; i+len(want) <= len(call); i++ {
		ok := true
		for j, w := range want {
			if call[i+j] != w {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
