package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// 权限点定义（6.1 RBAC 权限模型）。
const (
	PermMiddlewareRead    = "middleware:read"
	PermMiddlewareWrite   = "middleware:write"
	PermMonitorRead       = "monitor:read"
	PermAIUse             = "ai:use"
	PermAlertRead         = "alert:read"
	PermAlertWrite        = "alert:write"
	PermKnowledgeRead     = "knowledge:read"
	PermKnowledgeWrite    = "knowledge:write"
	PermFixPreview        = "fix:preview"
	PermFixExecute        = "fix:execute"
	PermAuditRead         = "audit:read"
	PermAuditSnapshot     = "audit:snapshot"
	PermUserManage        = "user:manage"
	PermApprovalRead      = "approval:read"
	PermApprovalDecide    = "approval:decide"
	PermLogAlertRead      = "logalert:read"
	PermLogAlertWrite     = "logalert:write"
	PermCodeAnalyze       = "code:analyze"
	PermSystemOverview    = "system:overview"
	PermSQLRead           = "sql:read"
	PermSystemConfigRead  = "system:config"
	PermSystemConfigWrite = "system:config:write"
)

// 内置角色（6.1）。
const (
	RoleAdmin    = "admin"
	RoleOps      = "ops"
	RoleDev      = "dev"
	RoleReadonly = "readonly"
)

// allPermissions 是内置角色的权限点集合。
var rolePermissions = map[string][]string{
	RoleAdmin: {
		PermMiddlewareRead, PermMiddlewareWrite, PermMonitorRead, PermAIUse,
		PermAlertRead, PermAlertWrite, PermKnowledgeRead, PermKnowledgeWrite,
		PermFixPreview, PermFixExecute, PermAuditRead, PermAuditSnapshot,
		PermUserManage, PermApprovalRead, PermApprovalDecide,
		PermLogAlertRead, PermLogAlertWrite, PermCodeAnalyze,
		PermSystemOverview, PermSQLRead, PermSystemConfigRead, PermSystemConfigWrite,
	},
	RoleOps: {
		PermMiddlewareRead, PermMiddlewareWrite, PermMonitorRead, PermAIUse,
		PermAlertRead, PermAlertWrite, PermKnowledgeRead, PermKnowledgeWrite,
		PermFixPreview, PermFixExecute, PermAuditRead,
		PermApprovalRead, PermLogAlertRead, PermLogAlertWrite, PermCodeAnalyze,
		PermSystemOverview, PermSQLRead, PermSystemConfigRead,
	},
	RoleDev: {
		PermMiddlewareRead, PermMonitorRead, PermAIUse,
		PermAlertRead, PermKnowledgeRead, PermKnowledgeWrite,
		PermFixPreview, PermLogAlertRead, PermCodeAnalyze, PermSystemOverview, PermSQLRead,
	},
	RoleReadonly: {
		PermMiddlewareRead, PermMonitorRead, PermAlertRead, PermKnowledgeRead, PermSystemOverview,
	},
}

// roleLevels 定义角色可直接执行的操作级别；L2 一律走审批（4.6）。
var roleLevels = map[string][]string{
	RoleAdmin:    {"L0", "L1"},
	RoleOps:      {"L0", "L1"},
	RoleDev:      {"L0"},
	RoleReadonly: {"L0"},
}

// 操作级别。
const (
	LevelRead = "L0"
	LevelLow  = "L1"
	LevelHigh = "L2"
)

// Session 是登录会话信息。
type Session struct {
	User        *model.User `json:"user"`
	Token       string      `json:"token"`
	ExpiresAt   time.Time   `json:"expires_at"`
	Permissions []string    `json:"permissions"`
	Levels      []string    `json:"levels"`
	EnvScope    []string    `json:"env_scope"`
	GroupScope  []string    `json:"group_scope"`
	// auth 为授权服务引用，供 ScopeOf 在服务层解析数据权限范围（不对外序列化）。
	auth *AuthService
}

