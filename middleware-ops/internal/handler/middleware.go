package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// ListMiddlewares 中间件实例列表。
func (h *Handler) ListMiddlewares(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	items, total, err := h.deps.Middleware.List(c.Request.Context(),
		c.Query("keyword"), c.Query("mw_type"), c.Query("environment"), c.Query("group"),
		h.scope(c), pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// GetMiddleware 实例详情。
func (h *Handler) GetMiddleware(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Middleware.Get(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 附带该实例的规则与最近告警数量，便于详情页直接展示。
	rules, ruleTotal, _ := h.deps.AlertSvc.ListRules(c.Request.Context(), id, 100, 0)
	alerts, alertTotal, _ := h.deps.AlertSvc.List(c.Request.Context(),
		alertFilter(id, "", "", ""), 10, 0)
	response.OK(c, gin.H{
		"instance":        item,
		"rules":           rules,
		"rule_total":      ruleTotal,
		"alerts":          alerts,
		"alert_total":     alertTotal,
		"metrics_catalog": h.deps.Metrics.Catalog(item.MWType),
	})
}

// CreateMiddleware 新增实例。
func (h *Handler) CreateMiddleware(c *gin.Context) {
	var in service.MiddlewareInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Middleware.Create(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateMiddleware 更新实例。
func (h *Handler) UpdateMiddleware(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.MiddlewareInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Middleware.Update(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// DeleteMiddleware 删除实例。
//
// 级别：L1；prod 环境属 L2，需先提交审批（4.6 / 6.2）。
func (h *Handler) DeleteMiddleware(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	instance, err := h.deps.Middleware.Get(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	if instance.Environment == model.EnvProd {
		// 生产环境删除强制审批：转发到审批服务创建工单。
		ticket, ticketErr := h.deps.Approval.Create(c.Request.Context(), service.ApprovalRequest{
			InstanceID:   instance.ID,
			Environment:  instance.Environment,
			ActionType:   "middleware_delete",
			ActionDetail: map[string]any{"instance_id": instance.ID, "name": instance.Name},
			Reason:       c.Query("reason"),
		}, h.operator(c))
		if ticketErr != nil {
			response.Fail(c, ticketErr)
			return
		}
		response.OK(c, gin.H{
			"status":    "pending_approval",
			"ticket_id": ticket.TicketID,
			"message":   "生产环境删除属 L2 高危操作，已创建审批工单，审批通过后请人工执行删除",
		})
		return
	}
	if err := h.deps.Middleware.Delete(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "已删除"})
}

// TestMiddleware 连接测试。
func (h *Handler) TestMiddleware(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	result, err := h.deps.Middleware.TestConnection(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// TestMiddlewareEndpoint 对未保存的连接参数执行探测。
func (h *Handler) TestMiddlewareEndpoint(c *gin.Context) {
	var in service.MiddlewareInput
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.Middleware.TestEndpoint(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// HealthMiddleware 触发即时健康探测。
func (h *Handler) HealthMiddleware(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	result, err := h.deps.Middleware.HealthCheck(c.Request.Context(), id, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// MiddlewareOptions 返回纳管元数据（类型、默认端口、分组、环境）。
func (h *Handler) MiddlewareOptions(c *gin.Context) {
	groups, envs, err := h.deps.Instances.ListGroups(c.Request.Context())
	if err != nil {
		response.Fail(c, apperr.Wrap(apperr.CodeInternal, err))
		return
	}
	types := []map[string]any{
		{"value": model.MWTypeRedis, "label": "Redis", "port": service.DefaultPort(model.MWTypeRedis), "phase": 1},
		{"value": model.MWTypeKafka, "label": "Kafka", "port": service.DefaultPort(model.MWTypeKafka), "phase": 1},
		{"value": model.MWTypeMySQL, "label": "MySQL", "port": service.DefaultPort(model.MWTypeMySQL), "phase": 1},
		{"value": model.MWTypePG, "label": "PostgreSQL", "port": service.DefaultPort(model.MWTypePG), "phase": 1},
		{"value": model.MWTypeES, "label": "Elasticsearch", "port": service.DefaultPort(model.MWTypeES), "phase": 1},
		{"value": model.MWTypeNginx, "label": "Nginx", "port": service.DefaultPort(model.MWTypeNginx), "phase": 1},
		{"value": model.MWTypeRMQ, "label": "RabbitMQ（二期）", "port": service.DefaultPort(model.MWTypeRMQ), "phase": 2},
	}
	response.OK(c, gin.H{
		"types": types, "groups": groups, "environments": envs,
		// Prometheus 中实际存在的 job 名：表单直接给候选，避免把容器名当 job 名填。
		"prom_jobs": h.deps.Metrics.PrometheusJobs(c.Request.Context()),
		// instance_name 的实际取值（= 集成名称）：表单给候选，
		// 避免"实例名填成 Exporter 容器名"这类对不上标签的经典故障。
		"prom_instance_names": h.deps.Metrics.PrometheusInstanceNames(c.Request.Context()),
		"status_options": []map[string]any{
			{"value": 1, "label": "在线"}, {"value": 0, "label": "离线"},
		},
	})
}
