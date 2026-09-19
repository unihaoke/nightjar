package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件的测试全部不依赖网络：远端是 t.TempDir() 里的本地裸库（见 gitfixture_test.go），
// 需要身份信息的地方都显式传 -c user.name/-c user.email，不依赖机器全局配置。

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatalf("Abs(%s) 失败: %v", p, err)
	}
	return abs
}

// samePath 比较两个路径是否指向同一位置（Windows 大小写不敏感）。
func samePath(t *testing.T, a, b string) bool {
	t.Helper()
	return strings.EqualFold(filepath.Clean(mustAbs(t, a)), filepath.Clean(mustAbs(t, b)))
}

func normalizeEOL(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return normalizeEOL(string(b))
}

// countingRunner 包装真实执行器并记录调用（用于断言命令次数与「零调用」）。
type countingRunner struct {
	inner Runner
	mu    sync.Mutex
	calls [][]string
}

func (c *countingRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	c.mu.Lock()
	c.calls = append(c.calls, append([]string{name}, args...))
	c.mu.Unlock()
	return c.inner.Run(ctx, dir, env, name, args...)
}

// countArg 统计出现该参数的命令条数。
func (c *countingRunner) countArg(arg string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		for _, a := range call {
			if a == arg {
				n++
				break
			}
		}
	}
	return n
}

// forbidRunner 被调用即计数；用于证明合规拒绝时完全不碰 git。
type forbidRunner struct{ calls int32 }

func (r *forbidRunner) Run(context.Context, string, []string, string, ...string) (string, string, error) {
	atomic.AddInt32(&r.calls, 1)
	return "", "", errors.New("本次不应执行任何命令")
}

// blockingRunner 一直阻塞到 ctx 结束，用于验证超时错误。
type blockingRunner struct{ calls int32 }

func (r *blockingRunner) Run(ctx context.Context, _ string, _ []string, _ string, _ ...string) (string, string, error) {
	atomic.AddInt32(&r.calls, 1)
	<-ctx.Done()
	return "", "", ctx.Err()
}

// TestEnsureCloneUnchangedUpdated 覆盖核心链路：首次 clone → 无变化 unchanged
// → 远端新增提交后 updated 且本地能看到新文件（"下次 pull 就行"的核心断言）。
func TestEnsureCloneUnchangedUpdated(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	f := NewFetcher(Options{RootDir: root})
	ctx := context.Background()

	req := Request{Service: "order-service", RepoURL: remote.url, Branch: "main", AllowOutbound: true}

	// 1) 首次 clone
	res, err := f.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("首次 Ensure Action = %q, 期望 %q", res.Action, ActionCloned)
	}
	if res.Revision == "" {
		t.Fatal("首次 Ensure Revision 为空，期望短 sha")
	}
	if len(res.Revision) < 7 {
		t.Fatalf("Revision = %q，短 sha 至少 7 位", res.Revision)
	}
	if res.Branch != "main" {
		t.Fatalf("Branch = %q, 期望 main", res.Branch)
	}
	if !samePath(t, res.LocalPath, filepath.Join(root, "order-service")) {
		t.Fatalf("LocalPath = %q, 期望 %q", res.LocalPath, filepath.Join(root, "order-service"))
	}
	if got := readFile(t, filepath.Join(res.LocalPath, "app", "main.go")); !strings.Contains(got, "func main()") {
		t.Fatalf("clone 后文件内容不符: %q", got)
	}
	if !strings.HasPrefix(remote.head(t), res.Revision) {
		t.Fatalf("Revision = %q, 远端 HEAD = %q", res.Revision, remote.head(t))
	}
	if !isGitDir(res.LocalPath) {
		t.Fatalf("%s 应该是 git 仓库", res.LocalPath)
	}

	// 2) 远端无变化 → unchanged，且不重复 clone
	res2, err := f.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("第二次 Ensure 失败: %v", err)
	}
	if res2.Action != ActionUnchanged {
		t.Fatalf("无变化时 Action = %q, 期望 %q", res2.Action, ActionUnchanged)
	}
	if res2.Revision != res.Revision {
		t.Fatalf("无变化时 Revision 变了: %q → %q", res.Revision, res2.Revision)
	}

	// 3) 远端新增提交 → updated，且本地能看到新文件与新提交
	remote.write(t, "app/new.go", "package main\n\n// added by remote\n")
	remote.commit(t, "add new.go")
	remote.push(t)
	wantHead := remote.head(t)

	res3, err := f.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("远端更新后 Ensure 失败: %v", err)
	}
	if res3.Action != ActionUpdated {
		t.Fatalf("远端更新后 Action = %q, 期望 %q", res3.Action, ActionUpdated)
	}
	if res3.Revision == res.Revision {
		t.Fatalf("远端更新后 Revision 没变: %q", res3.Revision)
	}
	if !strings.HasPrefix(wantHead, res3.Revision) {
		t.Fatalf("Revision = %q, 远端 HEAD = %q", res3.Revision, wantHead)
	}
	if got := readFile(t, filepath.Join(res3.LocalPath, "app", "new.go")); !strings.Contains(got, "added by remote") {
		t.Fatalf("更新后没看到远端新文件，内容=%q", got)
	}
	if got := readFile(t, filepath.Join(res3.LocalPath, "app", "main.go")); !strings.Contains(got, "func main()") {
		t.Fatalf("旧文件内容异常: %q", got)
	}
}

