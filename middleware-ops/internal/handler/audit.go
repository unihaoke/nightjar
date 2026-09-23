package handler

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
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
// 同步模式（ai_analysis.call_mode=sync）下响应里直接带 report；
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
	// 鉴权：AI 服务拿不到平台用户令牌，这里用独立的回调令牌（ai_analysis.callback_token）校验来源。
	// 优先 HMAC-SHA256 签名（Header 携带 X-Callback-Signature + X-Callback-Timestamp，防篡改防重放）；
	// 未携带签名时降级为明文令牌比对（X-Callback-Token / Authorization: Bearer / ?callback_token / ?token），向后兼容。
	// 令牌未配置时**拒绝所有回调**——宁可让结论走轮询兜底，也不能让任何人往平台里写结论。
	// 鉴权：AI 服务拿不到平台用户令牌，这里用独立的回调密钥校验来源。
	// 多密钥模式（配置了 callback_key_map）：必须携带 X-Callback-Key-Id 且命中对应 callbackSecret；
	// 单密钥模式：使用 callback_token 作为共享验签密钥（向后兼容）。
	// 密钥未配置时**拒绝所有回调**——宁可让结论走轮询兜底，也不能让任何人往平台里写结论。
	if h.deps.Config == nil {
		response.Fail(c, apperr.New(apperr.CodeUnauthorized, "服务未就绪：无法校验回调来源"))
		return
	}
	aiCfg := h.deps.Config.AIAnalysis
	keyID := strings.TrimSpace(c.GetHeader("X-Callback-Key-Id"))
	secret, ok := resolveCallbackSecret(aiCfg, keyID)
	if !ok {
		response.Fail(c, apperr.New(apperr.CodeUnauthorized,
			"回调密钥未配置或 X-Callback-Key-Id 未命中：无法校验回调来源（请在 AI 设置里配置 callback_token 或 callback_key_map）"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "读取回调内容失败"))
		return
	}

	// 优先走签名校验：签名消息 = timestamp + "." + body，密钥为 callback_token。
	sig := strings.TrimSpace(c.GetHeader("X-Callback-Signature"))
	if sig != "" {
		ts := strings.TrimSpace(c.GetHeader("X-Callback-Timestamp"))
		if !verifyCallbackSignature(secret, ts, sig, body) {
			response.Fail(c, apperr.New(apperr.CodeUnauthorized,
				"回调签名校验失败：请按 CodeAgent 规范携带 X-Callback-Timestamp(Unix秒)、X-Callback-Key-Id、X-Callback-Signature；"+
					"签名 = HMAC-SHA256(callbackSecret, timestamp + \".\" + rawBody)，值取 URL-safe Base64 无填充（可带 sha256= 前缀）"))
			return
		}
	} else {
		// 降级兼容：旧明文令牌校验（仅当未使用签名时生效）。
		got := strings.TrimSpace(c.GetHeader("X-Callback-Token"))
		if got == "" {
			got = strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		}
		if got == "" {
			got = strings.TrimSpace(c.Query("callback_token"))
		}
		if got == "" {
			got = strings.TrimSpace(c.Query("token"))
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(secret)) != 1 {
			response.Fail(c, apperr.New(apperr.CodeUnauthorized, "回调令牌无效或未配置（ai_analysis.callback_token）"))
			return
		}
	}
	// 回调报文的形状由对接协议决定：开放接口用 state + summary + rootCause，
	// 与 generic 的 task_id + answer 不是同一套字段，必须按协议解析。
	protocol := ""
	if h.deps.Config != nil {
		protocol = h.deps.Config.AIAnalysis.Protocol
	}
	taskID, status, answer, errMsg, err := service.ParseCallbackWithProtocol(body, protocol)
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

// callbackSignatureMaxAge 是回调签名时间戳允许的最大偏差（防重放窗口），对齐 CodeAgent 文档（§2.4）的 300 秒。
const callbackSignatureMaxAge = 5 * time.Minute

// resolveCallbackSecret 按 X-Callback-Key-Id 选取验签密钥（多密钥支持，对齐文档 §2.4）。
//
// 配置了 callback_key_map 时进入多密钥模式：回调必须携带匹配的 key-id，才能取到对应 callbackSecret；
// 否则回落到单密钥模式，使用 callback_token（向后兼容旧的单一接入密钥场景）。
// 返回 (secret, true) 表示取到可用密钥；("", false) 表示密钥未配置或 key-id 未命中（应拒绝回调）。
func resolveCallbackSecret(cfg config.AIAnalysisConfig, keyID string) (string, bool) {
	if len(cfg.CallbackKeyMap) > 0 {
		keyID = strings.TrimSpace(keyID)
		if keyID == "" {
			return "", false
		}
		s, ok := cfg.CallbackKeyMap[keyID]
		if !ok || strings.TrimSpace(s) == "" {
			return "", false
		}
		return strings.TrimSpace(s), true
	}
	t := strings.TrimSpace(cfg.CallbackToken)
	if t == "" {
		return "", false
	}
	return t, true
}

// verifyCallbackSignature 校验 AI 服务回调的 HMAC-SHA256 签名。
//
// 契约：签名消息 = timestamp + "." + body，密钥为 callback_token；
// 签名值放在 X-Callback-Signature（可选 "sha256=" 前缀，支持 hex / base64）。
// X-Callback-Timestamp（Unix 秒）必须存在且其值与当前时间差在 callbackSignatureMaxAge 内，否则视为重放或过期。
func verifyCallbackSignature(secret, tsHeader, sigHeader string, body []byte) bool {
	ts, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil {
		return false
	}
	if age := time.Now().Unix() - ts; age > int64(callbackSignatureMaxAge.Seconds()) || age < -int64(callbackSignatureMaxAge.Seconds()) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	sum := mac.Sum(nil)
	// 优先按 CodeAgent 文档（§2.3）规范比较：URL-safe Base64 无填充（RFC 4648 §5）。
	provided := strings.TrimSpace(sigHeader)
	if strings.HasPrefix(strings.ToLower(provided), "sha256=") {
		provided = provided[len("sha256="):]
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(base64.RawURLEncoding.EncodeToString(sum))) == 1 {
		return true
	}
	// 向后兼容：标准 Base64（带填充）、标准无填充、hex（大小写不敏感）。
	if subtle.ConstantTimeCompare([]byte(provided), []byte(base64.StdEncoding.EncodeToString(sum))) == 1 {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(provided), []byte(base64.RawStdEncoding.EncodeToString(sum))) == 1 {
		return true
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(provided)), []byte(strings.ToLower(hex.EncodeToString(sum)))) == 1
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


