package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// ListLogAlertRules 分页检索日志告警规则。
//
// 规则决定"多久打扰人一次"（去重窗口 / 冷却期 / 通知渠道 / 是否 AI 分析），
// 因此读写都要权限点，改动会写审计（见 service.recordRule）。
func (h *Handler) ListLogAlertRules(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.LogAlert.ListRules(c.Request.Context(), c.Query("keyword"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// LogAlertRuleDefaults 返回"没有命中任何规则时平台用的默认值"。
func (h *Handler) LogAlertRuleDefaults(c *gin.Context) {
	response.OK(c, h.deps.LogAlert.RuleDefaults(c.Request.Context()))
}

// CreateLogAlertRule 新建规则。
func (h *Handler) CreateLogAlertRule(c *gin.Context) {
	var in service.LogAlertRuleInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.LogAlert.CreateRule(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateLogAlertRule 更新规则。
func (h *Handler) UpdateLogAlertRule(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.LogAlertRuleInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.LogAlert.UpdateRule(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// DeleteLogAlertRule 删除规则。
func (h *Handler) DeleteLogAlertRule(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.LogAlert.DeleteRule(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "规则已删除"})
}

// ReanalyzeLogEvent 把一条日志事件重新放回后处理队列（页面上的「重新分析」）。
//
// 典型用法：冷却期把某条事件标成了"抑制"，但使用者认为这次必须立刻看结论——
// 手动点一下即可打破冷却（冷却记录会被清掉，AI 分析立即执行）。
func (h *Handler) ReanalyzeLogEvent(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.LogAlert.RequeueAnalysis(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{
		"ok":      true,
		"message": "已重新入队：通知与分析会在数秒内执行（可在列表中查看状态）",
	})
}
