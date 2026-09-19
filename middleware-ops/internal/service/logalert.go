package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// LogAlertService 实现日志告警域（4.8.1 / 4.8.2）。
//
// 完整链路（与 docs/LOG_INTEGRATION.md 一致）：
//
//	采集（Filebeat）→ Kafka → 本服务 Ingest：按规则做**窗口去重**与**冷却抑制** → 落库
//	  → 后处理（LogAlertWorker）：外发通知渠道 + AI 代码分析（拉取仓库并定位代码）
//
// 为什么要按"规则"而不是写死参数：去重窗口与冷却期决定了"多久打扰人一次"，
// 它随服务重要性而变（核心交易服务要立刻通知、批处理服务可以攒一攒），
// 因此必须由页面配置（log_alert_rules）。代码里**没有任何默认规则**：
// 没命中规则（或被屏蔽项命中）的日志不入库、不通知、不分析——
// 告警只来自使用者自己在平台上新增的规则。
type LogAlertService struct {
	servers    *repository.ServerRepository
	events     *repository.LogEventRepository
	repos      *repository.CodeRepoRepository
	rules      *repository.LogAlertRuleRepository
	exclusions *repository.LogAlertExclusionRepository
	cfg        *config.Config
	audit      *AuditService
	log        *zap.Logger
	// window 为**兜底**去重窗口（分钟）。真实窗口始终来自命中的规则；
	// 这里不再读取任何配置默认值（默认值已按产品要求移除），保留是因为
	// EnsureWindow 是既有对外接口。
	window time.Duration
	// cooldown 用 Redis 记录"最近一次外发时间"，实现冷却期（与指标告警同一套实现）。
	cooldown cooldownTracker
	// 规则/屏蔽项缓存：Ingest 是**每条日志**都会走的路径，如果每次都查一次表，
	// 一次日志风暴就会把 DB 打成瓶颈（同一条 SQL 每秒重复上千次）。
	// 规则是人工维护、极少变动的，因此按 TTL 缓存；增删改时立即失效
	// （见 invalidateRuleCache / invalidateExclusionCache）。
	ruleCacheMu sync.RWMutex
	ruleCache   []model.LogAlertRule
	ruleCacheAt time.Time
	exclCache   []model.LogAlertExclusion
	exclCacheAt time.Time
}

// ruleCacheTTL 是规则缓存的存活时间。
//
// 取 10 秒的权衡：并发实例（多副本）下，别人改的规则最多 10 秒后在本副本生效；
// 而本副本上的改动会立即失效缓存，所以"我改完立刻生效"。多副本之间的这点延迟
// 换来的是采集路径上不再有重复查询，值得。
const ruleCacheTTL = 10 * time.Second

// NewLogAlertService 构造日志告警服务。
func NewLogAlertService(
	servers *repository.ServerRepository,
	events *repository.LogEventRepository,
	repos *repository.CodeRepoRepository,
	rules *repository.LogAlertRuleRepository,
	exclusions *repository.LogAlertExclusionRepository,
	cfg *config.Config,
	store cache.Store,
	audit *AuditService,
	log *zap.Logger,
) *LogAlertService {
	return &LogAlertService{
		servers: servers, events: events, repos: repos, rules: rules, exclusions: exclusions,
		cfg: cfg, audit: audit, log: log, window: 5 * time.Minute,
		cooldown: newCooldownTracker(store, "logalert:cd:"),
	}
}

// LogReport 是应用/Agent 上报的日志条目。
type LogReport struct {
	// ServerID 或 ServerIP 至少提供一个。
	ServerID   int64  `json:"server_id"`
	ServerIP   string `json:"server_ip"`
	ServerName string `json:"server_name"`
	Service    string `json:"service" binding:"required"`
	// Level 取值 ERROR / FATAL / GC 等。
	Level string `json:"level" binding:"required"`
	// Message 为错误消息（用于生成指纹）。
	Message string `json:"message" binding:"required"`
	// Stacktrace 为异常堆栈原文。
	Stacktrace string `json:"stacktrace"`
	// ContextLines 为错误前后 N 行上下文（Agent 采集时提供）。
	ContextLines string `json:"context_lines"`
	// LogPath 为日志文件路径（日志集成：Filebeat 的 log.file.path）。
	// 排查时"哪个文件在报错"往往比"报了什么"更快定位到服务与模块。
	LogPath string `json:"log_path"`
	// Timestamp 为日志产生时间（缺省取当前时间）。
	Timestamp *time.Time `json:"timestamp"`
	// Count 为批量上报的重复次数。
	Count int `json:"count"`
	// AlertType 取值 error / gc / stack。
	AlertType string `json:"alert_type"`
}

