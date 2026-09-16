package guardrail

import (
	"strings"
	"testing"

	"middleware-ops/internal/engine"
)

// TestSQLGuardValidate 覆盖 AI 生成 SQL 的只读安全校验（5.5）。
func TestSQLGuardValidate(t *testing.T) {
	g := NewSQLGuard(100, 1000, nil)

	tests := []struct {
		name      string
		sql       string
		wantNote  string
		wantError string
		wantSQL   string
	}{
		{
			name:     "自动补加 LIMIT",
			sql:      "SELECT * FROM orders",
			wantNote: "已自动补加 LIMIT 100",
			wantSQL:  "SELECT * FROM orders LIMIT 100",
		},
		{
			name:    "超过上限收敛为最大值",
			sql:     "SELECT id FROM orders LIMIT 5000",
			wantSQL: "SELECT id FROM orders LIMIT 1000",
		},
		{
			name:    "保留合法 LIMIT",
			sql:     "SELECT id FROM orders LIMIT 20",
			wantSQL: "SELECT id FROM orders LIMIT 20",
		},
		{
			name:      "拒绝 DELETE",
			sql:       "DELETE FROM orders",
			wantError: "仅允许 SELECT",
		},
		{
			name:      "拒绝多语句",
			sql:       "SELECT 1; DROP TABLE users",
			wantError: "禁止多语句执行",
		},
		{
			name:      "拒绝注释注入",
			sql:       "SELECT 1 -- comment",
			wantError: "禁止 SQL 注释",
		},
		{
			name:      "拒绝 UPDATE",
			sql:       "UPDATE orders SET status = 1",
			wantError: "仅允许 SELECT",
		},
		{
			name:    "允许 WITH 只读 CTE",
			sql:     "WITH t AS (SELECT 1 AS c) SELECT c FROM t LIMIT 10",
			wantSQL: "WITH t AS (SELECT 1 AS c) SELECT c FROM t LIMIT 10",
		},
		{
			name:    "允许查询系统视图",
			sql:     "SELECT * FROM pg_stat_activity LIMIT 10",
			wantSQL: "SELECT * FROM pg_stat_activity LIMIT 10",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resolved, notes, err := g.Validate(tc.sql)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("期望校验失败，实际通过：%s", resolved)
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("错误信息不含 %q：%v", tc.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望校验通过，实际失败：%v", err)
			}
			if resolved != tc.wantSQL {
				t.Fatalf("SQL 归一化不符\n期望: %s\n实际: %s", tc.wantSQL, resolved)
			}
			if tc.wantNote != "" {
				found := false
				for _, note := range notes {
					if strings.Contains(note, tc.wantNote) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("期望备注含 %q，实际 %v", tc.wantNote, notes)
				}
			}
		})
	}
}

// TestSQLGuardTableAllowlist 校验表白名单（启用时生效）。
func TestSQLGuardTableAllowlist(t *testing.T) {
	g := NewSQLGuard(100, 1000, []string{"pg_stat_activity"})
	if _, _, err := g.Validate("SELECT * FROM pg_stat_activity"); err != nil {
		t.Fatalf("白名单内表被拒绝: %v", err)
	}
	if _, _, err := g.Validate("SELECT * FROM secret_table"); err == nil {
		t.Fatal("白名单外表未被拒绝")
	}
}

// TestScopeDataPermission 覆盖数据权限过滤（5.5：服务端强制，不依赖 Prompt）。
func TestScopeDataPermission(t *testing.T) {
	scope := Scope{
		RoleCode:     "ops",
		Environments: []string{"dev", "staging"},
		Groups:       []string{"payment"},
		ReadOnly:     true,
	}
	instances := []Instance{
		{ID: 1, Name: "redis-dev", Environment: "dev", GroupName: "payment"},
		{ID: 2, Name: "redis-prod", Environment: "prod", GroupName: "payment"},
		{ID: 3, Name: "redis-dev-other", Environment: "dev", GroupName: "order"},
	}
	filtered := scope.FilterInstances(instances)
	if len(filtered) != 1 || filtered[0].ID != 1 {
		t.Fatalf("数据权限过滤结果不符：%+v", filtered)
	}
	if !scope.CanRead(instances[0]) {
		t.Fatal("dev/payment 应可读")
	}
	if scope.CanRead(instances[1]) {
		t.Fatal("prod 环境不应可读")
	}
	// 管理员：不限制环境与分组。
	admin := Scope{AllowAllEnv: true}
	if len(admin.FilterInstances(instances)) != len(instances) {
		t.Fatal("管理员应可读全部实例")
	}
}

