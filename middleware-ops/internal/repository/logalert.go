package repository

import (
	"context"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// ServerRepository 提供服务器实例数据访问。
type ServerRepository struct {
	Base
}

// NewServerRepository 构造服务器仓储。
func NewServerRepository(db *gorm.DB) *ServerRepository {
	return &ServerRepository{Base: Base{db: db}}
}

// List 分页检索服务器。
func (r *ServerRepository) List(ctx context.Context, keyword, environment string, limit, offset int) ([]model.ServerInstance, int64, error) {
	q := r.withCtx(ctx).Model(&model.ServerInstance{})
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("name LIKE ? OR ip LIKE ? OR hostname LIKE ?", like, like, like)
	}
	if environment != "" {
		q = q.Where("environment = ?", environment)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count servers")
	}
	var items []model.ServerInstance
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list servers")
	}
	return items, total, nil
}

// Get 按 ID 查询服务器。
func (r *ServerRepository) Get(ctx context.Context, id int64) (*model.ServerInstance, error) {
	var item model.ServerInstance
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get server")
	}
	return &item, nil
}

// GetByIP 按 IP 查询（日志 Hook 上报时定位服务器）。
func (r *ServerRepository) GetByIP(ctx context.Context, ip string) (*model.ServerInstance, error) {
	var item model.ServerInstance
	if err := r.withCtx(ctx).Where("ip = ?", ip).First(&item).Error; err != nil {
		return nil, wrap(err, "get server by ip")
	}
	return &item, nil
}

// Create 新增服务器。
func (r *ServerRepository) Create(ctx context.Context, item *model.ServerInstance) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create server")
	}
	return nil
}

// Update 更新服务器。
func (r *ServerRepository) Update(ctx context.Context, item *model.ServerInstance) error {
	res := r.withCtx(ctx).Model(&model.ServerInstance{}).Where("id = ?", item.ID).
		Updates(map[string]any{
			"name":        item.Name,
			"ip":          item.IP,
			"hostname":    item.Hostname,
			"environment": item.Environment,
			"group_name":  item.GroupName,
			"status":      item.Status,
			"tags":        item.Tags,
		})
	if res.Error != nil {
		return wrap(res.Error, "update server")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update server")
	}
	return nil
}

// TouchSeen 记录最近心跳（Agent 上报）。
func (r *ServerRepository) TouchSeen(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	return wrap(r.withCtx(ctx).Model(&model.ServerInstance{}).Where("id = ?", id).
		Updates(map[string]any{"last_seen_at": now, "status": 1}).Error, "touch server seen")
}

// Delete 删除服务器。
func (r *ServerRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.ServerInstance{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete server")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "delete server")
	}
	return nil
}

// CodeRepoRepository 提供服务仓库映射数据访问（6.5 出网白名单按仓库维度）。
type CodeRepoRepository struct {
	Base
}

// NewCodeRepoRepository 构造代码仓库仓储。
func NewCodeRepoRepository(db *gorm.DB) *CodeRepoRepository {
	return &CodeRepoRepository{Base: Base{db: db}}
}

// List 分页检索仓库映射。
func (r *CodeRepoRepository) List(ctx context.Context, keyword string, limit, offset int) ([]model.CodeRepo, int64, error) {
	q := r.withCtx(ctx).Model(&model.CodeRepo{})
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("service_name LIKE ? OR repo_url LIKE ?", like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count code repos")
	}
	var items []model.CodeRepo
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list code repos")
	}
	return items, total, nil
}

// Get 按 ID 查询。
func (r *CodeRepoRepository) Get(ctx context.Context, id int64) (*model.CodeRepo, error) {
	var item model.CodeRepo
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get code repo")
	}
	return &item, nil
}

// FindByService 按服务名查询（日志告警定位代码时使用）。
func (r *CodeRepoRepository) FindByService(ctx context.Context, service string) (*model.CodeRepo, error) {
	var item model.CodeRepo
	if err := r.withCtx(ctx).Where("service_name = ?", service).First(&item).Error; err != nil {
		return nil, wrap(err, "find code repo by service")
	}
	return &item, nil
}

// Create 新增仓库映射。
func (r *CodeRepoRepository) Create(ctx context.Context, item *model.CodeRepo) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create code repo")
	}
	return nil
}

// Update 更新仓库映射（含出网白名单开关）。
func (r *CodeRepoRepository) Update(ctx context.Context, item *model.CodeRepo) error {
	res := r.withCtx(ctx).Model(&model.CodeRepo{}).Where("id = ?", item.ID).
		Updates(map[string]any{
			"service_name":      item.ServiceName,
			"repo_url":          item.RepoURL,
			"branch":            item.Branch,
			"local_path":        item.LocalPath,
			"language":          item.Language,
			"allow_third_party": item.AllowThirdParty,
		})
	if res.Error != nil {
		return wrap(res.Error, "update code repo")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update code repo")
	}
	return nil
}

