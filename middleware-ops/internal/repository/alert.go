package repository

import (
	"context"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// AlertRuleRepository 提供告警规则的数据访问。
type AlertRuleRepository struct {
	Base
}

// NewAlertRuleRepository 构造告警规则仓储。
func NewAlertRuleRepository(db *gorm.DB) *AlertRuleRepository {
	return &AlertRuleRepository{Base: Base{db: db}}
}

// List 分页检索规则。
func (r *AlertRuleRepository) List(ctx context.Context, instanceID int64, limit, offset int) ([]model.AlertRule, int64, error) {
	q := r.withCtx(ctx).Model(&model.AlertRule{})
	if instanceID > 0 {
		q = q.Where("instance_id = ?", instanceID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count rules")
	}
	var items []model.AlertRule
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list rules")
	}
	return items, total, nil
}

// AllEnabled 返回全部启用的规则（供定时评估）。
func (r *AlertRuleRepository) AllEnabled(ctx context.Context) ([]model.AlertRule, error) {
	var items []model.AlertRule
	if err := r.withCtx(ctx).Where("enabled = ?", true).Find(&items).Error; err != nil {
		return nil, wrap(err, "list enabled rules")
	}
	return items, nil
}

// Get 按 ID 查询规则。
func (r *AlertRuleRepository) Get(ctx context.Context, id int64) (*model.AlertRule, error) {
	var item model.AlertRule
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get rule")
	}
	return &item, nil
}

// Create 新增规则。
func (r *AlertRuleRepository) Create(ctx context.Context, rule *model.AlertRule) error {
	if err := r.withCtx(ctx).Create(rule).Error; err != nil {
		return wrap(err, "create rule")
	}
	return nil
}

// Update 更新规则。
func (r *AlertRuleRepository) Update(ctx context.Context, rule *model.AlertRule) error {
	res := r.withCtx(ctx).Model(&model.AlertRule{}).Where("id = ?", rule.ID).
		Updates(map[string]any{
			"name":            rule.Name,
			"instance_id":     rule.InstanceID,
			"mw_type":         rule.MWType,
			"metric_name":     rule.MetricName,
			"operator":        rule.Operator,
			"threshold":       rule.Threshold,
			"level":           rule.Level,
			"window":          rule.Window,
			"cooldown":        rule.Cooldown,
			"notify_channels": rule.NotifyChannels,
			"enabled":         rule.Enabled,
			"ai_enabled":      rule.AIEnabled,
			"description":     rule.Description,
		})
	if res.Error != nil {
		return wrap(res.Error, "update rule")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update rule")
	}
	return nil
}

// Delete 删除规则。
func (r *AlertRuleRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.AlertRule{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete rule")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "delete rule")
	}
	return nil
}

// AlertRepository 提供告警事件的数据访问。
type AlertRepository struct {
	Base
}

// NewAlertRepository 构造告警仓储。
func NewAlertRepository(db *gorm.DB) *AlertRepository {
	return &AlertRepository{Base: Base{db: db}}
}

// AlertFilter 是告警检索条件。
type AlertFilter struct {
	InstanceID int64
	MWType     string
	Level      string
	Status     string
	ClusterID  string
	From       *time.Time
	To         *time.Time
}

// query 构造检索查询。
func (r *AlertRepository) query(ctx context.Context, f AlertFilter) *gorm.DB {
	q := r.withCtx(ctx).Model(&model.Alert{})
	if f.InstanceID > 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	if f.MWType != "" {
		q = q.Where("mw_type = ?", f.MWType)
	}
	if f.Level != "" {
		q = q.Where("alert_level = ?", f.Level)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.ClusterID != "" {
		q = q.Where("cluster_id = ?", f.ClusterID)
	}
	if f.From != nil {
		q = q.Where("triggered_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("triggered_at <= ?", *f.To)
	}
	return q
}

// List 分页检索告警。
func (r *AlertRepository) List(ctx context.Context, f AlertFilter, limit, offset int) ([]model.Alert, int64, error) {
	var total int64
	if err := r.query(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count alerts")
	}
	var items []model.Alert
	if err := r.query(ctx, f).Order("triggered_at DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list alerts")
	}
	return items, total, nil
}

// Get 按 ID 查询告警。
func (r *AlertRepository) Get(ctx context.Context, id int64) (*model.Alert, error) {
	var item model.Alert
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get alert")
	}
	return &item, nil
}

// FindActiveByFingerprint 查询窗口内同指纹的活跃告警（用于去重合并）。
func (r *AlertRepository) FindActiveByFingerprint(ctx context.Context, fingerprint string, since time.Time) (*model.Alert, error) {
	var item model.Alert
	err := r.withCtx(ctx).
		Where("fingerprint = ? AND status = ? AND triggered_at >= ?", fingerprint, model.AlertStatusActive, since).
		Order("triggered_at DESC").First(&item).Error
	if err != nil {
		return nil, wrap(err, "find active alert")
	}
	return &item, nil
}

