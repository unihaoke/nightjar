// Package repo 维护「服务代码仓库」的本地缓存，供日志 AI 代码分析定位代码行。
//
// 链路背景：目标机 Filebeat → 平台 Kafka → 日志事件 → AI 代码分析。
// 分析要回答「这条日志里的错误对应哪一行代码」，前提是平台本地有一份与服务
// 当前版本一致的代码。因此本包负责：首次 clone 到本地缓存，之后每次直接更新
// （fetch + 重置到远端，或 pull --ff-only），不重复 clone——这既是用户明确要求，
// 也是带宽/磁盘/代码平台限流的现实约束。
//
// 目录布局：RootDir/<净化后的 Service>，每个服务一个子目录。
//
// 安全与合规约定：
//   - Service 会被净化成单层安全目录名；LocalPath 显式指定时必须严格落在 RootDir 内；
//   - RepoURL 里的凭据在日志与错误里一律脱敏（RedactURL）；
//   - AllowOutbound=false 时直接拒绝，不执行任何 git 命令（合规开关，见 6.5）。
//
// 已知风险（运维须知）：git 会把 clone 用的 RepoURL 写进 <LocalPath>/.git/config，
// 若 URL 内嵌令牌，令牌就落在缓存目录里——本包既要保证后续 fetch 可用，就无法抹掉它。
// 因此缓存根目录必须限定在平台内部（不随镜像/备份外泄），并建议改用 credential helper
// 或只读部署令牌。
//
// 非目标：不做浅克隆、不做代码索引/解析，只保证「本地有一份与远端一致的代码」。
package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.uber.org/zap"
)

// 默认值（硬性要求 6）：clone 首次拉全量，通常更久；fetch/pull 只取增量。
const (
	defaultCloneTimeout = 10 * time.Minute
	defaultPullTimeout  = 2 * time.Minute
	defaultGitBinary    = "git"
)

// Result.Action 的取值（硬性要求 4）。
const (
	// ActionCloned 表示本次是首次克隆。
	ActionCloned = "cloned"
	// ActionUpdated 表示本地缓存被更新到新的 revision。
	ActionUpdated = "updated"
	// ActionUnchanged 表示远端没有变化，本地缓存已是目标 revision。
	ActionUnchanged = "unchanged"
)

// Options 是缓存器的平台侧参数（来自 config，由调用方填）。
type Options struct {
	// RootDir 为仓库缓存根目录（如 /app/data/repos）。每个服务一个子目录。
	// 必填：LocalPath 越界校验也以它为基准。
	RootDir string
	// CloneTimeout / PullTimeout 为单次操作超时（clone 通常更久）。
	// 小于等于 0 时取默认值 10 分钟 / 2 分钟。
	CloneTimeout time.Duration
	PullTimeout  time.Duration
	// GitBinary 为 git 可执行文件路径，默认 "git"。
	GitBinary string
	// Log 可以为 nil（内部退化为 zap.NewNop()）。
	Log *zap.Logger
	// Runner 可选：注入命令执行器（测试用；为空时执行真实 git）。
	Runner Runner
}

// Runner 抽象一次外部命令执行，便于在测试里断言命令与模拟失败。
type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout string, stderr string, err error)
}

// Request 是一次"确保本地代码是最新的"请求。
type Request struct {
	Service string // 服务名（决定缓存子目录名）
	RepoURL string
	Branch  string // 为空表示远端默认分支
	// LocalPath 显式指定本地目录（来自 CodeRepo.local_path）；为空则由 RootDir/Service 推导。
	//
	// 安全提示（运维须知）：git 会把 clone 使用的 RepoURL 原样写进 <LocalPath>/.git/config，
	// 因此 URL 里若内嵌了访问令牌，令牌就会落在缓存目录里。缓存根目录必须限定在平台内部、
	// 不随镜像/备份外泄，并建议改用 credential helper 或只读部署令牌。
	LocalPath string
	// AllowOutbound 为合规开关：false 时**直接拒绝**（不允许把第三方代码拉到平台本地）。
	AllowOutbound bool
}

