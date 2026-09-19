package repo

import (
	"regexp"
	"strings"
)

// 本文件实现「仓库地址」与「访问凭据」的分离（INC-031）。
//
// 背景：早期设计要求把令牌直接写进 repo_url（`https://oauth2:<token>@gitlab/…`），于是：
//   - 令牌明文落在数据库列里（备份/只读账号同等敏感）；
//   - 接口与页面把 repo_url 原样回显，任何有「日志告警读」权限的人（含 dev 角色）都能看到令牌；
//   - git 会把它抄进缓存目录的 .git/config，又多一份静态暴露。
//
// 现在把两件事拆开：
//   - 入库的 RepoURL **永远不含凭据**（本文件负责把它剥干净）；
//   - 凭据单独加密保存（见 service 层的 CredentialEncrypted），只在真正执行 git 的那一刻拼回 URL。
//
// 兼容既有数据：读到内嵌凭据的旧 URL 时，服务层会用 SplitCredentials 拆分并就地迁移。

// credentialQueryPrefix 是"查询参数形式凭据"在存储时的前缀约定。
//
// 为什么要有它：凭据并不只有 userinfo 一种形态，还有 `?token=xxx` / `?access_token=xxx`
// 这类查询参数（GitLab、部分自建 Gitea/HTTP 文件服务都用）。两者在 URL 里的位置不同，
// 因此存储时要能区分：以 "?" 开头 ⇒ 查询参数形式（原样追加），否则 ⇒ `user:secret` userinfo 形式。
const credentialQueryPrefix = "?"

var (
	// reCredentialQuery 匹配需要搬走的查询参数（键名表与 redact.go 共用，避免两处清单分叉）。
	reCredentialQuery = regexp.MustCompile(`(?i)[?&](?:` + secretQueryKeys + `)=[^&#\s]+`)

	// reURLUserInfo 只匹配带 scheme 的 userinfo；scp 风格（git@host:path）没有 scheme，
	// 它的 "git@" 是用户名不是凭据，不能剥（剥了就克隆不了）。
	reURLUserInfo = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.\-]*://)([^/@\s]+)@`)
)

// SplitCredentials 把 URL 拆成「干净地址」与「凭据」两部分。
//
// 返回值：
//   - clean     ：不含任何凭据的地址（可直接入库、可直接回显）；
//   - credential：`user:secret` 形式，或 `?token=xxx` 形式（见 credentialQueryPrefix）；无凭据时为空。
//
// 只处理带 scheme 的 URL：scp 风格（`git@github.com:org/repo.git`）的用户名不是凭据，
// 原样返回（那种地址靠 SSH 密钥认证，平台不参与保管密钥）。
func SplitCredentials(raw string) (clean, credential string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}

	// ① 查询参数形式的凭据先摘走（顺序无关；多个都摘）。
	if matches := reCredentialQuery.FindAllString(trimmed, -1); len(matches) > 0 {
		query := make([]string, 0, len(matches))
		for _, m := range matches {
			// m 形如 "?token=xxx" 或 "&token=xxx"：统一成 "?key=value" 便于拼接与阅读。
			key := strings.TrimLeft(m, "?&")
			query = append(query, key)
			trimmed = strings.Replace(trimmed, m, "", 1)
		}
		// 摘掉参数后可能留下 "?&" / "?" / "&" 之类残渣，清理掉。
		trimmed = strings.TrimRight(trimmed, "?&")
		return trimmed, credentialQueryPrefix + strings.Join(query, "&")
	}

	// ② userinfo 形式（带 scheme 才算凭据）。
	if m := reURLUserInfo.FindStringSubmatch(trimmed); m != nil {
		userinfo := m[2]
		clean = m[1] + trimmed[len(m[0]):]
		if strings.Contains(userinfo, ":") {
			return clean, userinfo
		}
		// 只有一段（`https://<token>@host`）：GitLab/GitHub 的只读令牌惯用 `oauth2` 作用户名，
		// 直接当用户名会让令牌被当成用户名发出去、认证失败。这里固定补 `oauth2` 并写进文档。
		return clean, "oauth2:" + userinfo
	}
	return trimmed, ""
}

// SanitizeURL 只返回不含凭据的地址（入库与对外展示都用它）。
func SanitizeURL(raw string) string {
	clean, _ := SplitCredentials(raw)
	return RedactURL(clean)
}

// HasCredential 报告地址里是否内嵌了凭据（用于旧数据迁移与提示）。
func HasCredential(raw string) bool {
	_, credential := SplitCredentials(raw)
	return credential != ""
}

// WithCredential 把凭据拼回干净地址（只在执行 git 的那一刻调用，绝不落库）。
//
// 无法拼接时（例如 scp 风格地址配了令牌）原样返回干净地址：那种组合本来就该用 SSH 密钥，
// 拼一个不存在的 userinfo 只会产生更难懂的认证错误。
func WithCredential(clean, credential string) string {
	clean = strings.TrimSpace(clean)
	credential = strings.TrimSpace(credential)
	if credential == "" || clean == "" {
		return clean
	}
	if strings.HasPrefix(credential, credentialQueryPrefix) {
		sep := "?"
		if strings.Contains(clean, "?") {
			sep = "&"
		}
		return clean + sep + strings.TrimPrefix(credential, credentialQueryPrefix)
	}
	// scp 风格（git@host:org/repo.git）没有 scheme，无法插 userinfo。
	idx := strings.Index(clean, "://")
	if idx < 0 {
		return clean
	}
	return clean[:idx+3] + credential + "@" + clean[idx+3:]
}

// CredentialHasSecret 报告凭据串是否真的带了秘密（用于"是否配置了令牌"的展示）。
func CredentialHasSecret(credential string) bool {
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return false
	}
	if strings.HasPrefix(credential, credentialQueryPrefix) {
		return len(credential) > 1
	}
	// `user:` 这种只有用户名、没有口令的形态不算配置了凭据。
	idx := strings.LastIndex(credential, ":")
	return idx >= 0 && strings.TrimSpace(credential[idx+1:]) != ""
}