// Delete 删除仓库映射。
func (r *CodeRepoRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.CodeRepo{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete code repo")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "delete code repo")
	}
	return nil
}

// LogEventRepository 提供日志告警事件数据访问。
type LogEventRepository struct {
	Base
}

// NewLogEventRepository 构造日志事件仓储。
func NewLogEventRepository(db *gorm.DB) *LogEventRepository {
	return &LogEventRepository{Base: Base{db: db}}
}

// LogEventFilter 是日志事件检索条件。
type LogEventFilter struct {
	ServerID    int64
	ServiceName string
	AlertType   string
	Status      string
	Signature   string
	Keyword     string
	From        *time.Time
	To          *time.Time
}

// List 分页检索日志事件。
func (r *LogEventRepository) List(ctx context.Context, f LogEventFilter, limit, offset int) ([]model.LogAlertEvent, int64, error) {
	q := r.withCtx(ctx).Model(&model.LogAlertEvent{})
	if f.ServerID > 0 {
		q = q.Where("server_id = ?", f.ServerID)
	}
	if f.ServiceName != "" {
		q = q.Where("service_name = ?", f.ServiceName)
	}
	if f.AlertType != "" {
		q = q.Where("alert_type = ?", f.AlertType)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Signature != "" {
		q = q.Where("error_signature = ?", f.Signature)
	}
	if f.Keyword != "" {
		like := "%" + f.Keyword + "%"
		q = q.Where("error_signature LIKE ? OR raw_stacktrace LIKE ?", like, like)
	}
	if f.From != nil {
		q = q.Where("last_seen_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("last_seen_at <= ?", *f.To)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count log events")
	}
	var items []model.LogAlertEvent
	if err := q.Order("last_seen_at DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list log events")
	}
	return items, total, nil
}

// Get 按 ID 查询事件。
func (r *LogEventRepository) Get(ctx context.Context, id int64) (*model.LogAlertEvent, error) {
	var item model.LogAlertEvent
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get log event")
	}
	return &item, nil
}

// FindBySignature 查询窗口内同指纹事件（用于合并去重）。
func (r *LogEventRepository) FindBySignature(ctx context.Context, service, signature string, since time.Time) (*model.LogAlertEvent, error) {
	var item model.LogAlertEvent
	err := r.withCtx(ctx).
		Where("service_name = ? AND error_signature = ? AND last_seen_at >= ?", service, signature, since).
		Order("last_seen_at DESC").First(&item).Error
	if err != nil {
		return nil, wrap(err, "find log event by signature")
	}
	return &item, nil
}

// Create 写入事件。
func (r *LogEventRepository) Create(ctx context.Context, item *model.LogAlertEvent) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create log event")
	}
	return nil
}

// MergeCount 合并重复事件的计数与时间窗口。
func (r *LogEventRepository) MergeCount(ctx context.Context, id int64, count int, lastSeen time.Time) error {
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Updates(map[string]any{
			"error_count":  gorm.Expr("error_count + ?", count),
			"last_seen_at": lastSeen,
		}).Error, "merge log event count")
}

// UpdateStatus 更新事件状态。
func (r *LogEventRepository) UpdateStatus(ctx context.Context, id int64, status string) error {
	res := r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Update("status", status)
	if res.Error != nil {
		return wrap(res.Error, "update log event status")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update log event status")
	}
	return nil
}

// MarkAnalyzed 标记事件已产出代码分析。
func (r *LogEventRepository) MarkAnalyzed(ctx context.Context, id int64) error {
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Update("analyzed", true).Error, "mark log event analyzed")
}

// CountByStatus 统计各状态事件数量。
func (r *LogEventRepository) CountByStatus(ctx context.Context, since time.Time) (map[string]int64, error) {
	type row struct {
		Status string
		Total  int64
	}
	var rows []row
	q := r.withCtx(ctx).Model(&model.LogAlertEvent{}).
		Select("status, COUNT(*) AS total").Group("status")
	if !since.IsZero() {
		q = q.Where("last_seen_at >= ?", since)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count log events by status")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.Status] = item.Total
	}
	return out, nil
}

// CountByAnalysisState 统计各分析状态的事件数（页面用来说明"还有多少条在排队"）。
func (r *LogEventRepository) CountByAnalysisState(ctx context.Context) (map[string]int64, error) {
	type row struct {
		AnalysisState string
		Total         int64
	}
	var rows []row
	if err := r.withCtx(ctx).Model(&model.LogAlertEvent{}).
		Select("analysis_state, COUNT(*) AS total").Group("analysis_state").Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count log events by analysis state")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.AnalysisState] = item.Total
	}
	return out, nil
}