// signature 用于并发去重：同一 LocalPath 上「目标一致」的请求可以共用一次执行结果。
// LocalPath 本身已经是 map 的 key，因此这里只含远端与分支。
func (r Request) signature() string {
	return r.RepoURL + "\x00" + strings.TrimSpace(r.Branch)
}

// Result 是一次操作的结果。
type Result struct {
	LocalPath string
	Action    string // cloned | updated | unchanged
	Revision  string // 当前 HEAD 短 sha（失败时为空）
	Branch    string
}

// flight 表示一次正在进行中的同步操作，供并发调用者复用结果（singleflight 思路）。
type flight struct {
	sig  string
	done chan struct{}
	res  *Result
	err  error
}

// pathLock 是「同一 LocalPath 串行化」的槽位。
//
// refs 是引用计数：每个进入的调用者 +1，离开时 -1，归零即从 map 删除，
// 避免服务名很多时 map 无界增长（泄漏）。
type pathLock struct {
	mu     sync.Mutex
	refs   int
	flight *flight
}

// Fetcher 维护本地代码缓存。
type Fetcher struct {
	opts   Options
	runner Runner
	log    *zap.Logger
	// rootAbs 是 RootDir 的绝对路径（并解析已存在的软链接），所有越界判断以它为基准。
	rootAbs string

	mu    sync.Mutex
	locks map[string]*pathLock
}

// NewFetcher 构造缓存器：补齐默认值，并把 RootDir 归一化成绝对路径。
//
// 这里不返回 error（接口约定如此）：参数问题（如 RootDir 为空）在 Ensure 时
// 以明确错误返回，便于服务层统一处理。
func NewFetcher(opts Options) *Fetcher {
	if opts.CloneTimeout <= 0 {
		opts.CloneTimeout = defaultCloneTimeout
	}
	if opts.PullTimeout <= 0 {
		opts.PullTimeout = defaultPullTimeout
	}
	if strings.TrimSpace(opts.GitBinary) == "" {
		opts.GitBinary = defaultGitBinary
	}

	log := opts.Log
	if log == nil {
		log = zap.NewNop()
	}
	runner := opts.Runner
	if runner == nil {
		runner = execRunner{}
	}

	f := &Fetcher{
		opts:   opts,
		runner: runner,
		log:    log,
		locks:  make(map[string]*pathLock),
	}
	if abs, err := filepath.Abs(opts.RootDir); err == nil {
		f.rootAbs = resolveExisting(abs)
	} else {
		f.rootAbs = filepath.Clean(opts.RootDir)
	}
	return f
}