// IngestResult 是日志上报处理结果。
type IngestResult struct {
	EventID    string `json:"event_id"`
	Signature  string `json:"signature"`
	Merged     bool   `json:"merged"`
	Count      int    `json:"count"`
	Status     string `json:"status"`
	Suppressed bool   `json:"suppressed"`
	// Ignored 表示这条日志**没有产生告警事件**：被屏蔽项命中，或没命中任何规则。
	// 上报方（Hook / Kafka 消费者）据此知道"收到了，但按配置不告警"，而不是"平台出错了"。
	Ignored bool `json:"ignored"`
	// Reason 说明为什么被忽略（页面与调用方排查"为什么没看到这条日志"的唯一线索）。
	Reason string `json:"reason"`
	// RuleID 为本次命中的规则（0 表示未命中）。
	RuleID int64 `json:"rule_id"`
}

// Ingest 接收日志上报并执行去重聚合（4.8.2）。
func (s *LogAlertService) Ingest(ctx context.Context, in LogReport) (*IngestResult, error) {
	if strings.TrimSpace(in.Service) == "" || strings.TrimSpace(in.Message) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "service 与 message 不能为空")
	}
	serverID := in.ServerID
	if serverID == 0 && in.ServerIP != "" {
		if server, err := s.servers.GetByIP(ctx, in.ServerIP); err == nil {
			serverID = server.ID
		}
	}
	if serverID != 0 {
		if err := s.servers.TouchSeen(ctx, serverID); err != nil {
			s.log.Warn("更新服务器心跳失败", zap.Int64("server_id", serverID), zap.Error(err))
		}
	} else if in.ServerName != "" || in.ServerIP != "" || in.Service != "" {
		// 未知服务器自动注册（Agent 零配置上线）。
		server := &model.ServerInstance{
			Name: defaultString(in.ServerName, defaultString(in.ServerIP, in.Service)),
			IP:   in.ServerIP, Hostname: in.ServerName, Environment: model.EnvDev, Status: 1,
		}
		if err := s.servers.Create(ctx, server); err != nil {
			s.log.Warn("自动注册服务器失败", zap.Error(err))
		} else {
			serverID = server.ID
		}
	}

	signature := ErrorSignature(in.Message, in.Stacktrace)
	now := time.Now().UTC()
	if in.Timestamp != nil {
		now = in.Timestamp.UTC()
	}
	count := in.Count
	if count <= 0 {
		count = 1
	}
	alertType := defaultString(in.AlertType, detectAlertType(in))
	level := defaultString(in.Level, "ERROR")

	// ① 屏蔽判定：命中屏蔽项的日志**完全不入库**——不生成事件、不通知、不触发 AI。
	//
	// 放在规则匹配之前：屏蔽的语义是"这类错误根本不该成为告警"，
	// 与"命中后怎么处理"无关，因此不该受任何规则的开关影响。
	if item, blocked := s.exclusionFor(ctx, in.Service, in.Message); blocked {
		s.log.Debug("日志命中屏蔽项，已忽略",
			zap.String("service", in.Service), zap.Int64("exclusion_id", item.ID),
			zap.String("pattern", item.Pattern))
		return &IngestResult{
			Signature: signature, Status: model.LogEventIgnored, Ignored: true,
			Reason: "命中屏蔽项：" + item.Pattern,
		}, nil
	}

	// ② 选规则：没有命中任何规则就**不产生告警事件**。
	//
	// 平台不再有任何"默认规则/默认值"兜底：告警必须由使用者在页面上新增的规则触发，
	// 否则一条没配过的服务也会源源不断产生通知，而没人说得清它从哪来。
	rule, matched := s.matchRule(ctx, in.Service, signature, level)
	if !matched {
		s.log.Debug("日志未命中任何日志告警规则，已忽略",
			zap.String("service", in.Service), zap.String("signature", signature))
		return &IngestResult{
			Signature: signature, Status: model.LogEventIgnored, Ignored: true,
			Reason: "未命中任何日志告警规则（请先在「日志告警规则」中新增规则）",
		}, nil
	}

	// ③ 冷却判定：冷却期内**不重复通知、不重复触发 AI**，但事件照记录（抑制 ≠ 丢弃）。
	cooling, coolErr := s.cooldown.inCooldown(ctx, cooldownKey(in.Service, signature), rule.Cooldown, now)
	if coolErr != nil {
		// 读失败按"不冷却"处理：宁可多打扰一次，也不能因为 Redis 抖动把告警静默掉。
		s.log.Warn("日志告警冷却判定失败（本次按不冷却处理）", zap.Error(coolErr))
	}

	// ④ 窗口去重：窗口内同指纹合并到既有事件。
	if rule.DedupWindow > 0 {
		windowStart := now.Add(-time.Duration(rule.DedupWindow) * time.Minute)
		existing, err := s.events.FindBySignature(ctx, in.Service, signature, windowStart)
		if err == nil && existing != nil {
			return s.mergeInto(ctx, existing, in, count, now, rule, cooling, signature)
		}
		if err != nil && !repository.EnsureNotFound(err) {
			return nil, apperr.Wrap(apperr.CodeInternal, err)
		}
	}

	// ⑤ 新事件：入队等待后处理（通知 + AI 分析），并把冷却起点写下来。
	event := &model.LogAlertEvent{
		EventID:        "LE" + utils.Fingerprint(in.Service, signature, now.Format(time.RFC3339Nano))[:14],
		ServerID:       serverID,
		ServiceName:    in.Service,
		AlertType:      alertType,
		ErrorSignature: signature,
		RawStacktrace:  in.Stacktrace,
		ContextLines:   in.ContextLines,
		LogPath:        in.LogPath,
		ErrorCount:     count,
		Severity:       severityOf(level),
		Status:         model.LogEventPending,
		FirstSeenAt:    now,
		LastSeenAt:     now,
		RuleID:         rule.ID,
		DedupWindow:    rule.DedupWindow,
		AnalysisState:  model.LogAnalysisPending,
	}
	if rule.Cooldown > 0 {
		until := now.Add(time.Duration(rule.Cooldown) * time.Minute)
		event.CooldownUntil = &until
	}
	if cooling {
		event.Suppressed = true
		event.AnalysisState = model.LogAnalysisDisabled
		event.AnalysisError = suppressReason(event.CooldownUntil)
	}
	if err := s.events.Create(ctx, event); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !cooling && rule.Cooldown > 0 {
		// 冷却起点在"入队时"就打：后处理是异步的，若等通知成功再打点，
		// 队列积压期间同指纹的后续事件会继续入队，等于冷却期失效（告警风暴）。
		if err := s.cooldown.markSent(ctx, cooldownKey(in.Service, signature), now, rule.Cooldown); err != nil {
			s.log.Warn("记录日志告警冷却起点失败（下次可能重复通知）", zap.Error(err))
		}
	}

	s.log.Info("收到日志告警事件",
		zap.String("event_id", event.EventID), zap.String("service", event.ServiceName),
		zap.String("signature", signature), zap.String("level", level),
		zap.Int64("rule_id", rule.ID), zap.Bool("suppressed", cooling))

	return &IngestResult{
		EventID: event.EventID, Signature: signature, Merged: false,
		Count: count, Status: event.Status, Suppressed: cooling, RuleID: rule.ID,
	}, nil
}

