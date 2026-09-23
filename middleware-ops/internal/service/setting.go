package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/gorm"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// SettingService 实现「平台自管设置」：AI 提供方与通知渠道改由平台界面管理，改完即时生效。
//
// 背景：这两类配置以前只能写在 .env / configs/config.yaml 里——换一个 api_key 或 webhook 地址
// 就要改环境变量、重建容器，密钥还容易散落在多处。搬到平台后由界面管理，密钥加密落库。
//
// 三条约定（都是踩过的坑）：
//  1. 密钥只在响应里以 *_set / *_masked 出现，明文只存在于「内存配置」与「加密那一瞬间」；
//  2. 入参里空串表示「不修改」而不是「清空」——界面不回显明文，用户没动输入框时不能把密钥清掉，
//     要清空必须显式传 clear_xxx=true；
//  3. 保存 = 加密落库 → 覆盖内存 cfg → 重建引擎 / 热更新护栏配额，全过程不需要重启进程。
type SettingService struct {
	cfg     *config.Config
	db      *gorm.DB
	repo    *repository.SettingRepository
	cipher  cipherCodec
	engine  engine.Engine
	factory *engine.Factory
	cost    *guardrail.Cost
	notify  *NotifierService
	audit   *AuditService
	log     *zap.Logger

	// onAIApplied 在 AI 设置生效后被调用：让「AI 代码分析」的客户端按新配置重建。
	//
	// 用回调而不是直接依赖 CodeAnalysisService，是因为装配顺序上设置服务先于它创建
	//（启动时要先把平台设置对齐到内存，领域服务才装配），直接引用会形成顺序耦合。
	onAIApplied func()

	// saveMu 串行化保存动作。
	//
	// 保存不是一次纯内存操作：它要「读旧值 → 合并 → 加密落库 → 覆盖内存配置 → 重建引擎」。
	// 两个管理员同时保存（或同一人双击）会让两条链路交叉读写同一份内存配置，可能把 A 的
	// base_url 和 B 的密钥拼在一起落库。加一把进程内互斥锁，代价只有"保存偶尔多等几毫秒"。
	saveMu sync.Mutex
}

// SettingDeps 是构造设置服务所需的依赖。
type SettingDeps struct {
	Config *config.Config
	DB     *gorm.DB
	Repo   *repository.SettingRepository
	Cipher cipherCodec
	// Engine 传「工厂自身」而不是快照引擎：工厂实现了 Engine 接口，Reload 后各服务持有的
	// 指针自动指向新引擎（见 engine.Factory 的注释）。
	Engine  engine.Engine
	Factory *engine.Factory
	Cost    *guardrail.Cost
	// Notifier 用于把通知配置立刻推给发送链路（发送方读的是它自己的原子快照）。
	Notifier *NotifierService
	Audit    *AuditService
	Log      *zap.Logger
}

// NewSettingService 构造设置服务。
func NewSettingService(d SettingDeps) *SettingService {
	return &SettingService{
		cfg: d.Config, db: d.DB, repo: d.Repo, cipher: d.Cipher,
		engine: d.Engine, factory: d.Factory, cost: d.Cost, notify: d.Notifier,
		audit: d.Audit, log: d.Log,
	}
}

// SetAIAppliedHook 注册"AI 设置生效"回调（由容器在 CodeAnalysisService 装配后调用）。
func (s *SettingService) SetAIAppliedHook(fn func()) {
	if s == nil {
		return
	}
	s.onAIApplied = fn
}

// 设置项标识（platform_settings.name）。
const (
	SettingNameAI       = "ai"
	SettingNameNotify   = "notify"
	SettingNameSecurity = "security"
)

// 设置来源：platform=平台库生效，env=.env / config.yaml 兜底。
const (
	SettingSourcePlatform = "platform"
	SettingSourceEnv      = "env"
)

// settingSeedOperator 是「首次从 .env 导入」时记录的修改人，便于界面区分人工修改与自动导入。
const settingSeedOperator = "env-import"

// 用量统计来源（对应诊断记录表与代码分析表，表名由 model.TableNameOf 推导，见 INC-019）。
const (
	usageSourceDiagnosis    = "diagnosis"
	usageSourceCodeAnalysis = "code_analysis"
)

// 用量窗口限制：默认近 30 天，最多 90 天（再长对"看当天消耗"没有意义，只会拖慢聚合查询）。
const (
	defaultUsageDays = 30
	maxUsageDays     = 90
)

// dateLayout 与 utils.DayKey 一致（UTC 日粒度），保证用量页的「今天」与成本护栏的「今天」是同一天。
const dateLayout = "2006-01-02"

// ---------------------------------------------------------------------------
// 持久化形态（密文内容）
// ---------------------------------------------------------------------------

// aiProviderPayload 是单个 AI 提供方的持久化形态，**含明文密钥**。
//
// 明文只出现在「明文 JSON → AES-256-GCM 密文」这一瞬间以及进程内存里：落库的是整段密文，
// 接口响应只有 api_key_set / api_key_masked。
type aiProviderPayload struct {
	Enabled        bool    `json:"enabled"`
	Kind           string  `json:"kind"`
	BaseURL        string  `json:"base_url"`
	APIKey         string  `json:"api_key"`
	Model          string  `json:"model"`
	MaxTokens      int     `json:"max_tokens"`
	PricePerKToken float64 `json:"price_per_k_token"`
}

// aiAnalysisPayload 是「AI 代码分析」外部服务的持久化形态（含明文密钥）。
//
// 它就是日志告警那条链路的对接参数：平台把问题提交给它、它回调/被轮询回结论，
// 详见 internal/service/ai_analysis_client.go。放在 AI 设置里统一管理，
// 是为了让"密钥与地址"只有平台这一个来源（与提供方设置同样的三条约定）。
//
// 时长字段用字符串（如 "30m"）而不是纳秒数字：它要被界面编辑、也要被人读，
// 写 1800000000000 这种数字谁都看不出是多久。解析失败时保留原值。
type aiAnalysisPayload struct {
	Enabled         bool   `json:"enabled"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	SubmitPath      string `json:"submit_path"`
	QueryPath       string `json:"query_path"`
	CallbackURL     string `json:"callback_url"`
	CallbackToken   string `json:"callback_token"`
	Timeout         string `json:"timeout"`
	TaskTimeout     string `json:"task_timeout"`
	PollInterval    string `json:"poll_interval"`
	PollBatch       int    `json:"poll_batch"`
	CallMode        string `json:"call_mode"`
	SyncTimeout     string `json:"sync_timeout"`
	NotifyOnSubmit  bool   `json:"notify_on_submit"`
	// ---- 开放接口 v1（Protocol=openapi_v1）专用 ----
	Protocol        string `json:"protocol"`
	AuthHeader      string `json:"auth_header"`
	SyncSubmitPath  string `json:"sync_submit_path"`
	RepoLocatorMode string `json:"repo_locator_mode"`
	RepoGitURL      string `json:"repo_git_url"`
	Environment     string `json:"environment"`
	Priority        int    `json:"priority"`
	AutoVerify      bool   `json:"auto_verify"`
}

// aiSettingsPayload 是 AI 设置的完整持久化形态。
type aiSettingsPayload struct {
	Strategy        string            `json:"strategy"`
	ThirdParty      aiProviderPayload `json:"third_party"`
	SelfHosted      aiProviderPayload `json:"self_hosted"`
	CodeAnalysis    aiAnalysisPayload `json:"code_analysis"`
	DailyTokenQuota int64             `json:"daily_token_quota"`
	PerUserQuota    int64             `json:"per_user_daily_quota"`
}

// notifyChannelPayload 是 Webhook 类渠道的持久化形态（含明文 webhook 与签名密钥）。
type notifyChannelPayload struct {
	Enabled  bool     `json:"enabled"`
	Webhook  string   `json:"webhook"`
	Secret   string   `json:"secret"`
	Mentions []string `json:"mentions"`
}

// emailPayload 是邮件渠道的持久化形态（含明文 SMTP 口令）。
type emailPayload struct {
	Enabled  bool     `json:"enabled"`
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	UseTLS   bool     `json:"use_tls"`
}

// notifySettingsPayload 是通知设置的完整持久化形态。
type notifySettingsPayload struct {
	Enabled         bool                 `json:"enabled"`
	Feishu          notifyChannelPayload `json:"feishu"`
	WeCom           notifyChannelPayload `json:"wecom"`
	DingTalk        notifyChannelPayload `json:"dingtalk"`
	Email           emailPayload         `json:"email"`
	CardConfirmPath string               `json:"card_confirm_path"`
}

// ---------------------------------------------------------------------------
// 对外形态（响应）
// ---------------------------------------------------------------------------

// ProviderSettingsView 是单个提供方的展示形态：密钥只以「是否已配置 + 掩码」出现。
type ProviderSettingsView struct {
	Enabled        bool    `json:"enabled"`
	Kind           string  `json:"kind"`
	BaseURL        string  `json:"base_url"`
	APIKeySet      bool    `json:"api_key_set"`
	APIKeyMasked   string  `json:"api_key_masked"`
	Model          string  `json:"model"`
	MaxTokens      int     `json:"max_tokens"`
	PricePerKToken float64 `json:"price_per_k_token"`
}

// AIAnalysisView 是「AI 代码分析」的展示形态：两把密钥只以「是否已配置 + 掩码」出现。
type AIAnalysisView struct {
	Enabled             bool   `json:"enabled"`
	BaseURL             string `json:"base_url"`
	APIKeySet           bool   `json:"api_key_set"`
	APIKeyMasked        string `json:"api_key_masked"`
	SubmitPath          string `json:"submit_path"`
	QueryPath           string `json:"query_path"`
	CallbackURL         string `json:"callback_url"`
	CallbackTokenSet    bool   `json:"callback_token_set"`
	CallbackTokenMasked string `json:"callback_token_masked"`
	Timeout             string `json:"timeout"`
	TaskTimeout         string `json:"task_timeout"`
	PollInterval        string `json:"poll_interval"`
	PollBatch           int    `json:"poll_batch"`
	CallMode            string `json:"call_mode"`
	SyncTimeout         string `json:"sync_timeout"`
	NotifyOnSubmit      bool   `json:"notify_on_submit"`
	// Protocol 为对接协议：generic（默认）| openapi_v1。
	Protocol        string `json:"protocol"`
	AuthHeader      string `json:"auth_header"`
	SyncSubmitPath  string `json:"sync_submit_path"`
	RepoLocatorMode string `json:"repo_locator_mode"`
	RepoGitURL      string `json:"repo_git_url"`
	Environment     string `json:"environment"`
	Priority        int    `json:"priority"`
	AutoVerify      bool   `json:"auto_verify"`
	// Configured 表示"现在真的能调用"（开关已开且地址非空）。
	// 页面据此提示"填了但没生效"，避免管理员以为配好了却在日志页看到"未装配"。
	Configured bool `json:"configured"`
}

// AISettingsView 是 GET/PUT /api/settings/ai 的响应体。
type AISettingsView struct {
	Strategy          string               `json:"strategy"`
	ThirdParty        ProviderSettingsView `json:"third_party"`
	SelfHosted        ProviderSettingsView `json:"self_hosted"`
	CodeAnalysis      AIAnalysisView       `json:"code_analysis"`
	DailyTokenQuota   int64                `json:"daily_token_quota"`
	PerUserDailyQuota int64                `json:"per_user_daily_quota"`
	UpdatedBy         string               `json:"updated_by"`
	UpdatedAt         string               `json:"updated_at"`
	Source            string               `json:"source"`
	// ProvidersActive 为「当前真正会参与的提供方」标签（复用 engine.Keys 的格式），
	// 界面据此提示"配了但没生效"（例如第三方的 key 为空会被跳过）。
	ProvidersActive []string `json:"providers_active"`
}

// NotifyChannelView 是 Webhook 类渠道的展示形态。
type NotifyChannelView struct {
	Enabled       bool     `json:"enabled"`
	WebhookSet    bool     `json:"webhook_set"`
	WebhookMasked string   `json:"webhook_masked"`
	SecretSet     bool     `json:"secret_set"`
	Mentions      []string `json:"mentions"`
}

// EmailChannelView 是邮件渠道的展示形态（口令只回 bool）。
type EmailChannelView struct {
	Enabled     bool     `json:"enabled"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	Username    string   `json:"username"`
	PasswordSet bool     `json:"password_set"`
	From        string   `json:"from"`
	To          []string `json:"to"`
	UseTLS      bool     `json:"use_tls"`
}

