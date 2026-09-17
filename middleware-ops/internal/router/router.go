// Package router 注册全部 HTTP 路由。
//
// 权限点与操作级别在路由层显式声明（8.2 接口总览）：
// L0 只读、L1 低危（直接执行并留痕）、L2 高危（由审批服务拦截）。
package router

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/handler"
	mw "middleware-ops/internal/middleware"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// Options 是路由构造参数。
type Options struct {
	Config *config.Config
	Log    *zap.Logger
	Deps   *service.Deps
	// HookToken 为日志/告警上报接口的共享令牌（为空表示不校验，仅建议内网使用）。
	HookToken string
}

// New 构造 Gin 引擎。
func New(opt Options) *gin.Engine {
	cfg := opt.Config
	if cfg.App.Mode == "release" {
		gin.SetMode(gin.ReleaseMode)
	}
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	// SSE 与 WebSocket 需要禁用写超时；读超时保持较宽以支撑大请求体。
	engine.MaxMultipartMemory = 8 << 20
	if err := engine.SetTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		opt.Log.Warn("设置受信代理失败，客户端 IP 可能不准确", zap.Error(err))
	}

	h := handler.New(opt.Deps)
	limiter := mw.NewRateLimiter(cfg.Server.RateLimitPerMinute)

	engine.Use(
		mw.RequestID(),
		mw.Recovery(opt.Log),
		mw.Logger(opt.Log),
		mw.SecurityHeaders(),
		mw.CORS(nil),
		limiter.Middleware(),
	)

	// 健康检查与自身指标（无需认证）。
	engine.GET("/healthz", h.Health)
	engine.GET("/metrics", h.Metrics)
	// 集成中心的服务发现（Prometheus http_sd_configs 拉取，公开接口）。
	// 只暴露被管实例地址与标签，不含口令；需要鉴权时设置 integration.sd_token。
	engine.GET("/api/sd/integrations", h.IntegrationServiceDiscovery)

	api := engine.Group("/api")

	// 认证相关（部分无需鉴权）。
	api.POST("/auth/login", h.Login)
	auth := api.Group("")
	auth.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		auth.POST("/auth/logout", h.Logout)
		auth.GET("/auth/profile", h.Profile)
		auth.POST("/auth/password", h.ChangePassword)
	}

	// 中间件纳管（4.1）：读 L1、写 L1、删除 prod 走审批。
	instances := api.Group("/middlewares")
	instances.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		instances.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.ListMiddlewares)
		instances.GET("/options", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.MiddlewareOptions)
		instances.POST("/test", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.TestMiddlewareEndpoint)
		instances.GET("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.GetMiddleware)
		instances.POST("", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.CreateMiddleware)
		instances.PUT("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.UpdateMiddleware)
		instances.DELETE("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.DeleteMiddleware)
		instances.POST("/:id/test", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.TestMiddleware)
		instances.POST("/:id/health", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.HealthMiddleware)
	}

	// 集成中心（对齐云厂商控制台的一键集成）：页面选组件 → 填参数 → 自动暴露指标。
	// 与「中间件纳管」共用权限点：集成产物本身就是一个纳管实例。
	integrations := api.Group("/integrations")
	integrations.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		integrations.GET("/overview", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.IntegrationOverview)
		integrations.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.ListIntegrations)
		integrations.GET("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.GetIntegration)
		// 监控账号管理：查看 / 轮换口令（账号改自己口令，无需管理员凭据）/ 删除账号（需管理员凭据）
		integrations.GET("/accounts", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.ListIntegrationAccounts)
		integrations.POST("/preview", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.PreviewIntegration)
		// 日志接入：平台读取被管容器的 docker 配置反查日志位置，自建采集容器（被管项目零改动）
		integrations.POST("/logs/preview", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.PreviewLogCollect)
		integrations.POST("/logs", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.CreateLogCollect)
		integrations.POST("", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.CreateIntegration)
		integrations.PUT("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.UpdateIntegration)
		integrations.POST("/:id/apply", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.ApplyIntegration)
		integrations.POST("/:id/account/rotate", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.RotateIntegrationAccount)
		// 失败重试：带管理凭据=幂等重建账号；不带=只测连接 + 重建 Exporter
		integrations.POST("/:id/account/retry", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.RetryIntegrationAccount)
		integrations.POST("/:id/account/probe", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareRead), h.ProbeIntegrationAccount)
		integrations.POST("/:id/account/drop", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.DropIntegrationAccount)
		integrations.DELETE("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermMiddlewareWrite), h.DeleteIntegration)
	}

	// 统一监控（4.2）：全部 L0。
	metricsGroup := api.Group("/metrics")
	metricsGroup.Use(mw.Auth(opt.Deps.Auth, cfg), mw.RequirePerm(opt.Deps.Auth, service.PermMonitorRead))
	{
		metricsGroup.GET("/catalog", h.MetricsCatalog)
		metricsGroup.GET("/compare", h.CompareMetrics)
		metricsGroup.GET("/:id", h.GetMetrics)
		metricsGroup.GET("/:id/history", h.GetMetricsHistory)
		// 接入自检：纳管后"看不到监控"时先看这里，而不是靠猜。
		metricsGroup.GET("/:id/diagnose", h.DiagnoseMetrics)
	}

	// AI 诊断中心（4.3）：L0。
	aiGroup := api.Group("/ai")
	aiGroup.Use(mw.Auth(opt.Deps.Auth, cfg), mw.RequirePerm(opt.Deps.Auth, service.PermAIUse))
	{
		aiGroup.POST("/diagnose", h.Diagnose)          // SSE 流式
		aiGroup.POST("/diagnose/sync", h.DiagnoseSync) // 同步
		aiGroup.GET("/diagnosis-history", h.DiagnosisHistory)
		aiGroup.GET("/diagnosis/:id", h.DiagnosisDetail)
		aiGroup.POST("/diagnosis/:id/feedback", h.DiagnosisFeedback)
		aiGroup.GET("/quality", h.AIQuality)
		aiGroup.GET("/stream", h.StreamLogs)
	}
	// 代码分析（4.8.3）：L1。
	aiGroup.POST("/code-analyze", mw.RequirePerm(opt.Deps.Auth, service.PermCodeAnalyze), h.AnalyzeCode)
	aiGroup.GET("/code-analyses", mw.RequirePerm(opt.Deps.Auth, service.PermCodeAnalyze), h.ListCodeAnalyses)

	// 告警治理（4.4）。
	alerts := api.Group("/alerts")
	alerts.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		alerts.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermAlertRead), h.ListAlerts)
		alerts.GET("/options", mw.RequirePerm(opt.Deps.Auth, service.PermAlertRead), h.AlertOptions)
		alerts.GET("/history", mw.RequirePerm(opt.Deps.Auth, service.PermAlertRead), h.ListAlerts)
		alerts.GET("/rules", mw.RequirePerm(opt.Deps.Auth, service.PermAlertRead), h.ListAlertRules)
		alerts.POST("/rules", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.CreateAlertRule)
		alerts.PUT("/rules/:id", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.UpdateAlertRule)
		alerts.DELETE("/rules/:id", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.DeleteAlertRule)
		alerts.POST("/evaluate", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.EvaluateAlerts)
		alerts.POST("/cluster", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.ClusterAlerts)
		alerts.POST("/:id/ack", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.AckAlert)
		alerts.POST("/:id/resolve", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.ResolveAlert)
		alerts.POST("/notify-test", mw.RequirePerm(opt.Deps.Auth, service.PermAlertWrite), h.NotifyTest)
	}

	// 知识库（4.5）。
	knowledge := api.Group("/knowledge")
	knowledge.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		knowledge.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeRead), h.ListKnowledge)
		knowledge.GET("/stats", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeRead), h.KnowledgeStats)
		knowledge.GET("/options", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeRead), h.KnowledgeOptions)
		knowledge.GET("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeRead), h.GetKnowledge)
		knowledge.POST("", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeWrite), h.CreateKnowledge)
		knowledge.PUT("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeWrite), h.UpdateKnowledge)
		knowledge.POST("/:id/adopt", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeWrite), h.AdoptKnowledge)
		knowledge.DELETE("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermKnowledgeWrite), h.DeleteKnowledge)
	}

	// 修复执行（4.6）：预览 L0，执行 L1/L2。
	fix := api.Group("/fix")
	fix.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		fix.GET("/options", mw.RequirePerm(opt.Deps.Auth, service.PermFixPreview), h.FixOptions)
		fix.POST("/preview", mw.RequirePerm(opt.Deps.Auth, service.PermFixPreview), h.PreviewFix)
		fix.POST("/execute", mw.RequirePerm(opt.Deps.Auth, service.PermFixExecute), h.ExecuteFix)
		fix.GET("/history", mw.RequirePerm(opt.Deps.Auth, service.PermFixPreview), h.FixHistory)
		fix.POST("/validate-sql", mw.RequirePerm(opt.Deps.Auth, service.PermSQLRead), h.ValidateSQL)
	}

	// 审计（4.7）：管理员/运维。
	audit := api.Group("/audit")
	audit.Use(mw.Auth(opt.Deps.Auth, cfg), mw.RequirePerm(opt.Deps.Auth, service.PermAuditRead))
	{
		audit.GET("/logs", h.ListAuditLogs)
		audit.GET("/logs/:id", h.GetAuditLog)
		audit.GET("/verify", h.VerifyAudit)
		audit.GET("/snapshots", h.ListAuditSnapshots)
		audit.POST("/snapshot", mw.RequirePerm(opt.Deps.Auth, service.PermAuditSnapshot), h.SnapshotAudit)
	}

	// 日志告警域（4.8）。
	logAlerts := api.Group("/log-alerts")
	logAlerts.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		logAlerts.GET("/events", mw.RequirePerm(opt.Deps.Auth, service.PermLogAlertRead), h.ListLogEvents)
		logAlerts.GET("/events/:id", mw.RequirePerm(opt.Deps.Auth, service.PermLogAlertRead), h.GetLogEvent)
		logAlerts.PUT("/events/:id/status", mw.RequirePerm(opt.Deps.Auth, service.PermLogAlertWrite), h.UpdateLogEventStatus)
		logAlerts.GET("/servers", mw.RequirePerm(opt.Deps.Auth, service.PermLogAlertRead), h.ListServers)
		logAlerts.POST("/servers", mw.RequirePerm(opt.Deps.Auth, service.PermServerManage), h.CreateServer)
		logAlerts.PUT("/servers/:id", mw.RequirePerm(opt.Deps.Auth, service.PermServerManage), h.UpdateServer)
		logAlerts.DELETE("/servers/:id", mw.RequirePerm(opt.Deps.Auth, service.PermServerManage), h.DeleteServer)
		logAlerts.GET("/code-repos", mw.RequirePerm(opt.Deps.Auth, service.PermLogAlertRead), h.ListCodeRepos)
		logAlerts.POST("/code-repos", mw.RequirePerm(opt.Deps.Auth, service.PermServerManage), h.SaveCodeRepo)
	}

	// 日志/告警上报 Hook（供 Agent 与应用直接调用，使用共享令牌鉴权）。
	hooks := api.Group("/hooks")
	hooks.Use(hookAuth(opt.HookToken))
	{
		hooks.POST("/logs", h.IngestLogHook)
		hooks.POST("/alerts", h.IngestAlert)
	}

	// 系统与大盘（8.2）。
	system := api.Group("/system")
	system.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		system.GET("/overview", mw.RequirePerm(opt.Deps.Auth, service.PermSystemOverview), h.Overview)
		system.GET("/info", h.SystemInfo)
		system.GET("/config", mw.RequirePerm(opt.Deps.Auth, service.PermSystemConfigRead), h.ConfigView)
	}

	users := api.Group("/users")
	users.Use(mw.Auth(opt.Deps.Auth, cfg), mw.RequirePerm(opt.Deps.Auth, service.PermUserManage))
	{
		users.GET("", h.ListUsers)
		users.POST("", h.CreateUser)
		users.PUT("/:id", h.UpdateUser)
		users.DELETE("/:id", h.DeleteUser)
	}

	roles := api.Group("/roles")
	roles.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		roles.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermUserManage), h.ListRoles)
		roles.PUT("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermUserManage), h.UpdateRole)
	}

	approvals := api.Group("/approvals")
	approvals.Use(mw.Auth(opt.Deps.Auth, cfg))
	{
		approvals.GET("", mw.RequirePerm(opt.Deps.Auth, service.PermApprovalRead), h.ListApprovals)
		approvals.GET("/:id", mw.RequirePerm(opt.Deps.Auth, service.PermApprovalRead), h.GetApproval)
		approvals.POST("/:id/decide", mw.RequirePerm(opt.Deps.Auth, service.PermApprovalDecide), h.DecideApproval)
	}

	engine.NoRoute(func(c *gin.Context) {
		response.Fail(c, apperr.Newf(apperr.CodeNotFound, "接口不存在: %s %s", c.Request.Method, c.Request.URL.Path))
	})
	return engine
}

// hookAuth 校验上报 Hook 的共享令牌。
//
// 令牌为空时打印告警并放行（内网单机部署场景），生产环境必须配置。
func hookAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			c.Next()
			return
		}
		provided := c.GetHeader("X-Hook-Token")
		if provided == "" {
			provided = c.Query("token")
		}
		if provided != token {
			c.AbortWithStatusJSON(http.StatusUnauthorized, response.Body{
				Code: int(apperr.CodeUnauthorized), Message: "Hook 令牌无效",
			})
			return
		}
		c.Next()
	}
}

// Server 构造 http.Server（便于优雅关闭）。
func Server(cfg *config.Config, h http.Handler) *http.Server {
	return &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		Handler:      h,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  120 * time.Second,
	}
}