// TestToolRegistryRejectsWritableTool 校验只读工具集的强制约束（5.5）。
func TestToolRegistryRejectsWritableTool(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Mount(&fakeTool{name: "writable", readOnly: false}); err == nil {
		t.Fatal("非只读工具不应被挂载")
	}
	if err := registry.Mount(&fakeTool{name: "metrics", readOnly: true}); err != nil {
		t.Fatalf("只读工具挂载失败: %v", err)
	}
	if _, ok := registry.Get("metrics"); !ok {
		t.Fatal("已挂载工具查找失败")
	}
	if _, err := registry.Call(&ToolContext{Context: nil, Scope: Scope{ReadOnly: true}}, "unknown", nil); err == nil {
		t.Fatal("未挂载工具应被拒绝")
	}
}

type fakeTool struct {
	name     string
	readOnly bool
}

func (f *fakeTool) Name() string                                  { return f.name }
func (f *fakeTool) Decision() string                              { return "测试工具" }
func (f *fakeTool) ReadOnly() bool                                { return f.readOnly }
func (f *fakeTool) Run(*ToolContext, map[string]any) (any, error) { return nil, nil }

// TestLoopGuard 覆盖防死循环护栏（5.3）。
func TestLoopGuard(t *testing.T) {
	g := NewLoopGuard(3, 2, []string{"metrics"})
	if allow, _ := g.Allow("metrics", map[string]any{"id": 1}, "首次"); !allow {
		t.Fatal("首次调用应放行")
	}
	// 相同指纹第 2 次：仍未达到阈值（threshold=2）。
	if allow, _ := g.Allow("metrics", map[string]any{"id": 1}, "重复一次"); !allow {
		t.Fatal("第 2 次相同指纹应放行")
	}
	// 第 3 次相同指纹：达到阈值，终止。
	if allow, reason := g.Allow("metrics", map[string]any{"id": 1}, "再次重复"); allow {
		t.Fatal("重复达到阈值应中止")
	} else if !strings.Contains(reason, "重复") {
		t.Fatalf("中止原因不符: %s", reason)
	}
	if !g.Aborted() {
		t.Fatal("中止标记未设置")
	}

	// 步数上限。
	g2 := NewLoopGuard(2, 5, nil)
	g2.Allow("a", 1, "步骤1")
	g2.Allow("b", 2, "步骤2")
	if allow, reason := g2.Allow("c", 3, "步骤3"); allow {
		t.Fatal("超过最大步数应中止")
	} else if !strings.Contains(reason, "最大步数") {
		t.Fatalf("中止原因不符: %s", reason)
	}

	// 白名单外工具。
	g3 := NewLoopGuard(5, 5, []string{"allowed"})
	if allow, reason := g3.Allow("forbidden", 1, "越权工具"); allow {
		t.Fatal("白名单外工具应中止")
	} else if !strings.Contains(reason, "白名单") {
		t.Fatalf("中止原因不符: %s", reason)
	}
}

