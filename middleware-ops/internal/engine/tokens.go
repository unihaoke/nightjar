package engine

import (
	"strings"
	"unicode"
)

// EstimateTokens 估算文本 token 数。
//
// 诊断场景只需要「预算量级」而非精确计费值：
// 中日韩字符按 1 token/字，其余按 4 字符/token 估算，与主流分词器误差 <20%。
func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	cjk, other := 0, 0
	for _, r := range text {
		if isCJK(r) {
			cjk++
			continue
		}
		other++
	}
	tokens := cjk + (other+3)/4
	if tokens == 0 && len(text) > 0 {
		tokens = 1
	}
	return tokens
}

// EstimateMessagesTokens 估算消息集合的 token 总数（含角色开销）。
func EstimateMessagesTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += EstimateTokens(m.Content) + 4
	}
	return total
}

// EstimateUsage 在引擎未返回 usage 时兜底估算。
func EstimateUsage(messages []Message, completion string) Usage {
	prompt := EstimateMessagesTokens(messages)
	out := EstimateTokens(completion)
	return Usage{PromptTokens: prompt, CompletionTokens: out, TotalTokens: prompt + out}
}

// isCJK 判断是否为中日韩文字。
func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// Fill 用重复字符串填充到指定 rune 长度，用于本地确定性嵌入。
func Fill(seed string, size int) []float32 {
	out := make([]float32, size)
	runes := []rune(seed)
	if len(runes) == 0 {
		return out
	}
	for i := 0; i < size; i++ {
		out[i] = float32(runes[i%len(runes)]) / 65535.0
	}
	return out
}

// LooksLikeJSON 判断文本是否以 JSON 结构开头，用于质量护栏的快解析。
func LooksLikeJSON(s string) bool {
	trimmed := strings.TrimSpace(s)
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}