// mergeInto 把重复日志合并进既有事件，并给出"这次要不要再提醒"的结论。
//
// 语义（与指标告警一致，页面文案也照此写）：
//   - 冷却期内 → 只累加计数并标记抑制，绝不重复打扰；
//   - 冷却已过 → 重新入队（worker 会再发一次通知），但**不重跑 AI**：
//     同一条事件已经有结论了，重复分析只会白烧 token（worker 会跳过 Analyzed 的事件）。
func (s *LogAlertService) mergeInto(
	ctx context.Context, existing *model.LogAlertEvent, in LogReport, count int,
	now time.Time, rule model.LogAlertRule, cooling bool, signature string,
) (*IngestResult, error) {
	if err := s.events.MergeCount(ctx, existing.ID, count, now); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	if cooling {
		if !existing.Suppressed {
			if err := s.events.SetSuppressed(ctx, existing.ID, true); err != nil {
				s.log.Warn("标记日志事件为抑制失败", zap.Error(err))
			}
		}
		if existing.CooldownUntil != nil {
			if err := s.events.SetAnalysisState(ctx, existing.ID, model.LogAnalysisDisabled,
				suppressReason(existing.CooldownUntil)); err != nil {
				s.log.Warn("写入抑制原因失败", zap.Error(err))
			}
		}
		return &IngestResult{
			EventID: existing.EventID, Signature: signature, Merged: true,
			Count: existing.ErrorCount + count, Status: existing.Status, Suppressed: true,
		}, nil
	}

	// 冷却已过：重新入队通知（AI 由 worker 按 Analyzed 判断是否要跑）。
	until := now.Add(time.Duration(rule.Cooldown) * time.Minute)
	if rule.Cooldown > 0 {
		if err := s.cooldown.markSent(ctx, cooldownKey(in.Service, signature), now, rule.Cooldown); err != nil {
			s.log.Warn("记录日志告警冷却起点失败", zap.Error(err))
		}
	}
	if err := s.events.SetCooldown(ctx, existing.ID, until); err != nil {
		s.log.Warn("更新日志事件冷却时间失败", zap.Error(err))
	}
	if err := s.events.SetSuppressed(ctx, existing.ID, false); err != nil {
		s.log.Warn("解除日志事件抑制标记失败", zap.Error(err))
	}
	if !existing.Analyzed {
		if err := s.events.SetAnalysisState(ctx, existing.ID, model.LogAnalysisPending, ""); err != nil {
			s.log.Warn("重新入队日志事件分析失败", zap.Error(err))
		}
	}
	s.log.Info("日志告警在窗口内合并（冷却已过，将再次提醒）",
		zap.String("event_id", existing.EventID), zap.String("service", in.Service),
		zap.String("signature", signature), zap.Int("total", existing.ErrorCount+count))

	return &IngestResult{
		EventID: existing.EventID, Signature: signature, Merged: true,
		Count: existing.ErrorCount + count, Status: existing.Status, Suppressed: false,
	}, nil
}

