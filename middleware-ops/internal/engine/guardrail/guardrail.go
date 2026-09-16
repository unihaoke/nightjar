// Package guardrail 实现设计文档第五章的六道 AI 工程护栏。
//
// 六道护栏全部在 engine 层实现，与业务代码解耦：
//
//	① budget.go      上下文预算（5.2）
//	② loop_guard.go  防死循环（5.3）
//	③ timeout.go     超时与降级（5.4）
//	④ permscope.go   权限隔离与 SQL 规则校验（5.5）
//	⑤ quality.go     质量护栏（5.6）
//	⑥ cost.go        成本治理（5.7）
package guardrail

import (
	"time"

	"middleware-ops/internal/config"
)

// Defaults 是六道护栏的统一参数载体（由配置派生）。
type Defaults struct {
	// ① 上下文预算
	InputTokenBudget  int
	OutputTokenBudget int
	// 采集配额：错误前后 N 行 × 最多 M 个来源；代码 Top-K 文件 × ≤L 行。
	LogContextLines  int
	MaxLogSources    int
	MaxCodeFiles     int
	CodeLinesPerFile int

	// ② 防死循环
	MaxSteps            int
	LoopRepeatThreshold int
	LLMRetry            int

	// ③ 超时
	ConnectTimeout   time.Duration
	FirstByteTimeout time.Duration

	// ⑥ 成本治理
	CacheTTL                 time.Duration
	DailyTokenQuota          int64
	PerUserDailyTokenQuota   int64
	MaxConcurrency           int
	VectorReferenceThreshold float64

	// ④ SQL 规则校验
	SQLDefaultLimit   int
	SQLMaxLimit       int
	SQLTableAllowlist []string
}

// NewDefaults 从配置派生护栏参数。
func NewDefaults(cfg *config.Config) Defaults {
	g := cfg.Guardrail
	third := cfg.AIEngine.ThirdParty.Timeout
	return Defaults{
		InputTokenBudget:         g.InputTokenBudget,
		OutputTokenBudget:        g.OutputTokenBudget,
		LogContextLines:          g.LogContextLines,
		MaxLogSources:            g.MaxLogSources,
		MaxCodeFiles:             g.MaxCodeFiles,
		CodeLinesPerFile:         g.CodeLinesPerFile,
		MaxSteps:                 g.MaxSteps,
		LoopRepeatThreshold:      g.LoopRepeatThreshold,
		LLMRetry:                 g.LLMRetry,
		ConnectTimeout:           third.Connect,
		FirstByteTimeout:         third.FirstByte,
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
