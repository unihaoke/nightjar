// Package middleware 提供 Gin 中间件：追踪、恢复、跨域、限流、认证与审计。
package middleware

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
	"middleware-ops/internal/utils"
)

// ContextSessionKey 是会话在 gin.Context 中的键。
const ContextSessionKey = "mwops.session"

// ContextOperatorKey 是操作者在 gin.Context 中的键。
const ContextOperatorKey = "mwops.operator"

// RequestIDKey 是请求 ID 的上下文键。
const RequestIDKey = "mwops.request_id"

// RequestID 注入请求追踪 ID。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			id = utils.NewRequestID()
		}
		c.Set(RequestIDKey, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

// Logger 输出结构化访问日志。
func Logger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		c.Next()
		fields := []zap.Field{
			zap.String("method", c.Request.Method),
			zap.String("path", path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
			zap.String("ip", c.ClientIP()),
			zap.String("request_id", c.GetString(RequestIDKey)),
		}
		if session := SessionOf(c); session != nil && session.User != nil {
			fields = append(fields, zap.Int64("user_id", session.User.ID), zap.String("username", session.User.Username))
		}
		if len(c.Errors) > 0 {
			fields = append(fields, zap.String("errors", c.Errors.String()))
		}
		switch {
		case c.Writer.Status() >= 500:
			log.Error("http", fields...)
		case c.Writer.Status() >= 400:
			log.Warn("http", fields...)
		default:
			log.Debug("http", fields...)
		}
	}
}

// Recovery 捕获 panic 并返回统一错误结构。
func Recovery(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("请求处理 panic",
					zap.Any("panic", r),
					zap.String("path", c.Request.URL.Path),
					zap.String("request_id", c.GetString(RequestIDKey)),
					zap.Stack("stack"),
				)
				response.Fail(c, apperr.New(apperr.CodeInternal, "服务内部错误，请查看平台日志"))
			}
		}()
		c.Next()
	}
}

// CORS 处理跨域请求。
//
// 生产部署由 Nginx 同源代理，此处主要用于前后端分离开发场景。
func CORS(allowedOrigins []string) gin.HandlerFunc {
	allowAll := len(allowedOrigins) == 0
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = true
	}
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && (allowAll || allowed[origin]) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers", "Authorization,Content-Type,X-Request-ID")
			c.Header("Access-Control-Max-Age", "600")
			c.Header("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// SecurityHeaders 注入基础安全响应头（6.6 XSS/CSRF 防护）。
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		c.Next()
	}
}

// RateLimiter 实现应用层限流（Nginx 为第一层，见 8.1）。
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	limit   int
	window  time.Duration
}

type bucket struct {
	count   int
	resetAt time.Time
}

// NewRateLimiter 构造按用户/IP 的滑动窗口限流器。
func NewRateLimiter(limitPerMinute int) *RateLimiter {
	if limitPerMinute <= 0 {
		limitPerMinute = 600
	}
	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		limit:   limitPerMinute,
		window:  time.Minute,
	}
	go rl.gc()
	return rl
}

// Middleware 返回限流中间件。
func (r *RateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.ClientIP()
		if session := SessionOf(c); session != nil && session.User != nil {
			key = session.User.Username
		}
		if !r.allow(key) {
			response.Fail(c, apperr.Newf(apperr.CodeForbidden, "请求过于频繁，请稍后重试（上限 %d 次/分钟）", r.limit))
			return
		}
		c.Next()
	}
}

// allow 判断是否放行。
func (r *RateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[key]
	if !ok || now.After(b.resetAt) {
		r.buckets[key] = &bucket{count: 1, resetAt: now.Add(r.window)}
		return true
	}
	if b.count >= r.limit {
		return false
	}
	b.count++
	return true
}

// gc 清理过期桶。
func (r *RateLimiter) gc() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.Lock()
		now := time.Now()
		for key, b := range r.buckets {
			if now.After(b.resetAt) {
				delete(r.buckets, key)
			}
		}
		r.mu.Unlock()
	}
}

// Auth 校验 JWT 并注入会话。
func Auth(auth *service.AuthService, cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c, cfg.JWT.CookieName)
		if token == "" {
			response.Fail(c, apperr.New(apperr.CodeUnauthorized, ""))
			return
		}
		claims, err := auth.ParseToken(token)
		if err != nil {
			if service.IsExpired(err) {
				response.Fail(c, apperr.New(apperr.CodeTokenExpired, ""))
				return
			}
			response.Fail(c, apperr.New(apperr.CodeTokenInvalid, ""))
			return
		}
		// 每次都从数据库重建权限，保证角色/权限变更即时生效，并校验账号状态。
		session, err := auth.SessionOf(c.Request.Context(), claims.UserID)
		if err != nil {
			response.Fail(c, err)
			return
		}
		session.Token = token
		c.Set(ContextSessionKey, session)
		c.Set(ContextOperatorKey, service.Operator{
			UserID:   session.User.ID,
			Username: session.User.Username,
			IP:       c.ClientIP(),
			Agent:    truncate(c.Request.UserAgent(), 255),
		})
		c.Next()
	}
}

// RequirePerm 要求指定权限点。
func RequirePerm(auth *service.AuthService, perm string) gin.HandlerFunc {
	return func(c *gin.Context) {
		session := SessionOf(c)
		if err := auth.Require(session, perm); err != nil {
			response.Fail(c, err)
			return
		}
		c.Next()
	}
}

// extractToken 依次从 Authorization 头与 Cookie 中提取令牌。
func extractToken(c *gin.Context, cookieName string) string {
	header := c.GetHeader("Authorization")
	if header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			return strings.TrimSpace(parts[1])
		}
		return strings.TrimSpace(header)
	}
	if cookieName != "" {
		if cookie, err := c.Cookie(cookieName); err == nil {
			return cookie
		}
	}
	return ""
}

// SessionOf 从上下文提取会话。
func SessionOf(c *gin.Context) *service.Session {
	value, ok := c.Get(ContextSessionKey)
	if !ok {
		return nil
	}
	session, ok := value.(*service.Session)
	if !ok {
		return nil
	}
	return session
}

// OperatorOf 从上下文提取操作者。
func OperatorOf(c *gin.Context) service.Operator {
	value, ok := c.Get(ContextOperatorKey)
	if !ok {
		return service.Operator{IP: c.ClientIP()}
	}
	op, ok := value.(service.Operator)
	if !ok {
		return service.Operator{IP: c.ClientIP()}
	}
	return op
}

// ScopeOf 提取当前请求的数据权限范围。
func ScopeOf(c *gin.Context) service.Scope {
	session := SessionOf(c)
	if session == nil {
		return service.Scope{}
	}
	envs := session.EnvScope
	groups := session.GroupScope
	if session.User != nil && session.User.RoleCode == service.RoleAdmin && len(envs) == 0 {
		return service.Scope{Owner: session}
	}
	return service.Scope{EnvScope: envs, GroupScope: groups, Owner: session}
}

// truncate 截断字符串。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// WithContext 返回绑定请求的 context（供服务层使用）。
func WithContext(c *gin.Context) context.Context { return c.Request.Context() }