// Has 判断会话是否具备指定权限点。
func (s *Session) Has(perm string) bool {
	for _, p := range s.Permissions {
		if p == perm || p == "*" {
			return true
		}
	}
	return false
}

// AuthService 提供认证与授权能力。
type AuthService struct {
	cfg    *config.Config
	users  *repository.UserRepository
	roles  *repository.RoleRepository
	tokens *utils.TokenManager
	log    *zap.Logger
}

// NewAuthService 构造认证服务。
func NewAuthService(cfg *config.Config, users *repository.UserRepository, roles *repository.RoleRepository, tokens *utils.TokenManager, log *zap.Logger) *AuthService {
	return &AuthService{cfg: cfg, users: users, roles: roles, tokens: tokens, log: log}
}

// Bootstrap 在系统初始化时确保内置角色与管理员账号存在。
func (s *AuthService) Bootstrap(ctx context.Context) error {
	for code, perms := range rolePermissions {
		existing, err := s.roles.GetByCode(ctx, code)
		if err == nil && existing != nil {
			// 内置角色每次启动同步权限点，避免版本升级后权限遗漏。
			existing.Permissions = model.JSONStringSlice(perms)
			existing.Levels = model.JSONStringSlice(roleLevels[code])
			if updateErr := s.roles.Update(ctx, existing); updateErr != nil {
				return fmt.Errorf("同步内置角色 %s: %w", code, updateErr)
			}
			continue
		}
		if err != nil && !repository.EnsureNotFound(err) {
			return fmt.Errorf("查询角色 %s: %w", code, err)
		}
		role := &model.Role{
			Code:        code,
			Name:        roleDisplayName(code),
			Description: roleDescription(code),
			Permissions: model.JSONStringSlice(perms),
			Levels:      model.JSONStringSlice(roleLevels[code]),
			Builtin:     true,
		}
		if err := s.roles.Create(ctx, role); err != nil {
			return fmt.Errorf("创建内置角色 %s: %w", code, err)
		}
	}

	username := s.cfg.JWT.BootstrapAdmin
	if strings.TrimSpace(username) == "" {
		return nil
	}
	if _, err := s.users.GetByUsername(ctx, username); err == nil {
		return nil
	} else if !repository.EnsureNotFound(err) {
		return fmt.Errorf("查询管理员账号: %w", err)
	}
	hash, err := utils.HashPassword(s.cfg.JWT.BootstrapPass)
	if err != nil {
		return fmt.Errorf("生成管理员密码: %w", err)
	}
	admin := &model.User{
		Username:     username,
		PasswordHash: hash,
		Nickname:     "系统管理员",
		RoleCode:     RoleAdmin,
		Status:       1,
	}
	if err := s.users.Create(ctx, admin); err != nil {
		return fmt.Errorf("创建管理员账号: %w", err)
	}
	s.log.Warn("已创建初始管理员账号，请登录后立即修改密码",
		zap.String("username", username))
	return nil
}

// Login 校验凭据并签发令牌。
func (s *AuthService) Login(ctx context.Context, username, password, ip, userAgent string) (*Session, error) {
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeWrongCredential, "")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if user.Status != 1 {
		return nil, apperr.New(apperr.CodeUserDisabled, "")
	}
	if !utils.VerifyPassword(user.PasswordHash, password) {
		return nil, apperr.New(apperr.CodeWrongCredential, "")
	}
	session, err := s.issue(ctx, user)
	if err != nil {
		return nil, err
	}
	if err := s.users.TouchLogin(ctx, user.ID); err != nil {
		s.log.Warn("更新登录时间失败", zap.Int64("user_id", user.ID), zap.Error(err))
	}
	return session, nil
}

