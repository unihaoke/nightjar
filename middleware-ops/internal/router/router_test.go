package router

import (
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/service"
)

// TestSettingsRoutesRegistered 锁定「平台自管设置」的 7 个接口路径。
//
// 为什么值得单测：gin 在注册冲突路由时会直接 panic（例如出现 /settings/ai 与 /settings/:name
// 这类同时匹配的形状），编译期完全看不出来；而路由一旦改名，前端是按契约写死的，
// 表现就是"设置页整页 404"。这里用最轻量的方式（空依赖 + 只比对路由表）把它钉住。
func TestSettingsRoutesRegistered(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engineRouter := New(Options{
		Config: &config.Config{},
		Log:    zap.NewNop(),
		Deps:   &service.Deps{},
	})

	registered := make(map[string]bool, len(engineRouter.Routes()))
	for _, route := range engineRouter.Routes() {
		registered[route.Method+" "+route.Path] = true
	}
	want := []string{
		"GET /api/settings/ai",
		"PUT /api/settings/ai",
		"GET /api/settings/ai/usage",
		"POST /api/settings/ai/test",
		"GET /api/settings/notify",
		"PUT /api/settings/notify",
		"POST /api/settings/notify/test",
	}
	for _, route := range want {
		if !registered[route] {
			t.Fatalf("缺少路由 %s（前端「系统设置」页按该契约调用）", route)
		}
	}
}
