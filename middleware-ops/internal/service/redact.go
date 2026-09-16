package service

import (
	"regexp"
	"strings"

	"middleware-ops/internal/config"
)

// Redactor 负责出网内容脱敏（6.5）。
//
// 脱敏范围：IP、用户名（Windows/Unix 家目录）、手机号、请求 ID、
// 以及部署方额外配置的敏感词正则。
type Redactor struct {
	rules []*regexp.Regexp
	// maxLines 为代码片段行数上限（≤200 行/文件）。
	maxLines int
}

// 内置脱敏规则。
var (
	reIPv4      = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d+)?\b`)
	reIPv6      = regexp.MustCompile(`\b(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}\b`)
	reWinPath   = regexp.MustCompile(`[A-Za-z]:\\[^\s"']*`)
	reUnixHome  = regexp.MustCompile(`/(?:home|Users|root)/[^\s"':]+`)
	rePhone     = regexp.MustCompile(`\b1[3-9]\d{9}\b`)
	reRequestID = regexp.MustCompile(`(?i)\b(?:request[-_]?id|trace[-_]?id|span[-_]?id|x-request-id)\b\s*[:=]\s*[^\s,;)]+`)
	reUUID      = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	reEmail     = regexp.MustCompile(`\b[\w.+-]+@[\w-]+\.[\w.]+\b`)
	rePassword  = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api[-_]?key)\b\s*[:=]\s*[^\s,;)]+`)
)

// NewRedactor 构造脱敏器。
func NewRedactor(cfg *config.SecurityConfig) *Redactor {
	rules := []*regexp.Regexp{rePassword, reRequestID, rePhone, reIPv4, reIPv6, reWinPath, reUnixHome, reEmail, reUUID}
	for _, pattern := range cfg.SensitivePatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			continue
		}
		rules = append(rules, re)
	}
	maxLines := cfg.CodeSnippetMaxLines
	if maxLines <= 0 {
		maxLines = 200
	}
	return &Redactor{rules: rules, maxLines: maxLines}
}

// keyedPatterns 中的正则匹配「键=值」结构，脱敏时保留键名便于模型理解上下文。
var keyedPatterns = map[string]bool{
	rePassword.String():  true,
	reRequestID.String(): true,
}

// Redact 对文本执行脱敏。
//
// 对凭据/请求 ID 这类「键值对」保留键名（如 password=<redacted>），
// 对 IP、手机号、邮箱、路径等直接整体替换，避免残留可识别信息。
func (r *Redactor) Redact(text string) string {
	out := text
	for _, re := range r.rules {
		keyed := keyedPatterns[re.String()]
		out = re.ReplaceAllStringFunc(out, func(match string) string {
			if !keyed {
				return "<redacted>"
			}
			if idx := strings.IndexAny(match, ":="); idx > 0 {
				return match[:idx+1] + "<redacted>"
			}
			return "<redacted>"
		})
	}
	return out
}

// TruncateCode 按行数上限截断代码片段，并返回是否发生截断。
func (r *Redactor) TruncateCode(snippet string) (string, bool) {
	lines := strings.Split(snippet, "\n")
	if len(lines) <= r.maxLines {
		return snippet, false
	}
	return strings.Join(lines[:r.maxLines], "\n"), true
}

// MaxLines 返回代码片段行数上限。
func (r *Redactor) MaxLines() int { return r.maxLines }
