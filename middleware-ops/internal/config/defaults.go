package config

import (
	"time"

	"github.com/spf13/viper"
)

// setDefaults 写入全部默认值。
//
// 默认值即「单机零外部依赖可启动」的最小集合：Redis 缺省降级为内存实现，
// Prometheus 缺省使用内置指标源，AI 引擎缺省走自托管 + 规则兜底。
func setDefaults(v *viper.Viper) {
	v.SetDefault("app.name", "middleware-ops")
	v.SetDefault("app.version", "1.0.0")
	v.SetDefault("app.mode", "debug")

	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.read_timeout", 30*time.Second)
	v.SetDefault("server.write_timeout", 0) // SSE/WebSocket 需要禁用写超时
	v.SetDefault("server.shutdown_timeout", 15*time.Second)
	v.SetDefault("server.trusted_proxies", []string{"127.0.0.1", "::1"})
	v.SetDefault("server.rate_limit_per_minute", 600)

	v.SetDefault("database.host", "127.0.0.1")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.user", "mwo")
	v.SetDefault("database.password", "mwo")
	v.SetDefault("database.name", "middleware_ops")
	v.SetDefault("database.ssl_mode", "disable")
	v.SetDefault("database.time_zone", "UTC")
	v.SetDefault("database.max_open_conns", 50)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime_minutes", 60)
	v.SetDefault("database.auto_migrate", true)

	v.SetDefault("redis.addr", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 20)
	v.SetDefault("redis.dial_timeout", 3*time.Second)
	v.SetDefault("redis.read_timeout", 3*time.Second)
	v.SetDefault("redis.write_timeout", 3*time.Second)
	v.SetDefault("redis.stream_key", "mwops:ai:tasks")
	v.SetDefault("redis.group", "mwops-ai-workers")
	v.SetDefault("redis.queue_workers", 4)
	v.SetDefault("redis.fallback_to_memory", true)

	v.SetDefault("jwt.secret", "dev-only-secret-please-change-in-release-mode")
	v.SetDefault("jwt.issuer", "middleware-ops")
	v.SetDefault("jwt.access_ttl", 8*time.Hour)
	v.SetDefault("jwt.refresh_ttl", 7*24*time.Hour)
	v.SetDefault("jwt.cookie_name", "mwops_token")
	v.SetDefault("jwt.cookie_secure", false)
	v.SetDefault("jwt.cookie_same_site", "lax")
	v.SetDefault("jwt.bootstrap_admin", "admin")
	v.SetDefault("jwt.bootstrap_password", "Admin@12345")

	v.SetDefault("security.master_key_file", "./data/master.key")
	v.SetDefault("security.master_key", "")
	v.SetDefault("security.master_key_rotation_days", 90)
	v.SetDefault("security.outbound_whitelist", []string{})
	v.SetDefault("security.sensitive_patterns", []string{})
	v.SetDefault("security.code_snippet_max_lines", 200)

	v.SetDefault("ai_engine.strategy", "hybrid")
	v.SetDefault("ai_engine.third_party.enabled", false)
	v.SetDefault("ai_engine.third_party.kind", "openai")
	v.SetDefault("ai_engine.third_party.base_url", "https://api.deepseek.com/v1")
	v.SetDefault("ai_engine.third_party.model", "deepseek-chat")
	v.SetDefault("ai_engine.third_party.max_tokens", 2048)
	v.SetDefault("ai_engine.third_party.price_per_k_token", 0.002)
	v.SetDefault("ai_engine.third_party.timeout.connect", 5*time.Second)
	v.SetDefault("ai_engine.third_party.timeout.first_byte", 15*time.Second)
	v.SetDefault("ai_engine.third_party.timeout.total", 60*time.Second)
	v.SetDefault("ai_engine.third_party.timeout.tool_call", 10*time.Second)
	v.SetDefault("ai_engine.third_party.timeout.task_deadline", 2*time.Minute)

	v.SetDefault("ai_engine.self_hosted.enabled", true)
	v.SetDefault("ai_engine.self_hosted.kind", "mock")
	v.SetDefault("ai_engine.self_hosted.base_url", "http://127.0.0.1:11434/v1")
	v.SetDefault("ai_engine.self_hosted.model", "qwen2.5:7b")
	v.SetDefault("ai_engine.self_hosted.max_tokens", 2048)
	v.SetDefault("ai_engine.self_hosted.price_per_k_token", 0)
	v.SetDefault("ai_engine.self_hosted.timeout.connect", 5*time.Second)
	v.SetDefault("ai_engine.self_hosted.timeout.first_byte", 15*time.Second)
	v.SetDefault("ai_engine.self_hosted.timeout.total", 60*time.Second)
	v.SetDefault("ai_engine.self_hosted.timeout.tool_call", 10*time.Second)
	v.SetDefault("ai_engine.self_hosted.timeout.task_deadline", 2*time.Minute)

	v.SetDefault("ai_engine.fallback.failure_threshold", 2)
	v.SetDefault("ai_engine.fallback.open_duration", 60*time.Second)
	v.SetDefault("ai_engine.fallback.rules_engine_enabled", true)

	v.SetDefault("prometheus.base_url", "")
	v.SetDefault("prometheus.timeout", 8*time.Second)
	v.SetDefault("prometheus.cache_ttl", 15*time.Second)
	v.SetDefault("prometheus.retention", 15*24*time.Hour)
	v.SetDefault("prometheus.exporter_job_prefix", "middleware-exporter")

	v.SetDefault("integration.enabled", true)
	v.SetDefault("integration.output_dir", "./data/integrations")
	v.SetDefault("integration.file_sd_name", "integrations.json")
	v.SetDefault("integration.sd_token", "")
	v.SetDefault("integration.job_name", "middleware-integration")
	v.SetDefault("integration.auto_rules", true)
	v.SetDefault("integration.docker_enabled", false)
	v.SetDefault("integration.docker_host", "unix:///var/run/docker.sock")
	v.SetDefault("integration.exporter_network", "mwops")
	v.SetDefault("integration.default_environment", "dev")

	v.SetDefault("notify.enabled", true)
	v.SetDefault("notify.feishu.enabled", false)
	v.SetDefault("notify.wecom.enabled", false)
	v.SetDefault("notify.dingtalk.enabled", false)
	v.SetDefault("notify.email.enabled", false)
	v.SetDefault("notify.email.port", 465)
	v.SetDefault("notify.email.use_tls", true)
	v.SetDefault("notify.card_confirm_path", "/alerts")

	v.SetDefault("guardrail.input_token_budget", 8192)
	v.SetDefault("guardrail.output_token_budget", 2048)
	v.SetDefault("guardrail.log_context_lines", 20)
	v.SetDefault("guardrail.max_log_sources", 5)
	v.SetDefault("guardrail.max_code_files", 5)
	v.SetDefault("guardrail.code_lines_per_file", 200)
	v.SetDefault("guardrail.max_steps", 8)
	v.SetDefault("guardrail.loop_repeat_threshold", 2)
	v.SetDefault("guardrail.llm_retry", 2)
	v.SetDefault("guardrail.cache_ttl", 24*time.Hour)
	v.SetDefault("guardrail.daily_token_quota", 2000000)
	v.SetDefault("guardrail.per_user_daily_token_quota", 200000)
	v.SetDefault("guardrail.max_concurrency", 4)
	v.SetDefault("guardrail.vector_reference_threshold", 0.78)
	v.SetDefault("guardrail.sql_default_limit", 100)
	v.SetDefault("guardrail.sql_max_limit", 1000)
	v.SetDefault("guardrail.sql_table_allowlist", []string{
		"information_schema.tables", "pg_stat_activity", "pg_stat_statements",
	})

	v.SetDefault("scheduler.enabled", true)
	v.SetDefault("scheduler.health_probe_interval", time.Minute)
	v.SetDefault("scheduler.metric_rule_eval_interval", 30*time.Second)
	v.SetDefault("scheduler.alert_cluster_interval", 5*time.Minute)
	v.SetDefault("scheduler.audit_snapshot_cron", "0 10 0 * * *")
	v.SetDefault("scheduler.approval_expire_interval", time.Minute)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.file_path", "./data/logs/middleware-ops.log")
	v.SetDefault("log.max_size_mb", 64)
	v.SetDefault("log.max_backups", 10)
	v.SetDefault("log.max_age_days", 30)
	v.SetDefault("log.console", true)
}
