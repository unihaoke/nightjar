// Package cache 提供缓存、去重指纹与任务队列的统一抽象。
//
// 两种实现：
//   - redis 实现（生产，支撑多实例部署与 Redis Stream 任务队列）；
//   - 内存实现（单机 / Redis 不可用时的降级，保证平台仍可运行）。
package cache

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound 表示键不存在。
var ErrNotFound = errors.New("cache: key not found")

// Store 是缓存能力接口。
type Store interface {
	// Get 读取字符串值。
	Get(ctx context.Context, key string) (string, error)
	// Set 写入字符串值，ttl 为 0 表示不过期。
	Set(ctx context.Context, key string, value string, ttl time.Duration) error
	// Del 删除键。
	Del(ctx context.Context, keys ...string) error
	// Exists 判断键是否存在。
	Exists(ctx context.Context, key string) (bool, error)
	// IncrBy 原子自增，返回自增后的值。
	IncrBy(ctx context.Context, key string, delta int64) (int64, error)
	// Expire 设置过期时间。
	Expire(ctx context.Context, key string, ttl time.Duration) error
	// Ping 健康检查。
	Ping(ctx context.Context) error
	// Kind 返回实现类型（redis / memory）。
	Kind() string
	// Close 释放资源。
	Close() error
}

// Task 是一条异步任务（写入队列的最小单元）。
type Task struct {
	ID   string         `json:"id"`
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
	// EnqueuedAt 为入队时间，用于观测排队时长。
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// Queue 是任务队列接口（设计文档 3.4：所有 AI 任务入队列，单实例并发 ≤4）。
type Queue interface {
	// Enqueue 入队。
	Enqueue(ctx context.Context, task Task) error
	// Dequeue 出队；无任务时返回 ErrNotFound（调用方应休眠重试）。
	Dequeue(ctx context.Context) (Task, error)
	// Ack 确认任务完成。
	Ack(ctx context.Context, task Task) error
	// Len 返回待处理任务数。
	Len(ctx context.Context) (int64, error)
	// Kind 返回实现类型（redis_stream / memory）。
	Kind() string
	// Close 释放资源。
	Close() error
}

// Fingerprint 是告警去重指纹存储（4.4 规则级收敛）。
type Fingerprint struct {
	// LastSentAt 为最后一次发送时间，用于冷却期判断。
	LastSentAt time.Time `json:"last_sent_at"`
	// Count 为窗口内合并次数。
	Count int `json:"count"`
	// FirstSeenAt 为窗口起点。
	FirstSeenAt time.Time `json:"first_seen_at"`
}