// Ensure 保证 LocalPath 处有一份**与远端一致**的代码：不存在则 clone，存在则更新。
// 返回错误时，若本地已有旧副本，调用方仍可使用旧目录（错误里说明这一点）。
//
// 行为与取舍（硬性要求 1/2/3/4/5/6/9）：
//   - AllowOutbound=false 直接拒绝，连路径都不解析，确保零副作用、不执行任何 git；
//   - 缓存目录名由 Service 净化而来；LocalPath 显式指定时必须严格落在 RootDir 之内；
//   - 目标目录已存在但不是 git 仓库 → 明确报错，绝不覆盖（可能是别人的目录）；
//   - branch 非空：fetch --prune origin + checkout -B <branch> origin/<branch>。
//     这是「把缓存强制重置到远端」的取舍：本地手工改动会被丢弃。理由是缓存不是
//     工作区，日志分析要的就是「与远端一致的那一份代码」——脏缓存会让文件行号
//     与线上堆栈对不上，静默给出错误的定位；宁可按远端覆盖也不要错误的行号。
//   - branch 为空：pull --ff-only（不改写历史，分叉时报错由运维决定）；
//   - 操作前后各取一次 HEAD：相同 → unchanged，不同 → updated；首次 → cloned；
//   - 同一 LocalPath 并发调用只允许一个真正执行 git，其余等待并复用结果。
func (f *Fetcher) Ensure(ctx context.Context, req Request) (*Result, error) {
	if !req.AllowOutbound {
		// 硬性要求 9：合规开关关闭 → 直接拒绝，不执行任何 git 命令。
		f.log.Warn("拒绝拉取远端代码：出网许可未开启（合规开关）",
			zap.String("service", req.Service),
			zap.String("repo_url", RedactURL(req.RepoURL)))
		return nil, fmt.Errorf("%w：平台当前禁止拉取远端代码（服务 %q）；"+
			"如确需拉取，请把 code_repo.allow_outbound 设为 true（环境变量 MWOPS_CODE_REPO_ALLOW_OUTBOUND）后重启平台。"+
			"注意它与 CodeRepo.allow_third_party 是两件事：后者管「能否把代码片段发给第三方 AI」，"+
			"本开关管「平台能否 git clone/pull」",
			ErrOutboundDenied, req.Service)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("repo: 同步开始前上下文已结束: %w", err)
	}

	local, err := f.resolveLocalPath(req)
	if err != nil {
		return nil, err
	}

	// 硬性要求 5：同一 LocalPath 串行化；同签名的并发请求复用 leader 的结果。
	pl, fl, leader := f.begin(local, req.signature())
	if !leader {
		defer f.release(local, pl)
		select {
		case <-fl.done:
			return fl.res, fl.err
		case <-ctx.Done():
			return nil, fmt.Errorf("repo: 等待同一仓库（%s）的并发同步时上下文结束: %w", local, ctx.Err())
		}
	}
	defer f.release(local, pl)

	res, opErr := f.runLeader(ctx, req, local, pl, fl)

	if opErr != nil {
		f.log.Error("代码缓存同步失败",
			zap.String("service", req.Service),
			zap.String("local_path", local),
			zap.String("repo_url", RedactURL(req.RepoURL)),
			zap.String("branch", req.Branch),
			zap.Error(opErr))
		return res, opErr
	}
	f.log.Info("代码缓存同步完成",
		zap.String("service", req.Service),
		zap.String("action", res.Action),
		zap.String("revision", res.Revision),
		zap.String("branch", res.Branch),
		zap.String("local_path", res.LocalPath),
		zap.String("repo_url", RedactURL(req.RepoURL)))
	return res, nil
}

// HasCode 报告本地缓存里是否已经有一份**可用**的代码：目录存在、是 git 仓库、且仓库没坏。
//
// 为什么需要它：调用方要用它把「刷新间隔内可以复用本地副本」和「本地其实根本没有代码」
// 区分开——后者必须立刻 clone，不能因为"刚刚拉过"就跳过（见服务层 ensureRepo）。
// 判定"可用"而不是"存在"是刻意的：一个残缺的 .git 目录（clone 被中断留下的）
// 同样算"没有代码"，照它去定位代码只会得到空结果。
func (f *Fetcher) HasCode(ctx context.Context, req Request) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	local, err := f.resolveLocalPath(req)
	if err != nil {
		return false
	}
	exists, isRepo, err := probePath(local)
	if err != nil || !exists || !isRepo {
		return false
	}
	return f.repoHealthy(ctx, local)
}

// LocalPathFor 只做本地目录解析与越界校验，不执行任何 git 操作、不检查 AllowOutbound。
//
// 用途：降级场景——Ensure 失败但本地还留着旧副本时，调用方需要知道该用哪个目录；
// 也用于分析阶段直接定位代码目录。净化/越界规则由本包封装，避免调用方各写一份
// （尤其是 Service → 目录名的净化规则，调用方不应重复实现）。
func (f *Fetcher) LocalPathFor(req Request) (string, error) {
	return f.resolveLocalPath(req)
}