// TestEnsureDirtyCacheResetToRemote 验证「缓存被强制重置到远端」这一取舍：
// 有人手工改了缓存目录里的文件，Ensure 后内容回到远端版本。
func TestEnsureDirtyCacheResetToRemote(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	f := NewFetcher(Options{RootDir: root})
	ctx := context.Background()

	req := Request{Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true}
	res, err := f.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("首次 Ensure 失败: %v", err)
	}

	// 模拟「有人直接改缓存里的代码」。
	target := filepath.Join(res.LocalPath, "app", "main.go")
	if err := os.WriteFile(target, []byte("package main\n\n// tampered locally\n"), 0o644); err != nil {
		t.Fatalf("篡改缓存失败: %v", err)
	}

	res2, err := f.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("脏缓存 Ensure 失败: %v", err)
	}
	// HEAD 没变，所以按「前后 HEAD 比较」的规则是 unchanged；
	// 但工作区内容必须被 checkout --force 重置回远端版本。
	if res2.Action != ActionUnchanged {
		t.Fatalf("HEAD 未变时 Action = %q, 期望 %q", res2.Action, ActionUnchanged)
	}
	if got := readFile(t, target); strings.Contains(got, "tampered locally") {
		t.Fatalf("缓存里的手工改动没有被重置到远端: %q", got)
	}
}

