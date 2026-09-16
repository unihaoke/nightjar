package guardrail

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"middleware-ops/internal/apperr"
)

// Scope 是护栏④：权限隔离（防权限溢出）。
//
// 规则（5.5）：
//   - AI 只挂载只读工具集，永无执行权；
//   - 工具层强制按发起人数据权限过滤实例与环境，不依赖 Prompt 约束；
//   - 每次工具调用独立审计（看了哪些实例/文件）。
type Scope struct {
	// UserID / Username 用于审计归属。
	UserID   int64
	Username string
	RoleCode string
	// AllowAllEnv 表示不限环境（管理员）。
	AllowAllEnv bool
	// Environments / Groups 为数据权限白名单，空切片表示不限制该维度。
	Environments []string
	Groups       []string
	// ReadOnly 恒为 true：AI 工具层只读，执行权保留给人。
	ReadOnly bool
}

// Instance 是实例的最小描述，用于数据权限判定。
type Instance struct {
	ID          int64
	Name        string
	MWType      string
	Environment string
	GroupName   string
}

// EnvAllowed 判断环境是否在权限范围内。
func (s Scope) EnvAllowed(env string) bool {
	if s.AllowAllEnv || len(s.Environments) == 0 {
		return true
	}
	for _, item := range s.Environments {
		if item == env {
			return true
		}
	}
	return false
}

// GroupAllowed 判断分组是否在权限范围内。
func (s Scope) GroupAllowed(group string) bool {
	if len(s.Groups) == 0 {
		return true
	}
	for _, item := range s.Groups {
		if item == group {
			return true
		}
	}
	return false
}

// CanRead 判断是否可以读取该实例。
func (s Scope) CanRead(inst Instance) bool {
	return s.EnvAllowed(inst.Environment) && s.GroupAllowed(inst.GroupName)
}

// FilterInstances 过滤出有权限的实例集合（服务端强制执行，不依赖模型）。
func (s Scope) FilterInstances(items []Instance) []Instance {
	out := make([]Instance, 0, len(items))
	for _, inst := range items {
		if s.CanRead(inst) {
			out = append(out, inst)
		}
	}
	return out
}

// Describe 返回权限范围的可读描述，供诊断报告与审计展示。
func (s Scope) Describe() string {
	envs := "全部环境"
	if !s.AllowAllEnv && len(s.Environments) > 0 {
		envs = strings.Join(s.Environments, "/")
	}
	groups := "全部分组"
	if len(s.Groups) > 0 {
		groups = strings.Join(s.Groups, "/")
	}
	return fmt.Sprintf("角色=%s 环境=%s 分组=%s", s.RoleCode, envs, groups)
}

// ToolContext 是工具执行上下文，承载发起人数据权限与审计信息。
type ToolContext struct {
	Context context.Context
	Scope   Scope
	// Instance 为本次诊断的目标实例（已通过 CanRead 校验）。
	Instance Instance
	// Recorder 记录工具调用审计。
	Recorder AuditRecorder
}

// AuditRecorder 记录 AI 工具调用的审计条目（5.5）。
type AuditRecorder interface {
	RecordToolCall(userID int64, username, tool string, detail map[string]any, result string)
}

// ReadOnlyTool 是 AI 可挂载的只读工具。
//
// 接口刻意不提供任何写操作，从类型层面保证 AI 无执行权。
type ReadOnlyTool interface {
	// Name 返回工具名（同时用于白名单校验）。
	Name() string
	// Decision 说明「为什么需要该工具」（决策卡，5.3）。
	Decision() string
	// ReadOnly 恒为 true。
	ReadOnly() bool
	// Run 执行只读采集。
	Run(tc *ToolContext, args map[string]any) (any, error)
}

// ToolRegistry 是只读工具注册表。
//
// 注册时强制校验 ReadOnly()，任何只读性声明不一致的工具都会被拒绝挂载。
type ToolRegistry struct {
	tools map[string]ReadOnlyTool
	order []string
}

// NewToolRegistry 构造注册表。
func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]ReadOnlyTool)}
}

// Mount 挂载只读工具；若工具声明非只读则返回错误（防止权限溢出）。
func (r *ToolRegistry) Mount(tool ReadOnlyTool) error {
	if tool == nil {
		return fmt.Errorf("tool is nil")
	}
	if !tool.ReadOnly() {
		return fmt.Errorf("拒绝挂载工具 %s：AI 工具集必须只读", tool.Name())
	}
	name := tool.Name()
	if name == "" {
		return fmt.Errorf("tool name is empty")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %s already mounted", name)
	}
	r.tools[name] = tool
	r.order = append(r.order, name)
	return nil
}

