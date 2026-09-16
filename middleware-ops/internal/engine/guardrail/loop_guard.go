package guardrail

import (
	"fmt"
	"sync"

	"middleware-ops/internal/utils"
)

// LoopGuard 是护栏②：防死循环 / 任务失控。
//
// 规则（5.3）：
//   - 工具调用步数上限（默认 8 步）；
//   - 同一工具 + 同一参数状态指纹重复 N 次（默认 2 次）即终止；
//   - 工具白名单 + 每步决策说明；
//   - 达到上限时输出「已中止 + 中间结论 + 建议人工介入」，绝不静默失败。
//
// 一期为「单轮诊断 + 工具预采集」，不存在自主循环，本护栏用于约束
// 采集编排（多数据源串行采集）并为二期 Agent 复用。
type LoopGuard struct {
	mu          sync.Mutex
	maxSteps    int
	threshold   int
	steps       int
	seen        map[string]int
	whitelist   map[string]struct{}
	decisions   []Decision
	aborted     bool
	abortReason string
}

// Decision 记录一次「为什么需要该工具」的决策卡。
type Decision struct {
	Step   int    `json:"step"`
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
	// Fingerprint 为该次调用的状态指纹。
	Fingerprint string `json:"fingerprint"`
	// Repeated 表示该指纹此前出现过几次。
	Repeated int `json:"repeated"`
}

// NewLoopGuard 构造防死循环护栏。whitelist 为空表示不限制工具集合。
func NewLoopGuard(maxSteps, threshold int, whitelist []string) *LoopGuard {
	if maxSteps <= 0 {
		maxSteps = 8
	}
	if threshold <= 0 {
		threshold = 2
	}
	wl := make(map[string]struct{}, len(whitelist))
	for _, name := range whitelist {
		wl[name] = struct{}{}
	}
	return &LoopGuard{
		maxSteps:  maxSteps,
		threshold: threshold,
		seen:      make(map[string]int),
		whitelist: wl,
	}
}

// Fingerprint 计算「工具 + 参数」的状态指纹。
func Fingerprint(tool string, args any) string {
	return utils.Fingerprint(tool, fmt.Sprintf("%v", args))
}

// Allow 判定本次工具调用是否放行。
//
// 返回 allow=false 时 reason 说明中止原因，同时 Guard 进入 aborted 状态，
// 后续调用一律拒绝（调用方应输出中间结论与人工介入建议）。
func (g *LoopGuard) Allow(tool string, args any, reason string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.aborted {
		return false, g.abortReason
	}
	if len(g.whitelist) > 0 {
		if _, ok := g.whitelist[tool]; !ok {
			g.aborted = true
			g.abortReason = fmt.Sprintf("工具 %s 不在白名单内，已中止任务", tool)
			return false, g.abortReason
		}
	}
	if g.steps >= g.maxSteps {
		g.aborted = true
		g.abortReason = fmt.Sprintf("已达最大步数 %d，任务中止，请人工介入", g.maxSteps)
		return false, g.abortReason
	}

	fp := Fingerprint(tool, args)
	repeated := g.seen[fp]
	if repeated >= g.threshold {
		g.aborted = true
		g.abortReason = fmt.Sprintf("检测到重复调用（工具=%s 指纹=%s 重复 %d 次），任务中止", tool, fp[:8], repeated)
		return false, g.abortReason
	}

	g.steps++
	g.seen[fp] = repeated + 1
	g.decisions = append(g.decisions, Decision{
		Step:        g.steps,
		Tool:        tool,
		Reason:      reason,
		Fingerprint: fp,
		Repeated:    repeated,
	})
	return true, ""
}

// Steps 返回已执行步数。
func (g *LoopGuard) Steps() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.steps
}

// Aborted 报告任务是否已中止。
func (g *LoopGuard) Aborted() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aborted
}

// AbortReason 返回中止原因。
func (g *LoopGuard) AbortReason() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.abortReason
}

// Decisions 返回决策卡列表（可写入审计，实现「每步说明为什么需要该工具」）。
func (g *LoopGuard) Decisions() []Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Decision, len(g.decisions))
	copy(out, g.decisions)
	return out
}

// MarkFailure 记录一次工具失败。
//
// 设计约定（5.3）：工具调用失败**不自动重试**，仅记录原因。
func (g *LoopGuard) MarkFailure(tool, reason string) Decision {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := Decision{
		Step:        g.steps,
		Tool:        tool,
		Reason:      "失败未重试：" + reason,
		Fingerprint: Fingerprint(tool, reason),
	}
	g.decisions = append(g.decisions, d)
	return d
}