// TestBudgetFit 覆盖上下文预算与截断告知（5.2）。
func TestBudgetFit(t *testing.T) {
	budget := NewBudget(300, 512)
	sections := []Section{
		{Dimension: "metrics", Title: "指标", Body: strings.Repeat("指标数据 ", 200), Priority: 0},
		{Dimension: "logs", Title: "日志", Body: strings.Repeat("日志内容 ", 200), Priority: 1},
	}
	result := budget.FitMessages([]engine.Message{
		{Role: engine.RoleSystem, Content: SystemPromptStub},
		{Role: engine.RoleUser, Content: "Redis 为什么变慢了"},
	}, sections)

	if result.Used > result.Limit {
		t.Fatalf("预算适配后仍超限: used=%d limit=%d", result.Used, result.Limit)
	}
	if !result.Truncated() {
		t.Fatal("超预算时应记录被截断的维度")
	}
	if len(result.Dimensions()) == 0 {
		t.Fatal("应告知用户被截断的维度")
	}
	// 固定 Prompt 与用户问题必须保留。
	if len(result.Messages) < 2 {
		t.Fatalf("固定 Prompt 与用户问题不应被裁剪: %d", len(result.Messages))
	}
	if result.Messages[0].Role != engine.RoleSystem {
		t.Fatal("system 消息应排在首位且不被裁剪")
	}
}

// SystemPromptStub 模拟固定 Prompt（避免测试直接依赖业务模板）。
const SystemPromptStub = "你是中间件运维诊断专家，请输出 JSON。"

// TestSummarizeSeries 覆盖时序降采样摘要（5.2 不塞原始序列）。
func TestSummarizeSeries(t *testing.T) {
	values := []float64{1, 2, 3, 4, 100, 5, 6, 7, 8, 9}
	summary := SummarizeSeries("qps", "", values)
	if summary.Count != len(values) {
		t.Fatalf("采样数不符: %d", summary.Count)
	}
	if summary.Max != 100 {
		t.Fatalf("最大值计算错误: %v", summary.Max)
	}
	if summary.P95 < summary.Avg {
		t.Fatalf("P95 应不小于均值: p95=%v avg=%v", summary.P95, summary.Avg)
	}
	if len(summary.AnomalySegments) == 0 {
		t.Fatal("应检出异常片段")
	}
	if summary.TurningPoints == 0 {
		t.Fatal("应检出拐点")
	}
}

// TestQualityNormalize 覆盖质量护栏的「无证据标注推测」（5.6）。
func TestQualityNormalize(t *testing.T) {
	q := NewQuality(0.5)
	report, warnings := q.Normalize(&Report{RootCause: "内存不足", Confidence: 0.9}, []string{"logs"})
	if !report.Speculative {
		t.Fatal("无证据结论应标注为推测")
	}
	if len(warnings) == 0 {
		t.Fatal("应给出质量提示")
	}
	if report.GroundedRatio != 0 {
		t.Fatalf("证据覆盖率应为 0：%v", report.GroundedRatio)
	}

	ok, warnings2 := q.Normalize(&Report{
		RootCause:   "内存不足",
		Confidence:  0.8,
		Evidence:    []Evidence{{Source: "metric", Ref: "used_memory", Detail: "92%"}},
		Suggestions: []Suggestion{{Action: "扩容", Horizon: "short_term"}},
	}, nil)
	if ok.Speculative {
		t.Fatal("有硬证据不应标注为推测")
	}
	if len(warnings2) != 0 {
		t.Fatalf("不应有质量告警：%v", warnings2)
	}
	if ok.GroundedRatio != 1 {
		t.Fatalf("证据覆盖率应为 1：%v", ok.GroundedRatio)
	}
	if ok.Suggestions[0].Level == "" {
		t.Fatal("建议的操作级别应被补全")
	}
}

// TestParseReport 覆盖结构化报告解析（含代码块与前后说明）。
func TestParseReport(t *testing.T) {
	raw := "分析如下：\n```json\n{\"root_cause\":\"主从延迟\",\"confidence\":0.7," +
		"\"evidence\":[{\"source\":\"metric\",\"ref\":\"replication_lag\",\"detail\":\"12s\"}]," +
		"\"suggestions\":[{\"action\":\"拆分大事务\",\"horizon\":\"short_term\",\"risk\":\"中\",\"level\":\"L1\"}]," +
		"\"impact_scope\":\"读从库业务\",\"pending_confirm\":[\"确认是否有批量DDL\"]}\n```\n以上。"
	report, err := ParseReport(raw)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if report.RootCause != "主从延迟" {
		t.Fatalf("根因解析错误: %s", report.RootCause)
	}
	if len(report.Evidence) != 1 || len(report.Suggestions) != 1 {
		t.Fatalf("证据/建议解析错误: %+v", report)
	}
}

