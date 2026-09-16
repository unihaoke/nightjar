package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"middleware-ops/internal/config"
)

// RuleEngineName 是规则引擎（最终降级层）的名称。
const RuleEngineName = "rule_engine"

// Defaults 从配置派生护栏参数。
//
// 所有护栏共享同一份参数来源，避免默认值在多处漂移。
func Defaults(cfg *config.Config) GuardrailDefaults {
	g := cfg.Guardrail
	return GuardrailDefaults{
		InputTokenBudget:         g.InputTokenBudget,
		OutputTokenBudget:        g.OutputTokenBudget,
		LogContextLines:          g.LogContextLines,
		MaxLogSources:            g.MaxLogSources,
		MaxCodeFiles:             g.MaxCodeFiles,
		CodeLinesPerFile:         g.CodeLinesPerFile,
		MaxSteps:                 g.MaxSteps,
		LoopRepeatThreshold:      g.LoopRepeatThreshold,
		LLMRetry:                 g.LLMRetry,
		CacheTTL:                 g.CacheTTL,
		DailyTokenQuota:          g.DailyTokenQuota,
		PerUserDailyTokenQuota:   g.PerUserQuota,
		MaxConcurrency:           g.MaxConcurrency,
		VectorReferenceThreshold: g.VectorReferenceThreshold,
		SQLDefaultLimit:          g.SQLDefaultLimit,
		SQLMaxLimit:              g.SQLMaxLimit,
		SQLTableAllowlist:        g.SQLTableAllow,
	}
}

// GuardrailDefaults 是六道护栏的统一参数载体。
type GuardrailDefaults struct {
	// ① 上下文预算（5.2）
	InputTokenBudget  int
	OutputTokenBudget int
	LogContextLines   int
	MaxLogSources     int
	MaxCodeFiles      int
	CodeLinesPerFile  int
	// ② 防死循环（5.3）
	MaxSteps            int
	LoopRepeatThreshold int
	LLMRetry            int
	// ⑥ 成本治理（5.7）
	CacheTTL                 time.Duration
	DailyTokenQuota          int64
	PerUserDailyTokenQuota   int64
	MaxConcurrency           int
	VectorReferenceThreshold float64
	// ④ 权限隔离中的 SQL 规则校验（5.5）
	SQLDefaultLimit   int
	SQLMaxLimit       int
	SQLTableAllowlist []string
}

// Timeouts 从配置派生超时链（5.4）。
func Timeouts(cfg *config.Config, provider string) config.TimeoutConfig {
	var t config.TimeoutConfig
	switch provider {
	case "third_party":
		t = cfg.AIEngine.ThirdParty.Timeout
	default:
		t = cfg.AIEngine.SelfHosted.Timeout
	}
	if t.Connect <= 0 {
		t.Connect = 5 * time.Second
	}
	if t.FirstByte <= 0 {
		t.FirstByte = 15 * time.Second
	}
	if t.Total <= 0 {
		t.Total = 60 * time.Second
	}
	if t.ToolCall <= 0 {
		t.ToolCall = 10 * time.Second
	}
	if t.TaskDeadline <= 0 {
		t.TaskDeadline = 2 * time.Minute
	}
	return t
}

// Keys 返回当前生效的引擎与护栏配置摘要，用于启动日志与 /api/system/overview。
func Keys(cfg *config.Config) (strategy string, providers []string) {
	providers = make([]string, 0, 2)
	if cfg.AIEngine.ThirdParty.Enabled && strings.TrimSpace(cfg.AIEngine.ThirdParty.APIKey) != "" {
		providers = append(providers, providerLabel("third_party", cfg.AIEngine.ThirdParty))
	}
	if cfg.AIEngine.SelfHosted.Enabled {
		providers = append(providers, providerLabel("self_hosted", cfg.AIEngine.SelfHosted))
	}
	if len(providers) == 0 {
		providers = append(providers, RuleEngineName)
	}
	return cfg.AIEngine.Strategy, providers
}

// providerLabel 生成不含密钥的提供方描述。
func providerLabel(name string, p config.ProviderConfig) string {
	host := p.BaseURL
	if host == "" {
		host = "local"
	}
	return fmt.Sprintf("%s(%s/%s)", name, p.Kind, host)
}

// RedactConfig 返回配置的脱敏 JSON，避免密钥进入日志或接口响应（6.3）。
func RedactConfig(cfg *config.Config) string {
	masked := map[string]any{
		"strategy":                cfg.AIEngine.Strategy,
		"third_party_enabled":     cfg.AIEngine.ThirdParty.Enabled,
		"third_party_model":       cfg.AIEngine.ThirdParty.Model,
		"third_party_key_set":     cfg.AIEngine.ThirdParty.APIKey != "",
		"self_hosted_enabled":     cfg.AIEngine.SelfHosted.Enabled,
		"self_hosted_kind":        cfg.AIEngine.SelfHosted.Kind,
		"self_hosted_model":       cfg.AIEngine.SelfHosted.Model,
		"database_host":           cfg.Database.Host,
		"database_name":           cfg.Database.Name,
		"redis_addr":              cfg.Redis.Addr,
		"prometheus_base_url":     cfg.Prometheus.BaseURL,
		"master_key_source":       masterKeySource(cfg),
		"outbound_whitelist":      cfg.Security.OutboundWhitelist,
		"input_token_budget":      cfg.Guardrail.InputTokenBudget,
		"output_token_budget":     cfg.Guardrail.OutputTokenBudget,
		"max_concurrency":         cfg.Guardrail.MaxConcurrency,
		"daily_token_quota":       cfg.Guardrail.DailyTokenQuota,
		"vector_native_available": VectorNativeAvailable(),
	}
	b, err := json.Marshal(masked)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// masterKeySource 报告主密钥来源，不泄露密钥内容。
func masterKeySource(cfg *config.Config) string {
	if cfg.Security.MasterKey != "" {
		return "env/config"
	}
	if _, err := os.Stat(cfg.Security.MasterKeyFile); err == nil {
		return "file:" + cfg.Security.MasterKeyFile
	}
	return "generated"
}
