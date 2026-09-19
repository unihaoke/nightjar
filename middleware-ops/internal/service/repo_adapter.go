package service

import (
	"context"

	"middleware-ops/internal/repo"
)

// repoFetcherAdapter 把 internal/repo 的 Fetcher 适配成服务层需要的 RepoFetcher。
//
// 为什么要有这层适配而不是让服务层直接依赖 internal/repo：
// 服务层只关心"给我一份最新代码"这个能力（RepoFetcher 接口），
// 换成别的实现（本地镜像解包、对象存储下载）时不需要改服务层；
// 而缓存根目录、git 路径、超时属于**进程级**配置，在容器装配时一次性交给 Fetcher，
// 服务层每次只描述"这次要哪个服务的哪条分支"。
type repoFetcherAdapter struct {
	fetcher *repo.Fetcher
}

// NewRepoFetcher 构造适配器（供容器装配使用）。
func NewRepoFetcher(fetcher *repo.Fetcher) RepoFetcher {
	if fetcher == nil {
		return nil
	}
	return repoFetcherAdapter{fetcher: fetcher}
}

// Ensure 执行一次"确保本地代码是最新的"。
func (a repoFetcherAdapter) Ensure(ctx context.Context, req RepoFetchRequest) (RepoFetchResult, error) {
	result, err := a.fetcher.Ensure(ctx, repo.Request{
		Service: req.Service, RepoURL: req.RepoURL, Branch: req.Branch,
		LocalPath: req.LocalPath, AllowOutbound: req.AllowOutbound,
	})
	if result == nil {
		return RepoFetchResult{}, err
	}
	return RepoFetchResult{
		LocalPath: result.LocalPath, Action: result.Action,
		Revision: result.Revision, Branch: result.Branch,
	}, err
}