// Get 返回指定工具。
func (r *ToolRegistry) Get(name string) (ReadOnlyTool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

// Names 返回已挂载工具名（按挂载顺序）。
func (r *ToolRegistry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// Call 执行工具：校验白名单 → 记录决策与审计 → 执行。
func (r *ToolRegistry) Call(tc *ToolContext, name string, args map[string]any) (any, error) {
	tool, ok := r.tools[name]
	if !ok {
		return nil, apperr.Newf(apperr.CodeForbidden, "工具 %s 未挂载（只读白名单）", name)
	}
	if !tool.ReadOnly() {
		// 理论上不可达：挂载阶段已校验。保留为纵深防御。
		return nil, apperr.New(apperr.CodeForbidden, "AI 工具集只读，禁止执行写操作")
	}

	start := time.Now()
	value, err := tool.Run(tc, args)
	result := "success"
	if err != nil {
		result = "failed"
	}
	if tc.Recorder != nil {
		detail := map[string]any{
			"tool":        name,
			"decision":    tool.Decision(),
			"args":        args,
			"instance":    tc.Instance.Name,
			"env":         tc.Instance.Environment,
			"group":       tc.Instance.GroupName,
			"scope":       tc.Scope.Describe(),
			"duration_ms": time.Since(start).Milliseconds(),
		}
		if err != nil {
			detail["error"] = err.Error()
		}
		tc.Recorder.RecordToolCall(tc.Scope.UserID, tc.Scope.Username, name, detail, result)
	}
	return value, err
}

// SQLError 为 SQL 校验失败错误。
type SQLError struct {
	Reason string
}

// Error 实现 error 接口。
func (e *SQLError) Error() string { return "SQL 校验失败: " + e.Reason }

// SQLGuard 是 AI 生成 SQL 的规则校验器（5.5）。
//
// 校验项：只读语句 → 单语句 → 无注释/多语句注入 → 表白名单（可配） →
// 强制 LIMIT（默认 100，上限 1000）。
type SQLGuard struct {
	defaultLimit int
	maxLimit     int
	allowlist    []string
}

// NewSQLGuard 构造 SQL 校验器。
func NewSQLGuard(defaultLimit, maxLimit int, allowlist []string) *SQLGuard {
	if defaultLimit <= 0 {
		defaultLimit = 100
	}
	if maxLimit <= 0 {
		maxLimit = 1000
	}
	normalized := make([]string, 0, len(allowlist))
	for _, t := range allowlist {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			normalized = append(normalized, t)
		}
	}
	return &SQLGuard{defaultLimit: defaultLimit, maxLimit: maxLimit, allowlist: normalized}
}

var (
	// sqlIdentifier 匹配表名（支持 schema.table 与双引号标识）。
	sqlIdentifier = regexp.MustCompile(`(?i)\b(?:from|join)\s+("?[\w$]+"?(?:\s*\.\s*"?[\w$]+"?)?)`)
	// sqlLimit 匹配 LIMIT 子句。
	sqlLimit = regexp.MustCompile(`(?i)\blimit\s+(\d+)`)
	// sqlWriteKeyword 匹配任何写操作或 DDL 关键字。
	sqlWriteKeyword = regexp.MustCompile(`(?i)\b(insert|update|delete|drop|truncate|alter|create|grant|revoke|comment|copy|vacuum|analyze|refresh|call|do|merge|replace|lock|set|reset|show|use)\b`)
	// sqlDangerous 匹配无 WHERE 的高危模式，用于二次确认提示（6.6）。
	sqlDangerous = regexp.MustCompile(`(?i)\b(delete\s+from|update)\b[^;]*?(;|$)`)
)

// sqlSystemTables 是只读查询常见的系统视图，写关键字出现在其列名中时不得误判
// （例如 pg_stat_database 的 xact_commit 与 pg_stat_user_tables 的 n_mod_since_analyze）。
var sqlSystemTables = []string{
	"pg_stat", "pg_catalog", "information_schema", "performance_schema", "mysql.", "sys.",
}

// inSystemQuery 判断 SQL 是否主要面向系统视图。
func inSystemQuery(lower string) bool {
	for _, prefix := range sqlSystemTables {
		if strings.Contains(lower, prefix) {
			return true
		}
	}
	return false
}

// Validate 校验 SQL 并返回安全化后的语句。
//
// 返回值 resolved 为补齐 LIMIT 后的可执行语句；notes 记录所做的自动修正，
// 便于在诊断报告中透明展示。
func (g *SQLGuard) Validate(sql string) (resolved string, notes []string, err error) {
	raw := strings.TrimSpace(sql)
	if raw == "" {
		return "", nil, &SQLError{Reason: "SQL 为空"}
	}
	// 去掉末尾分号后再判断多语句。
	trimmed := strings.TrimRight(raw, "; \t\r\n")
	if strings.Contains(trimmed, ";") {
		return "", nil, &SQLError{Reason: "禁止多语句执行"}
	}
	if strings.Contains(raw, "--") || strings.Contains(raw, "/*") {
		return "", nil, &SQLError{Reason: "禁止 SQL 注释（可能隐藏多语句）"}
	}

	lower := strings.ToLower(trimmed)
	// CTE 场景：WITH ... SELECT 视为只读。
	readOnlyPrefix := strings.HasPrefix(lower, "select") || strings.HasPrefix(lower, "with") ||
		strings.HasPrefix(lower, "explain") || strings.HasPrefix(lower, "table")
	if !readOnlyPrefix {
		return "", nil, &SQLError{Reason: "仅允许 SELECT / WITH / EXPLAIN 只读查询"}
	}
	if loc := sqlWriteKeyword.FindStringIndex(lower); loc != nil && !inSystemQuery(lower) {
		keyword := lower[loc[0]:loc[1]]
		// EXPLAIN 语句中可能包含 analyze 关键字，需要放行合法用法。
		if keyword != "analyze" || !strings.Contains(lower, "explain") {
			return "", nil, &SQLError{Reason: fmt.Sprintf("包含非只读关键字 %q", keyword)}
		}
	}

	tables := g.tables(trimmed)
	if len(g.allowlist) > 0 && len(tables) > 0 {
		for _, t := range tables {
			if !g.allowed(t) {
				return "", nil, &SQLError{Reason: fmt.Sprintf("表 %s 不在白名单内", t)}
			}
		}
	}

	if m := sqlLimit.FindStringSubmatch(trimmed); m != nil {
		var value int
		if _, scanErr := fmt.Sscanf(m[1], "%d", &value); scanErr == nil && value > g.maxLimit {
			notes = append(notes, fmt.Sprintf("LIMIT %d 超过上限，已收敛为 %d", value, g.maxLimit))
			resolved = sqlLimit.ReplaceAllString(trimmed, fmt.Sprintf("LIMIT %d", g.maxLimit))
			return strings.TrimSpace(resolved), notes, nil
		}
		return trimmed, notes, nil
	}

	notes = append(notes, fmt.Sprintf("已自动补加 LIMIT %d（强制只读保护）", g.defaultLimit))
	return fmt.Sprintf("%s LIMIT %d", trimmed, g.defaultLimit), notes, nil
}

// tables 抽取 SQL 中引用的表名。
func (g *SQLGuard) tables(sql string) []string {
	matches := sqlIdentifier.FindAllStringSubmatch(sql, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(m[1], `"`, ""), " ", ""))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// allowed 判断表是否在白名单内（支持 schema.* 通配）。
func (g *SQLGuard) allowed(table string) bool {
	base := table
	if idx := strings.LastIndex(table, "."); idx >= 0 {
		base = table[idx+1:]
	}
	for _, item := range g.allowlist {
		if item == table || item == base {
			return true
		}
		if strings.HasSuffix(item, ".*") && strings.HasPrefix(table, strings.TrimSuffix(item, "*")) {
			return true
		}
	}
	return false
}

// IsHighRisk 判断语句是否属于高危操作（用于 L2 二次确认与审批，6.6）。
func IsHighRisk(statement string) (bool, string) {
	s := strings.ToLower(strings.TrimSpace(statement))
	if s == "" {
		return false, ""
	}
	if m := sqlDangerous.FindString(s); m != "" && !strings.Contains(s, "where") {
		return true, "DELETE/UPDATE 未带 WHERE 条件"
	}
	for _, kw := range []string{"drop ", "truncate ", "flushall", "flushdb", "shutdown", "restart"} {
		if strings.Contains(s, kw) {
			return true, "包含高危关键字 " + strings.TrimSpace(kw)
		}
	}
	return false, ""
}
