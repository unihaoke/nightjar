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
//
// Factory **自身实现 Engine 接口**（方法转发到当前引擎）。为什么这么做：
// 各 service 在装配期就把 engine.Engine 存进了自己的字段（诊断/告警/大盘/代码分析…），
// 若 Factory 只提供 Engine() 快照，那么管理员在「AI 设置」里点保存后重建出的新引擎
// 只能被工厂自己看到，既有的服务仍然抓着旧引擎——换了 api_key 也不生效，除非把所有
// 持有者都换一遍指针。让工厂成为稳定的门面、内部引擎用 RWMutex 保护后原子替换，
// 各个服务持有的指针就永远指向「当前生效的那个引擎」。
type Factory struct {
	cfg       *config.Config
	onDegrade func(reason string, from string)

	// mu 保护 engine / notes：Reload 会整块替换，读方（每次 Chat/Name/Status）要能拿到一致快照。
	mu     sync.RWMutex
	engine Engine
	notes  []string
}

// 编译期断言：Factory 必须满足 Engine 接口（deps.Engine 直接持有它）。
var _ Engine = (*Factory)(nil)

// NewFactory 构造引擎工厂。
func NewFactory(opt FactoryOptions) *Factory {
	return &Factory{cfg: opt.Config, onDegrade: opt.OnDegrade}
}

// Engine 返回当前引擎实例（惰性构建，首次调用时装配）。
func (f *Factory) Engine() Engine { return f.current() }

// current 读取当前引擎，未构建时按当前配置构建一次。
//
// 双检锁：热路径（每次推理）只取一次读锁，不阻塞并发推理；构建只在首次或 Reload 后发生。
func (f *Factory) current() Engine {
	f.mu.RLock()
	eng := f.engine
	f.mu.RUnlock()
	if eng != nil {
		return eng
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.engine == nil {
		f.engine, f.notes = f.build()
	}
	return f.engine
}

// Reload 按当前 cfg.AIEngine 重新装配引擎并原子替换。
//
// 调用方（设置服务）会先把新配置写进 cfg.AIEngine，再调本方法：这样「配置来源」始终只有
// 一份（内存里的 cfg），工厂不需要另存一套设置，也不会出现「工厂里的配置和 cfg 不一致」。
func (f *Factory) Reload() {
	eng, notes := f.build()
	f.mu.Lock()
	f.engine = eng
	f.notes = notes
	f.mu.Unlock()
}

// Notes 返回最近一次构建的降级说明，用于启动日志与系统概览。
//
// 先确保已构建：旧实现里 Notes 依赖调用方先调 Engine()，一旦有人直接读 Notes
// （例如 /api/system/info）就会拿到空列表，看起来像「没有任何降级提示」。
func (f *Factory) Notes() []string {
	f.current()
	f.mu.RLock()
	defer f.mu.RUnlock()
	return append([]string(nil), f.notes...)
}

// Name 转发到当前引擎。
func (f *Factory) Name() string { return f.current().Name() }

// Chat 转发到当前引擎。
func (f *Factory) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	return f.current().Chat(ctx, req)
}

// ChatStream 转发到当前引擎。
func (f *Factory) ChatStream(ctx context.Context, req ChatRequest) (Stream, error) {
	return f.current().ChatStream(ctx, req)
}

// Embed 转发到当前引擎。
func (f *Factory) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f.current().Embed(ctx, texts)
}

// Available 转发到当前引擎。
func (f *Factory) Available() bool { return f.current().Available() }

// Status 转发到当前引擎。
func (f *Factory) Status() Status { return f.current().Status() }

// build 依据策略装配引擎链，并返回本次构建产生的降级说明。
func (f *Factory) build() (Engine, []string) {
	cfg := f.cfg.AIEngine
	notes := make([]string, 0, 3)
	rule := NewRuleEngine("no LLM provider configured")

	third, thirdNote := f.newProvider("third_party", cfg.ThirdParty)
	self, selfNote := f.newProvider("self_hosted", cfg.SelfHosted)
	if thirdNote != "" {
		notes = append(notes, thirdNote)
	}
	if selfNote != "" {
		notes = append(notes, selfNote)
	}

	switch Strategy(cfg.Strategy) {
	case StrategyThirdParty:
		if third != nil {
			return third, notes
		}
		notes = append(notes, "third_party 策略下提供方不可用，已降级为规则引擎")
		return rule, notes
	case StrategySelfHosted:
		if self != nil {
			return self, notes
		}
		notes = append(notes, "self_hosted 策略下提供方不可用，已降级为规则引擎")
		return rule, notes
	default: // hybrid
		switch {
		case third != nil && self != nil:
			return NewHybrid(HybridOptions{Primary: third, Secondary: self, Rule: rule, OnDegrade: f.onDegrade}), notes
		case third != nil:
			return NewHybrid(HybridOptions{Primary: third, Rule: rule, OnDegrade: f.onDegrade}), notes
		case self != nil:
			return NewHybrid(HybridOptions{Primary: self, Rule: rule, OnDegrade: f.onDegrade}), notes
		default:
			notes = append(notes, "未启用任何 LLM 提供方，AI 诊断使用规则引擎（半自动结论）")
			return rule, notes
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
