package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// ListAuditLogs 审计日志列表（admin/运维）。
func (h *Handler) ListAuditLogs(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	filter := repository.AuditFilter{
		UserID:     queryInt64(c, "user_id"),
		InstanceID: queryInt64(c, "instance_id"),
		ActionType: c.Query("action_type"),
		Level:      c.Query("level"),
		Result:     c.Query("result"),
		Keyword:    c.Query("keyword"),
		From:       queryTime(c, "from"),
		To:         queryTime(c, "to"),
	}
	items, total, err := h.deps.Audit.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// GetAuditLog 审计日志详情。
func (h *Handler) GetAuditLog(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Audit.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// VerifyAudit 校验审计哈希链完整性（6.4）。
func (h *Handler) VerifyAudit(c *gin.Context) {
	fromID := queryInt64(c, "from_id")
	toID := queryInt64(c, "to_id")
	ok, brokenID, err := h.deps.Audit.Verify(c.Request.Context(), fromID, toID)
	if err != nil {
		response.Fail(c, err)
		return
	}
	message := "哈希链校验通过，审计日志未被篡改"
	if !ok {
		message = "哈希链校验失败，请立即排查"
	}
	response.OK(c, gin.H{"verified": ok, "broken_id": brokenID, "message": message})
}

// SnapshotAudit 生成审计哈希链快照（L1）。
func (h *Handler) SnapshotAudit(c *gin.Context) {
	snapshot, err := h.deps.Audit.Snapshot(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	h.auditUser(c, "audit_snapshot", map[string]any{
		"date": snapshot.SnapshotDate, "verified": snapshot.Verified, "logs": snapshot.LogCount,
	})
	response.OK(c, snapshot)
}

// ListAuditSnapshots 快照列表。
func (h *Handler) ListAuditSnapshots(c *gin.Context) {
	items, err := h.deps.Audit.ListSnapshots(c.Request.Context(), 30)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"list": items})
}

// ListLogEvents 日志告警事件列表。
func (h *Handler) ListLogEvents(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	filter := repository.LogEventFilter{
		ServerID:    queryInt64(c, "server_id"),
		ServiceName: c.Query("service"),
		AlertType:   c.Query("alert_type"),
		Status:      c.Query("status"),
		Signature:   c.Query("signature"),
		Keyword:     c.Query("keyword"),
		From:        queryTime(c, "from"),
		To:          queryTime(c, "to"),
	}
	items, total, err := h.deps.LogAlert.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// GetLogEvent 日志事件详情（含代码分析报告）。
func (h *Handler) GetLogEvent(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	event, err := h.deps.LogAlert.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	analysis, _ := h.deps.CodeAnalysis.GetByEvent(c.Request.Context(), id)
	response.OK(c, gin.H{"event": event, "analysis": analysis})
}

// UpdateLogEventStatus 更新事件状态（L1）。
func (h *Handler) UpdateLogEventStatus(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req struct {
		Status string `json:"status" binding:"required"`
	}
	if !bindJSON(c, &req) {
		return
	}
	if err := h.deps.LogAlert.UpdateStatus(c.Request.Context(), id, req.Status, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "状态已更新"})
}

// IngestLogHook 接收应用日志上报（HTTP Hook，零侵入接入）。
//
// 鉴权：与用户 JWT 分离，使用独立的 Hook 令牌（路由层校验）。
func (h *Handler) IngestLogHook(c *gin.Context) {
	var in service.LogReport
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.LogAlert.Ingest(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// AnalyzeCode 触发 AI 代码分析（一期：第三方 API + 本地检索兜底）。
func (h *Handler) AnalyzeCode(c *gin.Context) {
	var in service.CodeAnalysisRequest
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.CodeAnalysis.Analyze(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ListCodeAnalyses 代码分析报告列表。
func (h *Handler) ListCodeAnalyses(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.CodeAnalysis.List(c.Request.Context(), c.Query("service"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// ListServers 服务器列表。
func (h *Handler) ListServers(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.LogAlert.ListServers(c.Request.Context(), c.Query("keyword"), c.Query("environment"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// CreateServer 新增服务器。
func (h *Handler) CreateServer(c *gin.Context) {
	var in service.ServerInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.LogAlert.CreateServer(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateServer 更新服务器。
func (h *Handler) UpdateServer(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.ServerInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.LogAlert.UpdateServer(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// DeleteServer 删除服务器。
func (h *Handler) DeleteServer(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.LogAlert.DeleteServer(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "已删除"})
}

// ListCodeRepos 代码仓库映射列表。
func (h *Handler) ListCodeRepos(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.LogAlert.ListCodeRepos(c.Request.Context(), c.Query("keyword"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// SaveCodeRepo 新增或更新仓库映射（出网白名单开关，6.5）。
func (h *Handler) SaveCodeRepo(c *gin.Context) {
	var in service.CodeRepoInput
	if !bindJSON(c, &in) {
		return
	}
	id := queryInt64(c, "id")
	item, err := h.deps.LogAlert.UpsertCodeRepo(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}
