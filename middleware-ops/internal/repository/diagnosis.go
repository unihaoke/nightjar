package repository

import (
	"context"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// DiagnosisFilter 是诊断历史检索条件。
type DiagnosisFilter struct {
	UserID     int64
	InstanceID int64
	MWType     string
	Feedback   string
	Keyword    string
	From       *time.Time
	To         *time.Time
}

// DiagnosisRepository 提供 AI 诊断记录的访问。
type DiagnosisRepository struct {
	Base
}

// NewDiagnosisRepository 构造诊断仓储。
func NewDiagnosisRepository(db *gorm.DB) *DiagnosisRepository {
	return &DiagnosisRepository{Base: Base{db: db}}
}

// query 构造检索查询。
func (r *DiagnosisRepository) query(ctx context.Context, f DiagnosisFilter) *gorm.DB {
	q := r.withCtx(ctx).Model(&model.AIDiagnosis{})
	if f.UserID > 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.InstanceID > 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	if f.MWType != "" {
		q = q.Where("mw_type = ?", f.MWType)
	}
	if f.Feedback != "" {
		q = q.Where("feedback = ?", f.Feedback)
	}
	if f.Keyword != "" {
		q = q.Where("user_query LIKE ?", "%"+f.Keyword+"%")
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("created_at <= ?", *f.To)
	}
	return q
}

// List 分页检索诊断历史。
func (r *DiagnosisRepository) List(ctx context.Context, f DiagnosisFilter, limit, offset int) ([]model.AIDiagnosis, int64, error) {
	var total int64
	if err := r.query(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count diagnoses")
	}
	var items []model.AIDiagnosis
	if err := r.query(ctx, f).Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list diagnoses")
	}
	return items, total, nil
}

// Get 按 ID 查询诊断记录。
func (r *DiagnosisRepository) Get(ctx context.Context, id int64) (*model.AIDiagnosis, error) {
	var item model.AIDiagnosis
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get diagnosis")
	}
	return &item, nil
}

// Create 写入诊断记录。
func (r *DiagnosisRepository) Create(ctx context.Context, item *model.AIDiagnosis) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create diagnosis")
	}
	return nil
}

// UpdateFeedback 更新用户反馈（质量闭环，5.6）。
func (r *DiagnosisRepository) UpdateFeedback(ctx context.Context, id int64, feedback string) error {
	res := r.withCtx(ctx).Model(&model.AIDiagnosis{}).Where("id = ?", id).
		Update("feedback", feedback)
	if res.Error != nil {
		return wrap(res.Error, "update diagnosis feedback")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update diagnosis feedback")
	}
	return nil
}

// AttachKnowledge 回填沉淀的知识条目 ID（通过 suggestions/collected_metrics 之外的独立查询完成）。
func (r *DiagnosisRepository) CountFeedback(ctx context.Context) (map[string]int64, error) {
	type row struct {
		Feedback string
		Total    int64
	}
	var rows []row
	if err := r.withCtx(ctx).Model(&model.AIDiagnosis{}).
		Select("feedback, COUNT(*) AS total").Group("feedback").Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count feedback")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		key := item.Feedback
		if key == "" {
			key = "none"
		}
		out[key] = item.Total
	}
	return out, nil
}

// Stats 返回诊断总量、平均耗时与 token 消耗，供大盘与成本面板使用。
func (r *DiagnosisRepository) Stats(ctx context.Context, since time.Time) (total int64, avgDuration float64, tokens int64, degraded int64, err error) {
	type agg struct {
		Total    int64
		AvgDur   float64
		Tokens   int64
		Degraded int64
	}
	var row agg
	q := r.withCtx(ctx).Model(&model.AIDiagnosis{}).
		Select(`COUNT(*) AS total,
			COALESCE(AVG(duration_ms),0) AS avg_dur,
			COALESCE(SUM(cost_tokens),0) AS tokens,
			COALESCE(SUM(CASE WHEN engine_status <> 'ok' THEN 1 ELSE 0 END),0) AS degraded`)
	if !since.IsZero() {
		q = q.Where("created_at >= ?", since)
	}
	if err := q.Scan(&row).Error; err != nil {
		return 0, 0, 0, 0, wrap(err, "diagnosis stats")
	}
	return row.Total, row.AvgDur, row.Tokens, row.Degraded, nil
}