// Create 新增告警。
func (r *AlertRepository) Create(ctx context.Context, alert *model.Alert) error {
	if err := r.withCtx(ctx).Create(alert).Error; err != nil {
		return wrap(err, "create alert")
	}
	return nil
}

// BumpCount 累加窗口内的重复次数并刷新最近触发时间。
func (r *AlertRepository) BumpCount(ctx context.Context, id int64, triggeredAt time.Time) error {
	return wrap(r.withCtx(ctx).Model(&model.Alert{}).Where("id = ?", id).
		Updates(map[string]any{
			"count":        gorm.Expr("count + 1"),
			"triggered_at": triggeredAt,
		}).Error, "bump alert count")
}

// Ack 确认告警。
func (r *AlertRepository) Ack(ctx context.Context, id, userID int64) error {
	now := time.Now().UTC()
	res := r.withCtx(ctx).Model(&model.Alert{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":   model.AlertStatusAcknowledged,
			"acked_by": userID,
			"acked_at": now,
		})
	if res.Error != nil {
		return wrap(res.Error, "ack alert")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "ack alert")
	}
	return nil
}

// SetCluster 标注语义聚类分组（仅合并展示，不修改状态）。
func (r *AlertRepository) SetCluster(ctx context.Context, id int64, clusterID string) error {
	return wrap(r.withCtx(ctx).Model(&model.Alert{}).Where("id = ?", id).
		Update("cluster_id", clusterID).Error, "set alert cluster")
}

// SetStatus 更新告警状态（active → acknowledged/resolved）。
func (r *AlertRepository) SetStatus(ctx context.Context, id int64, status string, resolvedAt *time.Time) error {
	updates := map[string]any{"status": status}
	if resolvedAt != nil {
		updates["resolved_at"] = *resolvedAt
	}
	res := r.withCtx(ctx).Model(&model.Alert{}).Where("id = ?", id).Updates(updates)
	if res.Error != nil {
		return wrap(res.Error, "set alert status")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "set alert status")
	}
	return nil
}

// AttachDiagnosis 关联 AI 诊断记录。
func (r *AlertRepository) AttachDiagnosis(ctx context.Context, id, diagnosisID int64) error {
	return wrap(r.withCtx(ctx).Model(&model.Alert{}).Where("id = ?", id).
		Update("diagnosis_id", diagnosisID).Error, "attach alert diagnosis")
}

// CountByLevel 统计各等级活跃告警数量（大盘使用）。
func (r *AlertRepository) CountByLevel(ctx context.Context, since time.Time) (map[string]int64, error) {
	type row struct {
		Level string
		Total int64
	}
	var rows []row
	q := r.withCtx(ctx).Model(&model.Alert{}).
		Select("alert_level, COUNT(*) AS total").Group("alert_level")
	if !since.IsZero() {
		q = q.Where("triggered_at >= ?", since)
	}
	if err := q.Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count alerts by level")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.Level] = item.Total
	}
	return out, nil
}

// Trend 返回按小时聚合的告警趋势。
func (r *AlertRepository) Trend(ctx context.Context, since time.Time, buckets int) ([]TrendPoint, error) {
	var points []TrendPoint
	if err := r.withCtx(ctx).
		Raw(`SELECT to_char(date_trunc('hour', triggered_at), 'MM-DD HH24:00') AS label,
		            COUNT(*) AS total
		     FROM alerts
		     WHERE triggered_at >= ?
		     GROUP BY 1
		     ORDER BY 1`, since).Scan(&points).Error; err != nil {
		return nil, wrap(err, "alert trend")
	}
	if buckets > 0 && len(points) > buckets {
		points = points[len(points)-buckets:]
	}
	return points, nil
}

// TrendPoint 是趋势图上的一个数据点。
type TrendPoint struct {
	Label string  `json:"label"`
	Total int64   `json:"total"`
	Value float64 `json:"value,omitempty"`
}

// AlertEmbeddingRepository 提供告警向量数据访问。
type AlertEmbeddingRepository struct {
	Base
}

// NewAlertEmbeddingRepository 构造告警向量仓储。
func NewAlertEmbeddingRepository(db *gorm.DB) *AlertEmbeddingRepository {
	return &AlertEmbeddingRepository{Base: Base{db: db}}
}

// Create 写入向量。
func (r *AlertEmbeddingRepository) Create(ctx context.Context, item *model.AlertEmbedding) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create alert embedding")
	}
	return nil
}

// ListUnclustered 返回未聚类的向量，供离线聚类任务处理。
func (r *AlertEmbeddingRepository) ListUnclustered(ctx context.Context, limit int) ([]model.AlertEmbedding, error) {
	var items []model.AlertEmbedding
	if err := r.withCtx(ctx).Where("clustered = ?", false).
		Order("id ASC").Limit(limit).Find(&items).Error; err != nil {
		return nil, wrap(err, "list unclustered embeddings")
	}
	return items, nil
}

// MarkClustered 标记已聚类。
func (r *AlertEmbeddingRepository) MarkClustered(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return wrap(r.withCtx(ctx).Model(&model.AlertEmbedding{}).Where("id IN ?", ids).
		Update("clustered", true).Error, "mark clustered")
}
