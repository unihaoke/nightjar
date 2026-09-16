package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// ListAlerts 告警历史列表。
func (h *Handler) ListAlerts(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	filter := repository.AlertFilter{
		InstanceID: queryInt64(c, "instance_id"),
		MWType:     c.Query("mw_type"),
		Level:      c.Query("level"),
		Status:     c.Query("status"),
		ClusterID:  c.Query("cluster_id"),
		From:       queryTime(c, "from"),
		To:         queryTime(c, "to"),
	}
	items, total, err := h.deps.AlertSvc.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// AckAlert 确认告警（L1）。
func (h *Handler) AckAlert(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.AlertSvc.Ack(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "告警已确认"})
}

// ResolveAlert 标记告警已恢复（L1）。
func (h *Handler) ResolveAlert(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.AlertSvc.Resolve(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "告警已标记为已恢复"})
}

// ListAlertRules 告警规则列表。
func (h *Handler) ListAlertRules(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.AlertSvc.ListRules(c.Request.Context(), queryInt64(c, "instance_id"), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// CreateAlertRule 创建告警规则（L1）。
func (h *Handler) CreateAlertRule(c *gin.Context) {
	var in service.RuleInput
	if !bindJSON(c, &in) {
		return
	}
	rule, err := h.deps.AlertSvc.CreateRule(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rule)
}

// UpdateAlertRule 更新告警规则（L1）。
func (h *Handler) UpdateAlertRule(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.RuleInput
	if !bindJSON(c, &in) {
		return
	}
	rule, err := h.deps.AlertSvc.UpdateRule(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, rule)
}

// DeleteAlertRule 删除告警规则（L1）。
func (h *Handler) DeleteAlertRule(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.AlertSvc.DeleteRule(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "规则已删除"})
}

// ClusterAlerts 触发告警语义聚类（离线能力，L1）。
func (h *Handler) ClusterAlerts(c *gin.Context) {
	threshold := parseFloatDefault(c.Query("threshold"), h.deps.Guardrails.VectorReferenceThreshold)
	result, err := h.deps.AlertSvc.Cluster(c.Request.Context(), threshold)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// EvaluateAlerts 手动触发一轮规则评估（运维自检）。
func (h *Handler) EvaluateAlerts(c *gin.Context) {
	result, err := h.deps.AlertSvc.Evaluate(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// IngestAlert 接收外部告警推送（Prometheus Alertmanager / 自定义 Hook）。
//
// 该接口使用服务令牌鉴权（与用户 JWT 分离），由路由层单独挂载。
func (h *Handler) IngestAlert(c *gin.Context) {
	var in service.IngestAlertInput
	if !bindJSON(c, &in) {
		return
	}
	alert, err := h.deps.AlertSvc.Ingest(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, alert)
}

// AlertOptions 返回告警规则配置所需的选项（实例/指标/操作符）。
func (h *Handler) AlertOptions(c *gin.Context) {
	items, _, err := h.deps.Middleware.List(c.Request.Context(), "", "", "", "", h.scope(c), 200, 0)
	if err != nil {
		response.Fail(c, err)
		return
	}
	instances := make([]map[string]any, 0, len(items))
	for _, item := range items {
		instances = append(instances, map[string]any{
			"id": item.ID, "name": item.Name, "mw_type": item.MWType, "environment": item.Environment,
		})
	}
	response.OK(c, gin.H{
		"instances":     instances,
		"operators":     []string{">", ">=", "<", "<=", "==", "!="},
		"levels":        []string{"warning", "critical"},
		"channels":      []string{"feishu", "wecom", "dingtalk", "email"},
		"notify_status": h.deps.Notifier.ChannelStatus(),
	})
}

// NotifyTest 发送渠道自检消息。
func (h *Handler) NotifyTest(c *gin.Context) {
	channel := c.Query("channel")
	if channel == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "必须指定 channel"))
		return
	}
	if err := h.deps.Notifier.SendTest(c.Request.Context(), channel); err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	response.OK(c, gin.H{"message": "测试消息已发送，请检查渠道"})
}
