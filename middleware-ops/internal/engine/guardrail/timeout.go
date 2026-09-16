package guardrail

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Timeout 是护栏③：超时与降级链。
//
// 规则（5.4）：
//   - 工具调用 10s 超时；超时放弃该数据源，继续使用已有上下文并标注缺失维度；
//   - 整任务 DAG 超时 2min；
//   - 降级链：第三方连续失败 2 次 / P95 超阈值 → 本地 LLM → 规则引擎 + 知识库检索。
type Timeout struct {
	toolTimeout  time.Duration
	taskDeadline time.Duration
	// maxConcurrency 限制诊断并发，避免打爆外部 API 配额（5.7）。
	maxConcurrency int
	sem            chan struct{}
	mu             sync.Mutex
	// missing 记录因超时被放弃的数据源维度。
	missing []Truncation
}

// NewTimeout 构造超时护栏。
func NewTimeout(toolTimeout, taskDeadline time.Duration, maxConcurrency int) *Timeout {
	if toolTimeout <= 0 {
		toolTimeout = 10 * time.Second
	}
	if taskDeadline <= 0 {
		taskDeadline = 2 * time.Minute
	}
	if maxConcurrency <= 0 {
		maxConcurrency = 4
	}
	return &Timeout{
		toolTimeout:    toolTimeout,
		taskDeadline:   taskDeadline,
		maxConcurrency: maxConcurrency,
		sem:            make(chan struct{}, maxConcurrency),
	}
}

// TaskContext 为整任务施加 DAG 超时。
func (t *Timeout) TaskContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, t.taskDeadline)
}

// Acquire 获取并发额度；ctx 取消时返回错误（队列满时不排队等待过久）。
func (t *Timeout) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case t.sem <- struct{}{}:
		return func() { <-t.sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("acquire concurrency slot: %w", ctx.Err())
	case <-time.After(5 * time.Second):
		return nil, errors.New("诊断并发已满，请稍后重试")
	}
}

// Run 在工具级超时内执行 fn。
//
// 超时不会中断整体任务：调用方收到 ok=false 后应继续使用已有上下文，
// 并把 dimension 记入缺失维度（保证用户知道哪些数据没采集到）。
func (t *Timeout) Run(ctx context.Context, dimension string, fn func(ctx context.Context) error) (ok bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, t.toolTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		defer func() {
			// 工具实现可能 panic（例如第三方 SDK），此处兜底避免打挂进程。
			if r := recover(); r != nil {
				done <- fmt.Errorf("tool panic: %v", r)
			}
		}()
		done <- fn(cctx)
	}()

	select {
	case err := <-done:
		if err != nil {
			t.markMissing(dimension, "工具调用失败："+err.Error())
			return false, err
		}
		return true, nil
	case <-cctx.Done():
		t.markMissing(dimension, "工具调用超时（> "+t.toolTimeout.String()+"），已放弃该数据源")
		return false, fmt.Errorf("%w: %s", ctx.Err(), dimension)
	}
}

// markMissing 记录缺失维度。
func (t *Timeout) markMissing(dimension, reason string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.missing = append(t.missing, Truncation{
		Dimension: dimension,
		Reason:    reason,
		Kept:      0,
		Dropped:   1,
	})
}

// Missing 返回缺失维度列表。
func (t *Timeout) Missing() []Truncation {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Truncation, len(t.missing))
	copy(out, t.missing)
	return out
}

// RetryLLM 按指数退避重试 LLM 调用（5.3：LLM 失败可重试 2 次）。
func (t *Timeout) RetryLLM(ctx context.Context, attempts int, fn func(ctx context.Context) error) error {
	if attempts <= 0 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// 指数退避：200ms, 400ms, 800ms...
		backoff := time.Duration(200*(1<<i)) * time.Millisecond
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return lastErr
}
