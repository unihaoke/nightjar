package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// LogAlertService 实现日志告警域（4.8.1 / 4.8.2）。
//
// 采集入口：HTTP Hook（应用主动上报，零侵入）与 Agent 上报共用同一接口；
// 去重聚合：错误指纹 + 窗口去重 + 冷却期静默。
type LogAlertService struct {
	servers *repository.ServerRepository
	events  *repository.LogEventRepository
	repos   *repository.CodeRepoRepository
	audit   *AuditService
	log     *zap.Logger
	// window 为窗口去重时长（默认 5 分钟，见 4.8.2）。
	window time.Duration
}

// NewLogAlertService 构造日志告警服务。
func NewLogAlertService(
	servers *repository.ServerRepository,
	events *repository.LogEventRepository,
	repos *repository.CodeRepoRepository,
	audit *AuditService,
	log *zap.Logger,
) *LogAlertService {
	return &LogAlertService{servers: servers, events: events, repos: repos, audit: audit, log: log, window: 5 * time.Minute}
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

	// 窗口去重：同服务 + 同指纹合并计数。
	existing, err := s.events.FindBySignature(ctx, in.Service, signature, time.Now().UTC().Add(-s.window))
	if err == nil && existing != nil {
		if mergeErr := s.events.MergeCount(ctx, existing.ID, count, now); mergeErr != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, mergeErr)
		}
		return &IngestResult{
			EventID: existing.EventID, Signature: signature, Merged: true,
			Count: existing.ErrorCount + count, Status: existing.Status, Suppressed: existing.Suppressed,
		}, nil
	}
	if err != nil && !repository.EnsureNotFound(err) {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	event := &model.LogAlertEvent{
		EventID:        "LE" + utils.Fingerprint(in.Service, signature, now.Format(time.RFC3339Nano))[:14],
		ServerID:       serverID,
		ServiceName:    in.Service,
		AlertType:      alertType,
		ErrorSignature: signature,
		RawStacktrace:  in.Stacktrace,
		ContextLines:   in.ContextLines,
		ErrorCount:     count,
		Severity:       severityOf(in.Level),
		Status:         model.LogEventPending,
		FirstSeenAt:    now,
		LastSeenAt:     now,
	}
	if err := s.events.Create(ctx, event); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	// 错误指纹统计观测（便于识别高频指纹）。
	s.log.Info("收到日志告警事件",
		zap.String("event_id", event.EventID), zap.String("service", event.ServiceName),
		zap.String("signature", signature), zap.String("level", in.Level))
	return &IngestResult{
		EventID: event.EventID, Signature: signature, Merged: false,
		Count: count, Status: event.Status,
	}, nil
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
