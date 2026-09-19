package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"middleware-ops/internal/config"
)

// httpProvider 实现 OpenAI 兼容协议的第三方 / 自托管引擎。
//
// 覆盖 OpenAI、DeepSeek、vLLM、Ollama(/v1)、以及 Anthropic 的 OpenAI 兼容网关。
// 关键护栏落在这一层：连接超时、首字节超时、总时长超时（5.4）。
type httpProvider struct {
	name     string
	kind     string
	baseURL  string
	apiKey   string
	model    string
	maxToken int
	price    float64
	timeout  config.TimeoutConfig
	client   *http.Client
	// external 表示该提供方在组织外部（第三方 AI）：由工厂按提供方名字设置，
	// 供合规判定使用（见 Status.External）。它不影响任何网络行为，只是一个事实声明。
	external bool

	mu        sync.Mutex
	fails     int
	lastErr   string
	lastFail  time.Time
	openUntil time.Time
	// failureThreshold / openDuration 为熔断参数（5.4 降级链）。
	failureThreshold int
	openDuration     time.Duration
}

// HTTPProviderOptions 构造 httpProvider 的参数。
type HTTPProviderOptions struct {
	Name             string
	Kind             string
	BaseURL          string
	APIKey           string
	Model            string
	MaxTokens        int
	PricePerKToken   float64
	Timeout          config.TimeoutConfig
	FailureThreshold int
	OpenDuration     time.Duration
	// External 见 httpProvider.external：由工厂按提供方名字（third_party / self_hosted）设置。
	External bool
}

// NewHTTPProvider 构造可用的 HTTP 引擎；baseURL 为空时返回错误，由工厂决定降级。
func NewHTTPProvider(opt HTTPProviderOptions) (Engine, error) {
	if strings.TrimSpace(opt.BaseURL) == "" {
		return nil, errors.New("engine: base_url is empty")
	}
	if opt.MaxTokens <= 0 {
		opt.MaxTokens = 2048
	}
	if opt.FailureThreshold <= 0 {
		opt.FailureThreshold = 2
	}
	if opt.OpenDuration <= 0 {
		opt.OpenDuration = time.Minute
	}
	if opt.Timeout.Connect <= 0 {
		opt.Timeout.Connect = 5 * time.Second
	}
	if opt.Timeout.Total <= 0 {
		opt.Timeout.Total = 60 * time.Second
	}
	dialer := &net.Dialer{Timeout: opt.Timeout.Connect, KeepAlive: 30 * time.Second}
	return &httpProvider{
		name:             opt.Name,
		kind:             opt.Kind,
		baseURL:          strings.TrimRight(opt.BaseURL, "/"),
		apiKey:           opt.APIKey,
		model:            opt.Model,
		maxToken:         opt.MaxTokens,
		price:            opt.PricePerKToken,
		timeout:          opt.Timeout,
		failureThreshold: opt.FailureThreshold,
		openDuration:     opt.OpenDuration,
		external:         opt.External,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext:           dialer.DialContext,
				MaxIdleConns:          50,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   opt.Timeout.Connect,
				ResponseHeaderTimeout: opt.Timeout.FirstByte,
				ExpectContinueTimeout: time.Second,
			},
		},
	}, nil
}

// Name 返回引擎名称。
func (p *httpProvider) Name() string { return p.name }

// Available 报告引擎当前是否可用（含熔断状态）。
func (p *httpProvider) Available() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.openUntil.IsZero() {
		return true
	}
	// 熔断打开期内不可用；到期后自动半开探测。
	return time.Now().After(p.openUntil)
}

// Status 返回引擎健康快照。
func (p *httpProvider) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	open := !p.openUntil.IsZero() && time.Now().Before(p.openUntil)
	return Status{
		Name:             p.name,
		Available:        !open,
		Degraded:         p.fails > 0,
		CircuitOpen:      open,
		ConsecutiveFails: p.fails,
		LastError:        p.lastErr,
		LastFailureAt:    p.lastFail,
		External:         p.external,
	}
}

// markSuccess 重置连续失败计数。
func (p *httpProvider) markSuccess() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails = 0
	p.lastErr = ""
	p.openUntil = time.Time{}
}

// markFailure 记录失败并在达到阈值时打开熔断。
func (p *httpProvider) markFailure(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fails++
	p.lastErr = err.Error()
	p.lastFail = time.Now()
	if p.fails >= p.failureThreshold {
		p.openUntil = time.Now().Add(p.openDuration)
	}
}

