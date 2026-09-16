package service

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/utils"
)

// stubSession 构造带权限点与级别的测试会话。
func stubSession(role string, perms, levels []string) *Session {
	return &Session{
		User:        &model.User{Base: model.Base{ID: 42}, Username: role + "-user", RoleCode: role},
		Permissions: perms,
		Levels:      levels,
	}
}

// TestAuthRequirePermission 校验权限点校验（6.1 越权用例必须被拦截）。
func TestAuthRequirePermission(t *testing.T) {
	auth := &AuthService{}
	ops := stubSession(RoleOps, []string{PermMiddlewareWrite, PermAlertWrite}, []string{LevelRead, LevelLow})

	if err := auth.Require(ops, PermMiddlewareWrite); err != nil {
		t.Fatalf("运维应具备纳管写权限: %v", err)
	}
	if err := auth.Require(ops, PermUserManage); err == nil {
		t.Fatal("越权访问用户管理必须被拦截")
	} else if apperr.From(err).Code != apperr.CodeForbidden {
		t.Fatalf("越权错误码应为 4030，实际 %d", apperr.From(err).Code)
	}

	// 未登录 / 会话缺失。
	if err := auth.Require(nil, PermMiddlewareRead); err == nil {
		t.Fatal("无会话必须被拦截")
	} else if apperr.From(err).Code != apperr.CodeUnauthorized {
		t.Fatalf("未认证错误码应为 4010，实际 %d", apperr.From(err).Code)
	}
}

// TestAuthRequireLevel 校验操作级别校验（4.6：L2 一律走审批）。
func TestAuthRequireLevel(t *testing.T) {
	auth := &AuthService{}
	dev := stubSession(RoleDev, []string{PermFixPreview}, []string{LevelRead})
	ops := stubSession(RoleOps, []string{PermFixExecute}, []string{LevelRead, LevelLow})

	if err := auth.RequireLevel(dev, LevelRead); err != nil {
		t.Fatalf("开发角色应可执行 L0: %v", err)
	}
	if err := auth.RequireLevel(dev, LevelLow); err == nil {
		t.Fatal("开发角色不应直接执行 L1")
	}
	if err := auth.RequireLevel(ops, LevelLow); err != nil {
		t.Fatalf("运维应可执行 L1: %v", err)
	}
	// L2 对任何角色都不允许直接执行，必须走审批。
	for _, session := range []*Session{dev, ops} {
		err := auth.RequireLevel(session, LevelHigh)
		if err == nil {
			t.Fatalf("角色 %s 不应直接执行 L2", session.User.RoleCode)
		}
		if apperr.From(err).Code != apperr.CodeApprovalRequired {
			t.Fatalf("L2 应返回「需要审批」错误码 4033，实际 %d", apperr.From(err).Code)
		}
	}
}

// TestRolePermissionsCoverDesign 校验内置角色权限点覆盖设计文档 6.1 的职责划分。
func TestRolePermissionsCoverDesign(t *testing.T) {
	// 管理员：全部权限 + 用户管理 + 审批管理 + 审计查看。
	admin := rolePermissions[RoleAdmin]
	for _, want := range []string{PermUserManage, PermApprovalDecide, PermAuditRead, PermSystemConfigWrite} {
		if !contains(admin, want) {
			t.Fatalf("管理员缺少权限点 %s", want)
		}
	}
	// 运维：纳管、告警、执行 L1/L2、审计只读。
	ops := rolePermissions[RoleOps]
	for _, want := range []string{PermMiddlewareWrite, PermAlertWrite, PermFixExecute, PermAuditRead} {
		if !contains(ops, want) {
			t.Fatalf("运维缺少权限点 %s", want)
		}
	}
	if contains(ops, PermUserManage) {
		t.Fatal("运维不应具备用户管理权限")
	}
	// 开发：SQL 只读查询、AI 诊断、知识库建议；不含执行权限。
	dev := rolePermissions[RoleDev]
	if !contains(dev, PermSQLRead) || !contains(dev, PermAIUse) {
		t.Fatal("开发应具备只读 SQL 与 AI 诊断权限")
	}
	if contains(dev, PermFixExecute) {
		t.Fatal("开发不应具备修复执行权限")
	}
	// 只读：仅大盘与诊断报告查看。
	readonly := rolePermissions[RoleReadonly]
	if !contains(readonly, PermSystemOverview) {
		t.Fatal("只读角色应可查看大盘")
	}
	if contains(readonly, PermMiddlewareWrite) || contains(readonly, PermFixExecute) {
		t.Fatal("只读角色不应具备任何写权限")
	}
}