// securitySettingsPayload 是「合规设置」的持久化形态。
//
// 出网白名单决定"哪些服务的日志可以被送去外部 AI 分析"，是整条链路上最敏感的一道闸门：
// 默认空 = 全禁。它必须能在界面上逐条维护，而不是让管理员去改 .env 再重启容器——
// 那会让"放行一个新服务"变成一次部署动作。
type securitySettingsPayload struct {
	// OutboundWhitelist 为允许外发的服务名；"*" 表示全部放行。
	OutboundWhitelist []string `json:"outbound_whitelist"`
}

// SecuritySettingsView 是 GET/PUT /api/settings/security 的响应体。
type SecuritySettingsView struct {
	OutboundWhitelist []string `json:"outbound_whitelist"`
	UpdatedBy         string   `json:"updated_by"`
	UpdatedAt         string   `json:"updated_at"`
	// Source 为 platform（平台库生效）| env（仍读进程配置，保存后转为 platform）。
	Source string `json:"source"`
}

// SecuritySettingsInput 是 PUT /api/settings/security 的请求体。
type SecuritySettingsInput struct {
	OutboundWhitelist []string `json:"outbound_whitelist"`
}

// NotifySettingsView 是 GET/PUT /api/settings/notify 的响应体。
type NotifySettingsView struct {
	Enabled         bool              `json:"enabled"`
	Feishu          NotifyChannelView `json:"feishu"`
	WeCom           NotifyChannelView `json:"wecom"`
	DingTalk        NotifyChannelView `json:"dingtalk"`
	Email           EmailChannelView  `json:"email"`
	CardConfirmPath string            `json:"card_confirm_path"`
	UpdatedBy       string            `json:"updated_by"`
	UpdatedAt       string            `json:"updated_at"`
	Source          string            `json:"source"`
}

// ---------------------------------------------------------------------------
// 入参
// ---------------------------------------------------------------------------

// ProviderSettingsInput 是单个提供方的入参。
type ProviderSettingsInput struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	// APIKey 为空串表示「保持原密钥不变」，ClearAPIKey 为 true 表示清空，其余为覆盖。
	APIKey         string  `json:"api_key"`
	ClearAPIKey    bool    `json:"clear_api_key"`
	Model          string  `json:"model"`
	MaxTokens      int     `json:"max_tokens"`
	PricePerKToken float64 `json:"price_per_k_token"`
}

// AIAnalysisInput 是「AI 代码分析」的入参：两把密钥走三态语义（空=不变，clear=清空）。
type AIAnalysisInput struct {
	Enabled            bool   `json:"enabled"`
	BaseURL            string `json:"base_url"`
	APIKey             string `json:"api_key"`
	ClearAPIKey        bool   `json:"clear_api_key"`
	SubmitPath         string `json:"submit_path"`
	QueryPath          string `json:"query_path"`
	CallbackURL        string `json:"callback_url"`
	CallbackToken      string `json:"callback_token"`
	ClearCallbackToken bool   `json:"clear_callback_token"`
	Timeout            string `json:"timeout"`
	TaskTimeout        string `json:"task_timeout"`
	PollInterval       string `json:"poll_interval"`
	PollBatch          int    `json:"poll_batch"`
	CallMode           string `json:"call_mode"`
	SyncTimeout        string `json:"sync_timeout"`
	NotifyOnSubmit     bool   `json:"notify_on_submit"`
	// 开放接口 v1 专用；切回 generic 时这些字段被忽略但不清除（避免来回切换丢配置）。
	Protocol           string `json:"protocol"`
	AuthHeader         string `json:"auth_header"`
	SyncSubmitPath     string `json:"sync_submit_path"`
	RepoLocatorMode    string `json:"repo_locator_mode"`
	RepoGitURL         string `json:"repo_git_url"`
	Environment        string `json:"environment"`
	Priority           int    `json:"priority"`
	AutoVerify         bool   `json:"auto_verify"`
}

// AISettingsInput 是 PUT /api/settings/ai 的请求体。
type AISettingsInput struct {
	Strategy          string                `json:"strategy"`
	ThirdParty        ProviderSettingsInput `json:"third_party"`
	SelfHosted        ProviderSettingsInput `json:"self_hosted"`
	CodeAnalysis      AIAnalysisInput       `json:"code_analysis"`
	DailyTokenQuota   int64                 `json:"daily_token_quota"`
	PerUserDailyQuota int64                 `json:"per_user_daily_quota"`
}

// NotifyChannelInput 是 Webhook 类渠道的入参。
type NotifyChannelInput struct {
	Enabled bool `json:"enabled"`
	// Webhook / Secret 空串 = 不变；clear_webhook / clear_secret = true 表示清空。
	Webhook      string   `json:"webhook"`
	ClearWebhook bool     `json:"clear_webhook"`
	Secret       string   `json:"secret"`
	ClearSecret  bool     `json:"clear_secret"`
	Mentions     []string `json:"mentions"`
}

// EmailChannelInput 是邮件渠道的入参。
type EmailChannelInput struct {
	Enabled       bool     `json:"enabled"`
	Host          string   `json:"host"`
	Port          int      `json:"port"`
	Username      string   `json:"username"`
	Password      string   `json:"password"`
	ClearPassword bool     `json:"clear_password"`
	From          string   `json:"from"`
	To            []string `json:"to"`
	UseTLS        bool     `json:"use_tls"`
}

// NotifySettingsInput 是 PUT /api/settings/notify 的请求体。
type NotifySettingsInput struct {
	Enabled         bool               `json:"enabled"`
	Feishu          NotifyChannelInput `json:"feishu"`
	WeCom           NotifyChannelInput `json:"wecom"`
	DingTalk        NotifyChannelInput `json:"dingtalk"`
	Email           EmailChannelInput  `json:"email"`
	CardConfirmPath string             `json:"card_confirm_path"`
}

// ---------------------------------------------------------------------------
// 纯函数：密钥合并 / 掩码 / 配额计算（可单测，不依赖 DB）
// ---------------------------------------------------------------------------

// mergeSecret 合并密钥入参，三态语义与界面一致：
//   - 空串 = 保持不变（界面不回显明文，用户没动输入框就不该把密钥清掉）；
//   - clear = true = 明确清空（用户点了「清除」）；
//   - 非空 = 覆盖。
//
// clear 优先于非空：前端若同时带上浏览器残留值与「清除」标记，以用户的显式动作为准。
func mergeSecret(existing, in string, clear bool) string {
	if clear {
		return ""
	}
	if strings.TrimSpace(in) == "" {
		return existing
	}
	return in
}

// secretChange 描述一次密钥修改：是否换了值、是否是清空。
//
// 审计只记这两个布尔位，不记密钥本身——审计日志是要长期保存并对外开放检索的，
// 一旦写进明文密钥，等于把密钥永久留在了数据库里。
func secretChange(existing, in string, clear bool) (changed, cleared bool) {
	if clear {
		return existing != "", true
	}
	if strings.TrimSpace(in) == "" {
		return false, false
	}
	return in != existing, false
}

// maskSecret 生成密钥展示串：保留前 3 位与后 4 位（如 sk-****cdef）。
//
// 长度不足以同时容纳前后缀（会互相重叠、反而暴露更多字符）时整体打星。
// 用 rune 切分而不是字节切分，避免把多字节字符切坏成乱码。
func maskSecret(secret string) string {
	if secret == "" {
		return ""
	}
	r := []rune(secret)
	if len(r) < 7 {
		return "****"
	}
	return string(r[:3]) + "****" + string(r[len(r)-4:])
}

// remainingQuota 计算今日剩余额度。
//
// quota<=0 表示「不限额」，返回 -1 而不是一个大数或 0：0 会被界面与使用者读成
// 「今天已经用完了」，反而促使人去调配额。
func remainingQuota(quota, used int64) int64 {
	if quota <= 0 {
		return -1
	}
	if used >= quota {
		return 0
	}
	return quota - used
}

// usedRatio 计算今日额度使用率；不限额时为 0（不画进度条语义）。
func usedRatio(quota, used int64) float64 {
	if quota <= 0 {
		return 0
	}
	return float64(used) / float64(quota)
}

// normalizeUsageDays 归一化用量窗口：默认 30 天，上限 90 天。
func normalizeUsageDays(days int) int {
	if days <= 0 {
		return defaultUsageDays
	}
	if days > maxUsageDays {
		return maxUsageDays
	}
	return days
}

// usageAggRow 是按天聚合的一行（窗口内，来自两张表之一）。
type usageAggRow struct {
	Source string `gorm:"column:source"`
	DayKey string `gorm:"column:day_key"`
	Tokens int64  `gorm:"column:tokens"`
	Calls  int64  `gorm:"column:calls"`
}

// AIUsagePoint 是折线图上的一个点。
type AIUsagePoint struct {
	Date   string `json:"date"`
	Tokens int64  `json:"tokens"`
	Calls  int64  `json:"calls"`
}

