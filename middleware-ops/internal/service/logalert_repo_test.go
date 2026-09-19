package service

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/model"
)

// 本文件钉住「代码拉取」的三条原则里最容易退化、又最难从界面上看出来的部分：
//   - 没有代码时必须 clone（不能被"刚拉过"的记忆挡住）；
//   - 分析入口自己会补代码（不是只靠上游 worker 拉过一次）。

// stubRepoFetcher 记录调用，用于断言"到底有没有去 clone"。
type stubRepoFetcher struct {
	hasCode bool
	ensured int
	lastReq RepoFetchRequest
	err     error
	// files 是 TrackedFiles 的返回值（模拟 git 索引里的文件清单）；
	// filesErr 非空时模拟"索引不可用"，用于跑兜底遍历那条路径。
	files    []string
	filesErr error
	revision string
}

func (s *stubRepoFetcher) Ensure(ctx context.Context, req RepoFetchRequest) (RepoFetchResult, error) {
	s.ensured++
	s.lastReq = req
	if s.err != nil {
		return RepoFetchResult{}, s.err
	}
	return RepoFetchResult{LocalPath: "/data/repos/svc", Action: "cloned"}, nil
}

func (s *stubRepoFetcher) HasCode(ctx context.Context, req RepoFetchRequest) bool {
	return s.hasCode
}

func (s *stubRepoFetcher) TrackedFiles(ctx context.Context, req RepoFetchRequest) ([]string, error) {
	s.lastReq = req
	return s.files, s.filesErr
}

func (s *stubRepoFetcher) Revision(ctx context.Context, req RepoFetchRequest) string {
	return s.revision
}

// TestCanReuseLocalCache 钉住：本地没有代码时，无论"上次拉过多久"都不能走缓存。
func TestCanReuseLocalCache(t *testing.T) {
	now := time.Now()
	just := now.Add(-time.Second)
	old := now.Add(-time.Hour)

	cases := []struct {
		name     string
		hasCode  bool
		last     time.Time
		interval time.Duration
		want     bool
	}{
		{"有代码且刚拉过 → 复用", true, just, 5 * time.Minute, true},
		{"有代码但已过期 → 不复用", true, old, 5 * time.Minute, false},
		{"有代码但没拉过 → 不复用", true, time.Time{}, 5 * time.Minute, false},
		// 关键用例：容器重建后代码没了，即使"刚拉过"也必须重新 clone。
		{"没有代码且刚拉过 → 不复用（必须 clone）", false, just, 5 * time.Minute, false},
		{"没有代码且已过期 → 不复用", false, old, 5 * time.Minute, false},
		{"间隔为 0（每次都确认）→ 不复用", true, just, 0, false},
	}
	for _, c := range cases {
		if got := canReuseLocalCache(c.hasCode, c.last, now, c.interval); got != c.want {
			t.Fatalf("%s：canReuseLocalCache=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestEnsureRepoClonesWhenCodeMissing 钉住 ensureRepo 的短路条件里包含"本地有代码"。
//
// 退化路径很隐蔽：刷新间隔只看时间，不看代码在不在。缓存卷被重建后，
// 服务会一直返回 cached，分析拿着不存在的目录静默给出没有代码依据的结论。
func TestEnsureRepoClonesWhenCodeMissing(t *testing.T) {
	// 没有代码 + 刚拉过：必须真的去 Ensure（clone）。
	fetcher := &stubRepoFetcher{hasCode: false}
	w := &LogAlertWorker{fetcher: fetcher, log: zap.NewNop()}
	w.repoRefreshedAt.Store("svc", time.Now())

	repo := model.CodeRepo{ServiceName: "svc", RepoURL: "https://example.com/x.git", Branch: "main"}
	if _, err := w.syncRepo(context.Background(), repo); err != nil {
		t.Fatalf("syncRepo 失败: %v", err)
	}

	if fetcher.ensured != 1 {
		t.Fatalf("本地没有代码时应执行 1 次 Ensure，实际 %d 次", fetcher.ensured)
	}

	// 有代码 + 刚拉过 → 走本地副本，不去打扰远端（故障风暴里别反复 pull）。
	cached := &stubRepoFetcher{hasCode: true}
	w2 := &LogAlertWorker{fetcher: cached, log: zap.NewNop()}
	w2.repoRefreshedAt.Store("svc", time.Now())
	res, err := w2.syncRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("syncRepo 失败: %v", err)
	}
	if cached.ensured != 0 {
		t.Fatalf("有代码且在刷新间隔内时不应拉取，实际 %d 次", cached.ensured)
	}
	if res.Action != "cached" {
		t.Fatalf("Action = %q，期望 cached", res.Action)
	}
}

// TestCodeAnalysisEnsuresCodeBeforeLocating 钉住分析入口会自己补代码。
func TestCodeAnalysisEnsuresCodeBeforeLocating(t *testing.T) {
	// ① 本地没有代码 → 分析前 clone 一次，并把拿到的路径用于检索。
	fetcher := &stubRepoFetcher{hasCode: false}
	s := &CodeAnalysisService{log: zap.NewNop()}
	s.SetRepoFetcher(fetcher)
	repo := &model.CodeRepo{ServiceName: "svc", RepoURL: "https://example.com/x.git", Branch: "main"}
	s.ensureCode(context.Background(), repo)

	if fetcher.ensured != 1 {
		t.Fatalf("本地没有代码时应 clone 1 次，实际 %d 次", fetcher.ensured)
	}
	if repo.LocalPath != "/data/repos/svc" {
		t.Fatalf("应把 clone 得到的路径写回映射用于检索，实际 %q", repo.LocalPath)
	}

	// ② 本地已有代码 → 不重复拉取（一次手工分析不该触发一次 pull）。
	fetcher2 := &stubRepoFetcher{hasCode: true}
	s2 := &CodeAnalysisService{log: zap.NewNop()}
	s2.SetRepoFetcher(fetcher2)
	s2.ensureCode(context.Background(), &model.CodeRepo{ServiceName: "svc", RepoURL: "https://example.com/x.git"})
	if fetcher2.ensured != 0 {
		t.Fatalf("本地已有代码时不应拉取，实际 %d 次", fetcher2.ensured)
	}

	// ③ 没装配 fetcher 或没配仓库地址时静默跳过（不能因为拉取能力缺失就让分析失败）。
	(&CodeAnalysisService{log: zap.NewNop()}).ensureCode(context.Background(), repo)
	(&CodeAnalysisService{log: zap.NewNop(), fetcher: &stubRepoFetcher{}}).
		ensureCode(context.Background(), &model.CodeRepo{ServiceName: "svc"})
}