// TestCostBudgetAndCacheKey 覆盖成本治理（5.7）。
func TestCostBudgetAndCacheKey(t *testing.T) {
	cost := NewCost(1000, 500)
	if err := cost.Check(1); err != nil {
		t.Fatalf("初始预检应通过: %v", err)
	}
	if _, warning := cost.Commit(1, 100); warning != "" {
		t.Fatalf("低消耗不应告警: %s", warning)
	}
	if _, warning := cost.Commit(1, 500); warning == "" {
		t.Fatal("使用率超过 80% 应给出告警")
	}
	if err := cost.Check(1); err == nil {
		t.Fatal("用户预算用尽后应拒绝")
	}

	// 确定性缓存键：同实例 + 同问题签名。
	a := CacheKey(1, "Redis 为什么变慢了？")
	b := CacheKey(1, "redis 为什么变慢")
	c := CacheKey(2, "Redis 为什么变慢了？")
	if a != b {
		t.Fatalf("归一化后应命中同一缓存键: %s != %s", a, b)
	}
	if a == c {
		t.Fatalf("不同实例不应共享缓存键: %s", a)
	}
}

// TestIsHighRisk 覆盖高危语句识别（6.6）。
func TestIsHighRisk(t *testing.T) {
	cases := []struct {
		sql  string
		want bool
	}{
		{"DELETE FROM orders", true},
		{"DELETE FROM orders WHERE id = 1", false},
		{"TRUNCATE TABLE orders", true},
		{"FLUSHALL", true},
		{"SELECT * FROM orders", false},
	}
	for _, tc := range cases {
		got, _ := IsHighRisk(tc.sql)
		if got != tc.want {
			t.Fatalf("IsHighRisk(%q) = %v, 期望 %v", tc.sql, got, tc.want)
		}
	}
}

// TestInferLevel 覆盖操作级别推断（4.6：不确定时按最高风险处理）。
func TestInferLevel(t *testing.T) {
	cases := map[string]string{
		"查看 SLOWLOG 明细":       "L0",
		"确认告警":                "L1",
		"清理 Redis key":        "L2",
		"修改 maxmemory-policy": "L2",
	}
	for action, want := range cases {
		if got := InferLevel(action); got != want {
			t.Fatalf("InferLevel(%q) = %s, 期望 %s", action, got, want)
		}
	}
}

// TestEvalSetSize 校验内置评测集规模满足 20-50 条（5.6）。
func TestEvalSetSize(t *testing.T) {
	set := DefaultEvalSet()
	if len(set) < 20 || len(set) > 50 {
		t.Fatalf("评测集规模应为 20-50 条，实际 %d", len(set))
	}
	seen := make(map[string]bool, len(set))
	for _, c := range set {
		if c.ID == "" || c.Question == "" {
			t.Fatalf("评测用例字段不完整: %+v", c)
		}
		if seen[c.ID] {
			t.Fatalf("评测用例 ID 重复: %s", c.ID)
		}
		seen[c.ID] = true
	}
}

// TestEvaluateCase 覆盖评测回归判定。
func TestEvaluateCase(t *testing.T) {
	q := NewQuality(0.5)
	report := &Report{
		RootCause:  "Redis 内存压力偏高，可能触发淘汰",
		Confidence: 0.8,
		Evidence:   []Evidence{{Source: "metric", Ref: "memory_usage_percent", Detail: "92%"}},
	}
	result := q.Evaluate(EvalCase{
		ID: "redis-mem-01", ExpectKeywords: []string{"内存", "淘汰"},
	}, report, "")
	if !result.Passed {
		t.Fatalf("应判定通过: %+v", result)
	}
	forbidden := q.Evaluate(EvalCase{
		ID: "x", ExpectKeywords: []string{"内存"}, ForbidKeywords: []string{"内存"},
	}, report, "")
	if forbidden.Passed {
		t.Fatal("命中禁止关键词不应通过")
	}
}
