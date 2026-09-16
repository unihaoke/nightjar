package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
	"middleware-ops/internal/utils"
)

// LoginRequest 是登录入参。
type LoginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// Login 用户登录。
//
// 认证方式：JWT（响应体返回）+ SameSite Cookie（便于前端直连与静态资源场景）。
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if !bindJSON(c, &req) {
		return
	}
	cfg := h.deps.Config
	session, err := h.deps.Auth.Login(c.Request.Context(), req.Username, req.Password, c.ClientIP(), c.Request.UserAgent())
	if err != nil {
		// 登录失败同样写审计（4.7 覆盖登录动作）。
		h.deps.Audit.RecordAsync(c.Request.Context(), service.AuditEntry{
			Username: req.Username, ActionType: "login", Level: service.LevelRead, Result: "failed",
			IPAddress: c.ClientIP(), UserAgent: c.Request.UserAgent(),
			Detail: map[string]any{"reason": apperr.From(err).Message},
		})
		response.Fail(c, err)
		return
	}
	sameSite := parseSameSite(cfg.JWT.CookieSameSite)
	c.SetSameSite(sameSite)
	c.SetCookie(cfg.JWT.CookieName, session.Token, int(cfg.JWT.AccessTTL.Seconds()), "/", "", cfg.JWT.CookieSecure, true)

	h.deps.Audit.RecordAsync(c.Request.Context(), service.AuditEntry{
		UserID: session.User.ID, Username: session.User.Username, ActionType: "login",
		Level: service.LevelRead, Result: "success", IPAddress: c.ClientIP(),
		UserAgent: c.Request.UserAgent(), Route: c.FullPath(),
	})
	response.OK(c, session)
}

// Logout 退出登录（清除 Cookie）。
func (h *Handler) Logout(c *gin.Context) {
	cfg := h.deps.Config
	c.SetSameSite(parseSameSite(cfg.JWT.CookieSameSite))
	c.SetCookie(cfg.JWT.CookieName, "", -1, "/", "", cfg.JWT.CookieSecure, true)
	session := h.session(c)
	if session != nil && session.User != nil {
		h.deps.Audit.RecordAsync(c.Request.Context(), service.AuditEntry{
			UserID: session.User.ID, Username: session.User.Username, ActionType: "logout",
			Level: service.LevelRead, IPAddress: c.ClientIP(), UserAgent: c.Request.UserAgent(),
		})
	}
	response.OK(c, gin.H{"message": "已退出登录"})
}

// Profile 返回当前用户与权限信息。
func (h *Handler) Profile(c *gin.Context) {
	session := h.session(c)
	if session == nil {
		response.Fail(c, apperr.New(apperr.CodeUnauthorized, ""))
		return
	}
	envs, groups, allowAll := h.deps.Auth.ScopeOf(session)
	response.OK(c, gin.H{
		"user":        session.User,
		"permissions": session.Permissions,
		"levels":      session.Levels,
		"data_scope": gin.H{
			"environments": envs,
			"groups":       groups,
			"allow_all":    allowAll,
		},
		"engine": service.WatchEngineStatus(h.deps.Engine),
	})
}

// ChangePasswordRequest 是修改密码入参。
type ChangePasswordRequest struct {
	OldPassword string `json:"old_password" binding:"required,min=6"`
	NewPassword string `json:"new_password" binding:"required,min=8"`
}

// ChangePassword 修改自己的密码。
func (h *Handler) ChangePassword(c *gin.Context) {
	session := h.session(c)
	var req ChangePasswordRequest
	if !bindJSON(c, &req) {
		return
	}
	if !utils.VerifyPassword(session.User.PasswordHash, req.OldPassword) {
		response.Fail(c, apperr.New(apperr.CodeWrongCredential, "原密码不正确"))
		return
	}
	if req.NewPassword == req.OldPassword {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "新密码不能与原密码相同"))
		return
	}
	hash, err := utils.HashPassword(req.NewPassword)
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	if err := h.deps.Users.UpdatePassword(c.Request.Context(), session.User.ID, hash); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	h.deps.Audit.RecordAsync(c.Request.Context(), service.AuditEntry{
		UserID: session.User.ID, Username: session.User.Username, ActionType: "password_change",
		Level: service.LevelLow, IPAddress: c.ClientIP(), UserAgent: c.Request.UserAgent(),
	})
	response.OK(c, gin.H{"message": "密码已更新，请使用新密码重新登录"})
}