// issue 签发会话。
func (s *AuthService) issue(ctx context.Context, user *model.User) (*Session, error) {
	perms, levels, err := s.permissionsOf(ctx, user.RoleCode)
	if err != nil {
		return nil, err
	}
	token, expires, err := s.tokens.Issue(user.ID, user.Username, user.RoleCode, user.EnvScope, user.GroupScope)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return &Session{
		User:        user,
		Token:       token,
		ExpiresAt:   expires,
		Permissions: perms,
		Levels:      levels,
		EnvScope:    user.EnvScope,
		GroupScope:  user.GroupScope,
		auth:        s,
	}, nil
}

// permissionsOf 查询角色权限点。
//
// 角色被删除或未配置时按「只读」处理，遵循最小权限原则。
func (s *AuthService) permissionsOf(ctx context.Context, roleCode string) (perms, levels []string, err error) {
	role, err := s.roles.GetByCode(ctx, roleCode)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return []string{PermMiddlewareRead, PermSystemOverview}, []string{LevelRead}, nil
		}
		return nil, nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return role.Permissions, role.Levels, nil
}

// SessionOf 依据用户 ID 重建会话（用于每次请求刷新权限，避免令牌携带过期权限）。
func (s *AuthService) SessionOf(ctx context.Context, userID int64) (*Session, error) {
	user, err := s.users.Get(ctx, userID)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeUnauthorized, "账号不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if user.Status != 1 {
		return nil, apperr.New(apperr.CodeUserDisabled, "")
	}
	perms, levels, err := s.permissionsOf(ctx, user.RoleCode)
	if err != nil {
		return nil, err
	}
	return &Session{
		User:        user,
		Permissions: perms,
		Levels:      levels,
		EnvScope:    user.EnvScope,
		GroupScope:  user.GroupScope,
		auth:        s,
	}, nil
}

// ParseToken 解析并校验令牌，返回声明。
func (s *AuthService) ParseToken(token string) (*utils.Claims, error) {
	return s.tokens.Parse(token)
}

// ScopeOf 把会话转换为护栏层的数据权限范围（5.5 服务端强制过滤）。
func (s *AuthService) ScopeOf(session *Session) (envs, groups []string, allowAll bool) {
	if session == nil {
		return nil, nil, false
	}
	allowAll = session.User != nil && session.User.RoleCode == RoleAdmin && len(session.EnvScope) == 0
	return []string(session.EnvScope), []string(session.GroupScope), allowAll
}

// Require 校验权限点，不满足时返回 4030 错误。
func (s *AuthService) Require(session *Session, perm string) error {
	if session == nil {
		return apperr.New(apperr.CodeUnauthorized, "")
	}
	if !session.Has(perm) {
		return apperr.Newf(apperr.CodeForbidden, "缺少权限点 %s", perm)
	}
	return nil
}

// RequireLevel 校验操作级别是否允许直接执行（L2 需要审批，见 4.6）。
func (s *AuthService) RequireLevel(session *Session, level string) error {
	if session == nil {
		return apperr.New(apperr.CodeUnauthorized, "")
	}
	for _, item := range session.Levels {
		if item == level {
			return nil
		}
	}
	if level == LevelHigh {
		return apperr.New(apperr.CodeApprovalRequired, "L2 高危操作需提交审批（prod 强制）")
	}
	return apperr.Newf(apperr.CodeLevelDenied, "角色 %s 不允许执行 %s 级别操作", session.User.RoleCode, level)
}