// AIUsageSource 是分来源的消耗统计。
type AIUsageSource struct {
	Source string `json:"source"`
	Tokens int64  `json:"tokens"`
	Calls  int64  `json:"calls"`
}

// AIUsageUser 是按用户聚合的消耗（username 从 users 表 join，查不到时为空串）。
type AIUsageUser struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Tokens   int64  `json:"tokens"`
	Calls    int64  `json:"calls"`
}

// AIUsageView 是 GET /api/settings/ai/usage 的响应体。
type AIUsageView struct {
	TodayTokens       int64           `json:"today_tokens"`
	TodayCalls        int64           `json:"today_calls"`
	DailyQuota        int64           `json:"daily_quota"`
	PerUserDailyQuota int64           `json:"per_user_daily_quota"`
	RemainingToday    int64           `json:"remaining_today"`
	UsedRatio         float64         `json:"used_ratio"`
	WindowDays        int             `json:"window_days"`
	WindowTokens      int64           `json:"window_tokens"`
	WindowCalls       int64           `json:"window_calls"`
	Series            []AIUsagePoint  `json:"series"`
	BySource          []AIUsageSource `json:"by_source"`
	TopUsers          []AIUsageUser   `json:"top_users"`
}

// usageWindowStart 返回窗口起点（UTC 零点）：从 days 天前那天 00:00 起算。
func usageWindowStart(now time.Time, days int) time.Time {
	utc := now.UTC()
	midnight := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
	return midnight.AddDate(0, 0, -days)
}

// buildUsageSeries 把按天聚合的稀疏结果补齐为连续日期序列。
//
// 为什么必须补零：前端折线图按数组下标画点，缺一天就会把两个不相邻的日期直接连起来，
// 看上去"消耗平稳"，实际那天根本没有调用。补齐后 x 轴刻度与日期一一对应。
// 序列长度为 days+1：包含「days 天前」的起点与今天两天，统计口径与 window 一致。
func buildUsageSeries(rows []usageAggRow, days int, now time.Time) []AIUsagePoint {
	utc := now.UTC()
	start := usageWindowStart(utc, days)
	end := time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)

	byDay := make(map[string]AIUsagePoint, len(rows))
	for _, row := range rows {
		// 同一天可能同时有诊断与代码分析的消耗，必须累加而不是覆盖。
		point := byDay[row.DayKey]
		point.Date = row.DayKey
		point.Tokens += row.Tokens
		point.Calls += row.Calls
		byDay[row.DayKey] = point
	}

	out := make([]AIUsagePoint, 0, days+1)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		key := day.Format(dateLayout)
		point := byDay[key]
		point.Date = key
		out = append(out, point)
	}
	return out
}

// usageSummary 是窗口内的汇总结果。
type usageSummary struct {
	WindowTokens int64
	WindowCalls  int64
	TodayTokens  int64
	TodayCalls   int64
	BySource     []AIUsageSource
}

