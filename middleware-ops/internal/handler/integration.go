package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// IntegrationOverview 返回集成中心概览（组件模板 + 已集成数量）。
func (h *Handler) IntegrationOverview(c *gin.Context) {
	overview, err := h.deps.Integration.Overview(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, overview)
}

// ListIntegrations 集成列表。
func (h *Handler) ListIntegrations(c *gin.Context) {
	items, err := h.deps.Integration.List(c.Request.Context(), h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// GetIntegration 集成详情。
func (h *Handler) GetIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Integration.Get(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// IntegrationServiceDiscovery 暴露 Prometheus http_sd_configs 需要的目标文档。
//
// 这是一个**公开**接口（注册在鉴权组之外）：Prometheus 无法携带用户 JWT。
// 文档只包含被管实例的地址与标签，不含账号口令；需要收紧时设置
// integration.sd_token，并通过 ?token= 或 X-SD-Token 传入。
func (h *Handler) IntegrationServiceDiscovery(c *gin.Context) {
	if !h.deps.Config.Integration.Enabled {
		response.Fail(c, apperr.New(apperr.CodeNotFound, "集成中心未启用"))
		return
	}
	expected := h.deps.Integration.SDToken()
	if expected != "" {
		provided := c.GetHeader("X-SD-Token")
		if provided == "" {
			provided = c.Query("token")
		}
		if provided != expected {
			response.Fail(c, apperr.New(apperr.CodeUnauthorized, "服务发现令牌无效"))
			return
		}
	}
	body, err := h.deps.Integration.ServiceDiscovery(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(http.StatusOK, body)
}

// PreviewIntegration 预览生成的 Exporter 配置与抓取配置（不落库、不部署）。
func (h *Handler) PreviewIntegration(c *gin.Context) {
	var in service.IntegrationInput
	if !bindJSON(c, &in) {
		return
	}
	artifacts, err := h.deps.Integration.Preview(in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, artifacts)
}

// CreateIntegration 新建集成（L1：落库 + 重写 file_sd + 可选拉起 Exporter）。
func (h *Handler) CreateIntegration(c *gin.Context) {
	var in service.IntegrationInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Integration.Create(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateIntegration 更新集成（L1）。
func (h *Handler) UpdateIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.IntegrationInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Integration.Update(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// ApplyIntegration 重新应用集成（重写 file_sd + 重建 Exporter 容器）。
func (h *Handler) ApplyIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Integration.Apply(c.Request.Context(), id, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// DeleteIntegration 删除集成（L1）。
func (h *Handler) DeleteIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.Integration.Delete(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "集成已删除"})
}
