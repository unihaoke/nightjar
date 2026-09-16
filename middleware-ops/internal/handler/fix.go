package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// PreviewFix 预览修复操作（L0，含影响范围与风险）。
func (h *Handler) PreviewFix(c *gin.Context) {
	var in service.FixRequest
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.Fix.Preview(c.Request.Context(), in, h.operator(c), h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ExecuteFix 执行修复（L1 直接执行，L2 转审批）。
func (h *Handler) ExecuteFix(c *gin.Context) {
	var in service.FixRequest
	if !bindJSON(c, &in) {
		return
	}
	session := h.session(c)
	if err := h.deps.Auth.Require(session, service.PermFixExecute); err != nil {
		response.Fail(c, err)
		return
	}
	result, err := h.deps.Fix.Execute(c.Request.Context(), in, session, h.operator(c), h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// FixHistory 修复历史。
func (h *Handler) FixHistory(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.Fix.History(c.Request.Context(), queryInt64(c, "instance_id"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// FixOptions 返回动作目录与分级说明。
func (h *Handler) FixOptions(c *gin.Context) {
	response.OK(c, gin.H{
		"actions": service.ActionCatalog(),
		"levels": []map[string]any{
			{"level": "L0", "label": "只读", "desc": "查询、诊断、修复预览，直接执行"},
			{"level": "L1", "label": "低危", "desc": "确认告警、知识库编辑、创建规则，直接执行并留痕"},
			{"level": "L2", "label": "高危", "desc": "清理 key、重启、删数据、改配置、SQL 写操作，需二次确认 + 审批"},
		},
		"sql_guard": gin.H{
			"default_limit": h.deps.Guardrails.SQLDefaultLimit,
			"max_limit":     h.deps.Guardrails.SQLMaxLimit,
			"allowlist":     h.deps.Guardrails.SQLTableAllowlist,
		},
		"executor": "dry-run（默认仅预演，未接入真实执行客户端）",
	})
}

// ValidateSQL 校验 AI 生成 SQL 的只读安全性（5.5）。
func (h *Handler) ValidateSQL(c *gin.Context) {
	var req struct {
		SQL string `json:"sql" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	normalized, notes, err := h.deps.SQLGuard.Validate(req.SQL)
	if err != nil {
		response.Fail(c, apperr.Newf(apperr.CodeUnsafeSQL, "%s", err.Error()))
		return
	}
	highRisk, reason := service.IsHighRiskSQL(normalized)
	response.OK(c, gin.H{
		"normalized_sql":   normalized,
		"notes":            notes,
		"high_risk":        highRisk,
		"high_risk_reason": reason,
	})
}

// ListApprovals 审批工单列表。
func (h *Handler) ListApprovals(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	filter := repository.ApprovalFilter{
		Status:      c.Query("status"),
		Environment: c.Query("environment"),
		InstanceID:  queryInt64(c, "instance_id"),
	}
	if c.Query("mine") == "true" {
		if session := h.session(c); session != nil && session.User != nil {
			filter.ApplicantID = session.User.ID
		}
	}
	items, total, err := h.deps.Approval.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// GetApproval 工单详情。
func (h *Handler) GetApproval(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Approval.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// DecideApproval 审批决策（通过/驳回）。
func (h *Handler) DecideApproval(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Approved bool   `json:"approved"`
		Comment  string `json:"comment"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.deps.Auth.Require(h.session(c), service.PermApprovalDecide); err != nil {
		response.Fail(c, err)
		return
	}
	item, err := h.deps.Approval.Decide(c.Request.Context(), id, req.Approved, req.Comment, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}