// TestEnsureBranchMissing 分支不存在 → 报错且提示可操作（关键词命中）。
func TestEnsureBranchMissing(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	f := NewFetcher(Options{RootDir: root})

	_, err := f.Ensure(context.Background(), Request{
		Service: "svc", RepoURL: remote.url, Branch: "no-such-branch", AllowOutbound: true,
	})
	if err == nil {
		t.Fatal("分支不存在时应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "分支") && !strings.Contains(msg, "no-such-branch") {
		t.Fatalf("错误信息不可操作，缺少分支/引用关键词: %s", msg)
	}
	if !strings.Contains(msg, "no-such-branch") {
		t.Fatalf("错误信息里应当带上 git 输出片段（含分支名）: %s", msg)
	}
	// clone 失败不该留下半个目录，否则下次会被「已存在但不是仓库」挡住。
	if _, statErr := os.Stat(filepath.Join(root, "svc")); !os.IsNotExist(statErr) {
		t.Fatalf("克隆失败后残留目录: err=%v", statErr)
	}
}

// TestEnsureServiceNameSanitized 服务名带 ../.. 时目录仍落在 RootDir 内。
func TestEnsureServiceNameSanitized(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	absRoot := mustAbs(t, root)
	f := NewFetcher(Options{RootDir: root})

	res, err := f.Ensure(context.Background(), Request{
		Service: "../../etc/passwd", RepoURL: remote.url, Branch: "main", AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if !samePath(t, filepath.Dir(res.LocalPath), absRoot) {
		t.Fatalf("LocalPath %q 的父目录应为 RootDir %q", res.LocalPath, absRoot)
	}
	base := filepath.Base(res.LocalPath)
	if base == "." || base == ".." || base == "" {
		t.Fatalf("目录名未被净化: %q", base)
	}
	if strings.ContainsAny(base, `/\`) {
		t.Fatalf("目录名里仍有路径分隔符: %q", base)
	}
	if !strings.HasPrefix(res.LocalPath, absRoot+string(filepath.Separator)) {
		t.Fatalf("LocalPath %q 越出 RootDir %q", res.LocalPath, absRoot)
	}
}

// TestResolveLocalPathRejectsEscape 显式 LocalPath 越界必须被拒绝。
func TestResolveLocalPathRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	f := NewFetcher(Options{RootDir: root})
	ctx := context.Background()

	cases := []struct {
		name string
		req  Request
	}{
		{"绝对路径在 root 之外", Request{Service: "svc", RepoURL: "https://example.com/x.git", LocalPath: filepath.Join(outside, "x"), AllowOutbound: true}},
		{"相对路径越界", Request{Service: "svc", RepoURL: "https://example.com/x.git", LocalPath: filepath.Join(root, "..", "escape"), AllowOutbound: true}},
		{"LocalPath 等于 RootDir 自身", Request{Service: "svc", RepoURL: "https://example.com/x.git", LocalPath: root, AllowOutbound: true}},
		{"服务名是保留目录名 ..", Request{Service: "..", RepoURL: "https://example.com/x.git", AllowOutbound: true}},
		{"服务名为空", Request{Service: "   ", RepoURL: "https://example.com/x.git", AllowOutbound: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Ensure(ctx, tc.req)
			if err == nil {
				t.Fatal("期望报错，实际成功")
			}
			if !errors.Is(err, ErrPathEscape) && !errors.Is(err, ErrInvalidService) {
				t.Fatalf("错误类型不符（期望 ErrPathEscape/ErrInvalidService）: %v", err)
			}
		})
	}
}

// TestEnsureExplicitLocalPathInsideRoot 显式 LocalPath 落在 root 内时应正常工作。
func TestEnsureExplicitLocalPathInsideRoot(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	f := NewFetcher(Options{RootDir: root})
	local := filepath.Join(root, "nested", "custom-dir")

	res, err := f.Ensure(context.Background(), Request{
		Service: "svc", RepoURL: remote.url, Branch: "main", LocalPath: local, AllowOutbound: true,
	})
	if err != nil {
		t.Fatalf("Ensure 失败: %v", err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("Action = %q, 期望 %q", res.Action, ActionCloned)
	}
	if !samePath(t, res.LocalPath, local) {
		t.Fatalf("LocalPath = %q, 期望 %q", res.LocalPath, local)
	}
}

// TestEnsureExistingNonGitDirRefused 目录已存在但不是 git 仓库 → 明确报错且绝不覆盖。
func TestEnsureExistingNonGitDirRefused(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	local := filepath.Join(root, "svc")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	marker := filepath.Join(local, "keep.txt")
	if err := os.WriteFile(marker, []byte("do not touch"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}

	f := NewFetcher(Options{RootDir: root})
	_, err := f.Ensure(context.Background(), Request{
		Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true,
	})
	if err == nil {
		t.Fatal("已存在非 git 目录时应当报错")
	}
	if !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("错误类型不符（期望 ErrNotGitRepo）: %v", err)
	}
	if got := readFile(t, marker); got != "do not touch" {
		t.Fatalf("原有文件被改动: %q", got)
	}
	if isGitDir(local) {
		t.Fatal("拒绝覆盖时不应把该目录变成 git 仓库")
	}
}

// TestEnsureOutboundDeniedRunsNoGit 合规开关关闭 → 报错且零命令执行。
func TestEnsureOutboundDeniedRunsNoGit(t *testing.T) {
	runner := &forbidRunner{}
	f := NewFetcher(Options{RootDir: t.TempDir(), Runner: runner})

	_, err := f.Ensure(context.Background(), Request{
		Service: "svc", RepoURL: "https://user:secret@example.com/x.git", Branch: "main", AllowOutbound: false,
	})
	if err == nil {
		t.Fatal("AllowOutbound=false 时应当报错")
	}
	if !errors.Is(err, ErrOutboundDenied) {
		t.Fatalf("错误类型不符（期望 ErrOutboundDenied）: %v", err)
	}
	if !strings.Contains(err.Error(), "allow_outbound") {
		t.Fatalf("错误应指向真正的开关 code_repo.allow_outbound: %v", err)
	}
	if n := atomic.LoadInt32(&runner.calls); n != 0 {
		t.Fatalf("AllowOutbound=false 时执行了 %d 次命令，期望 0 次", n)
	}
}

// TestEnsureEmptyDirIsRecloned 钉住「目录存在但为空 → 当作没有代码，重新 clone」。
//
// 场景来自真实运维：clone 进行中服务被重建/进程被杀，目标目录已经建出来了却没拉到代码。
// 若按"已存在但不是仓库"硬失败，这个服务就**永远**拿不到代码，且报错看起来像是配置问题。
func TestEnsureEmptyDirIsRecloned(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	local := filepath.Join(root, "svc")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatalf("建空目录失败: %v", err)
	}
	f := NewFetcher(Options{RootDir: root})
	req := Request{Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true}

	if f.HasCode(context.Background(), req) {
		t.Fatal("空目录不应算作「已有代码」")
	}
	res, err := f.Ensure(context.Background(), req)
	if err != nil {
		t.Fatalf("空目录应触发 clone，实际失败: %v", err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("Action = %q，期望 %q", res.Action, ActionCloned)
	}
	if !isGitDir(local) {
		t.Fatalf("%s 应被重新克隆成 git 仓库", local)
	}
	if !f.HasCode(context.Background(), req) {
		t.Fatal("clone 后应算作「已有代码」")
	}
}

// TestEnsureBrokenGitDirIsRecloned 钉住「.git 存在但仓库不可用 → 重新 clone」。
//
// 同样是中断留下的半成品：这类目录能骗过"是不是 git 仓库"的检查，却在 fetch/pull 时失败，
// 表现为每次分析都失败且原因看着像网络问题。
func TestEnsureBrokenGitDirIsRecloned(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	f := NewFetcher(Options{RootDir: root})
	req := Request{Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true}

	if _, err := f.Ensure(context.Background(), req); err != nil {
		t.Fatalf("首次 clone 失败: %v", err)
	}
	// 把 .git 换成空目录：模拟 clone 写到一半被中断。
	if err := os.RemoveAll(filepath.Join(root, "svc", ".git")); err != nil {
		t.Fatalf("删除 .git 失败: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "svc", ".git"), 0o755); err != nil {
		t.Fatalf("重建空 .git 失败: %v", err)
	}
	if f.HasCode(context.Background(), req) {
		t.Fatal("损坏的仓库不应算作「已有代码」")
	}

	res, err := f.Ensure(context.Background(), req)
	if err != nil {
		t.Fatalf("损坏仓库应触发重新 clone，实际失败: %v", err)
	}
	if res.Action != ActionCloned {
		t.Fatalf("Action = %q，期望 %q", res.Action, ActionCloned)
	}
	if got := readFile(t, filepath.Join(root, "svc", "app", "main.go")); !strings.Contains(got, "func main()") {
		t.Fatalf("重新 clone 后文件内容不符: %q", got)
	}
}

// TestEnsureCloneTimeout 超时要能被识别为超时。
func TestEnsureCloneTimeout(t *testing.T) {
	runner := &blockingRunner{}
	f := NewFetcher(Options{RootDir: t.TempDir(), CloneTimeout: 30 * time.Millisecond, Runner: runner})

	_, err := f.Ensure(context.Background(), Request{
		Service: "svc", RepoURL: "https://example.com/x.git", Branch: "main", AllowOutbound: true,
	})
	if err == nil {
		t.Fatal("超时场景应当报错")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("错误应当可用 errors.Is(err, context.DeadlineExceeded) 判定: %v", err)
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Fatalf("错误信息里应能看出是超时: %v", err)
	}
	if n := atomic.LoadInt32(&runner.calls); n != 1 {
		t.Fatalf("阻塞的 clone 被调用了 %d 次，期望 1 次", n)
	}
}

// TestEnsureConcurrentSingleClone 并发 Ensure 同一服务时只真正执行一次 clone，
// 且所有调用都拿到结果、Revision 一致。
func TestEnsureConcurrentSingleClone(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	cr := &countingRunner{inner: execRunner{}}
	f := NewFetcher(Options{RootDir: root, Runner: cr})

	const n = 6
	results := make([]*Result, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.Ensure(context.Background(), Request{
				Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true,
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("第 %d 个并发 Ensure 失败: %v", i, errs[i])
		}
		if results[i] == nil || results[i].Revision == "" {
			t.Fatalf("第 %d 个并发 Ensure 结果不完整: %+v", i, results[i])
		}
		if results[i].Revision != results[0].Revision {
			t.Fatalf("并发结果的 Revision 不一致: %q vs %q", results[i].Revision, results[0].Revision)
		}
		if !samePath(t, results[i].LocalPath, results[0].LocalPath) {
			t.Fatalf("并发结果的 LocalPath 不一致: %q vs %q", results[i].LocalPath, results[0].LocalPath)
		}
	}
	if got := cr.countArg("clone"); got != 1 {
		t.Fatalf("clone 执行了 %d 次，期望 1 次（并发复用结果 + 串行化）", got)
	}
}

// TestEnsureSerializesDifferentRequests 不同签名（不同分支）的并发请求仍然串行执行 git，
// 不阻塞、不报错（保证同一目录不会被两个 git 同时操作）。
func TestEnsureSerializesDifferentRequests(t *testing.T) {
	remote := newTestRemote(t)
	root := t.TempDir()
	cr := &countingRunner{inner: execRunner{}}
	f := NewFetcher(Options{RootDir: root, Runner: cr})

	ctx := context.Background()
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, req := range []Request{
		{Service: "svc", RepoURL: remote.url, Branch: "main", AllowOutbound: true},
		{Service: "svc", RepoURL: remote.url, AllowOutbound: true}, // branch 为空 → 走 pull 路径
	} {
		wg.Add(1)
		go func(r Request) {
			defer wg.Done()
			if _, err := f.Ensure(ctx, r); err != nil {
				errCh <- err
			}
		}(req)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("同一路径不同签名的并发 Ensure 失败: %v", err)
	}
}

// panicOnceRunner 第一次调用即 panic（模拟注入的执行器崩掉），后续调用返回错误。
type panicOnceRunner struct {
	started chan struct{}
	release chan struct{}
	calls   int32
}

func (r *panicOnceRunner) Run(context.Context, string, []string, string, ...string) (string, string, error) {
	if atomic.AddInt32(&r.calls, 1) == 1 {
		close(r.started)
		<-r.release
		panic("模拟 Runner panic")
	}
	return "", "", errors.New("Runner 已不可用")
}

// TestEnsureWaiterNotStuckWhenLeaderPanics leader 的执行器 panic 时，
// 等待中的并发调用不能被永久阻塞（否则调用方协程会挂死）。
func TestEnsureWaiterNotStuckWhenLeaderPanics(t *testing.T) {
	runner := &panicOnceRunner{started: make(chan struct{}), release: make(chan struct{})}
	f := NewFetcher(Options{RootDir: t.TempDir(), Runner: runner})
	req := Request{Service: "svc", RepoURL: "https://example.com/x.git", Branch: "main", AllowOutbound: true}

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }() // leader 的 panic 不该炸掉测试进程
		_, _ = f.Ensure(context.Background(), req)
	}()
	<-runner.started

	followerErr := make(chan error, 1)
	go func() {
		_, err := f.Ensure(context.Background(), req)
		followerErr <- err
	}()

	// 给等待者一点时间挂到 leader 的 flight 上，然后让 leader 崩掉。
	time.Sleep(100 * time.Millisecond)
	close(runner.release)

	select {
	case err := <-followerErr:
		if err == nil {
			t.Fatal("leader panic 后等待者应拿到错误，而不是空结果")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待者被永久阻塞在 <-fl.done 上")
	}
	<-leaderDone
}

// TestLocalPathFor 解析辅助接口：只算路径、不执行 git，供降级场景定位旧副本。
func TestLocalPathFor(t *testing.T) {
	root := t.TempDir()
	runner := &forbidRunner{}
	f := NewFetcher(Options{RootDir: root, Runner: runner})

	got, err := f.LocalPathFor(Request{Service: "order-service"})
	if err != nil {
		t.Fatalf("LocalPathFor 失败: %v", err)
	}
	if !samePath(t, got, filepath.Join(root, "order-service")) {
		t.Fatalf("LocalPathFor = %q, 期望 %q", got, filepath.Join(root, "order-service"))
	}
	// 与 Ensure 的推导规则必须一致，否则降级时找不到旧副本
	// （TestEnsureCloneUnchangedUpdated 断言的正是 RootDir/Service 这个位置）。
	if _, err := f.LocalPathFor(Request{Service: "../x"}); err != nil {
		t.Fatalf("LocalPathFor 对可净化服务名不应报错: %v", err)
	}
	if _, err := f.LocalPathFor(Request{Service: "svc", LocalPath: filepath.Dir(root)}); !errors.Is(err, ErrPathEscape) {
		t.Fatalf("LocalPathFor 必须拒绝越界路径: %v", err)
	}
	if n := atomic.LoadInt32(&runner.calls); n != 0 {
		t.Fatalf("LocalPathFor 不应执行任何命令，实际 %d 次", n)
	}
}

// TestNewFetcherDefaults 默认值：clone 10 分钟、pull 2 分钟、git 二进制 "git"、默认执行器。
func TestNewFetcherDefaults(t *testing.T) {
	f := NewFetcher(Options{RootDir: t.TempDir()})
	if f.opts.CloneTimeout != 10*time.Minute {
		t.Fatalf("CloneTimeout = %s, 期望 10m", f.opts.CloneTimeout)
	}
	if f.opts.PullTimeout != 2*time.Minute {
		t.Fatalf("PullTimeout = %s, 期望 2m", f.opts.PullTimeout)
	}
	if f.opts.GitBinary != "git" {
		t.Fatalf("GitBinary = %q, 期望 git", f.opts.GitBinary)
	}
	if _, ok := f.runner.(execRunner); !ok {
		t.Fatalf("默认 Runner 类型 = %T, 期望 execRunner", f.runner)
	}
	if f.log == nil {
		t.Fatal("默认日志器不应为 nil")
	}

	// 注入 Runner 时必须用它，而不是真实执行器。
	cr := &countingRunner{inner: execRunner{}}
	f2 := NewFetcher(Options{RootDir: t.TempDir(), Runner: cr})
	if f2.runner != Runner(cr) {
		t.Fatalf("注入的 Runner 未被使用: %T", f2.runner)
	}
}

// TestEnsureRootDirEmpty RootDir 为空时给出明确错误（配置问题不该变成乱写文件）。
func TestEnsureRootDirEmpty(t *testing.T) {
	f := NewFetcher(Options{})
	_, err := f.Ensure(context.Background(), Request{Service: "svc", RepoURL: "https://example.com/x.git", AllowOutbound: true})
	if err == nil || !strings.Contains(err.Error(), "RootDir") {
		t.Fatalf("期望 RootDir 相关错误，实际: %v", err)
	}
}

// TestGitEnvInjected 默认执行器与 fetcher 都会注入防卡死的 git 环境变量。
func TestGitEnvInjected(t *testing.T) {
	env := gitEnv()
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(joined, "GIT_ASKPASS=/bin/echo") {
		t.Fatalf("gitEnv 缺少防卡死变量: %v", env)
	}
	// 已存在时不重复追加。
	merged := ensureGitEnv([]string{"GIT_TERMINAL_PROMPT=1", "PATH=/usr/bin"})
	count := 0
	for _, kv := range merged {
		if strings.HasPrefix(kv, "GIT_TERMINAL_PROMPT=") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("GIT_TERMINAL_PROMPT 被重复注入: %v", merged)
	}
	if !isGitCommand(`C:\Program Files\Git\cmd\git.exe`) || isGitCommand("gitlab-runner") {
		t.Fatal("isGitCommand 判定不正确")
	}
}

// TestSanitizeServiceName 净化规则的表格用例。
func TestSanitizeServiceName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"order-service", "order-service", false},
		{"api.v2_1", "api.v2_1", false},
		{"../../etc/passwd", "etc-passwd", false},
		{`a/b\c`, "a-b-c", false},
		{"svc name", "svc-name", false},
		{".", "", true},
		{"..", "", true},
		{"", "", true},
		{"...", "", true},
		{".../../..", "", true},
	}
	for _, tc := range cases {
		got, err := sanitizeServiceName(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("sanitizeServiceName(%q) 期望报错，实际得到 %q", tc.in, got)
			}
			if !errors.Is(err, ErrInvalidService) {
				t.Fatalf("sanitizeServiceName(%q) 错误类型不符: %v", tc.in, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("sanitizeServiceName(%q) 失败: %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("sanitizeServiceName(%q) = %q, 期望 %q", tc.in, got, tc.want)
		}
		if strings.ContainsAny(got, `/\`) {
			t.Fatalf("sanitizeServiceName(%q) 结果仍含分隔符: %q", tc.in, got)
		}
	}
}

// TestValidateBranch 分支名校验（参数注入防护）。
func TestValidateBranch(t *testing.T) {
	if err := validateBranch(""); err != nil {
		t.Fatalf("空分支名应当合法（表示远端默认分支）: %v", err)
	}
	if err := validateBranch("feature/x-1.2"); err != nil {
		t.Fatalf("正常分支名应当合法: %v", err)
	}
	if err := validateBranch("-f"); !errors.Is(err, ErrInvalidBranch) {
		t.Fatalf("以 - 开头的分支名必须被拒绝: %v", err)
	}
	if err := validateBranch("a b"); !errors.Is(err, ErrInvalidBranch) {
		t.Fatalf("含空白的分支名必须被拒绝: %v", err)
	}
}