// cooldownKey 是冷却记录键：服务 + 指纹（同一服务的同类错误才算"同一条告警"）。
func cooldownKey(service, signature string) string {
	return service + "|" + signature
}

// suppressReason 生成"为什么被抑制"的说明（页面直接展示，省得使用者猜）。
func suppressReason(until *time.Time) string {
	if until == nil {
		return "冷却期内抑制（事件仍已记录；如需立即分析可点「重新分析」）"
	}
	return "冷却期内抑制至 " + until.Local().Format("2006-01-02 15:04") +
		"（事件仍已记录；如需立即分析可点「重新分析」）"
}

// List 分页检索日志事件。
func (s *LogAlertService) List(ctx context.Context, f repository.LogEventFilter, limit, offset int) ([]model.LogAlertEvent, int64, error) {
	items, total, err := s.events.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// Get 查询事件详情。
func (s *LogAlertService) Get(ctx context.Context, id int64) (*model.LogAlertEvent, error) {
	item, err := s.events.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "日志事件不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// UpdateStatus 更新事件状态（L1）。
func (s *LogAlertService) UpdateStatus(ctx context.Context, id int64, status string, operator Operator) error {
	switch status {
	case model.LogEventPending, model.LogEventAnalyzing, model.LogEventResolved, model.LogEventIgnored:
	default:
		return apperr.Newf(apperr.CodeInvalidParam, "状态 %q 非法", status)
	}
	event, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.events.UpdateStatus(ctx, id, status); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "log_event_status",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"event_id": event.EventID, "status": status},
		})
	}
	return nil
}