// ListUsers 用户列表。
func (h *Handler) ListUsers(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.Users.List(c.Request.Context(), c.Query("keyword"), pageSize, offset)
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// UserRequest 是用户新增/更新入参。
type UserRequest struct {
	Username   string   `json:"username"`
	Password   string   `json:"password"`
	Nickname   string   `json:"nickname"`
	Email      string   `json:"email"`
	RoleCode   string   `json:"role_code" binding:"required"`
	EnvScope   []string `json:"env_scope"`
	GroupScope []string `json:"group_scope"`
	Status     *int16   `json:"status"`
}

// CreateUser 新增用户（管理员）。
func (h *Handler) CreateUser(c *gin.Context) {
	var req UserRequest
	if !bindJSON(c, &req) {
		return
	}
	if req.Username == "" || req.Password == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "用户名与初始密码不能为空"))
		return
	}
	if _, err := h.deps.Roles.GetByCode(c.Request.Context(), req.RoleCode); err != nil {
		response.Fail(c, apperr.Newf(apperr.CodeInvalidParam, "角色 %s 不存在", req.RoleCode))
		return
	}
	hash, err := utils.HashPassword(req.Password)
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	user := &model.User{
		Username: req.Username, PasswordHash: hash, Nickname: req.Nickname, Email: req.Email,
		RoleCode: req.RoleCode, EnvScope: model.JSONStringSlice(req.EnvScope),
		GroupScope: model.JSONStringSlice(req.GroupScope), Status: 1,
	}
	if req.Status != nil {
		user.Status = *req.Status
	}
	if err := h.deps.Users.Create(c.Request.Context(), user); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	h.auditUser(c, "user_create", map[string]any{"user_id": user.ID, "username": user.Username, "role": user.RoleCode})
	response.OK(c, user)
}

// UpdateUser 更新用户（管理员）。
func (h *Handler) UpdateUser(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req UserRequest
	if !bindJSON(c, &req) {
		return
	}
	user, err := h.deps.Users.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeNotFound, err))
		return
	}
	user.Nickname = req.Nickname
	user.Email = req.Email
	if req.RoleCode != "" {
		user.RoleCode = req.RoleCode
	}
	user.EnvScope = model.JSONStringSlice(req.EnvScope)
	user.GroupScope = model.JSONStringSlice(req.GroupScope)
	if req.Status != nil {
		user.Status = *req.Status
	}
	if err := h.deps.Users.Update(c.Request.Context(), user); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	if req.Password != "" {
		hash, hashErr := utils.HashPassword(req.Password)
		if hashErr != nil {
			response.Fail(c, apperr.Wrap(apperr.CodeInternal, hashErr))
			return
		}
		if err := h.deps.Users.UpdatePassword(c.Request.Context(), id, hash); err != nil {
			response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
			return
		}
	}
	h.auditUser(c, "user_update", map[string]any{
		"user_id": id, "role": user.RoleCode, "status": user.Status, "password_reset": req.Password != "",
	})
	response.OK(c, user)
}

// DeleteUser 删除用户（管理员，禁止删除自己）。
func (h *Handler) DeleteUser(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	session := h.session(c)
	if session.User.ID == id {
		response.Fail(c, apperr.New(apperr.CodeForbidden, "不能删除当前登录账号"))
		return
	}
	if err := h.deps.Users.Delete(c.Request.Context(), id); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	h.auditUser(c, "user_delete", map[string]any{"user_id": id})
	response.OK(c, gin.H{"message": "已删除"})
}

// ListRoles 角色列表。
func (h *Handler) ListRoles(c *gin.Context) {
	roles, err := h.deps.Auth.ListRoles(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"list": roles, "permissions": service.AllPermissionCatalog()})
}

// RoleRequest 是角色更新入参。
type RoleRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Permissions []string `json:"permissions"`
	Levels      []string `json:"levels"`
}

// UpdateRole 更新角色（管理员，内置角色不可改）。
func (h *Handler) UpdateRole(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req RoleRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.deps.Auth.UpdateRole(c.Request.Context(), id, req.Name, req.Description, req.Permissions, req.Levels); err != nil {
		response.Fail(c, err)
		return
	}
	h.auditUser(c, "role_update", map[string]any{"role_id": id, "permissions": req.Permissions})
	response.OK(c, gin.H{"message": "角色已更新"})
}

// auditUser 记录用户管理类审计。
func (h *Handler) auditUser(c *gin.Context, action string, detail map[string]any) {
	op := h.operator(c)
	h.deps.Audit.RecordAsync(c.Request.Context(), service.AuditEntry{
		UserID: op.UserID, Username: op.Username, ActionType: action, Level: service.LevelLow,
		IPAddress: op.IP, UserAgent: op.Agent, Route: c.FullPath(), Detail: detail,
	})
}

// parseSameSite 解析 SameSite 配置（默认 Lax，兼顾 CSRF 防护与可用性）。
func parseSameSite(value string) http.SameSite {
	switch value {
	case "strict":
		return http.SameSiteStrictMode
	case "none":
		return http.SameSiteNoneMode
	default:
		return http.SameSiteLaxMode
	}
}