// runLeader 在持有 pl.mu 的情况下执行同步，并把结果发布给同签名的等待者。
//
// pl.mu 串行化同一路径上的 git 操作（不同签名的请求会各自执行，但仍不并发）。
// 注意：这里等锁不看 ctx，代价是最多等前一次操作超时结束。这是刻意的取舍——
// 同一个缓存目录里并发跑两个 git 会互相破坏 index，宁可等也不要缓存损坏。
//
// finish 放在 defer 里：即使 ensureLocal 因注入的 Runner panic 而中断，
// 等待者也会被唤醒（否则会永久阻塞在 <-fl.done 上），并把「无结果」按错误上报。
// 发布在 Unlock 之前完成（defer 是后进先出：finish 先执行、Unlock 后执行），
// 避免刚放锁的另一个操作改了 HEAD，让同签名等待者拿到与 fl.res 不一致的状态。
func (f *Fetcher) runLeader(ctx context.Context, req Request, local string, pl *pathLock, fl *flight) (res *Result, err error) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	defer func() {
		if res == nil && err == nil {
			err = errors.New("repo: 同步未返回结果（内部错误，可能是注入的 Runner panic）")
		}
		f.finish(pl, fl, res, err)
	}()
	return f.ensureLocal(ctx, req, local)
}

// begin 取该路径的锁槽位；leader=true 表示本次调用负责真正执行 git。
//
// 引用计数与槽位删除都在 f.mu 下完成，保证不泄漏、也不会删掉别人正在用的槽位。
func (f *Fetcher) begin(path, sig string) (pl *pathLock, fl *flight, leader bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pl = f.locks[path]
	if pl == nil {
		pl = &pathLock{}
		f.locks[path] = pl
	}
	pl.refs++

	if pl.flight != nil && pl.flight.sig == sig {
		return pl, pl.flight, false // 已有同目标操作在进行：等它并复用结果
	}
	fl = &flight{sig: sig, done: make(chan struct{})}
	pl.flight = fl
	return pl, fl, true
}

// finish 发布操作结果并唤醒等待者（只由 leader 调用，且在持有 pl.mu 时调用）。
func (f *Fetcher) finish(pl *pathLock, fl *flight, res *Result, err error) {
	f.mu.Lock()
	if pl.flight == fl {
		pl.flight = nil
	}
	f.mu.Unlock()

	fl.res, fl.err = res, err
	close(fl.done)
}

// release 归还槽位引用；引用归零且无进行中操作时删除槽位，避免 map 无界增长。
func (f *Fetcher) release(path string, pl *pathLock) {
	f.mu.Lock()
	defer f.mu.Unlock()

	pl.refs--
	if pl.refs <= 0 && pl.flight == nil {
		if cur, ok := f.locks[path]; ok && cur == pl {
			delete(f.locks, path)
		}
	}
}

// resolveLocalPath 计算并校验本地目录（硬性要求 1）。
//
// 两道防线：
//  1. Service 净化为只含 [A-Za-z0-9._-] 的单层目录名（拒绝 "."、".."、空），
//     因此 "../../etc/passwd" 这类用户输入不可能穿出 RootDir；
//  2. LocalPath 显式指定时用 filepath.Rel 判断其必须严格落在 RootDir 之内，
//     并解析已存在部分的软链接后再判一次，防止 root 内的软链把写入引到外面。
func (f *Fetcher) resolveLocalPath(req Request) (string, error) {
	if strings.TrimSpace(f.opts.RootDir) == "" {
		return "", fmt.Errorf("repo: Options.RootDir 为空，无法确定仓库缓存根目录（应由 config 注入，如 /app/data/repos）")
	}

	var candidate string
	if p := strings.TrimSpace(req.LocalPath); p != "" {
		candidate = p
	} else {
		name, err := sanitizeServiceName(req.Service)
		if err != nil {
			return "", err
		}
		candidate = filepath.Join(f.opts.RootDir, name)
	}

	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("repo: 解析本地目录 %q 失败: %w", candidate, err)
	}
	if !withinDir(f.rootAbs, abs) {
		return "", fmt.Errorf("%w：LocalPath %q 解析为 %q，不在仓库缓存根目录 %q 之内"+
			"（Service 可能来自用户输入，禁止越界写入平台其它目录）",
			ErrPathEscape, req.LocalPath, abs, f.rootAbs)
	}
	if resolved := resolveExisting(abs); !withinDir(f.rootAbs, resolved) {
		return "", fmt.Errorf("%w：LocalPath %q 经符号链接解析后为 %q，越出根目录 %q",
			ErrPathEscape, req.LocalPath, resolved, f.rootAbs)
	}
	return abs, nil
}

