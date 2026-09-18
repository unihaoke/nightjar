// Package model 定义平台的全部持久化实体。
//
// 设计约定（见设计文档 7.1）：
//   - 指标数据存 Prometheus，本包不定义 metric_snapshots 之类的自有指标表；
//   - PostgreSQL 只保存元数据、告警、诊断、知识、审计；
//   - 向量字段统一声明为 Vector，在 pgvector 构建标签下映射为 vector(768)，
//     否则以 JSONB 存储并退化为应用层余弦相似度检索（见 db.VectorType）。
package model

import (
	"time"

	"gorm.io/gorm"
)

// 中间件类型（一期核心 5 种 + Nginx 基础监控 + RabbitMQ 二期）。
const (
	MWTypeRedis = "redis"
	MWTypeKafka = "kafka"
	MWTypeMySQL = "mysql"
	MWTypePG    = "pg"
	MWTypeES    = "es"
	MWTypeNginx = "nginx"
	MWTypeRMQ   = "rabbitmq"
	// MWTypeNode 为主机监控（node_exporter）：采集对象是服务器本身，不是中间件实例。
	MWTypeNode = "node"
)

// 环境分级（数据权限的隔离维度）。
const (
	EnvDev     = "dev"
	EnvStaging = "staging"
	EnvProd    = "prod"
)

// 告警级别。
const (
	AlertLevelWarning  = "warning"
	AlertLevelCritical = "critical"
)

// 告警状态。
const (
	AlertStatusActive       = "active"
	AlertStatusAcknowledged = "acknowledged"
	AlertStatusResolved     = "resolved"
)

// 知识库状态（质量闭环：诊断沉淀先入草稿，人工确认后参与检索）。
const (
	KnowledgeStatusDraft      = "draft"
	KnowledgeStatusPublished  = "published"
	KnowledgeStatusDeprecated = "deprecated"
)

// 诊断反馈（5.6 质量护栏）。
const (
	FeedbackUseful  = "useful"
	FeedbackUseless = "useless"
	FeedbackAdopted = "adopted"
)

// 引擎状态（5.4 降级链）。
const (
	EngineStatusOK       = "ok"
	EngineStatusDegraded = "degraded"
	EngineStatusFallback = "fallback"
)

// 日志告警事件状态。
const (
	LogEventPending   = "pending"
	LogEventAnalyzing = "analyzing"
	LogEventResolved  = "resolved"
	LogEventIgnored   = "ignored"
)

