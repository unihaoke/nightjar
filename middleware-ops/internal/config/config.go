// Package config 负责配置加载（YAML + 环境变量覆盖）与默认值治理。
package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config 是平台全部配置的根结构。
type Config struct {
	App         AppConfig         `mapstructure:"app"`
	Server      ServerConfig      `mapstructure:"server"`
	Database    DatabaseConfig    `mapstructure:"database"`
	Redis       RedisConfig       `mapstructure:"redis"`
	JWT         JWTConfig         `mapstructure:"jwt"`
	Security    SecurityConfig    `mapstructure:"security"`
	AIEngine    AIEngineConfig    `mapstructure:"ai_engine"`
	Prometheus  PrometheusConfig  `mapstructure:"prometheus"`
	Integration IntegrationConfig `mapstructure:"integration"`
	Notify      NotifyConfig      `mapstructure:"notify"`
	Guardrail   GuardrailConfig   `mapstructure:"guardrail"`
	Scheduler   SchedulerConfig   `mapstructure:"scheduler"`
	Log         LogConfig         `mapstructure:"log"`
}

// AppConfig 应用元信息。
type AppConfig struct {
	Name    string `mapstructure:"name"`
	Version string `mapstructure:"version"`
	Mode    string `mapstructure:"mode"` // debug / release
}

// ServerConfig HTTP 服务参数。
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	// TrustedProxies 为受信代理列表，决定审计中真实 IP 的取值来源（6.4）。
	TrustedProxies []string `mapstructure:"trusted_proxies"`
	// RateLimitPerMinute 为应用层限流（Nginx 为第一层，见 8.1）。
	RateLimitPerMinute int `mapstructure:"rate_limit_per_minute"`
}

// DatabaseConfig PostgreSQL 连接参数。
type DatabaseConfig struct {
	Host                   string `mapstructure:"host"`
	Port                   int    `mapstructure:"port"`
	User                   string `mapstructure:"user"`
	Password               string `mapstructure:"password"`
	Name                   string `mapstructure:"name"`
	SSLMode                string `mapstructure:"ssl_mode"`
	TimeZone               string `mapstructure:"time_zone"`
	MaxOpenConns           int    `mapstructure:"max_open_conns"`
	MaxIdleConns           int    `mapstructure:"max_idle_conns"`
	ConnMaxLifetimeMinutes int    `mapstructure:"conn_max_lifetime_minutes"`
	AutoMigrate            bool   `mapstructure:"auto_migrate"`
}

// DSN 拼接 PostgreSQL 连接串。
func (d DatabaseConfig) DSN() string {
	ssl := d.SSLMode
	if ssl == "" {
		ssl = "disable"
	}
	tz := d.TimeZone
	if tz == "" {
		tz = "UTC"
	}
	return fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s TimeZone=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, ssl, url.PathEscape(tz))
}