// summarizeUsage 汇总窗口总量、今日消耗与分来源明细。
//
// 今日直接取序列最后一天，与折线图同一个口径——否则"今日卡片"与"图上最后一根柱子"
// 会对不上（一个用 SQL 的今天、一个用日期轴的最后一天，边界差一小时就会互相打架）。
func summarizeUsage(points []AIUsagePoint, rows []usageAggRow) usageSummary {
	sources := make(map[string]AIUsageSource, 2)
	for _, row := range rows {
		item := sources[row.Source]
		item.Source = row.Source
		item.Tokens += row.Tokens
		item.Calls += row.Calls
		sources[row.Source] = item
	}

	out := usageSummary{}
	for _, point := range points {
		out.WindowTokens += point.Tokens
		out.WindowCalls += point.Calls
	}
	if len(points) > 0 {
		last := points[len(points)-1]
		out.TodayTokens = last.Tokens
		out.TodayCalls = last.Calls
	}

	// 固定两项顺序：界面的图例按顺序取色，顺序漂移会导致颜色跳变。
	out.BySource = make([]AIUsageSource, 0, len(sources))
	for _, name := range []string{usageSourceDiagnosis, usageSourceCodeAnalysis} {
		item := sources[name]
		item.Source = name
		out.BySource = append(out.BySource, item)
	}
	// 以后新增消耗来源时仍然展示出来，不让统计凭空消失。
	extra := make([]string, 0, len(sources))
	for name := range sources {
		if name != usageSourceDiagnosis && name != usageSourceCodeAnalysis {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		out.BySource = append(out.BySource, sources[name])
	}
	return out
}

// ---------------------------------------------------------------------------
// 加解密与落库
// ---------------------------------------------------------------------------

// persist 把整个 payload 加密后落库（Upsert）。
//
// 加密的是**整段 JSON**而不是单个字段：一来新增字段不用改加密逻辑，二来 base_url、webhook
// 这类地址同样可能带凭据（如 https://user:pass@host），整体加密不留死角。
func (s *SettingService) persist(ctx context.Context, name string, payload any, updatedBy string) error {
	if s.cipher == nil {
		return apperr.New(apperr.CodeInternal, "未配置主密钥，无法加密平台设置")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	encrypted, err := s.cipher.Encrypt(string(raw))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.repo.Upsert(ctx, name, encrypted, updatedBy); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	return nil
}

// loadSetting 读取并解密设置项；DB 无记录时 found=false（调用方自行决定回退策略）。
func (s *SettingService) loadSetting(ctx context.Context, name string, target any) (updatedBy string, updatedAt time.Time, found bool, err error) {
	item, err := s.repo.Get(ctx, name)
	if err != nil {
		return "", time.Time{}, false, apperr.Wrap(apperr.CodeInternal, err)
	}
	// 记录不存在或密文为空 = 还没在平台上配置过（首次启动的正常状态）。
	if item == nil || strings.TrimSpace(item.PayloadEncrypted) == "" {
		return "", time.Time{}, false, nil
	}
	if s.cipher == nil {
		return "", time.Time{}, false, apperr.New(apperr.CodeInternal, "未配置主密钥，无法解密平台设置")
	}
	raw, err := s.cipher.Decrypt(item.PayloadEncrypted)
	if err != nil {
		// 解密失败几乎只可能是主密钥变了（security.master_key / master.key 被替换）。
		// 这里必须显式报错而不是悄悄回退 .env：否则界面看上去"设置丢了"，
		// 管理员再点一次保存就把旧设置彻底覆盖掉了。
		return "", time.Time{}, false, apperr.Wrapf(apperr.CodeInternal, err,
			"平台设置 %q 解密失败：主密钥是否已变更？", name)
	}
	if err := json.Unmarshal([]byte(raw), target); err != nil {
		return "", time.Time{}, false, apperr.Wrapf(apperr.CodeInternal, err, "解析平台设置 %q 失败", name)
	}
	return item.UpdatedBy, item.UpdatedAt, true, nil
}

// ---------------------------------------------------------------------------
// 合规设置（出网白名单）
// ---------------------------------------------------------------------------

// securityPayloadFromConfig 以当前内存配置为基线（DB 无记录时的展示与合并基线）。
func securityPayloadFromConfig(cfg *config.Config) securitySettingsPayload {
	if cfg == nil {
		return securitySettingsPayload{}
	}
	return securitySettingsPayload{OutboundWhitelist: append([]string(nil), cfg.Security.OutboundWhitelist...)}
}

// normalizeWhitelist 规范化白名单：去空白、去重、丢空项。
//
// 去重不只是为了好看：同一个服务名写两遍会让"移除了一个、其实还在"这种误判发生，
// 而白名单是合规闸门，宁可严格也不能含糊。
func normalizeWhitelist(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		v := strings.TrimSpace(item)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// SecuritySettings 读取合规设置；DB 无记录时以进程配置（.env / config.yaml）为基线，source=env。
func (s *SettingService) SecuritySettings(ctx context.Context) (*SecuritySettingsView, error) {
	var payload securitySettingsPayload
	updatedBy, updatedAt, found, err := s.loadSetting(ctx, SettingNameSecurity, &payload)
	if err != nil {
		return nil, err
	}
	source := SettingSourceEnv
	if found {
		source = SettingSourcePlatform
	} else {
		payload = securityPayloadFromConfig(s.cfg)
	}
	return &SecuritySettingsView{
		OutboundWhitelist: normalizeWhitelist(payload.OutboundWhitelist),
		UpdatedBy:         updatedBy,
		UpdatedAt:         formatSettingTime(updatedAt),
		Source:            source,
	}, nil
}

// SaveSecuritySettings 保存合规设置：加密落库 → 立即覆盖内存配置 → 记审计。
//
// 白名单不需要"重建客户端"，只要把内存配置改掉，下一次合规判定就按新名单走——
// 判定是每次调用时读 cfg（见 CodeAnalysisService.outboundGate），不缓存。
func (s *SettingService) SaveSecuritySettings(ctx context.Context, in SecuritySettingsInput, operator Operator) (*SecuritySettingsView, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	next := securitySettingsPayload{OutboundWhitelist: normalizeWhitelist(in.OutboundWhitelist)}
	if err := s.persist(ctx, SettingNameSecurity, next, operator.Username); err != nil {
		return nil, err
	}
	s.applySecurityPayload(next)
	s.record(ctx, operator, "security_settings_update", map[string]any{
		"outbound_whitelist": next.OutboundWhitelist,
	})
	return s.SecuritySettings(ctx)
}

// applySecurityPayload 把白名单写回内存配置（保存与启动时共用）。
func (s *SettingService) applySecurityPayload(p securitySettingsPayload) {
	if s.cfg == nil {
		return
	}
	s.cfg.Security.OutboundWhitelist = normalizeWhitelist(p.OutboundWhitelist)
}

// ApplySecurity 在启动时把平台库里的合规设置对齐到内存。
//
// 无记录时**不动**内存配置：与通知设置同一套约定——没在平台配过就沿用进程配置，
// 界面上以它为基线展示（source=env），管理员保存一次即转为平台托管。
func (s *SettingService) ApplySecurity(ctx context.Context) error {
	var payload securitySettingsPayload
	updatedBy, _, found, err := s.loadSetting(ctx, SettingNameSecurity, &payload)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	s.applySecurityPayload(payload)
	s.log.Info("合规设置已按平台记录生效",
		zap.String("updated_by", updatedBy),
		zap.Strings("outbound_whitelist", payload.OutboundWhitelist))
	return nil
}

// formatSettingTime 统一时间展示口径（空值输出空串，界面不用再判 null）。
func formatSettingTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------------------
// AI 设置
// ---------------------------------------------------------------------------

// payloadFromConfig 以当前内存配置为基线生成 AI payload（DB 无记录时的合并基线 / 首次导入内容）。
// defaultAISettingsPayload 是无平台记录时的「平台默认」配置：不读取任何 .env / config.yaml，
// 提供方留空且未启用、额度归零（0 = 不限额），由管理员在界面上完成平台托管配置。
// 平台是 AI 设置的唯一来源，.env / config.yaml 只作进程级兜底，不再参与平台设置。
func defaultAISettingsPayload() aiSettingsPayload {
	return aiSettingsPayload{
		Strategy:   "hybrid",
		ThirdParty: aiProviderPayload{Enabled: false, Kind: "openai", MaxTokens: 2048},
		SelfHosted: aiProviderPayload{Enabled: false, Kind: "ollama", MaxTokens: 2048},
		// AI 代码分析默认关闭：没配外部服务地址就不该假装能分析
		//（否则日志事件会停在"排队中"，而使用者看到的原因却是"AI 分析中"）。
		CodeAnalysis: aiAnalysisPayload{
			Enabled: false, SubmitPath: defaultSubmitPath, QueryPath: defaultQueryPath,
			Timeout: "15s", TaskTimeout: "30m", PollInterval: "60s", PollBatch: defaultPollBatch,
			CallMode: defaultCallModeAsync, SyncTimeout: defaultSyncTimeout,
			Protocol: defaultProtocol, RepoLocatorMode: repoLocatorModeService,
		},
		DailyTokenQuota: 0,
		PerUserQuota:    0,
	}
}

// AI 代码分析的默认对接形状（与 config 里的历史默认值一致，界面首次打开就有可用值）。
const (
	defaultSubmitPath = "/v1/analyses"
	defaultQueryPath  = "/v1/analyses/{task_id}"
	defaultPollBatch  = 20
	// 调用方式默认异步（回调/轮询），除非用户明确选同步。
	defaultCallModeAsync = "async"
	defaultCallModeSync  = "sync"
	defaultSyncTimeout   = "5m"
)

// resolveCallMode 归一化调用方式：只认 async / sync，其它一律回落异步（默认更安全）。
func resolveCallMode(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case defaultCallModeAsync:
		return defaultCallModeAsync
	case defaultCallModeSync:
		return defaultCallModeSync
	default:
		return defaultCallModeAsync
	}
}

// parseDurationOr 解析 "30m" 这类时长；空值或非法时**保留原值**而不是静默改成 0。
//
// 静默归零的后果是超时立刻触发（0 超时 = 每次都超时），比"沿用旧值"危险得多。
func parseDurationOr(raw string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return fallback
	}
	return v
}

// applyTo 把 payload 写进 AI 引擎配置（不含护栏配额，配额由 applyQuotas 单独处理）。
func (p aiSettingsPayload) applyTo(cfg *config.AIEngineConfig) {
	cfg.Strategy = p.Strategy
	cfg.ThirdParty = config.ProviderConfig{
		Enabled: p.ThirdParty.Enabled, Kind: p.ThirdParty.Kind, BaseURL: p.ThirdParty.BaseURL,
		APIKey: p.ThirdParty.APIKey, Model: p.ThirdParty.Model, MaxTokens: p.ThirdParty.MaxTokens,
		PricePerKToken: p.ThirdParty.PricePerKToken,
		// 超时链与 embedding 模型仍由配置文件决定：它们不是"密钥搬家"的目标，
		// 平台设置里也不展示，这里保留原值避免保存一次界面就把它们清空。
		Timeout:        cfg.ThirdParty.Timeout,
		EmbeddingModel: cfg.ThirdParty.EmbeddingModel,
	}
	cfg.SelfHosted = config.ProviderConfig{
		Enabled: p.SelfHosted.Enabled, Kind: p.SelfHosted.Kind, BaseURL: p.SelfHosted.BaseURL,
		APIKey: p.SelfHosted.APIKey, Model: p.SelfHosted.Model, MaxTokens: p.SelfHosted.MaxTokens,
		PricePerKToken: p.SelfHosted.PricePerKToken,
		Timeout:        cfg.SelfHosted.Timeout,
		EmbeddingModel: cfg.SelfHosted.EmbeddingModel,
	}
}

// applyCodeAnalysisTo 把「AI 代码分析」落到根配置上（它不在 AIEngine 之下，是独立的一段）。
func (p aiSettingsPayload) applyCodeAnalysisTo(cfg *config.Config) {
	cfg.AIAnalysis = config.AIAnalysisConfig{
		Enabled:        p.CodeAnalysis.Enabled,
		BaseURL:        p.CodeAnalysis.BaseURL,
		APIKey:         p.CodeAnalysis.APIKey,
		SubmitPath:     defaultString(p.CodeAnalysis.SubmitPath, defaultSubmitPath),
		QueryPath:      defaultString(p.CodeAnalysis.QueryPath, defaultQueryPath),
		CallbackURL:    p.CodeAnalysis.CallbackURL,
		CallbackToken:  p.CodeAnalysis.CallbackToken,
		Timeout:        parseDurationOr(p.CodeAnalysis.Timeout, cfg.AIAnalysis.Timeout),
		TaskTimeout:    parseDurationOr(p.CodeAnalysis.TaskTimeout, cfg.AIAnalysis.TaskTimeout),
		PollInterval:   parseDurationOr(p.CodeAnalysis.PollInterval, cfg.AIAnalysis.PollInterval),
		PollBatch:      p.CodeAnalysis.PollBatch,
		CallMode:        resolveCallMode(p.CodeAnalysis.CallMode),
		SyncTimeout:     parseDurationOr(p.CodeAnalysis.SyncTimeout, cfg.AIAnalysis.SyncTimeout),
		NotifyOnSubmit:  p.CodeAnalysis.NotifyOnSubmit,
		Protocol:        resolveProtocol(p.CodeAnalysis.Protocol),
		AuthHeader:      strings.TrimSpace(p.CodeAnalysis.AuthHeader),
		SyncSubmitPath:  strings.TrimSpace(p.CodeAnalysis.SyncSubmitPath),
		RepoLocatorMode: resolveRepoLocatorMode(p.CodeAnalysis.RepoLocatorMode),
		RepoGitURL:      strings.TrimSpace(p.CodeAnalysis.RepoGitURL),
		Environment:     strings.TrimSpace(p.CodeAnalysis.Environment),
		Priority:        p.CodeAnalysis.Priority,
		AutoVerify:      p.CodeAnalysis.AutoVerify,
	}
	if cfg.AIAnalysis.PollBatch <= 0 {
		cfg.AIAnalysis.PollBatch = defaultPollBatch
	}
}

// view 生成对外展示形态。
func (p aiProviderPayload) view() ProviderSettingsView {
	return ProviderSettingsView{
		Enabled: p.Enabled, Kind: p.Kind, BaseURL: p.BaseURL,
		APIKeySet: strings.TrimSpace(p.APIKey) != "", APIKeyMasked: maskSecret(p.APIKey),
		Model: p.Model, MaxTokens: p.MaxTokens, PricePerKToken: p.PricePerKToken,
	}
}

// view 生成 AI 代码分析的展示形态（两把密钥只给掩码）。
func (p aiAnalysisPayload) view() AIAnalysisView {
	return AIAnalysisView{
		Enabled:             p.Enabled,
		BaseURL:             p.BaseURL,
		APIKeySet:           strings.TrimSpace(p.APIKey) != "",
		APIKeyMasked:        maskSecret(p.APIKey),
		SubmitPath:          p.SubmitPath,
		QueryPath:           p.QueryPath,
		CallbackURL:         p.CallbackURL,
		CallbackTokenSet:    strings.TrimSpace(p.CallbackToken) != "",
		CallbackTokenMasked: maskSecret(p.CallbackToken),
		Timeout:             p.Timeout,
		TaskTimeout:         p.TaskTimeout,
		PollInterval:        p.PollInterval,
		PollBatch:           p.PollBatch,
		CallMode:            p.CallMode,
		SyncTimeout:         p.SyncTimeout,
		NotifyOnSubmit:      p.NotifyOnSubmit,
		Protocol:            p.Protocol,
		AuthHeader:          p.AuthHeader,
		SyncSubmitPath:      p.SyncSubmitPath,
		RepoLocatorMode:     p.RepoLocatorMode,
		RepoGitURL:          p.RepoGitURL,
		Environment:         p.Environment,
		Priority:            p.Priority,
		AutoVerify:          p.AutoVerify,
		Configured:          p.Enabled && strings.TrimSpace(p.BaseURL) != "",
	}
}

// 协议常量与默认值（与 client 层同源，避免两处各写一套）。
const (
	protocolGeneric = "generic"
	protocolOpenAPI = "openapi_v1"
	defaultProtocol = protocolGeneric
	// 开放接口要求 API Key 放在 X-API-Key 头里（不带 Bearer 前缀）。
	defaultAuthHeaderOpenAPI = "X-API-Key"
	// 开放接口的默认端点（文档 §5 / §6 / 轮询）。
	defaultOpenAPISubmitPath = "/api/v1/openapi/tasks"
	defaultOpenAPISyncPath   = "/api/v1/openapi/analyze"
	defaultOpenAPIQueryPath  = "/api/v1/runs/{run_id}"
	// 仓库定位方式：service（用服务名当 host）| git_url（固定 git 地址）。
	repoLocatorModeService = "service"
	repoLocatorModeGitURL  = "git_url"
)

// resolveProtocol 归一化协议：只认 generic / openapi_v1，其它一律回落 generic。
//
// 为什么不能让非法值透传：协议决定请求体与响应解析的整套形状，
// 一个拼错的值会让链路以"AI 服务返回成功但没有结论"这种极难排查的方式失败。
func resolveProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case protocolOpenAPI:
		return protocolOpenAPI
	default:
		return protocolGeneric
	}
}

// resolveRepoLocatorMode 归一化仓库定位方式（只认 service / git_url）。
func resolveRepoLocatorMode(raw string) string {
	if strings.EqualFold(strings.TrimSpace(raw), repoLocatorModeGitURL) {
		return repoLocatorModeGitURL
	}
	return repoLocatorModeService
}