// MarkNotified 记录一次成功外发通知的时间。
func (r *LogEventRepository) MarkNotified(ctx context.Context, id int64, at time.Time) error {
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Updates(map[string]any{"notified_at": at}).Error, "mark log event notified")
}

// SetCooldown 设置冷却截止时间（窗口合并时用来续期，避免"合并一次就立刻又能通知"）。
func (r *LogEventRepository) SetCooldown(ctx context.Context, id int64, until time.Time) error {
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Updates(map[string]any{"cooldown_until": until}).Error, "set log event cooldown")
}

// SetSuppressed 标记/解除"冷却期内被抑制"（抑制不是丢弃：事件仍在列表里，只是不外发）。
func (r *LogEventRepository) SetSuppressed(ctx context.Context, id int64, suppressed bool) error {
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Updates(map[string]any{"suppressed": suppressed}).Error, "set log event suppressed")
}

// SetAnalysisState 更新 AI 分析状态与原因（失败原因会直接显示在页面上）。
func (r *LogEventRepository) SetAnalysisState(ctx context.Context, id int64, state, reason string) error {
	updates := map[string]any{"analysis_state": state, "analysis_error": reason}
	if state == model.LogAnalysisDone {
		updates["analyzed"] = true
	}
	return wrap(r.withCtx(ctx).Model(&model.LogAlertEvent{}).Where("id = ?", id).
		Updates(updates).Error, "set log event analysis state")
}

// ClaimForAnalysis 抢占一条待分析事件：pending → running。
//
// 为什么用"带条件的 UPDATE + 影响行数"而不是先查再改：后端可能多副本部署，
// 或者同一次定时任务与"重新分析"按钮并发——只有影响行数为 1 的那个调用者才算抢到，
// 否则同一条事件会被分析两次（浪费额度，还会写出两份结论）。
// 返回 claimed=false 表示别人已经拿走了，安静跳过即可（不是错误）。
func (r *LogEventRepository) ClaimForAnalysis(ctx context.Context, id int64) (claimed bool, err error) {
	res := r.withCtx(ctx).Model(&model.LogAlertEvent{}).
		Where("id = ? AND analysis_state = ?", id, model.LogAnalysisPending).
		Updates(map[string]any{"analysis_state": model.LogAnalysisRunning, "analysis_error": ""})
	if res.Error != nil {
		return false, wrap(res.Error, "claim log event for analysis")
	}
	return res.RowsAffected == 1, nil
}

// ListPendingAnalysis 取待处理事件（按 id 升序：先来的先处理，避免老事件永远排在队尾）。
func (r *LogEventRepository) ListPendingAnalysis(ctx context.Context, limit int) ([]model.LogAlertEvent, error) {
	var items []model.LogAlertEvent
	err := r.withCtx(ctx).
		Where("analysis_state = ?", model.LogAnalysisPending).
		Order("id ASC").Limit(limit).Find(&items).Error
	if err != nil {
		return nil, wrap(err, "list pending log events")
	}
	return items, nil
}

// RequeueAnalysis 把事件重新放回待处理队列（页面上的「重新分析」）。
func (r *LogEventRepository) RequeueAnalysis(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Model(&model.LogAlertEvent{}).
		Where("id = ?", id).
		Updates(map[string]any{"analysis_state": model.LogAnalysisPending, "analysis_error": ""})
	if res.Error != nil {
		return wrap(res.Error, "requeue log event analysis")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "requeue log event analysis")
	}
	return nil
}

// LogAlertRuleRepository 提供日志告警规则数据访问。
type LogAlertRuleRepository struct {
	Base
}

// NewLogAlertRuleRepository 构造日志告警规则仓储。
func NewLogAlertRuleRepository(db *gorm.DB) *LogAlertRuleRepository {
	return &LogAlertRuleRepository{Base: Base{db: db}}
}

// List 分页检索规则（按优先级升序：页面顺序即匹配顺序，便于对照）。
func (r *LogAlertRuleRepository) List(ctx context.Context, keyword string, limit, offset int) ([]model.LogAlertRule, int64, error) {
	q := r.withCtx(ctx).Model(&model.LogAlertRule{})
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("name LIKE ? OR service_name LIKE ? OR signature_pattern LIKE ?", like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count log alert rules")
	}
	var items []model.LogAlertRule
	if err := q.Order("priority ASC, id ASC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list log alert rules")
	}
	return items, total, nil
}

