package repo

import "regexp"

// 本文件实现凭据脱敏（硬性要求 7）。
//
// 背景：RepoURL 可能形如 https://user:token@host/x.git，也可能带 ?token=xxx。
// 这些值会出现在日志、审计、错误信息和 git 的 stderr 里——一旦落库或打到日志，
// 就等于凭据泄漏（git 报错时经常把完整 URL 回显出来，包括内嵌令牌）。
// 因此本包所有对外输出（日志、错误）都必须先过这里。

// secretQueryKeys 是需要在查询串里脱敏的键名（匹配时大小写不敏感）。
const secretQueryKeys = `access_token|auth_token|private_token|token|api_key|apikey|` +
	`password|passwd|pwd|secret|oauth2|sig|signature|x-amz-signature|x-amz-credential`

// redacted 是统一的脱敏占位符。
const redacted = "***"

var (
	// reSecretQuery 匹配 ?token=xxx / &access_token=xxx 这类查询参数，保留键名。
	reSecretQuery = regexp.MustCompile(`(?i)([?&](?:` + secretQueryKeys + `)=)[^&#\s]+`)

	// reUserInfo 匹配 scheme://user:pass@ 里的 userinfo 段（整体替换，连用户名一起隐藏）。
	reUserInfo = regexp.MustCompile(`(?i)([a-z][a-z0-9+.\-]*://)[^/@\s]+@`)

	// reBareUserPass 匹配没有 scheme 的 user:pass@host（少见，但凭据本身一样敏感）。
	// 注意不能误伤 scp 风格地址：git@github.com:org/repo.git 没有 "口令@"
	// 这种 "用户:口令@" 结构，因此不会被它命中。
	reBareUserPass = regexp.MustCompile(`([A-Za-z0-9._%+\-]+):[^@\s/'\"]+@`)

	// reAnyURL 用于在自由文本（主要是 git stderr）里挑出 URL 片段。
	reAnyURL = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s'"<>]+`)
)

// RedactURL 脱敏 URL 中的凭据：userinfo 整体与 token 类查询参数都替换为 ***。
//
// 实现取舍（为什么用字符串替换而不是 net/url）：
//  1. RepoURL 未必是规范 URL——可能是 scp 风格 git@host:org/repo.git、本地路径
//     或 Windows 盘符路径，url.Parse 会失败或改写形态，导致日志与真实配置对不上；
//  2. url.URL.String() 会重新编码查询串（顺序、百分号转义都变），
//     脱敏结果反而更难与配置逐字比对。
//
// 该函数对任意输入都不 panic、不做 TrimSpace（保持与原值逐字可比），
// 且是幂等的：重复脱敏结果不变；无凭据的 URL 原样返回。
//
// 该函数导出供服务层的审计/日志复用。
func RedactURL(raw string) string {
	if raw == "" {
		return raw
	}
	out := reSecretQuery.ReplaceAllString(raw, "${1}"+redacted)
	out = reUserInfo.ReplaceAllString(out, "${1}"+redacted+"@")
	out = reBareUserPass.ReplaceAllString(out, redacted+"@")
	return out
}

// redactText 对自由文本（git stderr）做脱敏。
//
// 先按 URL 片段逐个 RedactURL（覆盖 "unable to access 'https://user:tok@h/x.git/'"），
// 再用查询参数规则兜底，避免 stderr 把 URL 拆行/截断时漏掉 ?token=。
func redactText(s string) string {
	if s == "" {
		return s
	}
	out := reAnyURL.ReplaceAllStringFunc(s, RedactURL)
	out = reSecretQuery.ReplaceAllString(out, "${1}"+redacted)
	return out
}

// redactArgs 脱敏命令行参数，供 debug 日志使用（args 里可能含带令牌的 RepoURL）。
func redactArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, redactText(a))
	}
	return out
}