// ensureLocal 是串行化之后的实际操作：判定 clone 还是更新，并保证错误可操作。
//
// 「没有代码」的三种形态都必须落到 clone，否则该服务会永久拿不到代码：
//  1. 目录不存在（首次，或被重建的容器丢掉了缓存卷）——最常见；
//  2. 目录存在但为空——上次 clone 被中断（进程被 kill、容器重建）留下的空壳，
//     里面没有任何需要保护的内容，留着只会让后续每次都卡在"已存在但不是仓库"；
//  3. 目录有 .git 但 git 认为它不是一个可用仓库——同样是中断留下的半成品。
//
// 只有"目录里有内容且不是 git 仓库"才按硬性要求 2 拒绝覆盖：那可能是别人的目录，
// 删掉是不可逆的数据损失。
func (f *Fetcher) ensureLocal(ctx context.Context, req Request, local string) (*Result, error) {
	exists, isRepo, err := probePath(local)
	if err != nil {
		return nil, err
	}
	if !exists {
		return f.clone(ctx, req, local)
	}
	if !isRepo {
		empty, err := dirIsEmpty(local)
		if err != nil {
			return nil, fmt.Errorf("repo: 检查目录 %s 内容失败: %w", local, err)
		}
		if !empty {
			// 硬性要求 2：绝不覆盖已有内容——它可能是其它服务的缓存、也可能是人工放置的数据。
			return nil, fmt.Errorf("%w：目录 %s 已存在但没有 .git，拒绝覆盖；"+
				"请修改 RootDir/Service（改用其它目录）或确认该目录可以清理后手工删除再重试",
				ErrNotGitRepo, local)
		}
		// 空目录：clone 被中断留下的空壳，删掉重来是安全的（里面没有内容可丢）。
		f.log.Warn("缓存目录为空且不是 git 仓库，按「上次克隆被中断」处理并重新克隆",
			zap.String("local_path", local))
		if err := os.RemoveAll(local); err != nil {
			return nil, fmt.Errorf("repo: 清理空的残留目录 %s 失败: %w", local, err)
		}
		return f.clone(ctx, req, local)
	}
	if f.repoHealthy(ctx, local) {
		return f.update(ctx, req, local)
	}
	// .git 在但仓库不可用：半成品，留着只会让每次更新都失败。删掉重新 clone。
	f.log.Warn("缓存目录的 git 仓库不可用，按「上次克隆被中断」处理并重新克隆",
		zap.String("local_path", local))
	if err := os.RemoveAll(local); err != nil {
		return nil, fmt.Errorf("repo: 清理损坏的缓存目录 %s 失败: %w", local, err)
	}
	return f.clone(ctx, req, local)
}

// repoHealthy 判断目录里的 git 仓库能不能正常工作。
//
// 用 rev-parse --git-dir（只查询、不改动工作区）：clone 被打断留下的半成品 .git
// 会在这里失败，而它对 update 来说也确实不可用。
func (f *Fetcher) repoHealthy(ctx context.Context, local string) bool {
	if _, err := f.runGit(ctx, local, "rev-parse --git-dir", f.opts.PullTimeout, "",
		"-C", local, "rev-parse", "--git-dir"); err != nil {
		return false
	}
	return true
}