// Base 为所有实体共用的主键与时间戳。
type Base struct {
	ID        int64     `gorm:"primaryKey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// User 平台用户。
type User struct {
	Base
	Username     string `gorm:"size:64;uniqueIndex;not null" json:"username"`
	PasswordHash string `gorm:"size:128;not null" json:"-"`
	Nickname     string `gorm:"size:64" json:"nickname"`
	Email        string `gorm:"size:128" json:"email"`
	RoleCode     string `gorm:"size:32;index;not null" json:"role_code"`
	// EnvScope / GroupScope 为数据权限范围：为空表示全部环境 / 全部分组。
	EnvScope   JSONStringSlice `gorm:"type:text" json:"env_scope"`
	GroupScope JSONStringSlice `gorm:"type:text" json:"group_scope"`
	Status     int16           `gorm:"default:1" json:"status"` // 1-启用 0-禁用
	LastLogin  *time.Time      `json:"last_login"`
}

// Role RBAC 角色。
type Role struct {
	Base
	Code        string `gorm:"size:32;uniqueIndex;not null" json:"code"`
	Name        string `gorm:"size:64;not null" json:"name"`
	Description string `gorm:"size:255" json:"description"`
	// Permissions 为权限点列表，例如 middleware:write / fix:execute。
	Permissions JSONStringSlice `gorm:"type:text" json:"permissions"`
	// Levels 为该角色可直接执行的操作级别（L0/L1），L2 一律走审批。
	Levels    JSONStringSlice `gorm:"type:text" json:"levels"`
	Builtin   bool            `json:"builtin"`
	UserCount int64           `gorm:"-" json:"user_count"`
}

// MiddlewareInstance 中间件实例（4.1）。
type MiddlewareInstance struct {
	Base
	Name     string `gorm:"size:128;not null;index" json:"name"`
	MWType   string `gorm:"column:mw_type;size:16;not null;index" json:"mw_type"`
	Host     string `gorm:"size:255;not null" json:"host"`
	Port     int    `gorm:"not null" json:"port"`
	Username string `gorm:"size:128" json:"username"`
	// PasswordEncrypted 为 AES-256-GCM 密文，永不返回给前端（见 6.3）。
	PasswordEncrypted string          `gorm:"type:text" json:"-"`
	HasPassword       bool            `gorm:"-" json:"has_password"`
	Environment       string          `gorm:"size:16;default:dev;index" json:"environment"`
	GroupName         string          `gorm:"size:64;index" json:"group_name"`
	Tags              JSONStringSlice `gorm:"type:text" json:"tags"`
	Status            int16           `gorm:"default:1" json:"status"`
	// LastMessage 记录最近一次健康探测的结论，便于列表页直接展示。
	LastMessage string     `gorm:"size:255" json:"last_message"`
	LastCheckAt *time.Time `json:"last_check_at"`
	Config      JSONMap    `gorm:"type:text" json:"config"`
	// PromJob / PromInstance 为 Prometheus 查询时的标签匹配依据（4.2 不建自有指标表）。
	PromJob      string `gorm:"size:64" json:"prom_job"`
	PromInstance string `gorm:"size:128" json:"prom_instance"`
}

// TableName 显式指定表名，避免 GORM 复数化差异。
func (MiddlewareInstance) TableName() string { return "middleware_instances" }

// AIDiagnosis AI 诊断记录（7.1）。
type AIDiagnosis struct {
	Base
	UserID     int64  `gorm:"index;not null" json:"user_id"`
	InstanceID int64  `gorm:"index" json:"instance_id"`
	MWType     string `gorm:"size:16;index" json:"mw_type"`
	UserQuery  string `gorm:"type:text;not null" json:"user_query"`
	// CollectedMetrics 为上下文预算裁剪后的指标 / 日志摘要。
	CollectedMetrics JSONMap `gorm:"type:text" json:"collected_metrics"`
	DiagnosisResult  string  `gorm:"type:text" json:"diagnosis_result"`
	// Report 为结构化诊断报告（根因/证据/置信度/建议/影响/待确认）。
	Report       JSONMap         `gorm:"type:text" json:"report"`
	Suggestions  JSONMapList     `gorm:"type:text" json:"suggestions"`
	Feedback     string          `gorm:"size:16;index" json:"feedback"`
	EngineStatus string          `gorm:"size:32" json:"engine_status"`
	CostTokens   int             `gorm:"default:0" json:"cost_tokens"`
	EngineUsed   string          `gorm:"size:32" json:"engine_used"`
	DurationMS   int64           `json:"duration_ms"`
	CacheHit     bool            `json:"cache_hit"`
	Truncated    JSONStringSlice `gorm:"type:text" json:"truncated"`
}

// AlertRule 告警规则（4.4）。
type AlertRule struct {
	Base
	Name       string  `gorm:"size:128;not null" json:"name"`
	InstanceID int64   `gorm:"index" json:"instance_id"`
	MWType     string  `gorm:"size:16" json:"mw_type"`
	MetricName string  `gorm:"size:128;not null" json:"metric_name"`
	Operator   string  `gorm:"size:8;not null" json:"operator"` // > >= < <= == !=
	Threshold  float64 `json:"threshold"`
	Level      string  `gorm:"size:16;default:warning" json:"level"`
	// TimeWindow 为去重窗口（分钟），Cooldown 为静默冷却期（分钟）。
	//
	// 数据库列名刻意使用 time_window 而非 window：window 是 PostgreSQL 保留关键字，
	// 裸写会直接报语法错误（syntax error at or near "window"）。GORM 会对标识符加引号，
	// 但初始化脚本、手工 SQL、BI 工具等非 GORM 途径不会，因此从命名上规避更安全。
	TimeWindow     int             `gorm:"column:time_window;default:5" json:"time_window"`
	Cooldown       int             `gorm:"default:10" json:"cooldown"`
	NotifyChannels JSONStringSlice `gorm:"type:text" json:"notify_channels"`
	Enabled        bool            `gorm:"default:true" json:"enabled"`
	AIEnabled      bool            `gorm:"default:true" json:"ai_enabled"`
	Description    string          `gorm:"size:255" json:"description"`
}

// Alert 中间件阈值告警（与应用日志告警 log_alert_events 职责分离）。
type Alert struct {
	Base
	InstanceID   int64   `gorm:"index;not null" json:"instance_id"`
	RuleID       int64   `gorm:"index" json:"rule_id"`
	MWType       string  `gorm:"size:16;index" json:"mw_type"`
	AlertLevel   string  `gorm:"size:16;index" json:"alert_level"`
	AlertMessage string  `gorm:"size:512" json:"alert_message"`
	MetricValue  float64 `json:"metric_value"`
	// Fingerprint 为收敛指纹（规则 + 实例 + 级别），用于窗口去重与冷却抑制。
	Fingerprint string `gorm:"size:64;index" json:"fingerprint"`
	Status      string `gorm:"size:16;index" json:"status"`
	// Count 为窗口内被合并的重复次数。
	Count       int        `json:"count"`
	TriggeredAt time.Time  `gorm:"index" json:"triggered_at"`
	AckedBy     int64      `json:"acked_by"`
	AckedAt     *time.Time `json:"acked_at"`
	ResolvedAt  *time.Time `json:"resolved_at"`
	DiagnosisID int64      `json:"diagnosis_id"`
	ClusterID   string     `gorm:"size:64;index" json:"cluster_id"`
	Suppressed  bool       `json:"suppressed"`
}

// AlertEmbedding 告警向量（离线语义聚类，仅合并展示、不改状态）。
type AlertEmbedding struct {
	Base
	AlertID      int64  `gorm:"index" json:"alert_id"`
	AlertContent string `gorm:"type:text" json:"alert_content"`
	Embedding    Vector `gorm:"type:text" json:"-"`
	Clustered    bool   `json:"clustered"`
}

// KnowledgeBase 知识库条目（4.5 质量闭环）。
type KnowledgeBase struct {
	Base
	Title       string          `gorm:"size:255;not null" json:"title"`
	Content     string          `gorm:"type:text" json:"content"`
	MWType      string          `gorm:"size:16;index" json:"mw_type"`
	Tags        JSONStringSlice `gorm:"type:text" json:"tags"`
	Source      string          `gorm:"size:16;index" json:"source"` // manual/auto
	Status      string          `gorm:"size:16;index" json:"status"` // draft/published/deprecated
	AuthorID    int64           `json:"author_id"`
	AdoptCount  int             `gorm:"default:0" json:"adopt_count"`
	UseCount    int             `gorm:"default:0" json:"use_count"`
	Feedback    JSONMap         `gorm:"type:text" json:"feedback"`
	DiagnosisID int64           `json:"diagnosis_id"`
	Embedding   Vector          `gorm:"type:text" json:"-"`
	// Similarity 为检索时回填的相似度（不持久化）。
	Similarity float64 `gorm:"-" json:"similarity"`
}

// AuditLog 审计日志（只追加，哈希链见 6.4）。
type AuditLog struct {
	ID           int64   `gorm:"primaryKey" json:"id"`
	UserID       int64   `gorm:"index" json:"user_id"`
	Username     string  `gorm:"size:64" json:"username"`
	InstanceID   int64   `gorm:"index" json:"instance_id"`
	ActionType   string  `gorm:"size:32;index" json:"action_type"`
	ActionDetail JSONMap `gorm:"type:text" json:"action_detail"`
	Result       string  `gorm:"size:16" json:"result"`
	Level        string  `gorm:"size:4" json:"level"`
	IPAddress    string  `gorm:"size:45" json:"ip_address"`
	UserAgent    string  `gorm:"size:255" json:"user_agent"`
	Route        string  `gorm:"size:255" json:"route"`
	// HashPrev / HashSelf 构成每日快照校验的哈希链。
	HashPrev  string    `gorm:"size:64" json:"hash_prev"`
	HashSelf  string    `gorm:"size:64" json:"hash_self"`
	CreatedAt time.Time `gorm:"index" json:"created_at"`
}

// TableName 审计表名固定，禁止通过 ORM 变更表结构推断。
func (AuditLog) TableName() string { return "audit_logs" }

// Approval L2 高危操作审批工单（6.2：prod 强制审批，30min 超时自动拒绝）。
type Approval struct {
	Base
	// TicketID 为对外暴露的工单号（避免泄露自增 ID 的规模信息）。
	TicketID     string     `gorm:"size:40;uniqueIndex" json:"ticket_id"`
	ApplicantID  int64      `gorm:"index" json:"applicant_id"`
	ApproverID   int64      `json:"approver_id"`
	InstanceID   int64      `gorm:"index" json:"instance_id"`
	AlertID      int64      `gorm:"index" json:"alert_id"`
	DiagnosisID  int64      `json:"diagnosis_id"`
	Environment  string     `gorm:"size:16" json:"environment"`
	ActionType   string     `gorm:"size:64" json:"action_type"`
	ActionDetail JSONMap    `gorm:"type:text" json:"action_detail"`
	Preview      JSONMap    `gorm:"type:text" json:"preview"`
	Reason       string     `gorm:"size:512" json:"reason"`
	Status       string     `gorm:"size:16;index" json:"status"` // pending/approved/rejected/expired/executed/failed
	Comment      string     `gorm:"size:512" json:"comment"`
	ExpiresAt    time.Time  `json:"expires_at"`
	DecidedAt    *time.Time `json:"decided_at"`
	ExecutedAt   *time.Time `json:"executed_at"`
	ExecResult   JSONMap    `gorm:"type:text" json:"exec_result"`
}

// FixRecord 修复执行记录（4.6），与审批工单关联以便结果回填复核。
type FixRecord struct {
	Base
	ApprovalID   int64   `gorm:"index" json:"approval_id"`
	TicketID     string  `gorm:"size:40;index" json:"ticket_id"`
	UserID       int64   `gorm:"index" json:"user_id"`
	InstanceID   int64   `gorm:"index" json:"instance_id"`
	AlertID      int64   `json:"alert_id"`
	DiagnosisID  int64   `json:"diagnosis_id"`
	ActionType   string  `gorm:"size:64" json:"action_type"`
	Level        string  `gorm:"size:4" json:"level"`
	Command      string  `gorm:"size:512" json:"command"`
	ActionDetail JSONMap `gorm:"type:text" json:"action_detail"`
	Status       string  `gorm:"size:16;index" json:"status"` // success/failed/rejected/blocked
	Result       string  `gorm:"type:text" json:"result"`
	DryRun       bool    `json:"dry_run"`
	DurationMS   int64   `json:"duration_ms"`
}

// ServerInstance 应用服务器实例（4.8.1 日志采集对象）。
type ServerInstance struct {
	Base
	Name        string          `gorm:"size:128;not null" json:"name"`
	IP          string          `gorm:"size:45;index" json:"ip"`
	Hostname    string          `gorm:"size:128" json:"hostname"`
	Environment string          `gorm:"size:16;index" json:"environment"`
	GroupName   string          `gorm:"size:64" json:"group_name"`
	Status      int16           `gorm:"default:1" json:"status"`
	Tags        JSONStringSlice `gorm:"type:text" json:"tags"`
	LastSeenAt  *time.Time      `json:"last_seen_at"`
}

// CodeRepo 服务与代码仓库映射（6.5 出网白名单按仓库维度开启）。
type CodeRepo struct {
	Base
	ServiceName string `gorm:"size:128;index;not null" json:"service_name"`
	RepoURL     string `gorm:"size:255" json:"repo_url"`
	Branch      string `gorm:"size:64;default:main" json:"branch"`
	LocalPath   string `gorm:"size:255" json:"local_path"`
	Language    string `gorm:"size:32" json:"language"`
	// AllowThirdParty 为出网白名单开关，默认关闭（6.5）。
	AllowThirdParty bool       `gorm:"default:false" json:"allow_third_party"`
	LastPullAt      *time.Time `json:"last_pull_at"`
}

// LogAlertEvent 应用日志告警事件（错误指纹 + 窗口去重 + 冷却）。
type LogAlertEvent struct {
	Base
	EventID        string    `gorm:"size:40;uniqueIndex" json:"event_id"`
	ServerID       int64     `gorm:"index" json:"server_id"`
	ServiceName    string    `gorm:"size:128;index" json:"service_name"`
	AlertType      string    `gorm:"size:32;index" json:"alert_type"`
	ErrorSignature string    `gorm:"size:255;index" json:"error_signature"`
	RawStacktrace  string    `gorm:"type:text" json:"raw_stacktrace"`
	ContextLines   string    `gorm:"type:text" json:"context_lines"`
	ErrorCount     int       `gorm:"default:1" json:"error_count"`
	Severity       string    `gorm:"size:16" json:"severity"`
	Status         string    `gorm:"size:16;index" json:"status"`
	FirstSeenAt    time.Time `json:"first_seen_at"`
	LastSeenAt     time.Time `gorm:"index" json:"last_seen_at"`
	Analyzed       bool      `json:"analyzed"`
	Suppressed     bool      `json:"suppressed"`
}

// AICodeAnalysis AI 代码分析报告（4.8.3 三点式模板）。
type AICodeAnalysis struct {
	Base
	EventID       int64   `gorm:"index" json:"event_id"`
	EventKey      string  `gorm:"size:40;index" json:"event_key"`
	ServiceName   string  `gorm:"size:128" json:"service_name"`
	LocatedFile   string  `gorm:"size:512" json:"located_file"`
	LocatedLine   int     `json:"located_line"`
	CodeSnippet   string  `gorm:"type:text" json:"code_snippet"`
	RootCause     string  `gorm:"type:text" json:"root_cause"`
	EmergencyPlan string  `gorm:"type:text" json:"emergency_plan"`
	FixSuggestion string  `gorm:"type:text" json:"fix_suggestion"`
	ImpactScope   string  `gorm:"type:text" json:"impact_scope"`
	Confidence    float64 `json:"confidence"`
	Evidence      JSONMap `gorm:"type:text" json:"evidence"`
	EngineUsed    string  `gorm:"size:32" json:"engine_used"`
	EngineStatus  string  `gorm:"size:32" json:"engine_status"`
	CostTokens    int     `json:"cost_tokens"`
	OutboundOK    bool    `json:"outbound_ok"`
}

// NotificationLog 通知记录。
type NotificationLog struct {
	Base
	EventID int64      `gorm:"index" json:"event_id"`
	AlertID int64      `gorm:"index" json:"alert_id"`
	Channel string     `gorm:"size:32" json:"channel"`
	Target  string     `gorm:"size:255" json:"target"`
	Content string     `gorm:"type:text" json:"content"`
	Status  string     `gorm:"size:16" json:"status"`
	Error   string     `gorm:"size:512" json:"error"`
	SentAt  *time.Time `json:"sent_at"`
}

// AuditSnapshot 审计哈希链快照（每日落盘，周期校验）。
type AuditSnapshot struct {
	Base
	SnapshotDate string     `gorm:"size:16;uniqueIndex" json:"snapshot_date"`
	LastLogID    int64      `json:"last_log_id"`
	LogCount     int64      `json:"log_count"`
	ChainHash    string     `gorm:"size:64" json:"chain_hash"`
	FilePath     string     `gorm:"size:255" json:"file_path"`
	Verified     bool       `json:"verified"`
	VerifiedAt   *time.Time `json:"verified_at"`
}

// PlatformSetting 是平台自管的配置项（AI 提供方、通知渠道等），**密钥整体加密落库**。
//
// 为什么要有它：AI key 与通知渠道以前只能写在 .env 里 —— 改一个 webhook 地址要改环境变量、
// 重建容器、还可能把密钥散落在多处。搬到平台后由界面管理，改完即时生效，且密钥加密存储。
type PlatformSetting struct {
	Base
	// Name 为配置项标识（当前：ai / notify）。
	//
	// 刻意不叫 `key`：GORM 会生成索引名 idx_platform_settings_key，而该名字以 `_key` 结尾，
	// 与 PostgreSQL 默认约束名（<表>_<列>_key）形式相撞 —— schema 守卫会直接判失败（见 schema_test.go）。
	Name string `gorm:"size:64;uniqueIndex" json:"name"`
	// PayloadEncrypted 为 AES-256-GCM 密文（复用平台主密钥）；响应里绝不回传。
	PayloadEncrypted string `gorm:"type:text" json:"-"`
	// UpdatedBy 记录最后一次修改人，便于审计（UpdatedAt 由 Base 提供，GORM 自动维护）。
	UpdatedBy string `json:"updated_by"`
}

// TableName 显式指定表名，避免 GORM 复数化差异。
func (PlatformSetting) TableName() string { return "platform_settings" }

// MigrationList 返回 AutoMigrate 所需的实体顺序（外键语义上先主后从）。
func MigrationList() []any {
	return []any{
		&User{}, &Role{},
		&MiddlewareInstance{},
		&AIDiagnosis{},
		&AlertRule{}, &Alert{}, &AlertEmbedding{},
		&KnowledgeBase{},
		&AuditLog{}, &AuditSnapshot{},
		&Approval{}, &FixRecord{},
		&ServerInstance{}, &CodeRepo{}, &LogAlertEvent{}, &AICodeAnalysis{},
		&NotificationLog{},
		&PlatformSetting{},
	}
}

// Migrate 执行全部实体的自动迁移。
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(MigrationList()...)
}
