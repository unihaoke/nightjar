package repository

import (
	"context"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// ApprovalRepository 提供审批工单数据访问（6.2）。
type ApprovalRepository struct {
	Base
}

// NewApprovalRepository 构造审批仓储。
func NewApprovalRepository(db *gorm.DB) *ApprovalRepository {
	return &ApprovalRepository{Base: Base{db: db}}
}

// ApprovalFilter 是审批检索条件。
type ApprovalFilter struct {
	ApplicantID int64
	Status      string
	Environment string
	InstanceID  int64
}

// List 分页检索审批工单。
func (r *ApprovalRepository) List(ctx context.Context, f ApprovalFilter, limit, offset int) ([]model.Approval, int64, error) {
	q := r.withCtx(ctx).Model(&model.Approval{})
	if f.ApplicantID > 0 {
		q = q.Where("applicant_id = ?", f.ApplicantID)
	}
	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.Environment != "" {
		q = q.Where("environment = ?", f.Environment)
	}
	if f.InstanceID > 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count approvals")
	}
	var items []model.Approval
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list approvals")
	}
	return items, total, nil
}

// Get 按 ID 查询。
func (r *ApprovalRepository) Get(ctx context.Context, id int64) (*model.Approval, error) {
	var item model.Approval
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get approval")
	}
	return &item, nil
}

// GetByTicket 按工单号查询。
func (r *ApprovalRepository) GetByTicket(ctx context.Context, ticket string) (*model.Approval, error) {
	var item model.Approval
	if err := r.withCtx(ctx).Where("ticket_id = ?", ticket).First(&item).Error; err != nil {
		return nil, wrap(err, "get approval by ticket")
	}
	return &item, nil
}

// Create 创建工单。
func (r *ApprovalRepository) Create(ctx context.Context, item *model.Approval) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create approval")
	}
	return nil
}

// Decide 审批动作（通过/驳回）。
func (r *ApprovalRepository) Decide(ctx context.Context, id, approverID int64, approved bool, comment string) error {
	status := "rejected"
	if approved {
		status = "approved"
	}
	now := time.Now().UTC()
	res := r.withCtx(ctx).Model(&model.Approval{}).
		Where("id = ? AND status = ?", id, "pending").
		Updates(map[string]any{
			"status":      status,
			"approver_id": approverID,
			"comment":     comment,
			"decided_at":  now,
		})
	if res.Error != nil {
		return wrap(res.Error, "decide approval")
	}
	if res.RowsAffected == 0 {
		return wrap(gorm.ErrRecordNotFound, "decide approval")
	}
	return nil
}

// MarkExecuted 回填执行结果（结果复核，6.2）。
func (r *ApprovalRepository) MarkExecuted(ctx context.Context, id int64, status string, result map[string]any) error {
	now := time.Now().UTC()
	return wrap(r.withCtx(ctx).Model(&model.Approval{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":      status,
			"executed_at": now,
			"exec_result": model.JSONMap(result),
		}).Error, "mark approval executed")
}

// ExpireOverdue 把超时未审批的工单自动拒绝（30min，见 4.6）。
//
// 返回被拒绝的工单号列表，便于发送通知与写审计。
func (r *ApprovalRepository) ExpireOverdue(ctx context.Context, now time.Time) ([]string, error) {
	var items []model.Approval
	if err := r.withCtx(ctx).
		Where("status = ? AND expires_at <= ?", "pending", now).
		Find(&items).Error; err != nil {
		return nil, wrap(err, "list overdue approvals")
	}
	if len(items) == 0 {
		return nil, nil
	}
	ids := make([]int64, 0, len(items))
	tickets := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
		tickets = append(tickets, item.TicketID)
	}
	if err := r.withCtx(ctx).Model(&model.Approval{}).Where("id IN ?", ids).
		Updates(map[string]any{
			"status":     "expired",
			"comment":    "审批超时自动拒绝",
			"decided_at": now,
		}).Error; err != nil {
		return nil, wrap(err, "expire approvals")
	}
	return tickets, nil
}

// CountPending 统计待审批数量（大盘角标）。
func (r *ApprovalRepository) CountPending(ctx context.Context) (int64, error) {
	var total int64
	if err := r.withCtx(ctx).Model(&model.Approval{}).Where("status = ?", "pending").Count(&total).Error; err != nil {
		return 0, wrap(err, "count pending approvals")
	}
	return total, nil
}

// FixRepository 提供修复执行记录数据访问（4.6）。
type FixRepository struct {
	Base
}

// NewFixRepository 构造修复记录仓储。
func NewFixRepository(db *gorm.DB) *FixRepository {
	return &FixRepository{Base: Base{db: db}}
}

// List 分页检索修复历史。
func (r *FixRepository) List(ctx context.Context, instanceID int64, limit, offset int) ([]model.FixRecord, int64, error) {
	q := r.withCtx(ctx).Model(&model.FixRecord{})
	if instanceID > 0 {
		q = q.Where("instance_id = ?", instanceID)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count fix records")
	}
	var items []model.FixRecord
	if err := q.Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list fix records")
	}
	return items, total, nil
}

// Create 写入修复记录。
func (r *FixRepository) Create(ctx context.Context, item *model.FixRecord) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create fix record")
	}
	return nil
}
