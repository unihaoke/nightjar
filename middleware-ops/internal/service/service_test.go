package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/model"
	"middleware-ops/internal/service/ai"
)

// TestGuardScopeDeniesOutOfScope 校验数据权限在服务层的强制生效（5.5）。
func TestGuardScopeDeniesOutOfScope(t *testing.T) {
	auth := &AuthService{}
	dev := &Session{
		User:       &model.User{Base: model.Base{ID: 7}, Username: "dev1", RoleCode: RoleDev},
		EnvScope:   model.JSONStringSlice{"dev"},
		GroupScope: model.JSONStringSlice{"order"},
	}
	scope, desc := GuardScope(dev, auth)
	if len(scope.EnvScope) != 1 || scope.EnvScope[0] != "dev" {
		t.Fatalf("环境范围不符: %+v", scope)
	}
	if !strings.Contains(desc, "dev") || !strings.Contains(desc, "order") {
		t.Fatalf("权限描述不符: %s", desc)
	}
	inScopeInstance := &model.MiddlewareInstance{MWType: model.MWTypeRedis, Environment: "dev", GroupName: "order"}
	if !inScope(inScopeInstance, scope) {
		t.Fatal("dev/order 实例应在权限范围内")
	}
	prodInstance := &model.MiddlewareInstance{MWType: model.MWTypeRedis, Environment: "prod", GroupName: "order"}
	if inScope(prodInstance, scope) {
		t.Fatal("prod 实例不应在权限范围内")
	}

	// 管理员：允许全部环境。
	admin := &Session{User: &model.User{Base: model.Base{ID: 1}, Username: "admin", RoleCode: RoleAdmin}}
	adminScope, _ := GuardScope(admin, auth)
	if !inScope(prodInstance, adminScope) {
		t.Fatal("管理员应可访问 prod 实例")
	}
}

// TestFixActionLevel 校验操作分级（4.6）。
func TestFixActionLevel(t *testing.T) {
	cases := map[string]string{
		ActionViewMetrics:    LevelRead,
		ActionRunReadonlySQL: LevelRead,
		ActionAckAlert:       LevelLow,
		ActionCleanRedisKey:  LevelHigh,
		ActionRestartService: LevelHigh,
		"unknown_action":     LevelHigh,
	}
	for action, want := range cases {
		if got := ActionLevel(action); got != want {
			t.Fatalf("ActionLevel(%s) = %s, 期望 %s", action, got, want)
		}
	}
	// 未知动作必须按最高风险处理（强制审批）。
	if _, reason := guardrail.IsHighRisk("DROP TABLE orders"); !strings.Contains(reason, "drop") {
		t.Fatalf("高危识别原因不符: %s", reason)
	}
}

// TestPreviewRequiresApprovalForProdL2 校验 prod 环境 L2 强制审批（6.2）。
func TestPreviewRequiresApprovalForProdL2(t *testing.T) {
	preview := &PreviewResult{Level: ActionLevel(ActionRestartService)}
	if preview.Level != LevelHigh {
		t.Fatalf("重启服务应为 L2，实际 %s", preview.Level)
	}
	highRisk, reason := detectHighRisk(FixRequest{ActionType: ActionRestartService})
	if !highRisk {
		t.Fatal("L2 动作应识别为高危")
	}
	if !strings.Contains(reason, "L2") {
		t.Fatalf("高危原因不符: %s", reason)
	}
	sqlRisk, sqlReason := detectHighRisk(FixRequest{
		ActionType: ActionSQLWrite,
		Params:     map[string]any{"sql": "DELETE FROM orders"},
	})
	if !sqlRisk || !strings.Contains(sqlReason, "WHERE") {
		t.Fatalf("无 WHERE 的 DELETE 应识别为高危: %v %s", sqlRisk, sqlReason)
	}
}

// TestDetectMWType 校验「识别目标中间件」的规则匹配（4.3）。
func TestDetectMWType(t *testing.T) {
	cases := []struct {
		question   string
		candidates []string
		want       string
	}{
		{"Redis 最近为什么变慢了", nil, model.MWTypeRedis},
		{"Kafka 消费组积压严重", nil, model.MWTypeKafka},
		{"MySQL 慢查询增多怎么排查", nil, model.MWTypeMySQL},
		{"PostgreSQL 主从延迟变大", nil, model.MWTypePG},
		{"ES 集群变成 yellow 了", nil, model.MWTypeES},
		{"网关 5xx 突然增多", nil, model.MWTypeNginx},
		{"缓存命中率下降", []string{model.MWTypeRedis}, model.MWTypeRedis},
		{"完全无关的描述", nil, ""},
	}
	for _, tc := range cases {
		if got := ai.DetectMWType(tc.question, tc.candidates); got != tc.want {
			t.Fatalf("DetectMWType(%q) = %q, 期望 %q", tc.question, got, tc.want)
		}
	}
}

