package cache

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// memoryStore 是 Store 的进程内实现。
//
// 用途：单机部署与 Redis 不可用时的降级，保证「核心链路故障不影响 AI 诊断可用性」
// （设计文档 10. 可用性目标）。
type memoryStore struct {
	mu    sync.RWMutex
	items map[string]memoryItem
	stop  chan struct{}
	once  sync.Once
}

type memoryItem struct {
	value     string
	expiresAt time.Time
}

// NewMemoryStore 创建内存缓存并启动惰性清理协程。
func NewMemoryStore() Store {
	s := &memoryStore{
		items: make(map[string]memoryItem),
		stop:  make(chan struct{}),
	}
	go s.gcLoop()
	return s
}

// Kind 返回实现类型。
func (s *memoryStore) Kind() string { return "memory" }

// Get 读取键值。
func (s *memoryStore) Get(_ context.Context, key string) (string, error) {
	s.mu.RLock()
	item, ok := s.items[key]
	s.mu.RUnlock()
	if !ok {
		return "", ErrNotFound
	}
	if !item.expiresAt.IsZero() && time.Now().After(item.expiresAt) {
		s.mu.Lock()
		delete(s.items, key)
		s.mu.Unlock()
		return "", ErrNotFound
	}
	return item.value, nil
}

// Set 写入键值。
func (s *memoryStore) Set(_ context.Context, key, value string, ttl time.Duration) error {
	item := memoryItem{value: value}
	if ttl > 0 {
		item.expiresAt = time.Now().Add(ttl)
	}
	s.mu.Lock()
	s.items[key] = item
	s.mu.Unlock()
	return nil
}

// Del 删除键。
func (s *memoryStore) Del(_ context.Context, keys ...string) error {
	s.mu.Lock()
	for _, key := range keys {
		delete(s.items, key)
	}
	s.mu.Unlock()
	return nil
}

// Exists 判断键是否存在。
func (s *memoryStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Get(ctx, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

// IncrBy 原子自增。
func (s *memoryStore) IncrBy(_ context.Context, key string, delta int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[key]
	var current int64
	if item.value != "" {
		if _, err := fmtSscan(item.value, &current); err != nil {
			current = 0
		}
	}
	current += delta
	item.value = fmtInt(current)
	s.items[key] = item
	return current, nil
}

// Expire 设置过期时间。
func (s *memoryStore) Expire(_ context.Context, key string, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[key]
	if !ok {
		return ErrNotFound
	}
	if ttl > 0 {
		item.expiresAt = time.Now().Add(ttl)
	} else {
		item.expiresAt = time.Time{}
	}
	s.items[key] = item
	return nil
}

// Ping 健康检查。
func (s *memoryStore) Ping(context.Context) error { return nil }

// Close 停止清理协程。
func (s *memoryStore) Close() error {
	s.once.Do(func() { close(s.stop) })
	return nil
}

// gcLoop 周期性清理过期键。
func (s *memoryStore) gcLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			now := time.Now()
			s.mu.Lock()
			for key, item := range s.items {
				if !item.expiresAt.IsZero() && now.After(item.expiresAt) {
					delete(s.items, key)
				}
			}
			s.mu.Unlock()
		}
	}
}

// memoryQueue 是 Queue 的进程内实现（FIFO，支持多消费者）。
type memoryQueue struct {
	mu       sync.Mutex
	tasks    []Task
	inflight map[string]Task
	notify   chan struct{}
	stop     chan struct{}
	once     sync.Once
	// maxSize 防内存被无限占满。
	maxSize int
}

// NewMemoryQueue 创建内存队列并启动通知协程。
func NewMemoryQueue(maxSize int) Queue {
	if maxSize <= 0 {
		maxSize = 10000
	}
	return &memoryQueue{
		inflight: make(map[string]Task),
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
		maxSize:  maxSize,
	}
}

// Kind 返回实现类型。
func (q *memoryQueue) Kind() string { return "memory" }

// Enqueue 入队。
func (q *memoryQueue) Enqueue(_ context.Context, task Task) error {
	q.mu.Lock()
	if len(q.tasks)+len(q.inflight) >= q.maxSize {
		q.mu.Unlock()
		return errors.New("队列已满")
	}
	q.tasks = append(q.tasks, task)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return nil
}

// Dequeue 出队。
//
// 内存实现不阻塞：无任务时立即返回 ErrNotFound，由调用方按周期重试
// （与 Redis Stream 的 XREADGROUP BLOCK 语义对齐由上层 poller 处理）。
func (q *memoryQueue) Dequeue(_ context.Context) (Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.tasks) == 0 {
		return Task{}, ErrNotFound
	}
	task := q.tasks[0]
	q.tasks = q.tasks[1:]
	q.inflight[task.ID] = task
	return task, nil
}

// Ack 确认任务完成。
func (q *memoryQueue) Ack(_ context.Context, task Task) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.inflight, task.ID)
	return nil
}

// Len 返回待处理任务数。
func (q *memoryQueue) Len(context.Context) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return int64(len(q.tasks) + len(q.inflight)), nil
}

// Close 停止队列。
func (q *memoryQueue) Close() error {
	q.once.Do(func() { close(q.stop) })
	return nil
}

// Requeue 把超时未确认的任务重新入队（内存实现用于故障重试）。
func (q *memoryQueue) Requeue(ctx context.Context, olderThan time.Duration) (int, error) {
	q.mu.Lock()
	expired := make([]Task, 0, len(q.inflight))
	now := time.Now()
	for id, task := range q.inflight {
		if now.Sub(task.EnqueuedAt) > olderThan {
			expired = append(expired, task)
			delete(q.inflight, id)
		}
	}
	q.mu.Unlock()
	sort.Slice(expired, func(i, j int) bool { return expired[i].EnqueuedAt.Before(expired[j].EnqueuedAt) })
	for _, task := range expired {
		if err := q.Enqueue(ctx, task); err != nil {
			return 0, err
		}
	}
	return len(expired), nil
}