// MarkAnalyzed 标记事件已分析。
func (s *LogAlertService) MarkAnalyzed(ctx context.Context, id int64) error {
	if err := s.events.MarkAnalyzed(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	return nil
}

// RequeueAnalysis 把事件重新放回后处理队列（页面「重新分析」）。
//
// 除了改状态，还必须**清掉冷却记录**：否则事件虽然重新入队，后处理仍会认为"冷却中"而跳过通知，
// 使用者的观感是"点了重新分析什么都没发生"。清冷却正是"人工动作可以打破自动抑制"的体现。
func (s *LogAlertService) RequeueAnalysis(ctx context.Context, id int64, operator Operator) error {
	event, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.events.RequeueAnalysis(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.events.SetSuppressed(ctx, id, false); err != nil {
		s.log.Warn("解除事件抑制标记失败", zap.Error(err))
	}
	if err := s.cooldown.clear(ctx, cooldownKey(event.ServiceName, event.ErrorSignature)); err != nil {
		s.log.Warn("清除冷却记录失败（重新分析可能仍被判定为冷却中）", zap.Error(err))
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "log_event_reanalyze",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"event_id": event.EventID, "service": event.ServiceName},
		})
	}
	return nil
}

// Stats 返回日志事件统计。
func (s *LogAlertService) Stats(ctx context.Context, since time.Time) (map[string]any, error) {
	counts, err := s.events.CountByStatus(ctx, since)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	var total int64
	for _, v := range counts {
		total += v
	}
	return map[string]any{"total": total, "by_status": counts}, nil
}

// 服务器管理 ---------------------------------------------------------------

// ServerInput 是服务器入参。
type ServerInput struct {
	Name        string   `json:"name" binding:"required"`
	IP          string   `json:"ip"`
	Hostname    string   `json:"hostname"`
	Environment string   `json:"environment"`
	GroupName   string   `json:"group_name"`
	Tags        []string `json:"tags"`
}

// ListServers 分页查询服务器。
func (s *LogAlertService) ListServers(ctx context.Context, keyword, environment string, limit, offset int) ([]model.ServerInstance, int64, error) {
	items, total, err := s.servers.List(ctx, keyword, environment, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// CreateServer 新增服务器。
func (s *LogAlertService) CreateServer(ctx context.Context, in ServerInput, operator Operator) (*model.ServerInstance, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "服务器名称不能为空")
	}
	item := &model.ServerInstance{
		Name: in.Name, IP: in.IP, Hostname: in.Hostname,
		Environment: defaultString(in.Environment, model.EnvDev),
		GroupName:   in.GroupName, Tags: model.JSONStringSlice(in.Tags), Status: 1,
	}
	if err := s.servers.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "server_create",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"server_id": item.ID, "name": item.Name, "ip": item.IP},
		})
	}
	return item, nil
}

// UpdateServer 更新服务器。
func (s *LogAlertService) UpdateServer(ctx context.Context, id int64, in ServerInput, operator Operator) (*model.ServerInstance, error) {
	item, err := s.servers.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "服务器不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	item.Name = defaultString(in.Name, item.Name)
	item.IP = in.IP
	item.Hostname = in.Hostname
	if in.Environment != "" {
		item.Environment = in.Environment
	}
	item.GroupName = in.GroupName
	item.Tags = model.JSONStringSlice(in.Tags)
	if err := s.servers.Update(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// DeleteServer 删除服务器。
func (s *LogAlertService) DeleteServer(ctx context.Context, id int64, operator Operator) error {
	if _, err := s.servers.Get(ctx, id); err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "服务器不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.servers.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "server_delete",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"server_id": id},
		})
	}
	return nil
}

// 代码仓库映射 -------------------------------------------------------------

// CodeRepoInput 是仓库映射入参。
type CodeRepoInput struct {
	ServiceName string `json:"service_name" binding:"required"`
	RepoURL     string `json:"repo_url"`
	Branch      string `json:"branch"`
	LocalPath   string `json:"local_path"`
	Language    string `json:"language"`
	// AllowThirdParty 为出网白名单开关，默认关闭（6.5）。
	AllowThirdParty bool `json:"allow_third_party"`
}