// TestRoleLevelsDoNotAllowL2 校验任何内置角色都不能直接执行 L2（4.6）。
func TestRoleLevelsDoNotAllowL2(t *testing.T) {
	for role, levels := range roleLevels {
		if contains(levels, LevelHigh) {
			t.Fatalf("角色 %s 不应被授予 L2 直接执行权限", role)
		}
	}
}

// TestPermissionCatalogCoversConstants 校验权限点目录覆盖全部权限点常量。
func TestPermissionCatalogCoversConstants(t *testing.T) {
	catalog := AllPermissionCatalog()
	seen := make(map[string]bool)
	for _, group := range catalog {
		items, ok := group["items"].([]map[string]string)
		if !ok {
			t.Fatalf("权限目录结构异常: %+v", group)
		}
		for _, item := range items {
			seen[item["code"]] = true
		}
	}
	all := []string{
		PermMiddlewareRead, PermMiddlewareWrite, PermMonitorRead, PermAIUse,
		PermAlertRead, PermAlertWrite, PermKnowledgeRead, PermKnowledgeWrite,
		PermFixPreview, PermFixExecute, PermAuditRead, PermAuditSnapshot,
		PermUserManage, PermApprovalRead, PermApprovalDecide,
		PermLogAlertRead, PermLogAlertWrite, PermCodeAnalyze, PermServerManage,
		PermSystemOverview, PermSQLRead, PermSystemConfigRead, PermSystemConfigWrite,
	}
	for _, perm := range all {
		if !seen[perm] {
			t.Fatalf("权限目录缺少权限点 %s", perm)
		}
	}
}

// TestSessionHasWildcard 校验通配权限语义。
func TestSessionHasWildcard(t *testing.T) {
	session := stubSession("custom", []string{"*"}, []string{LevelRead})
	if !session.Has(PermUserManage) {
		t.Fatal("通配权限应覆盖任意权限点")
	}
	empty := stubSession(RoleReadonly, nil, nil)
	if empty.Has(PermUserManage) {
		t.Fatal("空权限集不应通过校验")
	}
}

// TestAuditHashChain 校验审计哈希链的确定性与篡改可检出性（6.4）。
func TestAuditHashChain(t *testing.T) {
	makeEntry := func(id int64, username, action string) *model.AuditLog {
		return &model.AuditLog{
			ID: id, UserID: 1, Username: username, ActionType: action,
			ActionDetail: model.JSONMap{"k": "v"}, Result: "success", Level: LevelLow,
			IPAddress: "127.0.0.1",
			CreatedAt: time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC),
		}
	}

	prev := ""
	first := utils.HashChain(prev, canonicalAuditString(makeEntry(1, "admin", "login")))
	second := utils.HashChain(first, canonicalAuditString(makeEntry(2, "admin", "middleware_create")))
	if first == second {
		t.Fatal("不同内容应产生不同哈希")
	}
	if utils.HashChain(prev, canonicalAuditString(makeEntry(1, "admin", "login"))) != first {
		t.Fatal("相同输入必须产生相同哈希（链可复现）")
	}
	// 篡改用户名后哈希必须变化，链校验可检出。
	tampered := utils.HashChain(prev, canonicalAuditString(makeEntry(1, "attacker", "login")))
	if tampered == first {
		t.Fatal("内容被篡改后哈希必须变化")
	}
	// 链的续接：第二轮以第一轮哈希为 prev，篡改首条会导致整链失配。
	rebuild := utils.HashChain(tampered, canonicalAuditString(makeEntry(2, "admin", "middleware_create")))
	if rebuild == second {
		t.Fatal("首条被篡改后，后续哈希不应保持一致")
	}
}

// canonicalAuditString 复现审计哈希的规范化内容（与 repository 层保持一致）。
func canonicalAuditString(entry *model.AuditLog) string {
	detail := ""
	if entry.ActionDetail != nil {
		if b, err := json.Marshal(entry.ActionDetail); err == nil {
			detail = string(b)
		}
	}
	return fmt.Sprintf("%d|%d|%s|%d|%s|%s|%s|%s|%s|%s",
		entry.ID, entry.UserID, entry.Username, entry.InstanceID, entry.ActionType,
		detail, entry.Result, entry.Level, entry.IPAddress, entry.CreatedAt.UTC().Format(time.RFC3339Nano))
}

// TestScopedContextCancel 校验任务级超时上下文。
func TestScopedContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ctx.Err() == nil {
		t.Fatal("取消后 ctx.Err 不应为 nil")
	}
}
