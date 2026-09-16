package utils

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// NewRequestID 生成请求追踪 ID。
func NewRequestID() string {
	raw := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw)
}

// HashChain 计算审计哈希链节点（6.4）。
//
// hash_self = SHA256(hash_prev | 规范化内容)，任一字段被篡改都会导致链校验失败。
func HashChain(prev, payload string) string {
	sum := sha256.Sum256([]byte(prev + "|" + payload))
	return hex.EncodeToString(sum[:])
}

// SHA256Hex 返回字符串的 SHA-256 十六进制摘要。
func SHA256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Fingerprint 计算错误指纹的稳定摘要。
func Fingerprint(parts ...string) string {
	return SHA256Hex(strings.Join(parts, "\u0001"))[:32]
}

var variablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`),
	regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?\b`),
	regexp.MustCompile(`\b\d{1,3}(\.\d{1,3}){3}(:\d+)?\b`),
	regexp.MustCompile(`\b[A-Za-z]:\\[^\s"']+`),
	regexp.MustCompile(`(?i)\b(?:request[-_]?id|trace[-_]?id|span[-_]?id)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?m)\b0x[0-9a-fA-F]+\b`),
	// 3 位以上的数字（ID、行号、耗时、计数等）统一模板化。
	regexp.MustCompile(`\b\d{3,}\b`),
	regexp.MustCompile(`"[^"]{0,64}"`),
}

// NormalizeError 把错误消息模板化：去除时间戳/IP/路径/长数字/引号内容等变量，
// 以便同一类异常归并为同一指纹（见 4.8.2）。
func NormalizeError(message string) string {
	out := message
	for _, re := range variablePatterns {
		out = re.ReplaceAllString(out, "<var>")
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > 512 {
		out = out[:512]
	}
	return out
}

// Truncate 按 rune 截断字符串，用于上下文预算裁剪。
func Truncate(s string, limit int) (string, bool) {
	if limit <= 0 {
		return "", len(s) > 0
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}
	return string(runes[:limit]), true
}

// TruncateLines 按行截断，返回结果与被截断行数。
func TruncateLines(s string, maxLines int) (string, int) {
	if maxLines <= 0 {
		return "", strings.Count(s, "\n") + 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return s, 0
	}
	return strings.Join(lines[:maxLines], "\n"), len(lines) - maxLines
}

// DayKey 返回 UTC 日期键，用于按日预算与快照命名。
func DayKey(t time.Time) string { return t.UTC().Format("2006-01-02") }
