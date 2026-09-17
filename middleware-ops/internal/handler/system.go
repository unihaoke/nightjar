package handler

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/config"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// Overview 全局大盘。
func (h *Handler) Overview(c *gin.Context) {
	data, err := h.deps.Dashboard.Overview(c.Request.Context(), h.session(c), h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// SystemInfo 返回系统信息与能力矩阵（对齐设计文档的能力声明）。
func (h *Handler) SystemInfo(c *gin.Context) {
	cfg := h.deps.Config
	strategy, providers := engine.Keys(cfg)
	response.OK(c, gin.H{
		"app": gin.H{
			"name": cfg.App.Name, "version": cfg.App.Version, "mode": cfg.App.Mode,
			"started_at": h.deps.StartedAt, "now": nowUnix(),
		},
		"ai_engine": gin.H{
			"strategy": strategy, "providers": providers, "notes": h.deps.EngineFactory.Notes(),
			"status":        service.WatchEngineStatus(h.deps.Engine),
			"tools":         h.deps.Registry.Names(),
			"eval_set_size": len(guardrailEvalSet()),
		},
		"infrastructure": gin.H{
			"database":   gin.H{"host": cfg.Database.Host, "name": cfg.Database.Name, "auto_migrate": cfg.Database.AutoMigrate},
			"redis":      gin.H{"addr": cfg.Redis.Addr, "cache_kind": h.deps.Cache.Kind(), "queue_kind": h.deps.Queue.Kind()},
			"prometheus": gin.H{"base_url": cfg.Prometheus.BaseURL, "source": h.deps.Metrics.MonitorKind()},
			"vector":     gin.H{"native_pgvector": engine.VectorNativeAvailable()},
		},
		"capability_matrix": capabilityMatrix(),
		"guardrail": gin.H{
			"input_token_budget":  h.deps.Guardrails.InputTokenBudget,
			"output_token_budget": h.deps.Guardrails.OutputTokenBudget,
			"max_concurrency":     h.deps.Guardrails.MaxConcurrency,
			"cache_ttl":           h.deps.Guardrails.CacheTTL.String(),
			"daily_token_quota":   h.deps.Guardrails.DailyTokenQuota,
			"per_user_quota":      h.deps.Guardrails.PerUserDailyTokenQuota,
			"sql_limits":          gin.H{"default": h.deps.Guardrails.SQLDefaultLimit, "max": h.deps.Guardrails.SQLMaxLimit},
			"log_budget":          gin.H{"context_lines": h.deps.Guardrails.LogContextLines, "max_sources": h.deps.Guardrails.MaxLogSources},
			"code_budget":         gin.H{"max_files": h.deps.Guardrails.MaxCodeFiles, "lines_per_file": h.deps.Guardrails.CodeLinesPerFile},
			"loop_guard":          gin.H{"max_steps": h.deps.Guardrails.MaxSteps, "repeat_threshold": h.deps.Guardrails.LoopRepeatThreshold},
		},
		"security": gin.H{
			"outbound_whitelist":    cfg.Security.OutboundWhitelist,
			"snippet_max_lines":     cfg.Security.CodeSnippetMaxLines,
			"key_rotation_days":     cfg.Security.MasterKeyRotationDays,
			"master_key_configured": cfg.Security.MasterKey != "" || cfg.Security.MasterKeyFile != "",
		},
		"notify": h.deps.Notifier.ChannelStatus(),
	})
}

// Health 健康检查（无需认证）。
//
// playbook_renderer 是「远程安装 playbook 渲染器」的版本号：远程安装报 YAML/Jinja 语法错时，
// 先 curl 这个端点即可判断跑的是不是旧镜像（字段缺失或版本低于代码里的常量
// 就说明后端镜像没重建，见 docs/POSTMORTEM.md INC-005/INC-006）。
// sshpass 是平台侧"能否用口令 SSH 登录目标机"的能力位（INC-007）。
func (h *Handler) Health(c *gin.Context) {
	response.OK(c, gin.H{
		"status":            "healthy",
		"version":           h.deps.Config.App.Version,
		"uptime":            nowUnix() - h.deps.StartedAt,
		"engine":            service.WatchEngineStatus(h.deps.Engine),
		"playbook_renderer": integration.PlaybookRendererVersion,
		// 平台代码修订号：远程安装/诊断类问题先比这个值，判断镜像里有没有对应修复。
		"code_revision": service.CodeRevision,
		// 口令方式远程安装依赖平台侧的 sshpass：这里直接暴露探测结果，
		// 排障时一眼看出是"镜像旧"还是"镜像没装 sshpass"（见 INC-007）。
		"sshpass": service.SSHPassAvailable(),
	})
}

// Metrics 暴露平台自身 Prometheus 指标（10. 平台自身可观测性）。
func (h *Handler) Metrics(c *gin.Context) {
	// 指标采集通过 prometheus/client_golang 的默认注册表暴露；
	// 此处仅返回文本格式的运行时摘要，完整指标由 /metrics 端点承接。
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(200, platformMetrics(h))
}

// platformMetrics 生成平台指标文本。
func platformMetrics(h *Handler) string {
	out := "# HELP mwops_up 平台存活\n# TYPE mwops_up gauge\nmwops_up 1\n"
	out += "# HELP mwops_engine_available AI 引擎可用性\n# TYPE mwops_engine_available gauge\n"
	available := 0
	if h.deps.Engine != nil && h.deps.Engine.Available() {
		available = 1
	}
	out += "mwops_engine_available " + strconv.Itoa(available) + "\n"
	out += "# HELP mwops_cache_degraded 缓存是否降级\n# TYPE mwops_cache_degraded gauge\n"
	degraded := 0
	if h.deps.Cache == nil || h.deps.Cache.Kind() != "redis" {
		degraded = 1
	}
	out += "mwops_cache_degraded " + strconv.Itoa(degraded) + "\n"
	out += "# HELP mwops_guardrail_concurrency 诊断最大并发\n# TYPE mwops_guardrail_concurrency gauge\n"
	out += "mwops_guardrail_concurrency " + strconv.Itoa(h.deps.Guardrails.MaxConcurrency) + "\n"
	return out
}

// nowUnix 返回当前秒级时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// guardrailEvalSet 返回评测集规模（不暴露用例内容）。
func guardrailEvalSet() []guardrail.EvalCase { return guardrail.DefaultEvalSet() }

// capabilityMatrix 返回能力矩阵（4.1，声明与实现一致）。
func capabilityMatrix() []map[string]any {
	rows := []struct {
		mwType  string
		manage  bool
		metrics bool
		alerts  bool
		ai      bool
		note    string
	}{
		{"redis", true, true, true, true, "一期核心"},
		{"mysql", true, true, true, true, "一期核心"},
		{"pg", true, true, true, true, "一期核心"},
		{"kafka", true, true, true, true, "一期核心"},
		{"es", true, true, true, true, "一期核心"},
		{"nginx", true, true, true, false, "仅基础监控，不含 AI 诊断"},
		{"rabbitmq", true, false, false, false, "二期：本版本仅纳管"},
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, map[string]any{
			"mw_type": r.mwType, "manage": r.manage, "metrics": r.metrics,
			"alerts": r.alerts, "ai_diagnose": r.ai, "note": r.note,
		})
	}
	return out
}

// ConfigView 返回脱敏后的运行配置（管理员）。
func (h *Handler) ConfigView(c *gin.Context) {
	cfg := h.deps.Config
	response.OK(c, gin.H{
		"redacted": engine.RedactConfig(cfg),
		"app":      gin.H{"name": cfg.App.Name, "version": cfg.App.Version, "mode": cfg.App.Mode},
		"server":   gin.H{"host": cfg.Server.Host, "port": cfg.Server.Port, "rate_limit_per_minute": cfg.Server.RateLimitPerMinute},
		"scheduler": gin.H{
			"enabled": cfg.Scheduler.Enabled, "health_probe": cfg.Scheduler.HealthProbe.String(),
			"rule_eval": cfg.Scheduler.MetricRuleEval.String(), "alert_cluster": cfg.Scheduler.AlertCluster.String(),
			"audit_snapshot_cron": cfg.Scheduler.AuditSnapshot,
		},
		"config_file": configFileHint(cfg),
	})
}

// configFileHint 提示配置文件位置（不泄露内容）。
func configFileHint(cfg *config.Config) string {
	if cfg.IsProd() {
		return "release 模式：请通过环境变量或挂载配置管理"
	}
	return "./configs/config.yaml 或环境变量 MWOPS_*"
}