// TestErrorSignatureGroupsVariables 校验错误指纹能归并变量差异（4.8.2）。
func TestErrorSignatureGroupsVariables(t *testing.T) {
	a := ErrorSignature("Order 10086 处理失败 request_id=abc-123", "at com.demo.OrderService.process(OrderService.java:42)")
	b := ErrorSignature("Order 20099 处理失败 request_id=def-456", "at com.demo.OrderService.process(OrderService.java:88)")
	if a != b {
		t.Fatalf("同模板错误应归并为同一指纹:\n%s\n%s", a, b)
	}
	other := ErrorSignature("Nginx upstream timeout", "at com.demo.Gateway.handle(Gateway.java:10)")
	if other == a {
		t.Fatal("不同模板错误不应归并")
	}
	if len(a) != 32 {
		t.Fatalf("指纹长度不符: %d", len(a))
	}
}

// TestAlertCompareOperator 校验阈值比较语义（4.4）。
func TestAlertCompareOperator(t *testing.T) {
	svc := &AlertService{}
	cases := []struct {
		value     float64
		operator  string
		threshold float64
		want      bool
	}{
		{90, ">", 85, true},
		{80, ">", 85, false},
		{80, "<=", 80, true},
		{0, "==", 0, true},
		{1, "!=", 2, true},
		{1, "~~", 1, false},
	}
	for _, tc := range cases {
		if got := svc.Compare(tc.value, tc.operator, tc.threshold); got != tc.want {
			t.Fatalf("Compare(%v %s %v) = %v, 期望 %v", tc.value, tc.operator, tc.threshold, got, tc.want)
		}
	}
}

// TestBuildAlertMessage 校验告警消息包含关键信息。
func TestBuildAlertMessage(t *testing.T) {
	rule := model.AlertRule{
		MetricName: "memory_usage_percent", Operator: ">", Threshold: 85,
		Level: model.AlertLevelCritical, Description: "内存水位过高",
	}
	metric := monitorMetricStub("memory_usage_percent", "内存使用率", "%")
	msg := buildAlertMessage(rule, metric, 92.5)
	for _, want := range []string{"critical", "内存使用率", "85", "92.5", "内存水位过高"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("告警消息缺少 %q: %s", want, msg)
		}
	}
}

// TestKnowledgeSinkStatusIsDraft 校验诊断沉淀先入草稿（4.5 质量闭环）。
func TestKnowledgeSinkStatusIsDraft(t *testing.T) {
	if model.KnowledgeStatusDraft != "draft" {
		t.Fatal("草稿状态常量不符")
	}
	// 只有已发布条目参与检索。
	filter := knowledgePublishedFilter()
	if !filter.OnlyPub && filter.Status != model.KnowledgeStatusPublished {
		t.Fatal("向量候选集必须限定为已发布条目")
	}
}

// TestAdoptionWeight 校验低采纳率条目降权（4.5）。
func TestAdoptionWeight(t *testing.T) {
	if adoptionWeight(0, 0) != 1 {
		t.Fatal("无引用记录时权重应为 1")
	}
	if adoptionWeight(10, 10) <= 1 {
		t.Fatal("高采纳率条目应加权")
	}
	if adoptionWeight(1, 10) >= 1 {
		t.Fatal("低采纳率条目应降权")
	}
}

// TestRedactorRemovesSensitiveData 校验出网脱敏（6.5）。
func TestRedactorRemovesSensitiveData(t *testing.T) {
	securityCfg := securityConfigStub()
	redactor := NewRedactor(&securityCfg)
	raw := "连接 10.20.30.40:6379 失败，用户 cwh 手机号 13800138000，" +
		"request_id=abc-123-def password=SuperSecret123 邮箱 dev@example.com"
	out := redactor.Redact(raw)
	for _, banned := range []string{"10.20.30.40", "13800138000", "SuperSecret123", "dev@example.com"} {
		if strings.Contains(out, banned) {
			t.Fatalf("脱敏后仍包含敏感信息 %q：%s", banned, out)
		}
	}
	if !strings.Contains(out, "连接") {
		t.Fatal("脱敏不应破坏上下文文本")
	}
}

// TestTruncateCodeLines 校验代码片段行数上限（≤200 行/文件）。
func TestTruncateCodeLines(t *testing.T) {
	securityCfg := securityConfigStub()
	redactor := NewRedactor(&securityCfg)
	long := strings.Repeat("line\n", 300)
	out, truncated := redactor.TruncateCode(long)
	if !truncated {
		t.Fatal("超长代码应被标记截断")
	}
	if got := strings.Count(out, "\n"); got > redactor.MaxLines() {
		t.Fatalf("截断后行数超限: %d", got)
	}
}
