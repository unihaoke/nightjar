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
	// MWTypeLog 为**日志集成**（Filebeat → 平台 Kafka）。
	//
	// 它虽然也记录在 middleware_instances（复用部署/尝试/自检那一套机制），
	// 但**不属于中间件纳管域**：没有指标、没有实例端口，也不该出现在「中间件纳管」列表、
	// 统一监控的实例下拉、指标告警规则的实例选择与大盘的实例统计里。
	// 域过滤统一走 service.MiddlewareDomainTypes()，不要在各处手写字符串比较。
	MWTypeLog = "log"
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

// AIAnalysisTask 记录一次「提交给外部 AI 服务的分析任务」（异步回调模型）。
//
// 为什么必须落库而不是只放在内存里：日志告警的 AI 分析是分钟级的慢操作，
// 提交方与收结论方往往不是同一次执行（进程重启、多副本、回调晚到）。
// 只有把任务号与状态写进库，回调到达时才能对上号，超时轮询也才有依据。
type AIAnalysisTask struct {
	Base
	// EventID 为触发这次分析的日志事件（0 表示手工提交的分析）。
	EventID int64 `gorm:"index" json:"event_id"`
	// ServiceName 为服务名（回调与超时处理都要按它回写事件）。
	ServiceName string `gorm:"size:128;index" json:"service_name"`
	// TaskID 为外部 AI 服务返回的任务号，也是幂等键（回调与轮询都靠它对上号）。
	TaskID string `gorm:"size:128;uniqueIndex" json:"task_id"`
	// RunID 为开放接口（openapi_v1）返回的运行 ID。
	//
	// 为什么两个号都要存：开放接口的回调报文里带的是 taskId（用它对号），
	// 而轮询与报告地址是按 runId 组织的（GET /api/v1/runs/{runId}）。
	// 只存 taskId 的话，回调一旦丢失，轮询就拿不到任何东西。
	RunID string `gorm:"size:128;index" json:"run_id"`
	// Status 取 submitted / succeeded / failed / timeout（见下面的状态常量）。
	Status string `gorm:"size:16;index;default:submitted" json:"status"`
	// Question 为提交给 AI 的问题（已脱敏），用于事后核对"到底问了什么"。
	Question string `gorm:"type:text" json:"question"`
	// Answer 为 AI 回传的结论原文（JSON 或纯文本）。
	Answer string `gorm:"type:text" json:"answer"`
	// Error 为失败原因（AI 侧报错、解析失败、超时等）。
	Error string `gorm:"size:512" json:"error"`
	// DeadlineAt 为任务超时时刻：超过它仍没有结论就按超时收尾，
	// 避免事件因为一次丢失的回调永远停在"分析中"。
	DeadlineAt *time.Time `gorm:"index" json:"deadline_at"`
	// CompletedAt 为收到结论（或判定失败）的时间。
	CompletedAt *time.Time `json:"completed_at"`
}

// 异步分析任务的状态常量。
const (
	// AIAnalysisTaskSubmitted 表示已提交给 AI 服务，等待回调或轮询结果。
	AIAnalysisTaskSubmitted = "submitted"
	// AIAnalysisTaskSucceeded 表示已收到结论。
	AIAnalysisTaskSucceeded = "succeeded"
	// AIAnalysisTaskFailed 表示 AI 侧明确失败，或结论无法解析。
	AIAnalysisTaskFailed = "failed"
	// AIAnalysisTaskTimeout 表示超过 DeadlineAt 仍没有结论（平台侧判定）。
	AIAnalysisTaskTimeout = "timeout"
)