// applyProtocolDefaults 在协议切换时把"还没被改过的"路径改写成该协议的默认值。
//
// 关键场景：管理员把协议从 generic 切到 openapi_v1 时，页面上留着的历史默认值
// /v1/analyses 会直接打到开放接口上，表现为 404。
// 只在"仍是上一个协议的默认值"时才改写，用户自己填过的路径一律保留。
func applyProtocolDefaults(p aiAnalysisPayload, prevProtocol string) aiAnalysisPayload {
	if p.Protocol == prevProtocol {
		return p
	}
	switch p.Protocol {
	case protocolOpenAPI:
		if p.SubmitPath == "" || p.SubmitPath == defaultSubmitPath {
			p.SubmitPath = defaultOpenAPISubmitPath
		}
		if p.SyncSubmitPath == "" {
			p.SyncSubmitPath = defaultOpenAPISyncPath
		}
		if p.QueryPath == "" || p.QueryPath == defaultQueryPath {
			p.QueryPath = defaultOpenAPIQueryPath
		}
		if strings.TrimSpace(p.AuthHeader) == "" {
			p.AuthHeader = defaultAuthHeaderOpenAPI
		}
	case protocolGeneric:
		if p.SubmitPath == "" || p.SubmitPath == defaultOpenAPISubmitPath {
			p.SubmitPath = defaultSubmitPath
		}
		if p.QueryPath == "" || p.QueryPath == defaultOpenAPIQueryPath {
			p.QueryPath = defaultQueryPath
		}
	}
	if p.RepoLocatorMode == "" {
		p.RepoLocatorMode = repoLocatorModeService
	}
	return p
}

// mergeAIAnalysis 合并 AI 代码分析入参：非密钥字段按"非空即覆盖"，密钥走三态语义。
//
// 路径与时长按"非空即覆盖"而不是整体覆盖：这几项界面上经常只改其中一个，
// 整体覆盖会把没填的字段清空（SubmitPath 空了等于客户端拼不出地址）。
func mergeAIAnalysis(old aiAnalysisPayload, in AIAnalysisInput) aiAnalysisPayload {
	out := old
	// 协议是枚举且决定整套报文形状：只有明确传值才切换，
	// 空值保持原样（否则前端漏传一次就会把开放接口悄悄改回 generic）。
	prevProtocol := resolveProtocol(old.Protocol)
	if v := strings.TrimSpace(in.Protocol); v != "" {
		out.Protocol = resolveProtocol(v)
	}
	out.Enabled = in.Enabled
	out.BaseURL = strings.TrimSpace(in.BaseURL)
	out.CallbackURL = strings.TrimSpace(in.CallbackURL)
	if v := strings.TrimSpace(in.SubmitPath); v != "" {
		out.SubmitPath = v
	}
	if v := strings.TrimSpace(in.QueryPath); v != "" {
		out.QueryPath = v
	}
	if v := strings.TrimSpace(in.Timeout); v != "" {
		out.Timeout = v
	}
	if v := strings.TrimSpace(in.TaskTimeout); v != "" {
		out.TaskTimeout = v
	}
	if v := strings.TrimSpace(in.PollInterval); v != "" {
		out.PollInterval = v
	}
	if in.PollBatch > 0 {
		out.PollBatch = in.PollBatch
	}
	out.CallMode = resolveCallMode(in.CallMode)
	if v := strings.TrimSpace(in.SyncTimeout); v != "" {
		out.SyncTimeout = v
	}
	out.NotifyOnSubmit = in.NotifyOnSubmit
	out.APIKey = mergeSecret(old.APIKey, in.APIKey, in.ClearAPIKey)
	out.CallbackToken = mergeSecret(old.CallbackToken, in.CallbackToken, in.ClearCallbackToken)

	// 开放接口专有字段（非空即覆盖，与路径/时长同一套语义）。
	if v := strings.TrimSpace(in.AuthHeader); v != "" {
		out.AuthHeader = v
	}
	if v := strings.TrimSpace(in.SyncSubmitPath); v != "" {
		out.SyncSubmitPath = v
	}
	if v := strings.TrimSpace(in.RepoLocatorMode); v != "" {
		out.RepoLocatorMode = resolveRepoLocatorMode(v)
	}
	if v := strings.TrimSpace(in.RepoGitURL); v != "" {
		out.RepoGitURL = v
	}
	if v := strings.TrimSpace(in.Environment); v != "" {
		out.Environment = v
	}
	if in.Priority > 0 {
		out.Priority = in.Priority
	} else if in.Priority < 0 {
		out.Priority = 0
	}
	out.AutoVerify = in.AutoVerify

	// 兜底：老记录里没有这两个路径时补默认值，避免客户端拼出空路径。
	if strings.TrimSpace(out.SubmitPath) == "" {
		out.SubmitPath = defaultSubmitPath
	}
	if strings.TrimSpace(out.QueryPath) == "" {
		out.QueryPath = defaultQueryPath
	}
	if out.PollBatch <= 0 {
		out.PollBatch = defaultPollBatch
	}
	return applyProtocolDefaults(out, prevProtocol)
}

// providersActive 计算「真正会参与的提供方」标签。
//
// 直接用展示用的 payload 计算，而不是读内存 cfg：两者一旦不同步（例如 DB 被外部改过），
// 页面会出现「api_key_set=true 但 providers_active 里没有该提供方」这种自相矛盾的展示。
func (p aiSettingsPayload) providersActive() []string {
	probe := &config.Config{}
	p.applyTo(&probe.AIEngine)
	_, providers := engine.Keys(probe)
	return providers
}

// AISettings 读取 AI 设置；DB 无记录时返回平台默认（不读取 .env / config.yaml）。
// 平台是 AI 设置的唯一来源：来源恒为 platform，界面不再区分「环境变量 / 平台」。
func (s *SettingService) AISettings(ctx context.Context) (*AISettingsView, error) {
	var payload aiSettingsPayload
	updatedBy, updatedAt, found, err := s.loadSetting(ctx, SettingNameAI, &payload)
	if err != nil {
		return nil, err
	}
	// 无平台记录即视为「尚未在平台配置」：用平台默认，绝不回退 .env / config.yaml。
	if !found {
		payload = defaultAISettingsPayload()
	}
	return &AISettingsView{
		Strategy:          payload.Strategy,
		ThirdParty:        payload.ThirdParty.view(),
		SelfHosted:        payload.SelfHosted.view(),
		CodeAnalysis:      payload.CodeAnalysis.view(),
		DailyTokenQuota:   payload.DailyTokenQuota,
		PerUserDailyQuota: payload.PerUserQuota,
		UpdatedBy:         updatedBy,
		UpdatedAt:         formatSettingTime(updatedAt),
		Source:            SettingSourcePlatform,
		ProvidersActive:   payload.providersActive(),
	}, nil
}

// SaveAISettings 保存 AI 设置：加密落库 → 覆盖内存配置 → 重建引擎 → 热更新护栏配额 → 记审计。
func (s *SettingService) SaveAISettings(ctx context.Context, in AISettingsInput, operator Operator) (*AISettingsView, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	if err := validateAIStrategy(in.Strategy); err != nil {
		return nil, err
	}
	if in.DailyTokenQuota < 0 || in.PerUserDailyQuota < 0 {
		return nil, apperr.New(apperr.CodeInvalidParam, "token 配额不能为负数（0 表示不限额）")
	}

	// 合并基线：DB 有记录用 DB，没有就用平台默认——保证 .env / config.yaml 的密钥绝不会因为
	// 「管理员只改了模型名（没重新输入密钥）」而被悄悄读进平台库（平台是 AI 设置的唯一来源）。
	var existing aiSettingsPayload
	_, _, found, err := s.loadSetting(ctx, SettingNameAI, &existing)
	if err != nil {
		return nil, err
	}
	if !found {
		existing = defaultAISettingsPayload()
	}

	next := existing
	if v := strings.TrimSpace(in.Strategy); v != "" {
		next.Strategy = v
	}
	next.ThirdParty = mergeAIProvider(existing.ThirdParty, in.ThirdParty)
	next.SelfHosted = mergeAIProvider(existing.SelfHosted, in.SelfHosted)
	next.CodeAnalysis = mergeAIAnalysis(existing.CodeAnalysis, in.CodeAnalysis)
	next.DailyTokenQuota = in.DailyTokenQuota
	next.PerUserQuota = in.PerUserDailyQuota

	if err := s.persist(ctx, SettingNameAI, next, operator.Username); err != nil {
		return nil, err
	}
	s.applyAIPayload(next)

	s.record(ctx, operator, "ai_settings_update", map[string]any{
		"strategy":                next.Strategy,
		"third_party_enabled":     next.ThirdParty.Enabled,
		"third_party_kind":        next.ThirdParty.Kind,
		"third_party_base_url":    redactCredentials(next.ThirdParty.BaseURL),
		"third_party_model":       next.ThirdParty.Model,
		"third_party_key_changed": thirdChanged(existing.ThirdParty, in.ThirdParty),
		"third_party_key_cleared": in.ThirdParty.ClearAPIKey,
		"self_hosted_enabled":     next.SelfHosted.Enabled,
		"self_hosted_kind":        next.SelfHosted.Kind,
		"self_hosted_base_url":    redactCredentials(next.SelfHosted.BaseURL),
		"self_hosted_model":       next.SelfHosted.Model,
		"self_hosted_key_changed": thirdChanged(existing.SelfHosted, in.SelfHosted),
		"self_hosted_key_cleared": in.SelfHosted.ClearAPIKey,
		"daily_token_quota":       next.DailyTokenQuota,
		"per_user_daily_quota":    next.PerUserQuota,
		"providers_active":        next.providersActive(),
		// AI 代码分析：只记开关、地址（脱敏）与"密钥是否被改过"，不记任何密钥。
		"code_analysis_enabled":        next.CodeAnalysis.Enabled,
		"code_analysis_base_url":       redactCredentials(next.CodeAnalysis.BaseURL),
		"code_analysis_key_changed":    analysisSecretChanged(existing.CodeAnalysis, in.CodeAnalysis),
		"code_analysis_call_mode":      next.CodeAnalysis.CallMode,
		"code_analysis_task_timeout":   next.CodeAnalysis.TaskTimeout,
		"code_analysis_notify_submit":  next.CodeAnalysis.NotifyOnSubmit,
	})

	return s.AISettings(ctx)
}

// thirdChanged 判断某提供方的密钥是否被换过（只记布尔，不记密钥）。
func thirdChanged(existing aiProviderPayload, in ProviderSettingsInput) bool {
	changed, _ := secretChange(existing.APIKey, in.APIKey, in.ClearAPIKey)
	return changed
}

// mergeAIProvider 合并提供方入参：非密钥字段整体覆盖（PUT 语义），密钥走三态语义。
func mergeAIProvider(old aiProviderPayload, in ProviderSettingsInput) aiProviderPayload {
	out := aiProviderPayload{
		Enabled:        in.Enabled,
		Kind:           strings.TrimSpace(in.Kind),
		BaseURL:        strings.TrimSpace(in.BaseURL),
		Model:          strings.TrimSpace(in.Model),
		MaxTokens:      in.MaxTokens,
		PricePerKToken: in.PricePerKToken,
	}
	// MaxTokens<=0 视为"未填"：HTTP 提供方内部会把 <=0 兜成 2048，若原样落库，
	// 界面会长期显示 0 而引擎实际跑 2048，对不上。
	if out.MaxTokens <= 0 {
		out.MaxTokens = old.MaxTokens
	}
	out.APIKey = mergeSecret(old.APIKey, in.APIKey, in.ClearAPIKey)
	return out
}

