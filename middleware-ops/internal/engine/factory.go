package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"middleware-ops/internal/config"
)

// FactoryOptions 构造引擎所需的依赖。
type FactoryOptions struct {
	Config *config.Config
	// OnDegrade 在降级链触发时回调（写审计与指标）。
	OnDegrade func(reason string, from string)
}

// Factory 按配置创建引擎，并实现「配置不可即降级」的启动语义。
type Factory struct {
	cfg       *config.Config
	onDegrade func(reason string, from string)

	once   sync.Once
	engine Engine
	notes  []string
}

// NewFactory 构造引擎工厂。
func NewFactory(opt FactoryOptions) *Factory {
	return &Factory{cfg: opt.Config, onDegrade: opt.OnDegrade}
}

// Engine 返回（惰性创建的）引擎实例。
func (f *Factory) Engine() Engine {
	f.once.Do(func() { f.engine = f.build() })
	return f.engine
}

// Notes 返回构建过程中的降级说明，用于启动日志与系统概览。
func (f *Factory) Notes() []string { return f.notes }

// build 依据策略装配引擎链。
func (f *Factory) build() Engine {
	cfg := f.cfg.AIEngine
	rule := NewRuleEngine("no LLM provider configured")

	third, thirdNote := f.newProvider("third_party", cfg.ThirdParty)
	self, selfNote := f.newProvider("self_hosted", cfg.SelfHosted)
	if thirdNote != "" {
		f.notes = append(f.notes, thirdNote)
	}
	if selfNote != "" {
		f.notes = append(f.notes, selfNote)
	}

	switch Strategy(cfg.Strategy) {
	case StrategyThirdParty:
		if third != nil {
			return third
		}
		f.notes = append(f.notes, "third_party 策略下提供方不可用，已降级为规则引擎")
		return rule
	case StrategySelfHosted:
		if self != nil {
			return self
		}
		f.notes = append(f.notes, "self_hosted 策略下提供方不可用，已降级为规则引擎")
		return rule
	default: // hybrid
		switch {
		case third != nil && self != nil:
			return NewHybrid(HybridOptions{Primary: third, Secondary: self, Rule: rule, OnDegrade: f.onDegrade})
		case third != nil:
			return NewHybrid(HybridOptions{Primary: third, Rule: rule, OnDegrade: f.onDegrade})
		case self != nil:
			return NewHybrid(HybridOptions{Primary: self, Rule: rule, OnDegrade: f.onDegrade})
		default:
			f.notes = append(f.notes, "未启用任何 LLM 提供方，AI 诊断使用规则引擎（半自动结论）")
			return rule
		}
	}
}

// newProvider 按配置构造提供方；返回 nil 表示不可用并给出原因。
func (f *Factory) newProvider(name string, p config.ProviderConfig) (Engine, string) {
	if !p.Enabled {
		return nil, fmt.Sprintf("%s 未启用", name)
	}
	timeout := Timeouts(f.cfg, name)
	kind := strings.ToLower(p.Kind)
	if kind == "" {
		kind = "openai"
	}
	// kind=mock 或 base_url 为空时，使用进程内确定性引擎，保证离线可用。
	if kind == "mock" || strings.TrimSpace(p.BaseURL) == "" {
		if kind != "mock" {
			return NewRuleEngine(name + " 未配置 base_url"), fmt.Sprintf("%s 缺少 base_url，改用规则引擎", name)
		}
		return NewRuleEngine(name + " 使用内置 mock 引擎"), fmt.Sprintf("%s 使用 mock 引擎（离线演示）", name)
	}
	provider, err := NewHTTPProvider(HTTPProviderOptions{
		Name:             name,
		Kind:             kind,
		BaseURL:          p.BaseURL,
		APIKey:           p.APIKey,
		Model:            p.Model,
		MaxTokens:        p.MaxTokens,
		PricePerKToken:   p.PricePerKToken,
		Timeout:          timeout,
		FailureThreshold: f.cfg.AIEngine.Fallback.FailureThreshold,
		OpenDuration:     f.cfg.AIEngine.Fallback.OpenDuration,
	})
	if err != nil {
		return nil, fmt.Sprintf("%s 初始化失败: %v", name, err)
	}
	return provider, ""
}

// VectorNativeAvailable 报告当前构建是否启用 pgvector 原生向量类型。
func VectorNativeAvailable() bool { return vectorNative }

// vectorNative 由构建标签文件注入，避免 engine 包反向依赖 db 包。
var vectorNative bool

// SetVectorNative 由 main 注入向量能力标志。
func SetVectorNative(v bool) { vectorNative = v }

// ChatOnce 是便捷方法：对单条用户问题执行一次同步推理。
func ChatOnce(ctx context.Context, e Engine, system, user string, maxTokens int) (*ChatResponse, error) {
	return e.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: RoleSystem, Content: system},
			{Role: RoleUser, Content: user},
		},
		MaxTokens:   maxTokens,
		Temperature: 0.2,
	})
}
