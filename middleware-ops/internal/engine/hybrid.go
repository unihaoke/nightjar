package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// hybrid 实现降级链：首选 → 备选 → 规则引擎（5.4）。
//
// 降级触发条件：首选连续失败达阈值（熔断打开）、首选不可用、或调用返回错误。
type hybrid struct {
	primaryName string
	primary     Engine
	secondary   Engine
	rule        Engine
	onDegrade   func(reason string, from string)
	mu          sync.Mutex
	lastDegrade string
	degraded    bool
}

// HybridOptions 构造 hybrid 引擎的参数。
type HybridOptions struct {
	Primary   Engine
	Secondary Engine
	Rule      Engine
	// OnDegrade 用于在降级发生时写审计与日志。
	OnDegrade func(reason string, from string)
}

// NewHybrid 构造混合引擎。
//
// primary 为空时直接返回 secondary；均不可用时不构造本类型（由工厂返回规则引擎）。
func NewHybrid(opt HybridOptions) Engine {
	if opt.Rule == nil {
		opt.Rule = NewRuleEngine("rule engine fallback")
	}
	return &hybrid{
		primaryName: nameOf(opt.Primary, "none"),
		primary:     opt.Primary,
		secondary:   opt.Secondary,
		rule:        opt.Rule,
		onDegrade:   opt.OnDegrade,
	}
}

// Name 返回引擎名称。
func (h *hybrid) Name() string { return string(StrategyHybrid) }

// Available 混合引擎在有任一层可用时即可用。
func (h *hybrid) Available() bool { return true }

// Status 返回整体健康状态。
func (h *hybrid) Status() Status {
	status := Status{Name: string(StrategyHybrid), Available: true}
	parts := make([]string, 0, 3)
	for _, e := range []Engine{h.primary, h.secondary, h.rule} {
		if e == nil {
			continue
		}
		st := e.Status()
		parts = append(parts, fmt.Sprintf("%s(avail=%t,circuit=%t)", st.Name, st.Available, st.CircuitOpen))
		if st.Name == h.primaryName && (st.CircuitOpen || !st.Available) {
			status.Degraded = true
			status.CircuitOpen = true
			status.LastError = st.LastError
			status.LastFailureAt = st.LastFailureAt
			status.ConsecutiveFails = st.ConsecutiveFails
		}
		// 合规判定取"链上是否含外部引擎"，而不是"本次实际用了哪一层"：
		// 主层熔断时会自动降级到备层，请求内容可能已经发出去过（也可能正要发往备层），
		// 这种"取决于运行时状态"的判断不能作为合规依据——只要链上有外部引擎就按外部处理。
		if st.External {
			status.External = true
		}
	}
	h.mu.Lock()
	if h.degraded {
		status.Degraded = true
	}
	h.mu.Unlock()
	status.LastError = fmt.Sprintf("%s | %s", status.LastError, joinNonEmpty(parts, ", "))
	return status
}

// Chat 按降级链执行同步推理。
func (h *hybrid) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if h.primary != nil && h.primary.Available() {
		resp, err := h.primary.Chat(ctx, req)
		if err == nil {
			h.clear()
			return resp, nil
		}
		h.degrade(err, h.primary.Name())
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// 任务级超时不再向下重试，避免整体耗时失控。
			return nil, err
		}
	} else if h.primary != nil {
		h.degrade(ErrUnavailable, h.primary.Name())
	}
	if h.secondary != nil && h.secondary.Available() {
		resp, err := h.secondary.Chat(ctx, req)
		if err == nil {
			return resp, nil
		}
		h.degrade(err, h.secondary.Name())
	}
	return h.rule.Chat(ctx, req)
}

// ChatStream 按降级链执行流式推理。
//
// 流式无法在已输出内容后无缝切换引擎，因此仅在「首个片段之前失败」时降级。
func (h *hybrid) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	chain := make([]Engine, 0, 3)
	if h.primary != nil {
		chain = append(chain, h.primary)
	}
	if h.secondary != nil {
		chain = append(chain, h.secondary)
	}
	chain = append(chain, h.rule)

	var lastErr error
	for _, e := range chain {
		if !e.Available() {
			continue
		}
		stream, err := e.ChatStream(ctx, req)
		if err == nil {
			if e == h.primary {
				h.clear()
			}
			return &failoverStream{inner: stream, engine: e, owner: h, req: req}, nil
		}
		lastErr = err
		h.degrade(err, e.Name())
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = ErrUnavailable
	}
	return nil, lastErr
}

// Embed 生成向量：优先主引擎，失败时退化为本地确定性嵌入。
func (h *hybrid) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	for _, e := range []Engine{h.primary, h.secondary} {
		if e == nil || !e.Available() {
			continue
		}
		vecs, err := e.Embed(ctx, texts)
		if err == nil && len(vecs) == len(texts) {
			return vecs, nil
		}
	}
	return h.rule.Embed(ctx, texts)
}

// degrade 记录降级事件。
func (h *hybrid) degrade(err error, from string) {
	reason := "unknown"
	if err != nil {
		reason = err.Error()
	}
	h.mu.Lock()
	h.degraded = true
	h.lastDegrade = fmt.Sprintf("%s -> fallback: %s", from, reason)
	h.mu.Unlock()
	if h.onDegrade != nil {
		h.onDegrade(reason, from)
	}
}

// clear 在首选恢复后清除降级标记。
func (h *hybrid) clear() {
	h.mu.Lock()
	h.degraded = false
	h.mu.Unlock()
}

// failoverStream 在流首个片段失败时切换到下一层引擎。
type failoverStream struct {
	owner    *hybrid
	inner    Stream
	engine   Engine
	req      ChatRequest
	started  bool
	switched bool
}

// Recv 返回片段，必要时执行一次首段失败切换。
func (f *failoverStream) Recv() (Chunk, error) {
	chunk, err := f.inner.Recv()
	if chunk.Delta != "" && !chunk.Done {
		f.started = true
	}
	if err == nil || errors.Is(err, io.EOF) {
		return chunk, err
	}
	if f.started || f.switched {
		return chunk, err
	}
	// 首个片段前失败：改用规则引擎，保证用户总能看到结论。
	f.switched = true
	f.owner.degrade(err, f.engine.Name())
	_ = f.inner.Close()
	fallback, ferr := f.owner.rule.ChatStream(context.Background(), f.req)
	if ferr != nil {
		return Chunk{Done: true, Err: err}, err
	}
	f.inner = fallback
	return f.inner.Recv()
}

// Close 释放底层流。
func (f *failoverStream) Close() error { return f.inner.Close() }

// nameOf 返回引擎名称。
func nameOf(e Engine, fallback string) string {
	if e == nil {
		return fallback
	}
	return e.Name()
}

// joinNonEmpty 连接非空字符串。
func joinNonEmpty(items []string, sep string) string {
	out := ""
	for _, item := range items {
		if item == "" {
			continue
		}
		if out != "" {
			out += sep
		}
		out += item
	}
	return out
}

// DegradeEvent 描述一次降级，供审计与指标记录。
type DegradeEvent struct {
	From   string    `json:"from"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}