// ListEnabled 取全部启用规则（匹配时用；规则数量是人工维护的，量级很小）。
func (r *LogAlertRuleRepository) ListEnabled(ctx context.Context) ([]model.LogAlertRule, error) {
	var items []model.LogAlertRule
	if err := r.withCtx(ctx).Where("enabled = ?", true).
		Order("priority ASC, id ASC").Find(&items).Error; err != nil {
		return nil, wrap(err, "list enabled log alert rules")
	}
	return items, nil
}

// Get 按 ID 查询。
func (r *LogAlertRuleRepository) Get(ctx context.Context, id int64) (*model.LogAlertRule, error) {
	var item model.LogAlertRule
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get log alert rule")
	}
	return &item, nil
}

// Create 新增规则。
func (r *LogAlertRuleRepository) Create(ctx context.Context, item *model.LogAlertRule) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create log alert rule")
	}
	return nil
}

// Update 更新规则。
func (r *LogAlertRuleRepository) Update(ctx context.Context, item *model.LogAlertRule) error {
	res := r.withCtx(ctx).Model(&model.LogAlertRule{}).Where("id = ?", item.ID).
		Updates(map[string]any{
			"name":              item.Name,
			"description":       item.Description,
			"service_name":      item.ServiceName,
			"signature_pattern": item.SignaturePattern,
			"min_severity":      item.MinSeverity,
			"dedup_window":      item.DedupWindow,
			"cooldown":          item.Cooldown,
			"notify_channels":   item.NotifyChannels,
			"ai_enabled":        item.AIEnabled,
			"enabled":           item.Enabled,
			"priority":          item.Priority,
		})
	if res.Error != nil {
		return wrap(res.Error, "update log alert rule")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update log alert rule")
	}
	return nil
}

// Delete 删除规则。
func (r *LogAlertRuleRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.LogAlertRule{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete log alert rule")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "delete log alert rule")
	}
	return nil
}

// GetByService 按服务名取启用规则（页面提示"这个服务当前命中哪条规则"）。
func (r *LogAlertRuleRepository) GetByService(ctx context.Context, service string) ([]model.LogAlertRule, error) {
	var items []model.LogAlertRule
	if err := r.withCtx(ctx).Where("enabled = ? AND (service_name = ? OR service_name = '')", true, service).
		Order("priority ASC, id ASC").Find(&items).Error; err != nil {
		return nil, wrap(err, "get log alert rules by service")
	}
	return items, nil
}

// CodeAnalysisRepository 提供 AI 代码分析报告数据访问。
type CodeAnalysisRepository struct {
	Base
}

// NewCodeAnalysisRepository 构造代码分析仓储。
func NewCodeAnalysisRepository(db *gorm.DB) *CodeAnalysisRepository {
	return &CodeAnalysisRepository{Base: Base{db: db}}
}

// List 分页检索分析报告。
func (r *CodeAnalysisRepository) List(ctx context.Context, service string, limit, offset int) ([]model.AICodeAnalysis, int64, error) {
	q := r.withCtx(ctx).Model(&model.AICodeAnalysis{})
	if service != "" {
		q = q.Where("service_name = ?", service)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count code analyses")
	}
	var items []model.AICodeAnalysis
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list code analyses")
	}
	return items, total, nil
}

// GetByEvent 按事件查询报告。
func (r *CodeAnalysisRepository) GetByEvent(ctx context.Context, eventID int64) (*model.AICodeAnalysis, error) {
	var item model.AICodeAnalysis
	if err := r.withCtx(ctx).Where("event_id = ?", eventID).Order("id DESC").First(&item).Error; err != nil {
		return nil, wrap(err, "get code analysis by event")
	}
	return &item, nil
}

// Create 写入报告。
func (r *CodeAnalysisRepository) Create(ctx context.Context, item *model.AICodeAnalysis) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create code analysis")
	}
	return nil
}

// NotificationLogRepository 提供通知记录数据访问。
type NotificationLogRepository struct {
	Base
}

// NewNotificationLogRepository 构造通知日志仓储。
func NewNotificationLogRepository(db *gorm.DB) *NotificationLogRepository {
	return &NotificationLogRepository{Base: Base{db: db}}
}

// Create 写入通知记录。
func (r *NotificationLogRepository) Create(ctx context.Context, item *model.NotificationLog) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create notification log")
	}
	return nil
}

// List 分页检索通知记录。
func (r *NotificationLogRepository) List(ctx context.Context, alertID int64, limit, offset int) ([]model.NotificationLog, int64, error) {
	q := r.withCtx(ctx).Model(&model.NotificationLog{})
	if alertID > 0 {
		q = q.Where("alert_id = ?", alertID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count notifications")
	}
	var items []model.NotificationLog
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list notifications")
	}
	return items, total, nil
}