// validateAIStrategy 校验策略取值（与 config.Validate 同一套取值，避免保存成功但引擎静默降级）。
func validateAIStrategy(strategy string) error {
	switch engine.Strategy(strings.TrimSpace(strategy)) {
	case engine.StrategyThirdParty, engine.StrategySelfHosted, engine.StrategyHybrid:
		return nil
	case "":
		// 空值表示沿用原策略。
		return nil
	default:
		return apperr.Newf(apperr.CodeInvalidParam, "AI 策略 %q 非法（可选 third_party/self_hosted/hybrid）", strategy)
	}
}

// analysisSecretChanged 汇报 AI 代码分析的两把密钥是否被改动（只记布尔）。
func analysisSecretChanged(old aiAnalysisPayload, in AIAnalysisInput) map[string]any {
	keyChanged, keyCleared := secretChange(old.APIKey, in.APIKey, in.ClearAPIKey)
	tokenChanged, tokenCleared := secretChange(old.CallbackToken, in.CallbackToken, in.ClearCallbackToken)
	return map[string]any{
		"api_key_changed":    keyChanged,
		"api_key_cleared":    keyCleared,
		"callback_token_changed": tokenChanged,
		"callback_token_cleared": tokenCleared,
	}
}

// applyAIPayload 把 payload 落到内存配置并让引擎 / 护栏立即生效。
func (s *SettingService) applyAIPayload(payload aiSettingsPayload) {
	payload.applyTo(&s.cfg.AIEngine)
	s.cfg.Guardrail.DailyTokenQuota = payload.DailyTokenQuota
	s.cfg.Guardrail.PerUserQuota = payload.PerUserQuota
	// AI 代码分析的客户端必须跟着换：它内部持有 base_url 与密钥，
	// 不重建的话"界面保存成功"而实际还在用旧地址调用（或仍在用已清空的密钥）。
	payload.applyCodeAnalysisTo(s.cfg)
	if s.onAIApplied != nil {
		s.onAIApplied()
	}
	// 引擎按新配置重建：Factory 自身是 Engine 门面，各 service 持有的指针自动看到新引擎。
	if s.factory != nil {
		s.factory.Reload()
	}
	// 护栏配额必须同步热更新：否则界面显示"配额已调大"，实际仍然按旧配额熔断。
	if s.cost != nil {
		s.cost.UpdateQuotas(payload.DailyTokenQuota, payload.PerUserQuota)
	}
}

// ApplyAI 在启动时把「平台库里的 AI 配置」对齐到内存：DB 有记录则以其为准覆盖内存 cfg；
// 无记录则不做任何导入——平台未配置时引擎按进程配置（config.yaml / .env 兜底）运行，
// 由管理员在「AI 设置」界面完成平台托管。平台是 AI 设置的唯一来源，启动阶段不再读取 .env。
//
// 为什么要"以 DB 为准"：否则会出现"界面改完 → 重启容器 → 又变回旧值"，
// 管理员会以为平台设置不生效；反过来不再做"首次导入"，是因为导入会把 .env 密钥写进平台库，
// 与"平台是唯一来源、不读 .env"的约定冲突。
func (s *SettingService) ApplyAI(ctx context.Context) error {
	var payload aiSettingsPayload
	updatedBy, _, found, err := s.loadSetting(ctx, SettingNameAI, &payload)
	if err != nil {
		return err
	}
	if !found {
		// 没有平台记录：不读 .env，不自动导入；引擎沿用进程级配置，等界面配置。
		return nil
	}
	s.applyAIPayload(payload)
	s.log.Info("AI 设置已按平台记录生效（平台为唯一来源，.env 不再参与）",
		zap.String("updated_by", updatedBy),
		zap.Strings("providers", payload.providersActive()))
	return nil
}

// ---------------------------------------------------------------------------
// 通知设置
// ---------------------------------------------------------------------------

// notifyPayloadFromConfig 以当前内存配置为基线生成通知 payload。
func notifyPayloadFromConfig(cfg *config.Config) notifySettingsPayload {
	channel := func(c config.WebhookChannelConfig) notifyChannelPayload {
		return notifyChannelPayload{Enabled: c.Enabled, Webhook: c.Webhook, Secret: c.Secret, Mentions: normalizeMentions(c.Mentions)}
	}
	return notifySettingsPayload{
		Enabled:  cfg.Notify.Enabled,
		Feishu:   channel(cfg.Notify.Feishu),
		WeCom:    channel(cfg.Notify.WeCom),
		DingTalk: channel(cfg.Notify.DingTalk),
		Email: emailPayload{
			Enabled: cfg.Notify.Email.Enabled, Host: cfg.Notify.Email.Host, Port: cfg.Notify.Email.Port,
			Username: cfg.Notify.Email.Username, Password: cfg.Notify.Email.Password,
			From: cfg.Notify.Email.From, To: normalizeMentions(cfg.Notify.Email.To), UseTLS: cfg.Notify.Email.UseTLS,
		},
		CardConfirmPath: cfg.Notify.CardConfirmPath,
	}
}

// toConfig 把 payload 转成内存配置值（不直接改写传入的配置对象）。
//
// 为什么返回新值而不是"原地写 &cfg.Notify"：通知发送链路在其它 goroutine 上读这份配置，
// 就地逐字段改写会让它拷到"半个旧配置 + 半个新配置"。整份新值交给 NotifierService.UpdateConfig
// 原子替换后，读方看到的永远是自洽的一份（见 notifier.go 的注释）。
func (p notifySettingsPayload) toConfig() config.NotifyConfig {
	return config.NotifyConfig{
		Enabled: p.Enabled,
		Feishu:  config.WebhookChannelConfig{Enabled: p.Feishu.Enabled, Webhook: p.Feishu.Webhook, Secret: p.Feishu.Secret, Mentions: normalizeMentions(p.Feishu.Mentions)},
		WeCom:   config.WebhookChannelConfig{Enabled: p.WeCom.Enabled, Webhook: p.WeCom.Webhook, Secret: p.WeCom.Secret, Mentions: normalizeMentions(p.WeCom.Mentions)},
		DingTalk: config.WebhookChannelConfig{
			Enabled: p.DingTalk.Enabled, Webhook: p.DingTalk.Webhook, Secret: p.DingTalk.Secret, Mentions: normalizeMentions(p.DingTalk.Mentions),
		},
		Email: config.EmailChannelConfig{
			Enabled: p.Email.Enabled, Host: p.Email.Host, Port: p.Email.Port, Username: p.Email.Username,
			Password: p.Email.Password, From: p.Email.From, To: normalizeMentions(p.Email.To), UseTLS: p.Email.UseTLS,
		},
		CardConfirmPath: p.CardConfirmPath,
	}
}

// applyNotifyPayload 让通知配置立即生效：
//  1. 覆盖内存 cfg（供界面展示、以及后续"以 .env 为基线"的合并读到的都是当前值）；
//  2. 把整份快照推给通知服务——发送链路读的是它自己那份原子快照，不推就等于没改。
func (s *SettingService) applyNotifyPayload(p notifySettingsPayload) {
	next := p.toConfig()
	s.cfg.Notify = next
	if s.notify != nil {
		s.notify.UpdateConfig(next)
	}
}

// redactCredentials 去掉 URL 里的 userinfo（https://user:pass@host → https://***:***@host）。
//
// 为什么审计里也要脱敏：审计长期保存且支持检索，而 base_url 完全可能写成带基本认证的形式
// （有些自建网关/代理就是要求把 token 放在 userinfo 里）。原样记录等于把凭据永久留在库里。
//
// 只处理「scheme 之后、第一个 / 之前」那段（userinfo 只可能出现在这里）：这样
// https://host/v1/@me 这类路径里带 @ 的合法地址不会被误改；没有 scheme 的写法
// （user:pass@host）无法可靠区分用户名与协议名，就整段打码——宁可少记一点。
func redactCredentials(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !strings.Contains(raw, "@") {
		return raw
	}
	prefix, rest := "", raw
	if i := strings.Index(raw, "://"); i >= 0 {
		prefix, rest = raw[:i+3], raw[i+3:]
	}
	end := len(rest)
	if i := strings.Index(rest, "/"); i >= 0 {
		end = i
	}
	authority := rest[:end]
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		// @ 落在路径/查询里，不是凭据。
		return raw
	}
	return prefix + "***:***@" + authority[at+1:] + rest[end:]
}

// hasSecrets 报告通知配置里是否有需要"搬家"的密钥。
func (p notifySettingsPayload) hasSecrets() bool {
	for _, c := range []notifyChannelPayload{p.Feishu, p.WeCom, p.DingTalk} {
		if strings.TrimSpace(c.Webhook) != "" || strings.TrimSpace(c.Secret) != "" {
			return true
		}
	}
	return strings.TrimSpace(p.Email.Password) != ""
}

// view 生成对外展示形态（webhook 只给掩码，secret/口令只给 bool）。
func (p notifyChannelPayload) view() NotifyChannelView {
	return NotifyChannelView{
		Enabled:       p.Enabled,
		WebhookSet:    strings.TrimSpace(p.Webhook) != "",
		WebhookMasked: maskSecret(p.Webhook),
		SecretSet:     strings.TrimSpace(p.Secret) != "",
		Mentions:      normalizeMentions(p.Mentions),
	}
}

// NotifySettings 读取通知设置；DB 无记录时回退到 .env / config.yaml（source=env）。
func (s *SettingService) NotifySettings(ctx context.Context) (*NotifySettingsView, error) {
	var payload notifySettingsPayload
	updatedBy, updatedAt, found, err := s.loadSetting(ctx, SettingNameNotify, &payload)
	if err != nil {
		return nil, err
	}
	source := SettingSourceEnv
	if found {
		source = SettingSourcePlatform
	} else {
		payload = notifyPayloadFromConfig(s.cfg)
	}
	return &NotifySettingsView{
		Enabled:  payload.Enabled,
		Feishu:   payload.Feishu.view(),
		WeCom:    payload.WeCom.view(),
		DingTalk: payload.DingTalk.view(),
		Email: EmailChannelView{
			Enabled: payload.Email.Enabled, Host: payload.Email.Host, Port: payload.Email.Port,
			Username: payload.Email.Username, PasswordSet: strings.TrimSpace(payload.Email.Password) != "",
			From: payload.Email.From, To: normalizeMentions(payload.Email.To), UseTLS: payload.Email.UseTLS,
		},
		CardConfirmPath: payload.CardConfirmPath,
		UpdatedBy:       updatedBy,
		UpdatedAt:       formatSettingTime(updatedAt),
		Source:          source,
	}, nil
}

