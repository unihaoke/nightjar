// Package engine 定义 AI 引擎抽象。
//
// 设计要点（设计文档 3.4 / 5.1）：
//   - 引擎只做「单轮诊断」，不迭代、不自主决策、不调用执行类工具；
//   - 第三方 / 自研 / 混合三种策略通过统一接口切换，运行期可降级；
//   - 业务代码只依赖本包的接口，护栏在 engine/guardrail 内实现，与业务解耦。
package engine

import (
	"context"
	"errors"
	"io"
	"time"
)

// Strategy 是引擎策略。
type Strategy string

// 支持的策略取值。
const (
	StrategyThirdParty Strategy = "third_party"
	StrategySelfHosted Strategy = "self_hosted"
	StrategyHybrid     Strategy = "hybrid"
)

// Role 是对话角色。
type Role string

// 支持的对话角色。
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message 是一条对话消息。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
}

// ChatRequest 是一次推理请求。
type ChatRequest struct {
	Messages []Message
	// MaxTokens 为输出上限，受输出预算约束。
	MaxTokens int
	// Temperature 为采样温度，诊断场景默认 0.2 以保证稳定性。
	Temperature float64
	// JSONMode 表示要求模型输出严格 JSON（质量护栏解析依赖）。
	JSONMode bool
	// Metadata 供审计与评测追踪（不发送给模型）。
	Metadata map[string]string
}

// Usage 记录 token 消耗，用于成本治理。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse 是一次推理结果。
type ChatResponse struct {
	Content string
	Usage   Usage
	Model   string
	// FinishReason 取值 stop / length / error。
	FinishReason string
	Duration     time.Duration
}

// Chunk 是流式输出片段。
type Chunk struct {
	Delta string
	Usage *Usage
	// Done 为 true 表示流结束。
	Done bool
	// Err 非空表示流中途失败（此时已输出内容仍然保留）。
	Err error
}

// Stream 是流式输出通道。
type Stream interface {
	// Recv 返回下一个片段；通道关闭后返回 io.EOF。
	Recv() (Chunk, error)
	// Close 释放底层连接。
	Close() error
}

// Engine 是统一 AI 引擎接口。
type Engine interface {
	// Name 返回引擎名称（third_party / self_hosted / hybrid / rule_engine）。
	Name() string
	// Chat 执行一次同步推理。
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream 执行一次流式推理。
	ChatStream(ctx context.Context, req ChatRequest) (Stream, error)
	// Embed 生成向量；返回空切片表示退化到本地确定性嵌入。
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Available 报告引擎当前是否可用（含熔断状态）。
	Available() bool
	// Status 返回引擎健康状态描述，用于 diagnosis 的 engine_status 字段。
	Status() Status
}

// Status 描述引擎健康状态。
type Status struct {
	Name             string    `json:"name"`
	Available        bool      `json:"available"`
	Degraded         bool      `json:"degraded"`
	CircuitOpen      bool      `json:"circuit_open"`
	ConsecutiveFails int       `json:"consecutive_fails"`
	LastError        string    `json:"last_error"`
	LastFailureAt    time.Time `json:"last_failure_at"`
	// External 表示本引擎可能把请求内容发往**组织外部**（第三方 AI 服务）。
	//
	// 这是**合规判定**的依据，不是健康信息：代码分析会把私有代码片段放进提示词，
	// 因此"这份内容会不会离开组织"必须能由引擎自己声明，而不是让调用方去猜配置。
	// 判定规则（保守优先）：
	//   - 第三方提供方（third_party）→ true；
	//   - 自建提供方（self_hosted，如内网 vLLM/Ollama）→ false；
	//   - 混合链（hybrid）→ 链上**任意**一层是外部即 true（降级可能落到那一层）；
	//   - 规则引擎（rule_engine）→ false（全部在进程内完成，不出网）。
	External bool `json:"external"`
}

// 引擎层错误。
var (
	// ErrUnavailable 表示引擎不可用（未启用 / 熔断打开）。
	ErrUnavailable = errors.New("engine unavailable")
	// ErrTimeout 表示推理超时。
	ErrTimeout = errors.New("engine timeout")
	// ErrBudgetExceeded 表示超出 token 预算。
	ErrBudgetExceeded = errors.New("token budget exceeded")
	// ErrInvalidResponse 表示模型输出无法解析为结构化报告。
	ErrInvalidResponse = errors.New("invalid model response")
)

// StreamReader 把 io.Reader 适配为 Stream 的通用实现，供各协议适配器复用。
type StreamReader interface {
	io.Reader
	io.Closer
}
