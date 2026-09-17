package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// ApprovalTTL 是审批工单有效期（4.6：超时 30min 自动拒绝）。
const ApprovalTTL = 30 * time.Minute

// ApprovalService 实现 L2 高危操作的审批链路（6.2）。
type ApprovalService struct {
	repo     *repository.ApprovalRepository
	notifier *NotifierService
	audit    *AuditService
	log      *zap.Logger
}

// NewApprovalService 构造审批服务。
func NewApprovalService(repo *repository.ApprovalRepository, notifier *NotifierService, audit *AuditService, log *zap.Logger) *ApprovalService {
	return &ApprovalService{repo: repo, notifier: notifier, audit: audit, log: log}
}

// ApprovalRequest 是创建工单入参。
type ApprovalRequest struct {
	InstanceID   int64          `json:"instance_id"`
	Environment  string         `json:"environment"`
	ActionType   string         `json:"action_type"`
	ActionDetail map[string]any `json:"action_detail"`
	Preview      map[string]any `json:"preview"`
	Reason       string         `json:"reason"`
	// AlertID / DiagnosisID 为来源上下文（告警 / 诊断），用于审批通过后回填来源告警，闭合处置链路。
	AlertID     int64 `json:"alert_id"`
	DiagnosisID int64 `json:"diagnosis_id"`
}

// Create 创建审批工单。
func (s *ApprovalService) Create(ctx context.Context, in ApprovalRequest, operator Operator) (*model.Approval, error) {
	ticket := &model.Approval{
		TicketID:     "AP" + utils.Fingerprint(in.ActionType, operator.Username, time.Now().UTC().Format(time.RFC3339Nano))[:12],
		ApplicantID:  operator.UserID,
		InstanceID:   in.InstanceID,
		AlertID:      in.AlertID,
		DiagnosisID:  in.DiagnosisID,
		Environment:  in.Environment,
		ActionType:   in.ActionType,
		ActionDetail: model.JSONMap(in.ActionDetail),
		Preview:      model.JSONMap(in.Preview),
		Reason:       in.Reason,
		Status:       "pending",
		ExpiresAt:    time.Now().UTC().Add(ApprovalTTL),
	}
	if err := s.repo.Create(ctx, ticket); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.auditRecord(ctx, operator, ticket.InstanceID, "approval_create", map[string]any{
		"ticket_id": ticket.TicketID, "action_type": ticket.ActionType,
		"environment": ticket.Environment, "reason": ticket.Reason,
		"alert_id": ticket.AlertID, "diagnosis_id": ticket.DiagnosisID,
	})
	if s.notifier != nil {
		s.notifier.NotifyApproval(ctx, ticket, "created")
	}
	return ticket, nil
}

// List 分页查询工单。
func (s *ApprovalService) List(ctx context.Context, f repository.ApprovalFilter, limit, offset int) ([]model.Approval, int64, error) {
	items, total, err := s.repo.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// Get 查询工单。
func (s *ApprovalService) Get(ctx context.Context, id int64) (*model.Approval, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "工单不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// GetByTicket 按工单号查询。
func (s *ApprovalService) GetByTicket(ctx context.Context, ticket string) (*model.Approval, error) {
	item, err := s.repo.GetByTicket(ctx, ticket)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "工单不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// Decide 审批通过或驳回（需 approval:decide 权限点）。
func (s *ApprovalService) Decide(ctx context.Context, id int64, approved bool, comment string, operator Operator) (*model.Approval, error) {
	ticket, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if ticket.Status != "pending" {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "工单状态为 %s，不可重复审批", ticket.Status)
	}
	if ticket.ApplicantID == operator.UserID {
		// 双人复核原则：申请人不能自审（等同 6.4 的双人复核要求）。
		return nil, apperr.New(apperr.CodeForbidden, "申请人与审批人不能为同一人")
	}
	if err := s.repo.Decide(ctx, id, operator.UserID, approved, comment); err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeInvalidParam, "工单状态已变更，请刷新后重试")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	updated, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	action := "approval_reject"
	if approved {
		action = "approval_approve"
	}
	s.auditRecord(ctx, operator, updated.InstanceID, action, map[string]any{
		"ticket_id": updated.TicketID, "comment": comment, "action_type": updated.ActionType,
	})
	if s.notifier != nil {
		state := "rejected"
		if approved {
			state = "approved"
		}
		s.notifier.NotifyApproval(ctx, updated, state)
	}
	return updated, nil
}

// MarkExecuted 回填执行结果（结果复核）。
func (s *ApprovalService) MarkExecuted(ctx context.Context, ticketID, status string, result map[string]any) error {
	ticket, err := s.GetByTicket(ctx, ticketID)
	if err != nil {
		return err
	}
	if err := s.repo.MarkExecuted(ctx, ticket.ID, status, result); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	return nil
}

// ExpireOverdue 处理超时工单（定时任务调用）。
func (s *ApprovalService) ExpireOverdue(ctx context.Context) ([]string, error) {
	tickets, err := s.repo.ExpireOverdue(ctx, time.Now().UTC())
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	for _, ticket := range tickets {
		s.log.Warn("审批超时自动拒绝", zap.String("ticket_id", ticket))
		s.auditRecord(ctx, Operator{Username: "system"}, 0, "approval_expire", map[string]any{"ticket_id": ticket})
	}
	return tickets, nil
}

// CountPending 统计待审批数量。
func (s *ApprovalService) CountPending(ctx context.Context) (int64, error) {
	total, err := s.repo.CountPending(ctx)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return total, nil
}

// auditRecord 写审计。
func (s *ApprovalService) auditRecord(ctx context.Context, op Operator, instanceID int64, action string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, InstanceID: instanceID,
		ActionType: action, Level: LevelHigh, IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}
