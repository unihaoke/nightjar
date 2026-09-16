package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// DiagnoseRequest 是 AI 诊断入参。
type DiagnoseRequest struct {
	InstanceID int64  `json:"instance_id"`
	Question   string `json:"question" binding:"required,min=2,max=1000"`
	MWType     string `json:"mw_type"`
	AlertID    int64  `json:"alert_id"`
	SkipCache  bool   `json:"skip_cache"`
}

// DiagnoseSync AI 诊断（同步）。
func (h *Handler) DiagnoseSync(c *gin.Context) {
	var req DiagnoseRequest
	if !bindJSON(c, &req) {
		return
	}
	resp, err := h.deps.Diagnoser.Diagnose(c.Request.Context(), service.Request{
		InstanceID: req.InstanceID, Question: req.Question, MWType: req.MWType,
		AlertID: req.AlertID, SkipCache: req.SkipCache,
	}, h.session(c), h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, resp)
}

// Diagnose AI 诊断（SSE 流式）。
//
// 事件类型（8.1）：meta / data / done / error。
func (h *Handler) Diagnose(c *gin.Context) {
	var req DiagnoseRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, apperr.Wrapf(apperr.CodeInvalidBody, err, "请求体解析失败: %v", err))
		return
	}
	session := h.session(c)
	if session == nil || session.User == nil {
		response.Fail(c, apperr.New(apperr.CodeUnauthorized, ""))
		return
	}

	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(200)

	flusher, _ := c.Writer.(gin.ResponseWriter)
	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	h.deps.Diagnoser.Stream(
		c.Request.Context(),
		service.Request{
			InstanceID: req.InstanceID, Question: req.Question, MWType: req.MWType,
			AlertID: req.AlertID, SkipCache: req.SkipCache,
		},
		session,
		h.operator(c),
		func(meta service.Meta) { send("meta", meta) },
		func(delta string) { send("data", gin.H{"delta": delta}) },
		func(resp *service.Response) { send("done", resp) },
		func(err error) {
			appErr := apperr.From(err)
			send("error", gin.H{"code": int(appErr.Code), "message": appErr.Message})
		},
	)
}

// DiagnosisHistory 诊断历史列表。
func (h *Handler) DiagnosisHistory(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	session := h.session(c)
	filter := repository.DiagnosisFilter{
		InstanceID: queryInt64(c, "instance_id"),
		MWType:     c.Query("mw_type"),
		Feedback:   c.Query("feedback"),
		Keyword:    c.Query("keyword"),
		From:       queryTime(c, "from"),
		To:         queryTime(c, "to"),
	}
	// 只读角色与开发角色仅可见自己的诊断历史（数据权限的最小可见集）。
	if session != nil && session.User != nil && !session.Has(service.PermAlertWrite) {
		filter.UserID = session.User.ID
	}
	if mine := c.Query("mine"); mine == "true" && session != nil && session.User != nil {
		filter.UserID = session.User.ID
	}
	items, total, err := h.deps.Diagnoses.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	// 列表不返回大字段（原始输出与上下文），避免响应过大。
	for i := range items {
		items[i].DiagnosisResult = ""
		items[i].CollectedMetrics = nil
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// DiagnosisDetail 诊断详情。
func (h *Handler) DiagnosisDetail(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Diagnoses.Get(c.Request.Context(), id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			response.Fail(c, apperr.New(apperr.CodeNotFound, "诊断记录不存在"))
			return
		}
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	session := h.session(c)
	if session != nil && session.User != nil && item.UserID != session.User.ID && !session.Has(service.PermAlertWrite) {
		response.Fail(c, apperr.New(apperr.CodeScopeDenied, "只能查看自己的诊断记录"))
		return
	}
	// 关联的知识草稿便于用户采纳（诊断沉淀的 auto 条目）。
	var entries []model.KnowledgeBase
	if list, _, listErr := h.deps.Knowledge.List(c.Request.Context(),
		repository.KnowledgeFilter{Source: "auto"}, 50, 0); listErr == nil {
		for _, entry := range list {
			if entry.DiagnosisID == id {
				entries = append(entries, entry)
			}
		}
	}
	response.OK(c, gin.H{"diagnosis": item, "knowledge_entries": entries})
}

// FeedbackRequest 是诊断反馈入参。
type FeedbackRequest struct {
	DiagnosisID int64  `json:"diagnosis_id" binding:"required"`
	Feedback    string `json:"feedback" binding:"required"`
}

// DiagnosisFeedback 提交诊断反馈（useful / useless / adopted）。
func (h *Handler) DiagnosisFeedback(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req FeedbackRequest
	if !bindJSON(c, &req) {
		return
	}
	if err := h.deps.KnowledgeSvc.Feedback(c.Request.Context(), id, req.Feedback, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "感谢反馈，已计入质量基线"})
}

// AIQuality 返回 AI 质量与成本指标（质量护栏观测面板）。
func (h *Handler) AIQuality(c *gin.Context) {
	since := time.Now().UTC().Add(-7 * 24 * time.Hour)
	if custom := queryTime(c, "from"); custom != nil {
		since = *custom
	}
	stats, err := h.deps.KnowledgeSvc.QualityStats(c.Request.Context(), since)
	if err != nil {
		response.Fail(c, err)
		return
	}
	guard := map[string]any{
		"input_token_budget":  h.deps.Guardrails.InputTokenBudget,
		"output_token_budget": h.deps.Guardrails.OutputTokenBudget,
		"max_concurrency":     h.deps.Guardrails.MaxConcurrency,
		"cache_ttl_hours":     h.deps.Guardrails.CacheTTL.Hours(),
		"daily_token_quota":   h.deps.Guardrails.DailyTokenQuota,
		"per_user_quota":      h.deps.Guardrails.PerUserDailyTokenQuota,
		"vector_threshold":    h.deps.Guardrails.VectorReferenceThreshold,
		"log_context_lines":   h.deps.Guardrails.LogContextLines,
		"max_log_sources":     h.deps.Guardrails.MaxLogSources,
		"sql_default_limit":   h.deps.Guardrails.SQLDefaultLimit,
		"sql_max_limit":       h.deps.Guardrails.SQLMaxLimit,
		"max_steps":           h.deps.Guardrails.MaxSteps,
		"loop_threshold":      h.deps.Guardrails.LoopRepeatThreshold,
	}
	cost := map[string]any{}
	if session := h.session(c); session != nil && session.User != nil {
		cost = h.deps.Cost.Snapshot(session.User.ID)
	}
	response.OK(c, gin.H{
		"quality":       stats,
		"guardrail":     guard,
		"cost":          cost,
		"engine":        service.WatchEngineStatus(h.deps.Engine),
		"tools":         h.deps.Registry.Names(),
		"eval_set_size": len(guardrail.DefaultEvalSet()),
	})
}

// StreamLogs 实时日志流（WebSocket/SSE 预留实现）。
//
// 一期以轮询日志告警列表为主，此处提供 SSE 心跳占位，便于前端统一接入。
func (h *Handler) StreamLogs(c *gin.Context) {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	flusher, ok := c.Writer.(interface{ Flush() })
	if !ok {
		response.Fail(c, apperr.New(apperr.CodeInternal, "当前服务器不支持流式响应"))
		return
	}
	c.Writer.WriteHeader(200)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case <-ticker.C:
			if _, err := io.WriteString(c.Writer, "event: ping\ndata: {}\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
