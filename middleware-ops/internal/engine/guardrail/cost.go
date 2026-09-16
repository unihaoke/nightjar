package guardrail

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"middleware-ops/internal/utils"
)

// Cost 是护栏⑥：成本治理。
//
// 规则（5.7）：
//   - 确定性缓存：同实例 + 同问题签名 24h 内直接返回（不耗 token）；
//   - 按次 + 按日预算（用户/团队维度），接近上限告警；
//   - 并发 ≤4 防打爆配额（由 Timeout 护栏的并发额度承接）；
//   - API key 配额监控：剩余量 / 日消耗，异常突增自动熔断。
type Cost struct {
	mu sync.Mutex
	// dailyTokens 全局日消耗（跨用户）。
	dailyTokens int64
	// userTokens 按用户的日消耗。
	userTokens map[int64]int64
	// userCalls 按用户的日调用次数。
	userCalls map[int64]int
	// day 为当前统计日期（UTC），跨天自动清零。
	day string

	dailyQuota   int64
	perUserQuota int64
	// spikeFactor 为突增判定系数：单次消耗 > 日均单次消耗 × 该系数即视为异常。
	spikeFactor float64
	// tripped 为熔断标记（异常突增或超预算）。
	tripped  bool
	tripNote string
}

// NewCost 构造成本护栏。
func NewCost(dailyQuota, perUserQuota int64) *Cost {
	return &Cost{
		userTokens:   make(map[int64]int64),
		userCalls:    make(map[int64]int),
		day:          utils.DayKey(time.Now()),
		dailyQuota:   dailyQuota,
		perUserQuota: perUserQuota,
		spikeFactor:  8,
	}
}

// Check 在调用前预检预算；返回错误表示应拒绝本次调用（不消耗 token）。
func (c *Cost) Check(userID int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked()
	if c.tripped {
		return fmt.Errorf("token 预算熔断中：%s", c.tripNote)
	}
	if c.dailyQuota > 0 && c.dailyTokens >= c.dailyQuota {
		c.tripped = true
		c.tripNote = fmt.Sprintf("平台日预算 %d tokens 已用尽", c.dailyQuota)
		return fmt.Errorf("%s", c.tripNote)
	}
	if c.perUserQuota > 0 && c.userTokens[userID] >= c.perUserQuota {
		return fmt.Errorf("用户日预算 %d tokens 已用尽，请次日重试或联系管理员", c.perUserQuota)
	}
	return nil
}

// Commit 记录一次实际消耗。
//
// 返回 reachedRatio 表示最小维度（用户或平台）的预算使用率，
// 调用方可在 >0.8 时发出接近上限告警。
func (c *Cost) Commit(userID int64, usage int) (reachedRatio float64, warning string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked()

	c.dailyTokens += int64(usage)
	c.userTokens[userID] += int64(usage)
	c.userCalls[userID]++

	avg := int64(0)
	if c.userCalls[userID] > 0 {
		avg = c.userTokens[userID] / int64(c.userCalls[userID])
	}
	// 异常突增熔断：单次消耗远高于该用户历史均值。
	if avg > 0 && float64(usage) > float64(avg)*c.spikeFactor && usage > 4000 {
		c.tripped = true
		c.tripNote = fmt.Sprintf("检测到 token 消耗异常突增（本次 %d，均值 %d），已熔断", usage, avg)
		return 1, c.tripNote
	}

	ratios := make([]float64, 0, 2)
	if c.dailyQuota > 0 {
		ratios = append(ratios, float64(c.dailyTokens)/float64(c.dailyQuota))
	}
	if c.perUserQuota > 0 {
		ratios = append(ratios, float64(c.userTokens[userID])/float64(c.perUserQuota))
	}
	for _, r := range ratios {
		if r > reachedRatio {
			reachedRatio = r
		}
	}
	if reachedRatio >= 0.8 {
		warning = fmt.Sprintf("Token 预算使用率已达 %.0f%%，请关注成本", reachedRatio*100)
	}
	return reachedRatio, warning
}

// Snapshot 返回成本快照，供 /api/system/overview 展示。
func (c *Cost) Snapshot(userID int64) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollDayLocked()
	return map[string]any{
		"day":          c.day,
		"daily_tokens": c.dailyTokens,
		"daily_quota":  c.dailyQuota,
		"user_tokens":  c.userTokens[userID],
		"user_quota":   c.perUserQuota,
		"user_calls":   c.userCalls[userID],
		"tripped":      c.tripped,
		"trip_note":    c.tripNote,
	}
}

// Tripped 报告熔断状态。
func (c *Cost) Tripped() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tripped
}

// Reset 手动解除熔断（管理员操作）。
func (c *Cost) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tripped = false
	c.tripNote = ""
}

// rollDayLocked 跨天清零（调用方需持锁）。
func (c *Cost) rollDayLocked() {
	today := utils.DayKey(time.Now())
	if today == c.day {
		return
	}
	c.day = today
	c.dailyTokens = 0
	c.userTokens = make(map[int64]int64)
	c.userCalls = make(map[int64]int)
	c.tripped = false
	c.tripNote = ""
}

// CacheKey 计算确定性缓存键：同实例 + 同问题签名。
//
// 问题签名只保留「意图相关」内容：去空白、统一小写、剔除标点与句末语气词，
// 使「Redis 为什么变慢了？」与「redis 为什么变慢」命中同一缓存，避免重复消耗 token。
func CacheKey(instanceID int64, question string) string {
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r', '?', '？', '!', '！', '.', '。', ',', '，', '、', ':', '：',
			'的', '了', '呢', '吗', '吧', '呀', '啊':
			return -1
		}
		return r
	}, strings.ToLower(strings.TrimSpace(question)))
	return fmt.Sprintf("ai:diag:%d:%s", instanceID, utils.SHA256Hex(normalized)[:16])
}

// PriceTokens 依据单价换算成本（元）。
func PriceTokens(tokens int, pricePerKToken float64) float64 {
	if tokens <= 0 || pricePerKToken <= 0 {
		return 0
	}
	return float64(tokens) / 1000 * pricePerKToken
}