// ListRoles 返回角色列表（含用户数）。
func (s *AuthService) ListRoles(ctx context.Context) ([]model.Role, error) {
	roles, err := s.roles.List(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	counts, err := s.users.CountByRole(ctx)
	if err == nil {
		for i := range roles {
			roles[i].UserCount = counts[roles[i].Code]
		}
	}
	return roles, nil
}

// UpdateRole 更新角色权限点（管理员）。
func (s *AuthService) UpdateRole(ctx context.Context, id int64, name, description string, perms, levels []string) error {
	roles, err := s.roles.List(ctx)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	var target *model.Role
	for i := range roles {
		if roles[i].ID == id {
			target = &roles[i]
			break
		}
	}
	if target == nil {
		return apperr.New(apperr.CodeNotFound, "角色不存在")
	}
	if target.Builtin {
		return apperr.New(apperr.CodeForbidden, "内置角色不允许修改权限点，请通过自定义角色扩展")
	}
	if name != "" {
		target.Name = name
	}
	if description != "" {
		target.Description = description
	}
	if len(perms) > 0 {
		target.Permissions = model.JSONStringSlice(perms)
	}
	if len(levels) > 0 {
		target.Levels = model.JSONStringSlice(levels)
	}
	if err := s.roles.Update(ctx, target); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	return nil
}

// PermissionCatalog 是权限点的分组目录（供前端角色配置页展示）。
type PermissionCatalog struct {
	Group string `json:"group"`
	Items []struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"items"`
}

// AllPermissionCatalog 返回全部权限点目录。
func AllPermissionCatalog() []map[string]any {
	groups := []struct {
		name  string
		items []struct{ code, name string }
	}{
		{"中间件纳管", []struct{ code, name string }{
			{PermMiddlewareRead, "查看中间件"}, {PermMiddlewareWrite, "纳管/编辑中间件"},
		}},
		{"监控", []struct{ code, name string }{
			{PermMonitorRead, "查看指标与大盘（大盘另需 system:overview）"},
		}},
		{"AI 能力", []struct{ code, name string }{
			{PermAIUse, "使用 AI 诊断"}, {PermCodeAnalyze, "AI 代码分析"}, {PermSQLRead, "只读 SQL 查询"},
		}},
		{"告警", []struct{ code, name string }{
			{PermAlertRead, "查看告警"}, {PermAlertWrite, "告警规则与确认"},
			{PermLogAlertRead, "查看日志告警"}, {PermLogAlertWrite, "处理日志告警"},
		}},
		{"知识库", []struct{ code, name string }{
			{PermKnowledgeRead, "查看知识库"}, {PermKnowledgeWrite, "维护知识库"},
		}},
		{"修复执行", []struct{ code, name string }{
			{PermFixPreview, "修复预览"}, {PermFixExecute, "修复执行（L2 仍需审批）"},
		}},
		{"审批", []struct{ code, name string }{
			{PermApprovalRead, "查看审批"}, {PermApprovalDecide, "审批决策"},
		}},
		{"审计", []struct{ code, name string }{
			{PermAuditRead, "查看审计日志"}, {PermAuditSnapshot, "生成/校验审计快照"},
		}},
		{"系统", []struct{ code, name string }{
			{PermUserManage, "用户与角色管理"},
			{PermSystemOverview, "查看全局大盘"}, {PermSystemConfigRead, "查看系统配置"},
			{PermSystemConfigWrite, "修改系统配置"},
		}},
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		items := make([]map[string]string, 0, len(g.items))
		for _, item := range g.items {
			items = append(items, map[string]string{"code": item.code, "name": item.name})
		}
		out = append(out, map[string]any{"group": g.name, "items": items})
	}
	return out
}

// ErrTokenExpired 兼容别名，便于上层直接判断。
var ErrTokenExpired = utils.ErrTokenExpired

// IsExpired 判断错误是否为令牌过期。
func IsExpired(err error) bool { return errors.Is(err, utils.ErrTokenExpired) }

// roleDisplayName 返回角色中文名。
func roleDisplayName(code string) string {
	switch code {
	case RoleAdmin:
		return "管理员"
	case RoleOps:
		return "运维"
	case RoleDev:
		return "开发"
	case RoleReadonly:
		return "只读"
	default:
		return code
	}
}

// roleDescription 返回角色描述。
func roleDescription(code string) string {
	switch code {
	case RoleAdmin:
		return "全部权限 + 用户管理 + 审批管理 + 审计查看"
	case RoleOps:
		return "纳管、告警、执行 L1/L2、审计只读"
	case RoleDev:
		return "SQL 只读查询、AI 诊断、知识库建议"
	case RoleReadonly:
		return "大盘与诊断报告查看"
	default:
		return ""
	}
}
