package repo

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 本文件是启动自检：用一条最廉价的命令确认 git 真的可执行。

// doctorTimeout 是自检超时上限。
//
// `git --version` 正常是毫秒级；给上限只为兜住极端情况（PATH 里指向一个会挂起的同名文件、
// 磁盘卡住），避免自检本身把启动流程拖死。
const doctorTimeout = 5 * time.Second

// ProbeGitBinary 检查 git 可执行文件是否可用，返回 `git --version` 的输出。
//
// 为什么要有这一步（而不是等到第一次 Ensure 失败再说）：
// 本包用 exec 调用外部 git（没有内置实现），缺 git 时平台**不会启动失败**，
// 而是每条日志事件的代码分析都失败（analysis_state=failed），原因藏在 analysis_error 里。
// 这种降级悄无声息，运维往往要等有人去看事件详情才知道是运行镜像少装了包。
// 把检查前置到启动这一刻，缺失时日志里直接给出结论与修法。
//
// gitBinary 为空时按 Options 的默认值（"git"）处理，与 Fetcher 实际使用的保持一致。
func ProbeGitBinary(gitBinary string) (string, error) {
	binary := strings.TrimSpace(gitBinary)
	if binary == "" {
		binary = defaultGitBinary
	}

	ctx, cancel := context.WithTimeout(context.Background(), doctorTimeout)
	defer cancel()

	stdout, stderr, err := (execRunner{}).Run(ctx, "", gitEnv(), binary, "--version")
	if err != nil {
		return "", fmt.Errorf("无法执行 %q --version：容器/运行环境未安装 git，或 PATH 中找不到该可执行文件"+
			"（请把 git 装进运行镜像，或把 code_repo.allow_outbound 设为 false 明确关闭代码拉取能力）: %w",
			binary, err)
	}

	// 少数构建把版本号打到 stderr；拿不到版本就按可用处理，但日志里没有版本会显得可疑，
	// 因此优先 stdout、回退 stderr。
	out := strings.TrimSpace(stdout)
	if out == "" {
		out = strings.TrimSpace(stderr)
	}
	return out, nil
}
