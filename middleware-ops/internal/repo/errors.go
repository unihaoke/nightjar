package repo

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"regexp"
	"strings"
)

// 本文件把 git 的英文 stderr 翻译成「可操作的中文结论」（硬性要求 8）。
//
// 设计判断：这条链路的错误既给人看（运维排查）也给机器看（告警/重试）。
// 只回一个 "exit status 128" 无法定位问题，所以每个错误都必须同时给出：
//  1. 中文结论——「该怎么办」；
//  2. 脱敏后的 git stderr 片段——「原始证据」，供人工核对与工单引用。
//
// 分类规则按「最可能直接给出修法」排序；多种特征同时出现时取前者，
// 例如 "unable to access ...: Repository not found" 应判为「仓库不存在」而不是「网络问题」。

// 本包的哨兵错误，便于调用方用 errors.Is 分类处理（是否重试、是否提示运维）。
var (
	// ErrOutboundDenied 表示合规开关 AllowOutbound=false，本次未执行任何 git 命令。
	ErrOutboundDenied = errors.New("repo: 出网许可未开启")

	// ErrPathEscape 表示 LocalPath（或由 Service 推导的目录）越出了 RootDir。
	ErrPathEscape = errors.New("repo: 本地路径越界")

	// ErrNotGitRepo 表示目标目录已存在但不是 git 仓库，出于安全拒绝覆盖。
	ErrNotGitRepo = errors.New("repo: 目标目录已存在但不是 git 仓库")

	// ErrInvalidService 表示 Service 无法净化成合法目录名。
	ErrInvalidService = errors.New("repo: 服务名非法")

	// ErrInvalidBranch 表示分支名会被 git 当成选项（参数注入防护）或含非法字符。
	ErrInvalidBranch = errors.New("repo: 分支名非法")
)

// gitErrorRule 是一条「stderr 特征 → 中文结论」的翻译规则。
type gitErrorRule struct {
	patterns []*regexp.Regexp
	advice   string
}

// gitErrorRules 是翻译表，顺序即优先级。
var gitErrorRules = []gitErrorRule{
	{ // 认证失败：三类典型输出分别来自 HTTPS、终端提示被关闭、SSH。
		patterns: mustRegexps(
			`(?i)Authentication failed`,
			`(?i)could not read Username`,
			`(?i)Permission denied \(publickey\)`,
			`(?i)Invalid username or password`,
			`(?i)HTTP Basic: Access denied`,
			`(?i)terminal prompts disabled`,
			`(?i)Authentication required`,
			`(?i)returned error: 401`,
		),
		advice: "认证失败：请检查凭据是否有效/未过期（HTTPS 用访问令牌、SSH 用部署密钥），" +
			"以及该令牌对该仓库是否有读权限；平台以服务方式运行、已关闭交互式输入，不会弹窗等待输入",
	},
	{ // 仓库不存在 / 无权限：GitHub 对「不存在」和「私有但无权访问」都回 Repository not found。
		patterns: mustRegexps(
			`(?i)Repository not found`,
			`(?i)repository .* does not exist`,
			`(?i)does not appear to be a git repository`,
			`(?i)returned error: 404`,
		),
		advice: "仓库不存在或当前凭据无权访问：请确认 RepoURL 拼写与令牌对该仓库的授权" +
			"（私有仓库需授予读权限）",
	},
	{ // 服务端返回 403：多为令牌权限不足或来源 IP 不在仓库侧白名单。
		patterns: mustRegexps(`(?i)returned error: 403`, `(?i)Access denied`),
		advice:   "服务端拒绝访问（403）：请检查令牌权限范围与该仓库/代码平台的 IP 访问白名单",
	},
	{ // 分支/引用不存在：也可能是空仓库（没有任何提交）。
		patterns: mustRegexps(
			`(?i)couldn't find remote ref`,
			`(?i)Remote branch .* not found`,
			`(?i)not found in upstream`,
			`(?i)invalid refspec`,
			`(?i)pathspec .* did not match`,
			`(?i)unknown revision or path not in the working tree`,
		),
		advice: "分支或引用不存在：请确认 Branch 配置是否拼写正确，或该仓库是否还没有任何提交（空仓库）",
	},
	{ // 磁盘写满：缓存目录所在分区空间不足。
		patterns: mustRegexps(`(?i)No space left on device`, `(?i)disk quota exceeded`),
		advice:   "磁盘空间不足：请清理仓库缓存目录（Options.RootDir）后重试",
	},
	{ // 缓存目录损坏：目录在但不是合法仓库（例如被手工改动/半途中断）。
		patterns: mustRegexps(`(?i)not a git repository`),
		advice:   "本地缓存目录已不是合法 git 仓库：可删除该服务目录后重试（会重新 clone）",
	},
	{ // 证书校验失败：常见于自签代码平台或代理做了 TLS 拦截。
		patterns: mustRegexps(
			`(?i)SSL certificate problem`,
			`(?i)server certificate verification failed`,
			`(?i)unable to get local issuer certificate`,
		),
		advice: "TLS 证书校验失败：请把代码平台的 CA 证书加入平台信任链（或改用受信域名），不要直接关闭校验",
	},
	{ // 分叉/脏缓存导致的快进失败（branch 为空的 pull --ff-only 路径）。
		patterns: mustRegexps(
			`(?i)Not possible to fast-forward`,
			`(?i)Your local changes to the following files would be overwritten`,
			`(?i)Please commit your changes or stash them`,
			`(?i)You are not currently on a branch`,
		),
		advice: "本地缓存与远端不一致，无法直接快进：缓存目录是只读缓存、不应手工改动，" +
			"请为该服务显式配置 Branch（会强制重置到远端），或删除该缓存目录后重新 clone",
	},
	{ // DNS 与连接类：具体特征先判，避免被下面的宽泛规则吃掉。
		patterns: mustRegexps(
			`(?i)Could not resolve host`,
			`(?i)Connection timed out`,
			`(?i)Connection refused`,
			`(?i)Network is unreachable`,
			`(?i)Failed to connect`,
			`(?i)Operation timed out`,
			`(?i)no route to host`,
		),
		advice: "无法访问远端：请检查平台出网白名单、代理配置与网络连通性（DNS 解析、443/22 端口）",
	},
	{ // 宽泛的 "unable to access"：兜住剩余的出网失败（放最后，避免误吞上面的分类）。
		patterns: mustRegexps(`(?i)unable to access`, `(?i)Could not read from remote repository`),
		advice:   "无法访问远端仓库：请检查出网白名单与网络（含代理），并确认仓库地址可达",
	},
}

