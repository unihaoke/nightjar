package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// AISettings 读取 AI 设置（管理员）。
//
// 响应里**没有明文密钥**：只有 api_key_set 与 api_key_masked。
func (h *Handler) AISettings(c *gin.Context) {
	data, err := h.deps.Settings.AISettings(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// SaveAISettings 保存 AI 设置并立即生效（加密落库 → 重建引擎 → 热更新护栏配额）。
func (h *Handler) SaveAISettings(c *gin.Context) {
	var in service.AISettingsInput
	if !bindJSON(c, &in) {
		return
	}
	data, err := h.deps.Settings.SaveAISettings(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// AIUsage 返回 token 消费与剩余额度（days 默认 30，上限 90）。
func (h *Handler) AIUsage(c *gin.Context) {
	data, err := h.deps.Settings.AIUsage(c.Request.Context(), atoiDefault(c.Query("days"), 0))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// TestAISettings 用当前生效配置做一次连通性自检。
//
// 失败返回 HTTP 200 + ok=false + message（原因），而不是 500：
// 自检的目的就是把原因原样展示给配置人，500 会被前端统一处理成"服务异常"。
func (h *Handler) TestAISettings(c *gin.Context) {
	ok, engineName, message, latencyMs, err := h.deps.Settings.TestAI(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"ok": ok, "engine": engineName, "message": message, "latency_ms": latencyMs,
	})
}

// NotifySettings 读取通知渠道设置（webhook 只回掩码，secret/口令只回 bool）。
func (h *Handler) NotifySettings(c *gin.Context) {
	data, err := h.deps.Settings.NotifySettings(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// SaveNotifySettings 保存通知渠道设置并立即生效（NotifierService 持有配置指针）。
func (h *Handler) SaveNotifySettings(c *gin.Context) {
	var in service.NotifySettingsInput
	if !bindJSON(c, &in) {
		return
	}
	data, err := h.deps.Settings.SaveNotifySettings(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, data)
}

// notifyTestInput 是渠道自检的请求体（也兼容 ?channel= 查询参数）。
type notifyTestInput struct {
	Channel string `json:"channel"`
}

// TestNotifySettings 给指定渠道发一条测试消息。
//
// 发送前先预检渠道是否具备发送条件：NotifierService 的发送链路会吞掉渠道错误
// （告警通知不能因为一个渠道失败而中断），若不做预检，"渠道没配 webhook"也会显示成功。
func (h *Handler) TestNotifySettings(c *gin.Context) {
	var in notifyTestInput
	if c.Request.ContentLength > 0 {
		if !bindJSON(c, &in) {
			return
		}
	}
	channel := in.Channel
	if channel == "" {
		channel = c.Query("channel")
	}
	switch channel {
	case "feishu", "wecom", "dingtalk", "email":
	default:
		response.Fail(c, apperr.Newf(apperr.CodeInvalidParam,
			"渠道 %q 非法（可选 feishu/wecom/dingtalk/email）", channel))
		return
	}
	if err := h.deps.Notifier.ReadyForTest(channel); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInvalidParam, err))
		return
	}
	if err := h.deps.Notifier.SendTest(c.Request.Context(), channel); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	response.OK(c, gin.H{"ok": true, "channel": channel, "message": "已发送"})
}
