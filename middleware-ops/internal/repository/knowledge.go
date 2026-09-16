package repository

import (
	"context"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// KnowledgeFilter 是知识库检索条件。
type KnowledgeFilter struct {
	Keyword string
	MWType  string
	Status  string
	Source  string
	Tag     string
	OnlyPub bool
}

// KnowledgeRepository 提供知识库数据访问。
type KnowledgeRepository struct {
	Base
}

// NewKnowledgeRepository 构造知识库仓储。
func NewKnowledgeRepository(db *gorm.DB) *KnowledgeRepository {
	return &KnowledgeRepository{Base: Base{db: db}}
}

// query 构造检索查询。
func (r *KnowledgeRepository) query(ctx context.Context, f KnowledgeFilter) *gorm.DB {
	q := r.withCtx(ctx).Model(&model.KnowledgeBase{})
	if f.Keyword != "" {
		like := "%" + f.Keyword + "%"
		q = q.Where("title LIKE ? OR content LIKE ?", like, like)
	}
	if f.MWType != "" {
		q = q.Where("mw_type = ?", f.MWType)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	} else if f.OnlyPub {
		q = q.Where("status = ?", model.KnowledgeStatusPublished)
	}
	if f.Source != "" {
		q = q.Where("source = ?", f.Source)
	}
	if f.Tag != "" {
		// 标签以 JSON 数组存储，使用参数化的模糊匹配（不拼接原始值以外的 SQL）。
		q = q.Where("tags LIKE ?", "%\""+f.Tag+"\"%")
	}
	return q
}

// List 分页检索知识条目。
func (r *KnowledgeRepository) List(ctx context.Context, f KnowledgeFilter, limit, offset int) ([]model.KnowledgeBase, int64, error) {
	var total int64
	if err := r.query(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count knowledge")
	}
	var items []model.KnowledgeBase
	if err := r.query(ctx, f).Order("adopt_count DESC, id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list knowledge")
	}
	return items, total, nil
}

// Get 按 ID 查询知识条目。
func (r *KnowledgeRepository) Get(ctx context.Context, id int64) (*model.KnowledgeBase, error) {
	var item model.KnowledgeBase
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get knowledge")
	}
	return &item, nil
}

// Create 新增知识条目。
func (r *KnowledgeRepository) Create(ctx context.Context, item *model.KnowledgeBase) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create knowledge")
	}
	return nil
}

// Update 更新知识条目。
func (r *KnowledgeRepository) Update(ctx context.Context, item *model.KnowledgeBase) error {
	res := r.withCtx(ctx).Model(&model.KnowledgeBase{}).Where("id = ?", item.ID).
		Updates(map[string]any{
			"title":   item.Title,
			"content": item.Content,
			"mw_type": item.MWType,
			"tags":    item.Tags,
			"status":  item.Status,
		})
	if res.Error != nil {
		return wrap(res.Error, "update knowledge")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "update knowledge")
	}
	return nil
}

// UpdateEmbedding 回填向量。
func (r *KnowledgeRepository) UpdateEmbedding(ctx context.Context, id int64, embedding model.Vector) error {
	return wrap(r.withCtx(ctx).Model(&model.KnowledgeBase{}).Where("id = ?", id).
		Update("embedding", embedding).Error, "update knowledge embedding")
}

// BumpAdopt 累加采纳次数（质量闭环：低采纳率条目降权）。
func (r *KnowledgeRepository) BumpAdopt(ctx context.Context, id int64) error {
	return wrap(r.withCtx(ctx).Model(&model.KnowledgeBase{}).Where("id = ?", id).
		Update("adopt_count", gorm.Expr("adopt_count + 1")).Error, "bump adopt count")
}

// BumpUse 累加被引用次数。
func (r *KnowledgeRepository) BumpUse(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	return wrap(r.withCtx(ctx).Model(&model.KnowledgeBase{}).Where("id IN ?", ids).
		Update("use_count", gorm.Expr("use_count + 1")).Error, "bump use count")
}

// Delete 删除知识条目。
func (r *KnowledgeRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.KnowledgeBase{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete knowledge")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "delete knowledge")
	}
	return nil
}

// CountByStatus 统计各状态条目数量。
func (r *KnowledgeRepository) CountByStatus(ctx context.Context) (map[string]int64, error) {
	type row struct {
		Status string
		Total  int64
	}
	var rows []row
	if err := r.withCtx(ctx).Model(&model.KnowledgeBase{}).
		Select("status, COUNT(*) AS total").Group("status").Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count knowledge by status")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.Status] = item.Total
	}
	return out, nil
}

// CandidatesForVector 返回参与向量检索的候选集（已发布 + 有向量）。
//
// 默认构建下 pgvector 不可用，检索在应用层完成，因此需要拉取候选集；
// 候选规模由「已发布条目」上限约束（知识库为人工确认后入库，规模可控）。
func (r *KnowledgeRepository) CandidatesForVector(ctx context.Context, mwType string, limit int) ([]model.KnowledgeBase, error) {
	q := r.withCtx(ctx).
		Where("status = ? AND embedding IS NOT NULL", model.KnowledgeStatusPublished)
	if mwType != "" {
		q = q.Where("mw_type = ? OR mw_type = ''", mwType)
	}
	var items []model.KnowledgeBase
	if err := q.Order("adopt_count DESC").Limit(limit).Find(&items).Error; err != nil {
		return nil, wrap(err, "list vector candidates")
	}
	return items, nil
}