// LogAlertEvent 应用日志告警事件（错误指纹 + 窗口去重 + 冷却）。
type LogAlertEvent struct {
	Base
	EventID        string `gorm:"size:40;uniqueIndex" json:"event_id"`
	ServerID       int64  `gorm:"index" json:"server_id"`
	ServiceName    string `gorm:"size:128;index" json:"service_name"`
	AlertType      string `gorm:"size:32;index" json:"alert_type"`
	ErrorSignature string `gorm:"size:255;index" json:"error_signature"`
	// ErrorMessage 为这条日志**原始的**错误消息（通知与页面展示用）。
	//
	// 为什么指纹之外还要存一份原文：ErrorSignature 是同一类错误的哈希摘要（聚合与去重靠它），
	// 不是人能读的文本——通知里只发指纹，值班同学看到的就是一串十六进制。
	// 这里保留首条真实消息（截断留存），让"到底报了什么错"在 IM 里一眼可见。
	ErrorMessage  string `gorm:"size:1024" json:"error_message"`
	RawStacktrace string `gorm:"type:text" json:"raw_stacktrace"`
	ContextLines  string `gorm:"type:text" json:"context_lines"`
	// LogPath 为这条日志来自哪个文件（日志集成：Filebeat 的 log.file.path）。
	// 排查时"哪个文件在报错"往往比"报了什么"更快定位到服务与模块。
	LogPath string `gorm:"size:512" json:"log_path"`
	// ErrorCount 为窗口期内同指纹事件的累计次数。
	ErrorCount  int       `gorm:"default:1" json:"error_count"`
	Severity    string    `gorm:"size:16" json:"severity"`
	Status      string    `gorm:"size:16;index" json:"status"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `gorm:"index" json:"last_seen_at"`
	Analyzed    bool      `json:"analyzed"`
	Suppressed  bool      `json:"suppressed"`
	// ---------------------------------------------------------------
	// 规则化处理（4.8.2 增强）：命中的规则、本次使用的窗口、冷却与通知、AI 分析状态
	//
	// 为什么把这些"处理过程"落库而不是只放在内存/日志里：页面必须能回答
	// "这条告警为什么没通知我"——是命中冷却（cooldown_until）、还是规则关了 AI（analysis_state=disabled）、
	// 还是分析失败（analysis_error）。没有这几列，使用者只能靠猜。
	// ---------------------------------------------------------------
	// RuleID 为命中的日志告警规则（0 表示按平台默认值处理）。
	RuleID int64 `gorm:"index;default:0" json:"rule_id"`
	// DedupWindow 为本次使用的去重窗口（分钟）。
	DedupWindow int `json:"dedup_window"`
	// CooldownUntil 为冷却截止时间：在此之前同指纹不再通知、不再触发 AI。
	CooldownUntil *time.Time `gorm:"index" json:"cooldown_until"`
	// NotifiedAt 为最近一次外发通知的时间（空表示还没通知过）。
	NotifiedAt *time.Time `json:"notified_at"`
	// AnalysisState 为 AI 代码分析的排队与执行状态：pending/running/done/failed/disabled。
	AnalysisState string `gorm:"size:16;index;default:pending" json:"analysis_state"`
	// AnalysisError 为分析失败或跳过的原因（页面直接展示，避免"分析中"永远转圈）。
	AnalysisError string `gorm:"size:512" json:"analysis_error"`
}

// 日志事件的处理状态常量（AnalysisState）。
const (
	// LogAnalysisPending 表示已入队，等待通知与 AI 分析。
	LogAnalysisPending = "pending"
	// LogAnalysisRunning 表示正在分析（避免并发重复分析同一条事件）。
	LogAnalysisRunning = "running"
	// LogAnalysisDone 表示分析已完成（可能有结论，也可能是"规则未启用 AI"这类正常结束）。
	LogAnalysisDone = "done"
	// LogAnalysisFailed 表示分析失败（原因在 AnalysisError，页面上可点「重新分析」）。
	LogAnalysisFailed = "failed"
	// LogAnalysisDisabled 表示规则关闭了 AI 分析：这是明确的配置结果，不是故障。
	LogAnalysisDisabled = "disabled"
	// LogAnalysisAwaiting 表示已把问题提交给外部 AI 服务，正在等它的回调（或轮询结论）。
	//
	// 与 running 的区别：running 是"本进程正在做"，awaiting 是"球在对方场地"——
	// 进程重启也不会丢，因为它只反映库里的任务状态，而不是内存里的执行状态。
	LogAnalysisAwaiting = "awaiting"
)

// LogAlertRule 日志告警规则（4.8.2）。
//
// 为什么日志告警需要独立一套规则、而不是复用指标告警的 AlertRule：
// 两者的"判定输入"根本不同——指标规则比的是数值（metric > threshold），
// 日志规则比的是**错误指纹与级别**（哪个服务的哪类错误）。硬塞进一张表会让两边都读不懂。
// 但"去重窗口 / 冷却期 / 通知渠道 / AI 开关"这四个概念刻意与指标规则**同名同语义**，
// 使用者在两个页面看到的是一套心智模型。
type LogAlertRule struct {
	Base
	Name        string `gorm:"size:128;not null;uniqueIndex" json:"name"`
	Description string `gorm:"size:255" json:"description"`
	// ServiceName 为空表示匹配任意服务。
	ServiceName string `gorm:"size:128;index" json:"service_name"`
	// SignaturePattern 按**日志消息原文**（上报的 message）匹配：普通文本按子串，`/re/` 形式按正则；为空表示任意。
	//
	// 匹配原文而不是错误指纹：指纹是归一化后的哈希（见 ErrorSignature），
	// 使用者写不出"我想匹配的那句话"对应的哈希，只能写出日志里的文本。
	// 与屏蔽项（log_alert_exclusions.pattern）刻意同一套写法，两个页面填的东西一样。
	// 列名沿用 signature_pattern 只为不动存量配置（改列名会让 AutoMigrate 新开一列、旧配置全失效）。
	SignaturePattern string `gorm:"size:255" json:"signature_pattern"`
	// MinSeverity 为最低级别（INFO/WARN/ERROR/FATAL）；为空表示不限级别。
	MinSeverity string `gorm:"size:16" json:"min_severity"`
	// DedupWindow 为去重窗口（分钟）：窗口内同指纹只合并计数，不新增事件。
	// 列名刻意用 dedup_window：window 是 PostgreSQL 保留字（同 AlertRule.time_window 的教训）。
	DedupWindow int `gorm:"default:5" json:"dedup_window"`
	// Cooldown 为冷却期（分钟）：冷却期内同指纹不再通知、不再触发 AI，但**事件仍记录**。
	Cooldown       int             `gorm:"default:10" json:"cooldown"`
	NotifyChannels JSONStringSlice `gorm:"type:text" json:"notify_channels"`
	// AIEnabled 表示是否自动把问题提交给外部 AI 服务做分析。
	AIEnabled bool `gorm:"default:true" json:"ai_enabled"`
	Enabled   bool `gorm:"default:true;index" json:"enabled"`
	// Priority 数字小的优先：多条规则同时命中时取第一条，便于"特例压过通用"。
	Priority int `gorm:"default:100" json:"priority"`
}

// LogAlertExclusion 日志告警屏蔽项：命中的日志**不入库、不通知、不分析**。
//
// 为什么单独一张表、而不是做成 LogAlertRule 的一个字段：
// 规则回答"命中之后怎么处理"，屏蔽项回答"这类错误根本不该成为告警"
// （典型是框架打印的噪音，如 Request method 'GET' is not supported）。
// 两者的生效位置也不同——屏蔽在所有规则**之前**生效，命中即结束；
// 若塞进规则表，同一条屏蔽就得在每条规则里重复配一遍。
type LogAlertExclusion struct {
	Base
	// Name 为屏蔽项的备注（可空）：页面上主要展示 Pattern，名字只用于说明"为什么屏蔽"。
	Name string `gorm:"size:128" json:"name"`
	// ServiceName 为空表示对任意服务生效。
	ServiceName string `gorm:"size:128;index" json:"service_name"`
	// Pattern 匹配**日志原文（message）**：普通文本按子串匹配，`/re/` 形式按正则匹配；必填。
	//
	// 刻意不匹配 error_signature：指纹是哈希值，使用者写不出"我想屏蔽的那句话"对应的哈希，
	// 只能写出日志消息里的文本——匹配原文才是他能真正配置的维度。
	Pattern string `gorm:"size:512;not null" json:"pattern"`
	Enabled bool   `gorm:"default:true;index" json:"enabled"`
}

// TableName 显式指定表名，避免 GORM 复数化差异。
func (LogAlertExclusion) TableName() string { return "log_alert_exclusions" }

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
	// RepoRevision 是本次分析所用的代码版本（短 sha，来源见 repo.Fetcher.Revision）。
	//
	// 为什么必须落库：行号定位会随代码演进而失效，事后复核时要能回答
	// "这条结论当时看的是哪一版代码"——没有它，"定位错了"与"代码已经改了"无法区分（INC-032）。
	RepoRevision string `gorm:"size:64" json:"repo_revision"`
	// ReportURL 是外部 AI 服务给出的完整报告地址（开放接口的 reportUrl）。
	//
	// 平台上只存结论摘要（补丁全文、验证结果、完整 Markdown 都在对方报告页），
	// 没有这个字段，使用者在平台上看到结论后想看细节就只能去翻通知记录里的链接。
	ReportURL string `gorm:"size:512" json:"report_url"`
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

// KafkaConsumeStat 是日志消费链路的**累计**计数（按 topic + 消费组一行）。
//
// 为什么要有它：消费者的 consumed 原本只是进程内的原子计数，容器重建后归零，
// 而 Kafka 位点已经提交、消息不会重投——页面上的"已消费条数"因此永远小于
// 真正消费过的总量。落库后页面给出的是可跨重启、跨副本累加的累计值。
type KafkaConsumeStat struct {
	Base
	Topic    string `gorm:"size:128;index" json:"topic"`
	GroupID  string `gorm:"size:128;index" json:"group_id"`
	Consumed int64  `json:"consumed"`
	// Ingested 为真正入库（新增或合并进既有事件）的条数。
	Ingested int64 `json:"ingested"`
	// Ignored 为平台有意不入库的条数（命中屏蔽项 / 未命中规则）。
	Ignored int64 `json:"ignored"`
	Dropped int64 `json:"dropped"`
	Failed  int64 `json:"failed"`
}

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
		&ServerInstance{}, &LogAlertEvent{}, &LogAlertRule{}, &LogAlertExclusion{}, &AICodeAnalysis{}, &AIAnalysisTask{},
		&KafkaConsumeStat{},
		&NotificationLog{},
		&PlatformSetting{},
	}
}

// Migrate 执行全部实体的自动迁移。
func Migrate(db *gorm.DB) error {
	return db.AutoMigrate(MigrationList()...)
}