// clone 首次克隆（硬性要求 2）。
func (f *Fetcher) clone(ctx context.Context, req Request, local string) (*Result, error) {
	if strings.TrimSpace(req.RepoURL) == "" {
		return nil, fmt.Errorf("repo: 本地目录 %s 不存在且 RepoURL 为空，无法克隆（请在 CodeRepo 中配置仓库地址）", local)
	}
	branch := strings.TrimSpace(req.Branch)
	if err := validateBranch(branch); err != nil {
		return nil, err
	}

	args := []string{"clone"}
	if branch != "" {
		args = append(args, "--branch", branch)
	}
	// 用 "--" 分隔选项与位置参数：即使 RepoURL/分支以 "-" 开头也不会被当成 git 选项。
	args = append(args, "--", req.RepoURL, local)

	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return nil, fmt.Errorf("repo: 创建缓存目录 %s 失败: %w", filepath.Dir(local), err)
	}

	if _, err := f.runGit(ctx, filepath.Dir(local), "clone", f.opts.CloneTimeout, req.RepoURL, args...); err != nil {
		// 失败的 clone 会留下半个目录；只清理「本次由我们创建的」目标目录
		// （进入这里之前已确认该路径不存在），避免污染缓存根目录、
		// 也避免下次误判成「已存在但不是仓库」而永久卡住。
		if rmErr := os.RemoveAll(local); rmErr != nil {
			f.log.Warn("清理克隆失败的残留目录失败", zap.String("local_path", local), zap.Error(rmErr))
		}
		return nil, err
	}

	return &Result{
		LocalPath: local,
		Action:    ActionCloned,
		Revision:  f.shortRevision(ctx, local),
		Branch:    f.resultBranch(ctx, req, local),
	}, nil
}

// update 更新已有缓存（硬性要求 3/4）。
func (f *Fetcher) update(ctx context.Context, req Request, local string) (*Result, error) {
	branch := strings.TrimSpace(req.Branch)
	// 变更判定：操作前后各取一次 HEAD（完整 sha 比较，短 sha 只用于对外展示）。
	before := f.headRevision(ctx, local)

	// 远端地址与缓存不一致时只告警不改写：URL 变化常常只是令牌轮换，
	// 直接覆盖 origin 或报错都会破坏「下次 pull 就行」的预期。
	f.warnRemoteMismatch(ctx, req, local)

	var err error
	if branch != "" {
		if err = validateBranch(branch); err == nil {
			_, err = f.runGit(ctx, local, "fetch", f.opts.PullTimeout, req.RepoURL,
				"-C", local, "fetch", "--prune", "origin")
		}
		if err == nil {
			// checkout -B + --force：把缓存分支强制重置到远端，丢弃本地手工改动，
			// 保证缓存内容与远端一致（见 Ensure 注释里的取舍说明）。
			_, err = f.runGit(ctx, local, "checkout", f.opts.PullTimeout, req.RepoURL,
				"-C", local, "checkout", "--force", "-B", branch, "origin/"+branch)
		}
	} else {
		_, err = f.runGit(ctx, local, "pull", f.opts.PullTimeout, req.RepoURL,
			"-C", local, "pull", "--ff-only")
	}
	if err != nil {
		// 硬性要求：本地已有旧副本时明确告知，调用方可以降级使用。
		return nil, fmt.Errorf("%w；本地保留同步前的旧副本（%s），可降级继续使用，但代码可能不是最新，"+
			"行号定位会有偏差", err, local)
	}

	after := f.headRevision(ctx, local)
	action := ActionUpdated
	if before != "" && before == after {
		action = ActionUnchanged
	}
	return &Result{
		LocalPath: local,
		Action:    action,
		Revision:  f.shortRevision(ctx, local),
		Branch:    f.resultBranch(ctx, req, local),
	}, nil
}

// warnRemoteMismatch 比较缓存里 origin 的 URL 与本次请求的 RepoURL，不一致只告警。
// 失败（例如没有 origin）忽略：更新流程本身会给出真正的错误。
func (f *Fetcher) warnRemoteMismatch(ctx context.Context, req Request, local string) {
	out, err := f.runGit(ctx, local, "remote-get-url", f.opts.PullTimeout, "",
		"-C", local, "remote", "get-url", "origin")
	if err != nil {
		return
	}
	got := strings.TrimSpace(out)
	if got == "" || strings.TrimSpace(req.RepoURL) == "" || got == strings.TrimSpace(req.RepoURL) {
		return
	}
	f.log.Warn("缓存目录的 origin 与本次配置的 RepoURL 不一致，仍按缓存里的 origin 更新",
		zap.String("local_path", local),
		zap.String("origin", RedactURL(got)),
		zap.String("configured", RedactURL(req.RepoURL)))
}