// SaveNotifySettings 保存通知设置：加密落库 → 原地更新内存配置（立即生效）→ 记审计。
func (s *SettingService) SaveNotifySettings(ctx context.Context, in NotifySettingsInput, operator Operator) (*NotifySettingsView, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	if in.Email.Port < 0 || in.Email.Port > 65535 {
		return nil, apperr.New(apperr.CodeInvalidParam, "SMTP 端口必须在 1-65535 之间")
	}

	var existing notifySettingsPayload
	_, _, found, err := s.loadSetting(ctx, SettingNameNotify, &existing)
	if err != nil {
		return nil, err
	}
	if !found {
		existing = notifyPayloadFromConfig(s.cfg)
	}

	next := notifySettingsPayload{
		Enabled:         in.Enabled,
		Feishu:          mergeNotifyChannel(existing.Feishu, in.Feishu),
		WeCom:           mergeNotifyChannel(existing.WeCom, in.WeCom),
		DingTalk:        mergeNotifyChannel(existing.DingTalk, in.DingTalk),
		Email:           mergeEmailChannel(existing.Email, in.Email),
		CardConfirmPath: strings.TrimSpace(in.CardConfirmPath),
	}

	if err := s.persist(ctx, SettingNameNotify, next, operator.Username); err != nil {
		return nil, err
	}
	s.applyNotifyPayload(next)

	s.record(ctx, operator, "notify_settings_update", map[string]any{
		"enabled":                  next.Enabled,
		"card_confirm_path":        next.CardConfirmPath,
		"feishu_enabled":           next.Feishu.Enabled,
		"feishu_webhook_changed":   channelChange(existing.Feishu, in.Feishu),
		"wecom_enabled":            next.WeCom.Enabled,
		"wecom_webhook_changed":    channelChange(existing.WeCom, in.WeCom),
		"dingtalk_enabled":         next.DingTalk.Enabled,
		"dingtalk_webhook_changed": channelChange(existing.DingTalk, in.DingTalk),
		"email_enabled":            next.Email.Enabled,
		"email_host":               next.Email.Host,
		"email_password_changed":   emailPasswordChanged(existing.Email, in.Email),
	})

	return s.NotifySettings(ctx)
}

// channelChange 汇报渠道密钥（webhook/secret）的变更情况：只记布尔与枚举，不记值。
func channelChange(existing notifyChannelPayload, in NotifyChannelInput) map[string]any {
	webhookChanged, webhookCleared := secretChange(existing.Webhook, in.Webhook, in.ClearWebhook)
	secretChanged, secretCleared := secretChange(existing.Secret, in.Secret, in.ClearSecret)
	return map[string]any{
		"webhook_changed": webhookChanged,
		"webhook_cleared": webhookCleared,
		"secret_changed":  secretChanged,
		"secret_cleared":  secretCleared,
	}
}

// emailPasswordChanged 汇报 SMTP 口令是否变更。
func emailPasswordChanged(existing emailPayload, in EmailChannelInput) map[string]any {
	changed, cleared := secretChange(existing.Password, in.Password, in.ClearPassword)
	return map[string]any{"password_changed": changed, "password_cleared": cleared}
}

// mergeNotifyChannel 合并 Webhook 类渠道：开关/成员整体覆盖，webhook/secret 走三态语义。
func mergeNotifyChannel(old notifyChannelPayload, in NotifyChannelInput) notifyChannelPayload {
	return notifyChannelPayload{
		Enabled:  in.Enabled,
		Webhook:  mergeSecret(old.Webhook, in.Webhook, in.ClearWebhook),
		Secret:   mergeSecret(old.Secret, in.Secret, in.ClearSecret),
		Mentions: normalizeMentions(in.Mentions),
	}
}

// mergeEmailChannel 合并邮件渠道：口令走三态语义；端口 <=0 视为未填，沿用旧值。
func mergeEmailChannel(old emailPayload, in EmailChannelInput) emailPayload {
	port := in.Port
	if port <= 0 {
		port = old.Port
	}
	if port <= 0 {
		port = 465
	}
	return emailPayload{
		Enabled:  in.Enabled,
		Host:     strings.TrimSpace(in.Host),
		Port:     port,
		Username: strings.TrimSpace(in.Username),
		Password: mergeSecret(old.Password, in.Password, in.ClearPassword),
		From:     strings.TrimSpace(in.From),
		To:       normalizeMentions(in.To),
		UseTLS:   in.UseTLS,
	}
}

