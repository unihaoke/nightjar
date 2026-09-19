package config

import (
	"time"

	"github.com/spf13/viper"
)

// setDefaults 写入全部默认值。
//
// 默认值即「单机零外部依赖可启动」的最小集合：Redis 缺省降级为内存实现，
// Prometheus 未配置 base_url 时监控数据源禁用：**平台只使用真实数据**，
// AI 引擎缺省走自托管 + 规则兜底。
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
	// base_url 为空时不返回任何指标（页面显示「无数据」）：平台只使用真实数据，
	// 历史上用于生成模拟指标的 mock_enabled 开关已移除。
	// 关键：**空结果绝不补齐**——选择器写错时 Prometheus 会正常返回空序列，
	// 若在这里补假数据，前端就会画出一条看起来很正常的曲线，把"实例其实没接上"彻底掩盖。

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
	// 远程安装（Ansible）：默认开启——远程是集成中心的默认部署路径，
	// 关掉它等于让主功能不可用。生产若要求更严可显式设为 false（prod 环境本就走审批工单）。
	v.SetDefault("integration.allow_remote_install", true)
	v.SetDefault("integration.ansible.enabled", true)
	v.SetDefault("integration.ansible.binary", "ansible-playbook")
	v.SetDefault("integration.ansible.inventory_dir", "./data/ansible")
	v.SetDefault("integration.ansible.timeout", 15*time.Minute)
	v.SetDefault("integration.ansible.default_ssh_port", 22)
	v.SetDefault("integration.ansible.become", true)
	v.SetDefault("integration.ansible.install_mode", "docker")
	v.SetDefault("integration.ansible.install_dir", "/opt/mwops-exporter")
	v.SetDefault("integration.ansible.docker_network", "host")
	v.SetDefault("integration.ansible.check_mode", false)
	v.SetDefault("integration.ansible.extra_args", []string{})

	v.SetDefault("notify.enabled", true)
	v.SetDefault("notify.feishu.enabled", false)
	v.SetDefault("notify.wecom.enabled", false)
	v.SetDefault("notify.dingtalk.enabled", false)
	v.SetDefault("notify.email.enabled", false)
	v.SetDefault("notify.email.port", 465)
	v.SetDefault("notify.email.use_tls", true)
	v.SetDefault("notify.card_confirm_path", "/alerts")

	// 日志总线（日志集成）：enabled 默认 true 但 brokers 默认为空——
	// "没配地址"是合法的降级态（裸机调试、暂不接日志），平台照常启动。
	v.SetDefault("kafka.enabled", true)
	v.SetDefault("kafka.brokers", []string{})
	v.SetDefault("kafka.log_topic", "mwops-logs")
	v.SetDefault("kafka.group_id", "mwops-log-ingest")
	v.SetDefault("kafka.client_id", "mwops-backend")
	v.SetDefault("kafka.external_host", "127.0.0.1")
	v.SetDefault("kafka.external_port", 9092)
	v.SetDefault("kafka.filebeat_version", "8.16.0")
	v.SetDefault("kafka.max_bytes", 10*1024*1024)
	v.SetDefault("kafka.session_timeout", 30*time.Second)
	v.SetDefault("kafka.commit_interval", time.Second)
	// 首次消费从头开始：宁可重复处理（日志事件本身按指纹去重），也不要漏掉积压日志。
	v.SetDefault("kafka.start_offset", "earliest")

	// 日志告警的后处理运行参数。
	// 注意：这里**不设任何规则默认值**——日志告警规则一律在平台上新增后才生效，
	// 没有命中规则的日志不产生事件（没有 default_* 兜底，避免"零配置也在告警"）。
	v.SetDefault("log_alert.worker_interval_seconds", 15)
	v.SetDefault("log_alert.worker_batch", 10)
	v.SetDefault("log_alert.analyze_timeout", 2*time.Minute)

	// AI 代码分析用的仓库本地缓存：首次 clone、之后按最小间隔更新（见 internal/repo）。
	v.SetDefault("code_repo.cache_dir", "./data/repos")
	v.SetDefault("code_repo.clone_timeout", 10*time.Minute)
	v.SetDefault("code_repo.pull_timeout", 2*time.Minute)
	v.SetDefault("code_repo.refresh_interval_seconds", 300)
	v.SetDefault("code_repo.allow_outbound", true)

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
