package handler

import (
	"crypto/subtle"
	"io"
	"strings"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
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

// AnalyzeCode 提交一次 AI 分析（异步：返回 task_id，结论由回调/轮询带回）。
//
// 同步模式（ai_analysis.sync_mode）下响应里直接带 report；
// 否则只返回 task_id，页面按事件的分析报告查询结论。
func (h *Handler) AnalyzeCode(c *gin.Context) {
	var in service.CodeAnalysisRequest
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.CodeAnalysis.Submit(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// AIAnalysisCallback 接收外部 AI 分析服务回传的结论（异步模型的第二半）。
//
// 鉴权：AI 服务拿不到平台的用户令牌，这里用独立的回调令牌（ai_analysis.callback_token）；
// 令牌未配置时**拒绝所有回调**——宁可让结论走轮询兜底，也不能让任何人往平台里写结论。
//
// 幂等：AI 服务没收到 2xx 会重试，CompleteTask 内部按"当前状态必须是 submitted"更新，
// 重复回调不会写出两份报告。
func (h *Handler) AIAnalysisCallback(c *gin.Context) {
	want := ""
	if h.deps.Config != nil {
		want = strings.TrimSpace(h.deps.Config.AIAnalysis.CallbackToken)
	}
	got := strings.TrimSpace(c.GetHeader("X-Callback-Token"))
	if got == "" {
		got = strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	}
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		response.Fail(c, apperr.New(apperr.CodeUnauthorized, "回调令牌无效或未配置（ai_analysis.callback_token）"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "读取回调内容失败"))
		return
	}
	taskID, status, answer, errMsg, err := service.ParseCallback(body)
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, err.Error()))
		return
	}
	if err := h.deps.LogAlertWorker.CompleteTask(c.Request.Context(), taskID, status, answer, errMsg); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "ok", "task_id": taskID})
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