// ListCodeRepos 分页查询仓库映射。
func (s *LogAlertService) ListCodeRepos(ctx context.Context, keyword string, limit, offset int) ([]model.CodeRepo, int64, error) {
	items, total, err := s.repos.List(ctx, keyword, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// UpsertCodeRepo 新增或更新仓库映射。
func (s *LogAlertService) UpsertCodeRepo(ctx context.Context, id int64, in CodeRepoInput, operator Operator) (*model.CodeRepo, error) {
	if id > 0 {
		item, err := s.repos.Get(ctx, id)
		if err != nil {
			if repository.EnsureNotFound(err) {
				return nil, apperr.New(apperr.CodeNotFound, "仓库映射不存在")
			}
			return nil, apperr.Wrap(apperr.CodeInternal, err)
		}
		item.ServiceName = in.ServiceName
		item.RepoURL = in.RepoURL
		item.Branch = defaultString(in.Branch, "main")
		item.LocalPath = in.LocalPath
		item.Language = in.Language
		item.AllowThirdParty = in.AllowThirdParty
		if err := s.repos.Update(ctx, item); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, err)
		}
		return item, nil
	}
	item := &model.CodeRepo{
		ServiceName: in.ServiceName, RepoURL: in.RepoURL, Branch: defaultString(in.Branch, "main"),
		LocalPath: in.LocalPath, Language: in.Language, AllowThirdParty: in.AllowThirdParty,
	}
	if err := s.repos.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "code_repo_save",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"id": item.ID, "service": item.ServiceName, "allow_third_party": item.AllowThirdParty},
		})
	}
	return item, nil
}

// ErrorSignature 生成错误指纹：异常类名 + 错误消息模板（去变量）。
//
// 例如 "java.lang.NullPointerException at OrderService.process()"（4.8.2）。
func ErrorSignature(message, stacktrace string) string {
	// 优先从堆栈中提取第一行业务位置（at xxx.yyy(...)）。
	location := ""
	for _, line := range strings.Split(stacktrace, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "at ") {
			location = strings.TrimSpace(strings.TrimPrefix(trimmed, "at "))
			// 去掉文件路径与行号变量，保留类与方法。
			if idx := strings.Index(location, "("); idx > 0 {
				location = location[:idx]
			}
			break
		}
	}
	template := utils.NormalizeError(message)
	if location != "" {
		return utils.Fingerprint(template, location)
	}
	return utils.Fingerprint(template)
}

// detectAlertType 依据日志等级推断告警类型。
func detectAlertType(in LogReport) string {
	lower := strings.ToLower(in.Level + " " + in.Message)
	switch {
	case strings.Contains(lower, "full gc") || strings.Contains(lower, "gc"):
		return "gc"
	case strings.Contains(in.Stacktrace, "\tat ") || strings.Contains(in.Stacktrace, "Traceback"):
		return "stack"
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "exception"):
		return "error"
	default:
		return "error"
	}
}

// severityOf 映射日志等级为告警级别。
func severityOf(level string) string {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "FATAL", "CRITICAL", "PANIC":
		return model.AlertLevelCritical
	default:
		return model.AlertLevelWarning
	}
}

// SignatureLabel 生成指纹的可读标签（前端展示用）。
func SignatureLabel(signature string) string {
	if len(signature) <= 8 {
		return signature
	}
	return signature[:8]
}

// EnsureWindow 保证窗口时长合理。
func (s *LogAlertService) EnsureWindow(d time.Duration) {
	if d > 0 {
		s.window = d
	}
}

// FormatEventKey 生成事件对外键（与数据库 EventID 一致）。
func FormatEventKey(prefix string, parts ...string) string {
	return prefix + utils.Fingerprint(parts...)[:14]
}

// windowDesc 返回窗口描述（用于前端提示）。
func (s *LogAlertService) windowDesc() string {
	return fmt.Sprintf("%d 分钟", int(s.window.Minutes()))
}
