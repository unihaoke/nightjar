// Package handler 实现 HTTP 接口层。
//
// 约定（8.1）：
//   - 统一响应 {code, message, data}；
//   - 分页 page/page_size（默认 20，上限 100）；
//   - 权限点与操作级别在 handler 中显式声明，L2 由审批服务拦截。
package handler

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/middleware"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// Handler 聚合全部接口处理器。
type Handler struct {
	deps *service.Deps
}

// New 构造接口层。
func New(deps *service.Deps) *Handler {
	return &Handler{deps: deps}
}

// Deps 暴露依赖（供路由注册与健康检查使用）。
func (h *Handler) Deps() *service.Deps { return h.deps }

// session 获取当前会话。
func (h *Handler) session(c *gin.Context) *service.Session { return middleware.SessionOf(c) }

// operator 获取当前操作者。
func (h *Handler) operator(c *gin.Context) service.Operator { return middleware.OperatorOf(c) }

// scope 获取数据权限范围。
func (h *Handler) scope(c *gin.Context) service.Scope { return middleware.ScopeOf(c) }

// bindJSON 解析请求体。
func bindJSON(c *gin.Context, target any) bool {
	if err := c.ShouldBindJSON(target); err != nil {
		response.Fail(c, apperr.Wrapf(apperr.CodeInvalidBody, err, "请求体解析失败: %v", err))
		return false
	}
	return true
}

// bindQuery 解析查询参数。
func bindQuery(c *gin.Context, target any) bool {
	if err := c.ShouldBindQuery(target); err != nil {
		response.Fail(c, apperr.Wrapf(apperr.CodeInvalidQuery, err, "查询参数不合法: %v", err))
		return false
	}
	return true
}

// page 解析分页参数。
func page(c *gin.Context) (pageNo, pageSize, offset int) {
	q := response.PageQuery{
		Page:     atoiDefault(c.Query("page"), 0),
		PageSize: atoiDefault(c.Query("page_size"), 0),
	}
	return q.Normalize()
}

// idParam 解析路径中的 ID。
func idParam(c *gin.Context, name string) (int64, bool) {
	raw := c.Param(name)
	id := atoi64Default(raw, 0)
	if id <= 0 {
		response.Fail(c, apperr.Newf(apperr.CodeInvalidParam, "路径参数 %s 必须是正整数", name))
		return 0, false
	}
	return id, true
}

// queryInt64 解析查询参数中的整数。
func queryInt64(c *gin.Context, name string) int64 {
	return atoi64Default(c.Query(name), 0)
}

// queryTime 解析 RFC3339 时间参数，失败或未提供时返回 nil。
func queryTime(c *gin.Context, name string) *time.Time {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return nil
	}
	parsed, err := parseTime(raw)
	if err != nil {
		return nil
	}
	return &parsed
}