// normalizeMentions 去空白并保证非 nil：JSON 应输出 [] 而不是 null，前端不必再判空。
func normalizeMentions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, item := range in {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ApplyNotify 在启动时把「生效的通知配置」对齐到平台库（与 ApplyAI 同一套语义）。
func (s *SettingService) ApplyNotify(ctx context.Context) error {
	var payload notifySettingsPayload
	updatedBy, _, found, err := s.loadSetting(ctx, SettingNameNotify, &payload)
	if err != nil {
		return err
	}
	if found {
		s.applyNotifyPayload(payload)
		s.log.Info("通知设置已按平台记录生效（.env 中的同名配置不再覆盖平台设置）",
			zap.String("updated_by", updatedBy))
		return nil
	}

	seed := notifyPayloadFromConfig(s.cfg)
	if !seed.hasSecrets() {
		return nil
	}
	if err := s.persist(ctx, SettingNameNotify, seed, settingSeedOperator); err != nil {
		return err
	}
	s.applyNotifyPayload(seed)
	s.log.Info("通知设置已从 .env / config.yaml 首次导入平台，后续请用界面「通知设置」管理（.env 仅作兜底）")
	return nil
}

// ---------------------------------------------------------------------------
// AI 用量
// ---------------------------------------------------------------------------

// AIUsage 统计 token 消费与剩余额度。
//
// 数据来源就是已有的两张业务表（诊断记录 / 代码分析）的 cost_tokens，
// 不额外维护"用量流水表"：流水表要保证与业务写入强一致，反而多一处可能对不上的账。
func (s *SettingService) AIUsage(ctx context.Context, days int) (*AIUsageView, error) {
	days = normalizeUsageDays(days)
	now := time.Now().UTC()
	start := usageWindowStart(now, days)

	rows, err := s.usageRows(ctx, start)
	if err != nil {
		return nil, err
	}
	topUsers, err := s.topUsageUsers(ctx, start)
	if err != nil {
		return nil, err
	}

	series := buildUsageSeries(rows, days, now)
	summary := summarizeUsage(series, rows)
	dailyQuota := s.cfg.Guardrail.DailyTokenQuota
	perUserQuota := s.cfg.Guardrail.PerUserQuota

	return &AIUsageView{
		TodayTokens:       summary.TodayTokens,
		TodayCalls:        summary.TodayCalls,
		DailyQuota:        dailyQuota,
		PerUserDailyQuota: perUserQuota,
		RemainingToday:    remainingQuota(dailyQuota, summary.TodayTokens),
		UsedRatio:         usedRatio(dailyQuota, summary.TodayTokens),
		WindowDays:        days,
		WindowTokens:      summary.WindowTokens,
		WindowCalls:       summary.WindowCalls,
		Series:            series,
		BySource:          summary.BySource,
		TopUsers:          topUsers,
	}, nil
}

// usagePointColumns 是「一条消耗记录」的投影（两张表列名一致，便于 UNION ALL）。
//
// 时间分桶用 to_char(... AT TIME ZONE 'UTC')：与 utils.DayKey、成本护栏的跨天清零同一口径，
// 否则同一次调用在"今日卡片"和"折线图"里可能落到不同的日子。
const usagePointColumns = `to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day_key,
           COALESCE(cost_tokens, 0) AS tokens,
           1 AS calls`

// usageTables 解析用量统计需要的真实表名。
//
// 表名必须由 model.TableNameOf 推导，不能照直觉写：GORM 把 AIDiagnosis 落地成
// a_idiagnoses（AI 里的 ID 被当作常见缩写改写后再切词），照直觉写 ai_diagnoses
// 会在运行期报 SQLSTATE 42P01 relation does not exist（INC-019）。
func usageTables() (diagnosis, codeAnalysis, user string, err error) {
	diagnosis = model.TableNameOf(&model.AIDiagnosis{})
	codeAnalysis = model.TableNameOf(&model.AICodeAnalysis{})
	user = model.TableNameOf(&model.User{})
	if diagnosis == "" || codeAnalysis == "" || user == "" {
		// 空表名不能当成"没问题"：那会拼出一条永远报错的 SQL，把真实原因藏起来。
		return "", "", "", apperr.New(apperr.CodeInternal, "无法解析用量统计的数据表名")
	}
	return diagnosis, codeAnalysis, user, nil
}

// usageRows 聚合窗口内「按来源 + 按天」的 token 消耗。
//
// calls 记的是"一次消耗记录"（含命中确定性缓存、cost_tokens=0 的那次请求）：
// 缓存命中同样是用户发起的一次诊断，把它从调用次数里抹掉会让"调用量"看起来忽高忽低；
// token 维度仍然是真实消耗，缓存命中天然贡献 0。
func (s *SettingService) usageRows(ctx context.Context, start time.Time) ([]usageAggRow, error) {
	diagnosis, codeAnalysis, _, err := usageTables()
	if err != nil {
		return nil, err
	}
	// 拼接进去的是**代码推导出的表名**（不含任何用户输入），列名与占位符保持字面量。
	query := fmt.Sprintf(`
SELECT source, day_key, COALESCE(SUM(tokens), 0) AS tokens, COALESCE(SUM(calls), 0) AS calls
FROM (
    SELECT ? AS source, %s
    FROM %s
    WHERE created_at >= ?
    UNION ALL
    SELECT ? AS source, %s
    FROM %s
    WHERE created_at >= ?
) t
GROUP BY source, day_key
ORDER BY day_key`,
		usagePointColumns, diagnosis, usagePointColumns, codeAnalysis)
	var rows []usageAggRow
	if err := s.db.WithContext(ctx).
		Raw(query, usageSourceDiagnosis, start, usageSourceCodeAnalysis, start).
		Scan(&rows).Error; err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return rows, nil
}

// topUsageUsers 取窗口内消耗最高的 10 个用户，username 从 users 表 join。
//
// 只统计诊断记录：代码分析表没有 user_id 列，无法归属到人，
// 强行按"服务"折算成用户会给出错误的责任归属。
func (s *SettingService) topUsageUsers(ctx context.Context, start time.Time) ([]AIUsageUser, error) {
	diagnosis, _, user, err := usageTables()
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`
SELECT d.user_id AS user_id,
       COALESCE(u.username, '') AS username,
       COALESCE(SUM(COALESCE(d.cost_tokens, 0)), 0) AS tokens,
       COUNT(*) AS calls
FROM %s d
LEFT JOIN %s u ON u.id = d.user_id
WHERE d.created_at >= ?
GROUP BY d.user_id, u.username
ORDER BY tokens DESC, d.user_id ASC
LIMIT 10`, diagnosis, user)
	var rows []AIUsageUser
	if err := s.db.WithContext(ctx).Raw(query, start).Scan(&rows).Error; err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if rows == nil {
		rows = []AIUsageUser{}
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// 引擎自检
// ---------------------------------------------------------------------------

// testAITimeout 限制自检耗时：设置页点「测试」不应把 HTTP 请求挂在那里等 60 秒。
const testAITimeout = 20 * time.Second

// probeEngine 用「ping」最小请求验证一个引擎是否可用，返回是否成功、可读原因与耗时（毫秒）。
//
// 失败**不产生 error**：自检的意义就是把失败原因显示给配置人看（401 / 模型名不存在 /
// 连不上），用 error 反而会被前端统一处理成"服务异常"，看不到真正原因。
// 混合策略下提供方失败会自动降级到规则引擎并**返回成功**，只看 err 会把坏密钥报成"测试通过"，
// 因此再看一眼引擎状态：处于降级态就如实把原因报出来。
func probeEngine(ctx context.Context, eng engine.Engine) (ok bool, message string, latencyMs int64) {
	ctx, cancel := context.WithTimeout(ctx, testAITimeout)
	defer cancel()
	start := time.Now()
	resp, err := eng.Chat(ctx, engine.ChatRequest{
		Messages: []engine.Message{{Role: engine.RoleUser, Content: "ping"}},
		// 输出上限调到很小：自检只验证"地址/密钥/模型是否可用"，
		// 不该为一次连通性测试消耗正常诊断的 token 预算。
		MaxTokens:   8,
		Temperature: 0,
	})
	latency := time.Since(start).Milliseconds()
	if err != nil {
		// 错误串里通常带完整 URL：先脱敏再截断，避免把 base_url 里的凭据显示到页面上。
		return false, truncateMessage("调用失败：" + redactCredentials(err.Error())), latency
	}
	if status := eng.Status(); status.Degraded {
		reason := strings.TrimSpace(status.LastError)
		if reason == "" {
			reason = "已降级"
		}
		return false, truncateMessage("提供方不可用：" + redactCredentials(reason)), latency
	}
	content := strings.TrimSpace(resp.Content)
	if content == "" {
		content = "引擎返回空内容"
	}
	return true, truncateMessage(content), latency
}

// TestAI 用当前生效配置发一次最小请求，返回是否可用与耗时。
//
// 失败**不返回 error**（即不产生 500）：自检的意义就是"把失败原因显示给配置人看"，
// 用 500 反而会被前端统一处理成"服务异常"，看不到真正的原因（401 / 模型名不存在 / 连不上）。
func (s *SettingService) TestAI(ctx context.Context) (bool, string, string, int64, error) {
	if s.engine == nil {
		return false, "", "AI 引擎未装配", 0, nil
	}
	ok, message, latency := probeEngine(ctx, s.engine)
	return ok, s.engine.Name(), message, latency, nil
}

// buildTempProvider 用单个提供方的合并配置临时构造一个引擎（不落库、不改内存 cfg）。
//
// 返回 (nil, 原因) 表示无法构造（未启用 / 缺 base_url）；kind=mock 会返回规则引擎，
// 由调用方决定如何展示（离线引擎没有外部连接可测，但调用链路可用）。口径与 factory.newProvider 一致。
func buildTempProvider(name string, cfg *config.Config, p aiProviderPayload) (engine.Engine, string) {
	if !p.Enabled {
		return nil, "提供方未启用"
	}
	timeout := engine.Timeouts(cfg, name)
	kind := strings.ToLower(strings.TrimSpace(p.Kind))
	if kind == "" {
		kind = "openai"
	}
	if kind == "mock" {
		return engine.NewRuleEngine(name + " 使用内置 mock 引擎"), ""
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return nil, "未配置 Base URL，无法测试外部连接（离线演示可改用 mock 引擎）"
	}
	prov, err := engine.NewHTTPProvider(engine.HTTPProviderOptions{
		Name:             name,
		Kind:             kind,
		BaseURL:          p.BaseURL,
		APIKey:           p.APIKey,
		Model:            p.Model,
		MaxTokens:        p.MaxTokens,
		PricePerKToken:   p.PricePerKToken,
		Timeout:          timeout,
		FailureThreshold: cfg.AIEngine.Fallback.FailureThreshold,
		OpenDuration:     cfg.AIEngine.Fallback.OpenDuration,
	})
	if err != nil {
		return nil, "提供方初始化失败：" + err.Error()
	}
	return prov, ""
}

// TestAIProvider 在「不保存」的前提下，用单个提供方当前的合并配置验证其调用是否正确。
//
// 合并基线同 SaveAISettings：DB 有记录用 DB，没有就用平台默认——不读 .env。
// 这样"只填了新密钥没动其它字段"时，自测用的是"已存密钥 + 新密钥"的合并结果，
// 而不是把旧密钥清空后测一个空 key。失败时只返回原因（不抛 500），便于页面就地展示。
func (s *SettingService) TestAIProvider(ctx context.Context, key string, in ProviderSettingsInput) (bool, string, string, int64, error) {
	var existing aiSettingsPayload
	_, _, found, err := s.loadSetting(ctx, SettingNameAI, &existing)
	if err != nil {
		return false, "", "", 0, err
	}
	if !found {
		existing = defaultAISettingsPayload()
	}
	var old aiProviderPayload
	switch key {
	case "third_party":
		old = existing.ThirdParty
	case "self_hosted":
		old = existing.SelfHosted
	default:
		return false, "", "", 0, apperr.Newf(apperr.CodeInvalidParam,
			"provider %q 非法（可选 third_party/self_hosted）", key)
	}
	merged := mergeAIProvider(old, in)
	eng, reason := buildTempProvider(key, s.cfg, merged)
	if eng == nil {
		return false, key, reason, 0, nil
	}
	// 规则 / mock 引擎是离线确定性引擎：没有外部连接可测，但调用链路可用。
	// 单独走一条路径，避免被 Status().Degraded（规则引擎恒为降级态）误判成"测试失败"。
	if eng.Name() == engine.RuleEngineName {
		ctx, cancel := context.WithTimeout(ctx, testAITimeout)
		defer cancel()
		start := time.Now()
		_, e := eng.Chat(ctx, engine.ChatRequest{
			Messages: []engine.Message{{Role: engine.RoleUser, Content: "ping"}},
			MaxTokens: 8, Temperature: 0,
		})
		latency := time.Since(start).Milliseconds()
		if e != nil {
			return false, eng.Name(), truncateMessage("调用失败：" + redactCredentials(e.Error())), latency, nil
		}
		return true, eng.Name(),
			"离线确定性引擎（mock/规则），调用链路可用，不访问外部服务；如需验证真实密钥请改用 openai/ollama 并填写 Base URL",
			latency, nil
	}
	ok, message, latency := probeEngine(ctx, eng)
	return ok, eng.Name(), message, latency, nil
}

// TestCodeAnalysis 用「AI 代码分析」当前的合并配置做一次连通性自检（不落库、不改内存配置）。
//
// 与 TestAIProvider 同一套语义：表单里只改了密钥还没保存时，测的是
// "已存配置 + 本次填写"的合并结果，而不是把旧密钥清空后测一个空 key。
//
// 判定口径（关键取舍）：
//   - 2xx         → 通；
//   - 401 / 403   → 鉴权问题，失败；
//   - 404 / 422   → **算通**：能走到仓库定位说明地址、密钥、报文形状都对，
//                    探针服务名没注册是预期的（真实告警会带真实服务名）；
//   - 其它 / 网络错误 → 失败，原因原样给出去。
func (s *SettingService) TestCodeAnalysis(ctx context.Context, in AIAnalysisInput) (bool, string, string, int64, error) {
	var existing aiSettingsPayload
	_, _, found, err := s.loadSetting(ctx, SettingNameAI, &existing)
	if err != nil {
		return false, "", "", 0, err
	}
	if !found {
		existing = defaultAISettingsPayload()
	}
	merged := mergeAIAnalysis(existing.CodeAnalysis, in)

	// 借用当前内存配置作时长基线，再按合并结果覆盖（applyCodeAnalysisTo 只覆盖非空项）。
	var probeCfg config.Config
	if s.cfg != nil {
		probeCfg.AIAnalysis = s.cfg.AIAnalysis
	}
	(aiSettingsPayload{CodeAnalysis: merged}).applyCodeAnalysisTo(&probeCfg)

	name := protocolLabel(probeCfg.AIAnalysis.Protocol)
	client := NewAIAnalysisClient(probeCfg.AIAnalysis, s.log)
	if !client.Configured() {
		return false, name, "未启用或服务地址为空：请先打开「启用」并填写 Base URL", 0, nil
	}
	res, err := client.Probe(ctx)
	if err != nil {
		return false, name, truncateMessage("连接失败：" + redactCredentials(err.Error())), 0, nil
	}
	openAPI := client.isOpenAPI()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		msg := "服务可达"
		if res.Message != "" {
			msg += "，本次探测状态：" + res.Message
		}
		return true, name, truncateMessage(msg), res.LatencyMS, nil
	}
	if res.StatusCode == http.StatusNotFound || res.StatusCode == http.StatusUnprocessableEntity {
		if openAPI {
			return true, name, "服务可达且鉴权通过（探针服务 " + probeServiceName +
				" 未在该服务注册，属预期）；真实告警会带实际服务名去定位仓库，请确认控制台已配置 hostPatterns",
				res.LatencyMS, nil
		}
	}
	return false, name, truncateMessage(httpErrorHint(res.StatusCode, nil, openAPI)), res.LatencyMS, nil
}

// protocolLabel 把协议值翻成页面上看得懂的名字（测试结果标题用）。
func protocolLabel(protocol string) string {
	if strings.EqualFold(strings.TrimSpace(protocol), ProtocolOpenAPIV1) {
		return "开放接口 v1"
	}
	return "通用协议"
}

// truncateMessage 截断过长的引擎返回，避免把整篇模型输出塞进设置页提示。
func truncateMessage(msg string) string {
	const limit = 200
	r := []rune(msg)
	if len(r) <= limit {
		return msg
	}
	return string(r[:limit]) + "…"
}

// ---------------------------------------------------------------------------
// 审计
// ---------------------------------------------------------------------------

// record 写审计（与同包其它服务一致的异步写法）。
func (s *SettingService) record(ctx context.Context, op Operator, action string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, ActionType: action, Level: LevelLow,
		IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}