// RedisConfig 缓存 / 队列参数。Addr 为空时自动降级为进程内实现。
type RedisConfig struct {
	Addr         string        `mapstructure:"addr"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	StreamKey    string        `mapstructure:"stream_key"`
	Group        string        `mapstructure:"group"`
	QueueWorkers int           `mapstructure:"queue_workers"`
	// FallbackToMemory 表示 Redis 不可用时是否允许降级为内存实现。
	FallbackToMemory bool `mapstructure:"fallback_to_memory"`
}

// JWTConfig 认证参数。
type JWTConfig struct {
	Secret         string        `mapstructure:"secret"`
	Issuer         string        `mapstructure:"issuer"`
	AccessTTL      time.Duration `mapstructure:"access_ttl"`
	RefreshTTL     time.Duration `mapstructure:"refresh_ttl"`
	CookieName     string        `mapstructure:"cookie_name"`
	CookieSecure   bool          `mapstructure:"cookie_secure"`
	CookieSameSite string        `mapstructure:"cookie_same_site"`
	BootstrapAdmin string        `mapstructure:"bootstrap_admin"`
	BootstrapPass  string        `mapstructure:"bootstrap_password"`
}

// SecurityConfig 密钥与脱敏参数（6.3 / 6.5）。
type SecurityConfig struct {
	// MasterKeyFile 为 0600 权限的主密钥文件，AES-256-GCM 使用其内容派生密钥。
	MasterKeyFile string `mapstructure:"master_key_file"`
	// MasterKey 支持环境变量直接注入，优先级高于文件。
	MasterKey string `mapstructure:"master_key"`
	// MasterKeyRotationDays 用于密钥轮换提醒（90 天，见 6.3）。
	MasterKeyRotationDays int `mapstructure:"master_key_rotation_days"`
	// OutboundWhitelist 为服务级出网白名单，默认空表示全部禁止第三方分析。
	OutboundWhitelist []string `mapstructure:"outbound_whitelist"`
	// SensitivePatterns 为出网脱敏的额外正则。
	SensitivePatterns []string `mapstructure:"sensitive_patterns"`
	// CodeSnippetMaxLines 为出网代码片段行数上限（≤200 行/文件）。
	CodeSnippetMaxLines int `mapstructure:"code_snippet_max_lines"`
}

// AIEngineConfig AI 引擎策略（3.4：third_party / self_hosted / hybrid）。
type AIEngineConfig struct {
	Strategy   string         `mapstructure:"strategy"`
	ThirdParty ProviderConfig `mapstructure:"third_party"`
	SelfHosted ProviderConfig `mapstructure:"self_hosted"`
	Fallback   FallbackConfig `mapstructure:"fallback"`
}

// ProviderConfig 单个模型提供方配置。
type ProviderConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Kind 取值 openai / anthropic / ollama / mock，决定请求协议适配器。
	Kind      string        `mapstructure:"kind"`
	BaseURL   string        `mapstructure:"base_url"`
	APIKey    string        `mapstructure:"api_key"`
	Model     string        `mapstructure:"model"`
	MaxTokens int           `mapstructure:"max_tokens"`
	Timeout   TimeoutConfig `mapstructure:"timeout"`
	// PricePerKToken 用于成本治理换算（元/千 token）。
	PricePerKToken float64 `mapstructure:"price_per_k_token"`
	// EmbeddingModel 留空表示使用本地确定性嵌入（无外部依赖）。
	EmbeddingModel string `mapstructure:"embedding_model"`
}

// TimeoutConfig 超时链参数（5.4）。
type TimeoutConfig struct {
	Connect      time.Duration `mapstructure:"connect"`
	FirstByte    time.Duration `mapstructure:"first_byte"`
	Total        time.Duration `mapstructure:"total"`
	ToolCall     time.Duration `mapstructure:"tool_call"`
	TaskDeadline time.Duration `mapstructure:"task_deadline"`
}

// FallbackConfig 降级链参数（5.4）。
type FallbackConfig struct {
	// FailureThreshold 为第三方连续失败次数阈值。
	FailureThreshold int `mapstructure:"failure_threshold"`
	// OpenDuration 为熔断打开时长，到期后半开探测。
	OpenDuration time.Duration `mapstructure:"open_duration"`
	// RulesEngineEnabled 表示降级到规则引擎 + 知识库检索。
	RulesEngineEnabled bool `mapstructure:"rules_engine_enabled"`
}

// PrometheusConfig 监控查询参数（4.2：不自建采集器）。
type PrometheusConfig struct {
	// BaseURL 为空时使用内置指标模拟器，保证离线可运行。
	BaseURL   string        `mapstructure:"base_url"`
	Timeout   time.Duration `mapstructure:"timeout"`
	CacheTTL  time.Duration `mapstructure:"cache_ttl"`
	Retention time.Duration `mapstructure:"retention"`
	// ExporterJobPrefix 为 PromQL 标签匹配前缀。
	ExporterJobPrefix string `mapstructure:"exporter_job_prefix"`
}

// IntegrationConfig 是「集成中心」参数（对齐云厂商控制台的一键集成能力）。
//
// 工作方式：集成中心把「Exporter 部署信息 + Prometheus 抓取标签」渲染成产物，
// 抓取目标同时以两种形式对外提供：
//
//  1. HTTP 服务发现（主通道）：GET /api/sd/integrations，Prometheus 用
//     http_sd_configs 周期性拉取，因此新增集成**不需要**重启或 reload Prometheus；
//  2. file_sd 文件（副产物）：写入 OutputDir/FileSDName，供人工核对或交给
//     外部 Prometheus 使用。
//
// 为什么主通道用 HTTP 而不是共享卷：后端容器以非 root 用户运行，而共享命名卷的
// 属主取决于「哪个容器先初始化该卷」，顺序不可控——file_sd 会静默写不进去。
// HTTP 通道没有属主概念，跨主机部署也能用。
type IntegrationConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// OutputDir 为产物目录（file_sd 文件落在这里，供人工/外部 Prometheus 使用）。
	OutputDir string `mapstructure:"output_dir"`
	// FileSDName 为 file_sd 文件名。
	FileSDName string `mapstructure:"file_sd_name"`
	// SDToken 为服务发现接口的只读令牌；留空表示不鉴权
	// （该接口只暴露地址与标签，不含任何口令）。
	SDToken string `mapstructure:"sd_token"`
	// JobName 为 file_sd 抓取任务名；集成的纳管实例 prom_job 会写为该值。
	JobName string `mapstructure:"job_name"`
	// AutoRules 表示集成成功后自动创建模板内的推荐告警规则。
	AutoRules bool `mapstructure:"auto_rules"`
	// DockerEnabled 表示允许平台调用 Docker Engine API 拉起 Exporter 容器。
	// 默认关闭：挂载 docker.sock 等于把宿主机 root 权限交给平台容器。
	DockerEnabled bool `mapstructure:"docker_enabled"`
	// DockerHost 支持 unix:///var/run/docker.sock 或 tcp://host:port。
	DockerHost string `mapstructure:"docker_host"`
	// ExporterNetwork 为 Exporter 容器加入的网络，必须与 Prometheus 同网络。
	ExporterNetwork string `mapstructure:"exporter_network"`
	// DefaultEnvironment 为集成的默认环境（数据权限按环境隔离）。
	DefaultEnvironment string `mapstructure:"default_environment"`
}

// NotifyConfig 通知渠道（4.4：飞书/企微必选，钉钉/邮件备选）。
type NotifyConfig struct {
	Enabled  bool                 `mapstructure:"enabled"`
	Feishu   WebhookChannelConfig `mapstructure:"feishu"`
	WeCom    WebhookChannelConfig `mapstructure:"wecom"`
	DingTalk WebhookChannelConfig `mapstructure:"dingtalk"`
	Email    EmailChannelConfig   `mapstructure:"email"`
	// CardConfirmPath 为 IM 卡片「查看详情」回跳地址，执行统一回 Web 端（6.2）。
	CardConfirmPath string `mapstructure:"card_confirm_path"`
}

// WebhookChannelConfig 通用 Webhook 渠道配置。
type WebhookChannelConfig struct {
	Enabled  bool     `mapstructure:"enabled"`
	Webhook  string   `mapstructure:"webhook"`
	Secret   string   `mapstructure:"secret"`
	Mentions []string `mapstructure:"mentions"`
}

// EmailChannelConfig 邮件渠道配置。
type EmailChannelConfig struct {
	Enabled  bool     `mapstructure:"enabled"`
	Host     string   `mapstructure:"host"`
	Port     int      `mapstructure:"port"`
	Username string   `mapstructure:"username"`
	Password string   `mapstructure:"password"`
	From     string   `mapstructure:"from"`
	To       []string `mapstructure:"to"`
	UseTLS   bool     `mapstructure:"use_tls"`
}

// GuardrailConfig 六道护栏参数（设计文档第五章）。
type GuardrailConfig struct {
	// InputTokenBudget / OutputTokenBudget 为上下文预算（5.2）。
	InputTokenBudget  int `mapstructure:"input_token_budget"`
	OutputTokenBudget int `mapstructure:"output_token_budget"`
	// LogContextLines / MaxLogSources / MaxCodeFiles / CodeLinesPerFile 为采集配额（5.2）。
	LogContextLines  int `mapstructure:"log_context_lines"`
	MaxLogSources    int `mapstructure:"max_log_sources"`
	MaxCodeFiles     int `mapstructure:"max_code_files"`
	CodeLinesPerFile int `mapstructure:"code_lines_per_file"`

	// MaxSteps / LoopRepeatThreshold 为防死循环参数（5.3，二期 Agent 生效）。
	MaxSteps            int `mapstructure:"max_steps"`
	LoopRepeatThreshold int `mapstructure:"loop_repeat_threshold"`
	LLMRetry            int `mapstructure:"llm_retry"`

	// 成本治理（5.7）。
	CacheTTL        time.Duration `mapstructure:"cache_ttl"`
	DailyTokenQuota int64         `mapstructure:"daily_token_quota"`
	PerUserQuota    int64         `mapstructure:"per_user_daily_token_quota"`
	MaxConcurrency  int           `mapstructure:"max_concurrency"`
	// VectorReferenceThreshold 为「参考案例」展示阈值，不作为最终诊断（5.7）。
	VectorReferenceThreshold float64 `mapstructure:"vector_reference_threshold"`

	// SQLGuard 为 AI 生成 SQL 的规则校验参数（5.5）。
	SQLDefaultLimit int      `mapstructure:"sql_default_limit"`
	SQLMaxLimit     int      `mapstructure:"sql_max_limit"`
	SQLTableAllow   []string `mapstructure:"sql_table_allowlist"`
}

// SchedulerConfig 定时任务开关与周期（3.4）。
type SchedulerConfig struct {
	Enabled        bool          `mapstructure:"enabled"`
	HealthProbe    time.Duration `mapstructure:"health_probe_interval"`
	MetricRuleEval time.Duration `mapstructure:"metric_rule_eval_interval"`
	AlertCluster   time.Duration `mapstructure:"alert_cluster_interval"`
	AuditSnapshot  string        `mapstructure:"audit_snapshot_cron"`
	ApprovalExpire time.Duration `mapstructure:"approval_expire_interval"`
}

// LogConfig 平台自身日志。
type LogConfig struct {
	Level      string `mapstructure:"level"`
	FilePath   string `mapstructure:"file_path"`
	MaxSizeMB  int    `mapstructure:"max_size_mb"`
	MaxBackups int    `mapstructure:"max_backups"`
	MaxAgeDays int    `mapstructure:"max_age_days"`
	Console    bool   `mapstructure:"console"`
}

// Load 读取配置文件并应用环境变量覆盖。
//
// 环境变量规则：前缀 MWOPS_，层级以下划线连接，例如 MWOPS_DATABASE_HOST。
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigType("yaml")
	if path != "" {
		v.SetConfigFile(path)
	} else {
		v.SetConfigName("config")
		v.AddConfigPath(".")
		v.AddConfigPath("./configs")
		v.AddConfigPath("/etc/middleware-ops")
	}

	setDefaults(v)
	v.SetEnvPrefix("MWOPS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !isNotFound(err, &notFound) {
			return nil, fmt.Errorf("read config: %w", err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}
	cfg.applyEnvOnly()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// isNotFound 判断配置缺失错误（显式路径缺失同样视为可使用默认值）。
func isNotFound(err error, target *viper.ConfigFileNotFoundError) bool {
	if _, ok := err.(viper.ConfigFileNotFoundError); ok {
		return true
	}
	return target != nil && strings.Contains(err.Error(), "no such file")
}

// applyEnvOnly 补齐 AutomaticEnv 无法覆盖的嵌套字段（viper 已知限制）。
func (c *Config) applyEnvOnly() {
	if v := viper.GetString("security.master_key"); v != "" {
		c.Security.MasterKey = v
	}
	if v := viper.GetString("ai_engine.third_party.api_key"); v != "" {
		c.AIEngine.ThirdParty.APIKey = v
	}
	if v := viper.GetString("ai_engine.self_hosted.api_key"); v != "" {
		c.AIEngine.SelfHosted.APIKey = v
	}
	if v := viper.GetString("notify.feishu.webhook"); v != "" {
		c.Notify.Feishu.Webhook = v
	}
	if v := viper.GetString("notify.wecom.webhook"); v != "" {
		c.Notify.WeCom.Webhook = v
	}
}

// Validate 校验关键配置，把「启动即失败」的问题挡在启动阶段。
func (c *Config) Validate() error {
	switch c.AIEngine.Strategy {
	case "third_party", "self_hosted", "hybrid":
	default:
		return fmt.Errorf("ai_engine.strategy 非法: %q（可选 third_party/self_hosted/hybrid）", c.AIEngine.Strategy)
	}
	if c.App.Mode == "release" && len(c.JWT.Secret) < 32 {
		return fmt.Errorf("生产模式下 jwt.secret 长度必须 ≥32，当前 %d", len(c.JWT.Secret))
	}
	if c.Security.MasterKey != "" && len(c.Security.MasterKey) < 16 {
		return fmt.Errorf("security.master_key 长度必须 ≥16")
	}
	if c.Guardrail.InputTokenBudget <= 0 || c.Guardrail.OutputTokenBudget <= 0 {
		return fmt.Errorf("guardrail 上下文预算必须为正数")
	}
	if c.Guardrail.MaxConcurrency <= 0 {
		return fmt.Errorf("guardrail.max_concurrency 必须为正数")
	}
	return nil
}

// IsProd 报告是否为生产模式。
func (c *Config) IsProd() bool { return c.App.Mode == "release" }
