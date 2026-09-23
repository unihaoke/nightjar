package repository

import (
	"context"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// AIAnalysisTaskRepository 提供异步 AI 分析任务的数据访问。
//
// 任务是"提交—回调—超时"这条链路的唯一事实来源：提交后写一条 submitted，
// 回调到达时按 TaskID 定位并改写状态，超时扫描按 DeadlineAt 找出该收尾的任务。
type AIAnalysisTaskRepository struct {
	Base
}

// NewAIAnalysisTaskRepository 构造异步分析任务仓储。
func NewAIAnalysisTaskRepository(db *gorm.DB) *AIAnalysisTaskRepository {
	return &AIAnalysisTaskRepository{Base: Base{db: db}}
}

// Create 写入一条新任务（提交成功后调用）。
func (r *AIAnalysisTaskRepository) Create(ctx context.Context, item *model.AIAnalysisTask) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create ai analysis task")
	}
	return nil
}

// GetByTaskID 按外部任务号查询（回调与轮询的入口）。
func (r *AIAnalysisTaskRepository) GetByTaskID(ctx context.Context, taskID string) (*model.AIAnalysisTask, error) {
	var item model.AIAnalysisTask
	if err := r.withCtx(ctx).Where("task_id = ?", taskID).First(&item).Error; err != nil {
		return nil, wrap(err, "get ai analysis task by task id")
	}
	return &item, nil
}

// GetByRunID 按运行 ID 查询（开放接口下 runId 在提交响应/回调/轮询间是同一稳定标识）。
func (r *AIAnalysisTaskRepository) GetByRunID(ctx context.Context, runID string) (*model.AIAnalysisTask, error) {
	var item model.AIAnalysisTask
	if err := r.withCtx(ctx).Where("run_id = ?", runID).First(&item).Error; err != nil {
		return nil, wrap(err, "get ai analysis task by run id")
	}
	return &item, nil
}

// GetByEvent 取某条事件最近一次任务（页面展示"这条告警的 AI 任务在哪一步"）。
func (r *AIAnalysisTaskRepository) GetByEvent(ctx context.Context, eventID int64) (*model.AIAnalysisTask, error) {
	var item model.AIAnalysisTask
	if err := r.withCtx(ctx).Where("event_id = ?", eventID).Order("id DESC").First(&item).Error; err != nil {
		return nil, wrap(err, "get ai analysis task by event")
	}
	return &item, nil
}

// ListUnfinished 取仍未收到结论且未超时的任务（轮询兜底用）。
func (r *AIAnalysisTaskRepository) ListUnfinished(ctx context.Context, limit int) ([]model.AIAnalysisTask, error) {
	var items []model.AIAnalysisTask
	if err := r.withCtx(ctx).
		Where("status = ?", model.AIAnalysisTaskSubmitted).
		Order("id ASC").Limit(limit).Find(&items).Error; err != nil {
		return nil, wrap(err, "list unfinished ai analysis tasks")
	}
	return items, nil
}

// ListTimedOut 取已超过截止时间仍未收尾的任务。
func (r *AIAnalysisTaskRepository) ListTimedOut(ctx context.Context, now time.Time, limit int) ([]model.AIAnalysisTask, error) {
	var items []model.AIAnalysisTask
	if err := r.withCtx(ctx).
		Where("status = ? AND deadline_at IS NOT NULL AND deadline_at < ?",
			model.AIAnalysisTaskSubmitted, now).
		Order("id ASC").Limit(limit).Find(&items).Error; err != nil {
		return nil, wrap(err, "list timed out ai analysis tasks")
	}
	return items, nil
}

// Complete 写入终态（结论或失败）。
//
// 用带条件的 UPDATE（当前状态必须是 submitted）保证幂等：
// 回调重复投递、或回调与轮询同时到达时，第二个调用者影响行数为 0，不会覆盖已有结论。
func (r *AIAnalysisTaskRepository) Complete(ctx context.Context, taskID, status, answer, errMsg string, at time.Time) (bool, error) {
	res := r.withCtx(ctx).Model(&model.AIAnalysisTask{}).
		Where("task_id = ? AND status = ?", taskID, model.AIAnalysisTaskSubmitted).
		Updates(map[string]any{
			"status":       status,
			"answer":       answer,
			"error":        errMsg,
			"completed_at": at,
		})
	if res.Error != nil {
		return false, wrap(res.Error, "complete ai analysis task")
	}
	return res.RowsAffected == 1, nil
}
