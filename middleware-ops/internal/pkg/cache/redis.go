package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisStore 是 Store 的 Redis 实现。
type redisStore struct {
	client *redis.Client
}

// NewRedisStore 创建 Redis 缓存实现。
func NewRedisStore(client *redis.Client) Store {
	return &redisStore{client: client}
}

// Kind 返回实现类型。
func (s *redisStore) Kind() string { return "redis" }

// Get 读取键值。
func (s *redisStore) Get(ctx context.Context, key string) (string, error) {
	val, err := s.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis get %s: %w", key, err)
	}
	return val, nil
}

// Set 写入键值。
func (s *redisStore) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	if err := s.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("redis set %s: %w", key, err)
	}
	return nil
}

// Del 删除键。
func (s *redisStore) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return s.client.Del(ctx, keys...).Err()
}

// Exists 判断键是否存在。
func (s *redisStore) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// IncrBy 原子自增。
func (s *redisStore) IncrBy(ctx context.Context, key string, delta int64) (int64, error) {
	return s.client.IncrBy(ctx, key, delta).Result()
}

// Expire 设置过期时间。
func (s *redisStore) Expire(ctx context.Context, key string, ttl time.Duration) error {
	return s.client.Expire(ctx, key, ttl).Err()
}

// Ping 健康检查。
func (s *redisStore) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

// Close 释放连接。
func (s *redisStore) Close() error { return s.client.Close() }

// redisQueue 是基于 Redis Stream 的任务队列（设计文档 2.1）。
//
// 使用消费组保证多实例下任务只被消费一次；未 ACK 的消息可通过
// XAUTOCLAIM 语义重新分配（此处以 Requeue 暴露给上层调度）。
type redisQueue struct {
	client    *redis.Client
	stream    string
	group     string
	consumer  string
	blockTime time.Duration
}

// NewRedisQueue 创建 Redis Stream 队列，并确保消费组存在。
func NewRedisQueue(ctx context.Context, client *redis.Client, stream, group, consumer string, block time.Duration) (Queue, error) {
	if block <= 0 {
		block = 5 * time.Second
	}
	q := &redisQueue{client: client, stream: stream, group: group, consumer: consumer, blockTime: block}
	// 消费组已存在时 Redis 返回 BUSYGROUP，视为成功。
	if err := client.XGroupCreateMkStream(ctx, stream, group, "$").Err(); err != nil &&
		!containsBusyGroup(err) {
		return nil, fmt.Errorf("create consumer group: %w", err)
	}
	return q, nil
}

// Kind 返回实现类型。
func (q *redisQueue) Kind() string { return "redis_stream" }

// Enqueue 入队。
func (q *redisQueue) Enqueue(ctx context.Context, task Task) error {
	values := map[string]any{
		"id":          task.ID,
		"type":        task.Type,
		"enqueued_at": task.EnqueuedAt.Format(time.RFC3339Nano),
	}
	for k, v := range task.Data {
		values["data."+k] = fmt.Sprintf("%v", v)
	}
	return q.client.XAdd(ctx, &redis.XAddArgs{Stream: q.stream, Values: values}).Err()
}

// Dequeue 出队（阻塞读取）。
func (q *redisQueue) Dequeue(ctx context.Context) (Task, error) {
	res, err := q.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    q.group,
		Consumer: q.consumer,
		Streams:  []string{q.stream, ">"},
		Count:    1,
		Block:    q.blockTime,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("xreadgroup: %w", err)
	}
	if len(res) == 0 || len(res[0].Messages) == 0 {
		return Task{}, ErrNotFound
	}
	msg := res[0].Messages[0]
	task := Task{Data: map[string]any{}}
	if v, ok := msg.Values["id"].(string); ok {
		task.ID = v
	}
	if v, ok := msg.Values["type"].(string); ok {
		task.Type = v
	}
	if v, ok := msg.Values["enqueued_at"].(string); ok {
		if ts, err := time.Parse(time.RFC3339Nano, v); err == nil {
			task.EnqueuedAt = ts
		}
	}
	for k, v := range msg.Values {
		if len(k) > 5 && k[:5] == "data." {
			task.Data[k[5:]] = v
		}
	}
	task.Data["_stream_id"] = msg.ID
	return task, nil
}

// Ack 确认任务完成。
func (q *redisQueue) Ack(ctx context.Context, task Task) error {
	streamID, _ := task.Data["_stream_id"].(string)
	if streamID == "" {
		return nil
	}
	return q.client.XAck(ctx, q.stream, q.group, streamID).Err()
}

// Len 返回待处理任务数。
func (q *redisQueue) Len(ctx context.Context) (int64, error) {
	info, err := q.client.XPending(ctx, q.stream, q.group).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return 0, nil
		}
		return 0, err
	}
	total := int64(0)
	if info != nil {
		total = info.Count
	}
	length, err := q.client.XLen(ctx, q.stream).Result()
	if err != nil {
		return total, nil
	}
	return total + length, nil
}

// Close 为无状态实现，直接返回 nil。
func (q *redisQueue) Close() error { return nil }

// containsBusyGroup 判断消费组已存在错误。
func containsBusyGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "BUSYGROUP")
}