// resultBranch 给出结果里的分支名：请求指定则用它，否则探测当前分支（detached 时为空）。
func (f *Fetcher) resultBranch(ctx context.Context, req Request, local string) string {
	if b := strings.TrimSpace(req.Branch); b != "" {
		return b
	}
	out, err := f.runGit(ctx, local, "rev-parse --abbrev-ref", f.opts.PullTimeout, "",
		"-C", local, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	if b := strings.TrimSpace(out); b != "HEAD" {
		return b
	}
	return ""
}

// headRevision 取 HEAD 的完整 sha；失败返回空串（仅用于变更判定，失败不致命）。
func (f *Fetcher) headRevision(ctx context.Context, local string) string {
	out, err := f.runGit(ctx, local, "rev-parse HEAD", f.opts.PullTimeout, "",
		"-C", local, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// shortRevision 取 HEAD 的短 sha（对外展示用，硬性要求 4）。
func (f *Fetcher) shortRevision(ctx context.Context, local string) string {
	out, err := f.runGit(ctx, local, "rev-parse --short HEAD", f.opts.PullTimeout, "",
		"-C", local, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// runGit 是本包唯一的 git 出口：统一注入环境变量、套超时、脱敏日志、翻译错误。
//
// 收敛到一处的原因：脱敏/超时/错误翻译任何一项漏在某个调用点都会变成线上问题
// （凭据进日志、卡死、不可诊断的错误），集中处理才可审计。
//
// dir 是子进程工作目录；rawURL 仅用于错误信息（内部会脱敏）。
// 本地只读操作（rev-parse / remote get-url）也复用 PullTimeout：它们本是毫秒级命令，
// 给上限只是防止极端情况（仓库损坏、磁盘卡住）把调用方拖住。
func (f *Fetcher) runGit(ctx context.Context, dir, op string, timeout time.Duration, rawURL string, args ...string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	f.log.Debug("执行 git 命令",
		zap.String("op", op),
		zap.String("dir", dir),
		zap.Strings("args", redactArgs(args)),
		zap.Duration("timeout", timeout))

	stdout, stderr, err := f.runner.Run(tctx, dir, gitEnv(), f.opts.GitBinary, args...)
	if err == nil {
		return stdout, nil
	}

	detail := gitErrorDetail(stderr)
	suffix := ""
	if detail != "" {
		suffix = "；git 输出：" + detail
	}

	// 超时必须能一眼看出是超时（硬性要求 6），并且能用 errors.Is(err, context.DeadlineExceeded) 判定。
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return stdout, fmt.Errorf("repo: git %s 超时（超时上限 %s，目录 %s）%s；"+
			"仓库过大或网络过慢时可调大 Options.CloneTimeout/PullTimeout: %w",
			op, timeout, dir, suffix, context.DeadlineExceeded)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, fmt.Errorf("repo: git %s 被取消（目录 %s）%s: %w", op, dir, suffix, ctxErr)
	}
	return stdout, translateGitError(op, rawURL, stderr, err)
}

// validateBranch 校验分支名，主要防「分支名被当成 git 选项」的参数注入。
//
// branch 会出现在 `git checkout -B <branch> origin/<branch>` 与 `git clone --branch`，
// 以 "-" 开头的值（如 "-f"）会被 git 解析成选项。git 本身也不接受含空白/控制字符的
// 分支名，这里提前给出中文错误，比事后解析 git 输出更明确。
// 空字符串合法，表示「用远端默认分支」。
func validateBranch(branch string) error {
	if branch == "" {
		return nil
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("%w：分支名 %q 以 \"-\" 开头，会被 git 当成命令行选项（参数注入防护），请去掉该前缀",
			ErrInvalidBranch, branch)
	}
	if strings.ContainsFunc(branch, unicode.IsSpace) || strings.ContainsFunc(branch, unicode.IsControl) {
		return fmt.Errorf("%w：分支名 %q 含空白或控制字符，git 不接受该分支名", ErrInvalidBranch, branch)
	}
	return nil
}
