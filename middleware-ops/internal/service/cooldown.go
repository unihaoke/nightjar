package service

import (
	"context"
	"encoding/json"
	"time"

	"middleware-ops/internal/pkg/cache"
)

// cooldownTracker 用 Redis 记录"某个指纹最近一次外发的时间"，实现冷却期。
//
// 为什么抽出来共用：指标告警（AlertService）与日志告警（LogAlertService）都需要
// "一段时间内不要重复打扰人"这件事，语义完全一样。此前只有指标侧有实现，
// 如果日志侧再写一份，两边早晚会在 TTL、key 格式、失败处理上分叉——
// 使用者看到的就是"同一个平台里两种冷却行为"。
//
// 约定（与既有指标告警保持一致，改动需同时评估两侧）：
//   - key：<prefix><指纹>，value：最近一次外发时间的 JSON；
//   - TTL = 冷却时长 × 2（留一倍余量：冷却期到了还要能读到最后一次时间，
//     否则键先过期、冷却判定就失效了）；
//   - 冷却分钟数 <= 0 或没有 Redis 时视为"不冷却"（永不抑制），
//     这是有意的降级：宁可多打扰几次，也不要因为 Redis 抖动把告警全吞掉。
type cooldownTracker struct {
	store  cache.Store
	prefix string
}

// newCooldownTracker 构造冷却追踪器（prefix 例如 "alert:cd:" / "logalert:cd:"）。
func newCooldownTracker(store cache.Store, prefix string) cooldownTracker {
	return cooldownTracker{store: store, prefix: prefix}
}

// inCooldown 判断指纹是否处于冷却期。
func (c cooldownTracker) inCooldown(ctx context.Context, fingerprint string, cooldownMinutes int, now time.Time) (bool, error) {
	if cooldownMinutes <= 0 || c.store == nil {
		return false, nil
	}
	raw, err := c.store.Get(ctx, c.prefix+fingerprint)
	if err != nil {
		// 键不存在是正常路径（第一次告警）；其它错误（Redis 抖动）按"不冷却"处理并交由调用方记录，
		// 绝不能在读失败时判成"冷却中"——那等于把告警静默掉。
		if err == cache.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	var last time.Time
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return false, nil
	}
	return now.Sub(last) < time.Duration(cooldownMinutes)*time.Minute, nil
}

// markSent 记录最近一次外发时间（同时刷新 TTL）。
func (c cooldownTracker) markSent(ctx context.Context, fingerprint string, now time.Time, cooldownMinutes int) error {
	if c.store == nil {
		return nil
	}
	payload, err := json.Marshal(now)
	if err != nil {
		return err
	}
	ttl := time.Duration(cooldownMinutes) * time.Minute * 2
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	return c.store.Set(ctx, c.prefix+fingerprint, string(payload), ttl)
}

// clear 清掉冷却记录（用于"人工重新分析/立即提醒"这类要打破冷却的动作）。
func (c cooldownTracker) clear(ctx context.Context, fingerprint string) error {
	if c.store == nil {
		return nil
	}
	return c.store.Del(ctx, c.prefix+fingerprint)
}