// maxSnippet 是错误信息里保留的 git stderr 字符数上限（按 rune 计）。
//
// 上限存在的理由：git 输出可能很长（进度、ref 列表），而错误会进日志、审计与告警，
// 必须保证单条错误可读、不淹没有效信息。
const maxSnippet = 400

// gitErrorDetail 把 stderr 压成单行、脱敏、限长，作为错误里的原始证据。
func gitErrorDetail(stderr string) string {
	s := redactText(strings.TrimSpace(stderr))
	if s == "" {
		return ""
	}
	// 压成单行：多行 stderr 直接拼进错误会让日志按行散开，反而不易读。
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxSnippet {
		s = string(r[:maxSnippet]) + "…(已截断)"
	}
	return s
}

// classifyGitError 按翻译表给出中文结论文本；未命中时给出通用结论。
func classifyGitError(stderr string) string {
	for _, rule := range gitErrorRules {
		for _, re := range rule.patterns {
			if re.MatchString(stderr) {
				return rule.advice
			}
		}
	}
	return "git 执行失败（未能自动分类）：请按下方 git 输出排查仓库地址、分支与网络"
}

// translateGitError 把一次 git 调用的失败翻译成可操作的中文错误。
//
// op 为操作名（clone/fetch/checkout/pull/rev-parse 等）；rawURL 为远端地址，
// 会脱敏后带进错误，便于定位是哪个仓库；stderr 是 git 的输出。
// 返回的 error 一定用 %w 包裹原始 err，保留 errors.Is/As 语义。
func translateGitError(op, rawURL, stderr string, err error) error {
	if err == nil {
		return nil
	}

	detail := gitErrorDetail(stderr)
	target := RedactURL(strings.TrimSpace(rawURL))

	// 命令根本没跑起来（git 不存在/工作目录不存在）时，stderr 是空的，
	// 按 stderr 分类会退化成「未能自动分类」，所以先单独识别。
	var execErr *exec.Error
	var cause string
	switch {
	case errors.As(err, &execErr):
		cause = "git 可执行文件不存在或不可执行：请检查容器内是否安装 git，或把 Options.GitBinary 配成绝对路径"
	case errors.Is(err, fs.ErrNotExist):
		cause = "命令的工作目录不存在：请检查 Options.RootDir 是否可写/已挂载"
	default:
		cause = classifyGitError(stderr)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "repo: git %s 失败：%s", op, cause)
	if target != "" {
		fmt.Fprintf(&b, "（远端仓库 %s）", target)
	}
	if detail != "" {
		fmt.Fprintf(&b, "；git 输出：%s", detail)
	}
	return fmt.Errorf("%s: %w", b.String(), err)
}

// mustRegexps 编译一组固定正则；模式写错属于编程错误，启动即 panic。
func mustRegexps(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, regexp.MustCompile(p))
	}
	return out
}