type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Chat 执行一次同步推理。
func (p *httpProvider) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if !p.Available() {
		return nil, fmt.Errorf("%w: %s circuit open", ErrUnavailable, p.name)
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout.Total)
	defer cancel()

	body := p.buildRequest(req, false)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	start := time.Now()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	p.applyHeaders(httpReq)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		p.markFailure(err)
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%w: %s", ErrTimeout, p.name)
		}
		return nil, fmt.Errorf("call %s: %w", p.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		p.markFailure(err)
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 300 {
		p.markFailure(fmt.Errorf("status %d", resp.StatusCode))
		return nil, fmt.Errorf("engine %s status %d: %s", p.name, resp.StatusCode, firstLine(payload))
	}

	var parsed chatCompletionResponse
	if err := json.Unmarshal(payload, &parsed); err != nil {
		p.markFailure(err)
		return nil, fmt.Errorf("%w: decode response: %v", ErrInvalidResponse, err)
	}
	if parsed.Error != nil {
		p.markFailure(errors.New(parsed.Error.Message))
		return nil, fmt.Errorf("engine %s error: %s", p.name, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		p.markFailure(errors.New("empty choices"))
		return nil, fmt.Errorf("%w: empty choices", ErrInvalidResponse)
	}

	p.markSuccess()
	content := parsed.Choices[0].Message.Content
	usage := Usage{
		PromptTokens:     parsed.Usage.PromptTokens,
		CompletionTokens: parsed.Usage.CompletionTokens,
		TotalTokens:      parsed.Usage.TotalTokens,
	}
	if usage.TotalTokens == 0 {
		usage = EstimateUsage(req.Messages, content)
	}
	model := parsed.Model
	if model == "" {
		model = p.model
	}
	return &ChatResponse{
		Content:      content,
		Usage:        usage,
		Model:        model,
		FinishReason: parsed.Choices[0].FinishReason,
		Duration:     time.Since(start),
	}, nil
}

// ChatStream 执行一次流式推理，流式超时按「首字节 + 静默期」重置（5.4）。
func (p *httpProvider) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	if !p.Available() {
		return nil, fmt.Errorf("%w: %s circuit open", ErrUnavailable, p.name)
	}
	body := p.buildRequest(req, true)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// 总时长由任务级 DAG 超时兜底；这里仅放置首字节超时的连接配置。
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	p.applyHeaders(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		p.markFailure(err)
		return nil, fmt.Errorf("stream %s: %w", p.name, err)
	}
	if resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		_ = resp.Body.Close()
		p.markFailure(fmt.Errorf("status %d", resp.StatusCode))
		return nil, fmt.Errorf("engine %s status %d: %s", p.name, resp.StatusCode, firstLine(payload))
	}
	return &sseStream{provider: p, resp: resp, scanner: bufio.NewScanner(resp.Body)}, nil
}

// Embed 调用向量接口；未配置 embedding 模型时返回空结果，由调用方使用本地嵌入。
func (p *httpProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout.Total)
	defer cancel()
	payload, err := json.Marshal(map[string]any{"model": p.model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/embeddings", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	p.applyHeaders(req)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embed status %d", resp.StatusCode)
	}
	var parsed struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode embedding: %w", err)
	}
	out := make([][]float32, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		out = append(out, item.Embedding)
	}
	return out, nil
}

// buildRequest 组装请求体。
func (p *httpProvider) buildRequest(req ChatRequest, stream bool) chatCompletionRequest {
	msgs := make([]chatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, chatMessage{Role: string(m.Role), Content: m.Content})
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 || maxTokens > p.maxToken {
		maxTokens = p.maxToken
	}
	out := chatCompletionRequest{
		Model:       p.model,
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: req.Temperature,
		Stream:      stream,
	}
	if stream {
		out.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	if req.JSONMode {
		out.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	return out
}

// applyHeaders 写入鉴权头（Anthropic 兼容网关可通过 kind 切换）。
func (p *httpProvider) applyHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey == "" {
		return
	}
	switch strings.ToLower(p.kind) {
	case "anthropic":
		req.Header.Set("x-api-key", p.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// sseStream 解析 SSE 流。
type sseStream struct {
	provider *httpProvider
	resp     *http.Response
	scanner  *bufio.Scanner
	usage    *Usage
	done     bool
	closed   bool
}

// Recv 返回下一个片段。
func (s *sseStream) Recv() (Chunk, error) {
	if s.done {
		return Chunk{Done: true, Usage: s.usage}, io.EOF
	}
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			s.done = true
			s.provider.markSuccess()
			return Chunk{Done: true, Usage: s.usage}, nil
		}
		var parsed struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			// 单行解析失败不终止整条流，记录后跳过（避免上游协议微调导致诊断中断）。
			continue
		}
		if parsed.Usage != nil {
			s.usage = &Usage{
				PromptTokens:     parsed.Usage.PromptTokens,
				CompletionTokens: parsed.Usage.CompletionTokens,
				TotalTokens:      parsed.Usage.TotalTokens,
			}
		}
		if len(parsed.Choices) == 0 {
			continue
		}
		delta := parsed.Choices[0].Delta.Content
		if parsed.Choices[0].FinishReason != "" {
			s.done = true
		}
		if delta == "" && !s.done {
			continue
		}
		return Chunk{Delta: delta, Done: s.done, Usage: s.usage}, nil
	}
	if err := s.scanner.Err(); err != nil {
		s.provider.markFailure(err)
		s.done = true
		return Chunk{Done: true, Err: err, Usage: s.usage}, nil
	}
	s.done = true
	s.provider.markSuccess()
	return Chunk{Done: true, Usage: s.usage}, io.EOF
}

// Close 释放响应体。
func (s *sseStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.resp.Body.Close()
}

// firstLine 提取响应体首行，避免错误信息过长。
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
