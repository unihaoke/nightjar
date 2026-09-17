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

// PreviewLogCollect 预览日志接入：平台读取被管容器的 docker 配置，反查日志位置。
//
// **读不到位置会直接失败**（不猜路径），这是刻意的：猜错的后果是采集容器空转、
// 日志页永远为空，比直接报错难排查得多。
func (h *Handler) PreviewLogCollect(c *gin.Context) {
	var in service.LogCollectInput
	if !bindJSON(c, &in) {
		return
	}
	plan, err := h.deps.Integration.PreviewLogCollect(c.Request.Context(), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, plan)
}

// CreateLogCollect 创建日志采集容器（平台侧，被管项目零改动）。
func (h *Handler) CreateLogCollect(c *gin.Context) {
	var in service.LogCollectInput
	if !bindJSON(c, &in) {
		return
	}
	plan, err := h.deps.Integration.CreateLogCollect(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, plan)
}

// PreviewIntegration 预览生成的 Exporter 配置与抓取配置（不落库、不部署）。
func (h *Handler) PreviewIntegration(c *gin.Context) {
	var in service.IntegrationInput
	if !bindJSON(c, &in) {
		return
	}
	artifacts, err := h.deps.Integration.Preview(c.Request.Context(), in)
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
//
// 请求体可选：远程部署需要 SSH 凭据（ssh_user/ssh_password 或 ssh_key），凭据不落库。
func (h *Handler) ApplyIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.SSHCredsInput
	// 允许空体（本机部署的老前端仍可无体调用）。
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&in); err != nil {
			response.Fail(c, apperr.New(apperr.CodeInvalidParam, "请求体不是合法的 JSON"))
			return
		}
	}
	item, err := h.deps.Integration.Apply(c.Request.Context(), id, h.operator(c), in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// VerifyIntegration 按 Prometheus 现状重新核验（无副作用：不重装、不需要凭据）。
//
// 用于"外部原因已经修好，但状态还停在待处理"：核验只在部署后跑几次，
// 之后不会自己再核对；这个接口让使用者（以及前端按钮）随时刷新状态。
func (h *Handler) VerifyIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.Integration.VerifyNow(c.Request.Context(), id, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// SelfCheckIntegration 端到端自检：按环节给出"哪一环断了 + 下一步做什么"。
//
// 只读、不需要凭据、不重装：回答"Exporter 还在不在、Prometheus 抓没抓到、指标是否真的有数据"。
func (h *Handler) SelfCheckIntegration(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	result, err := h.deps.Integration.SelfCheck(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ListIntegrationAccounts 汇总各集成的只读监控账号现状（平台代管与否、能否轮换）。
func (h *Handler) ListIntegrationAccounts(c *gin.Context) {
	items, err := h.deps.Integration.ListAccounts(c.Request.Context(), h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"items": items})
}

// RotateIntegrationAccount 轮换平台托管的监控账号口令。
//
// 不需要管理凭据：账号可以改自己的口令（MySQL ALTER USER USER() /
// PostgreSQL ALTER ROLE CURRENT_USER），平台持有该账号口令即可自助完成。
func (h *Handler) RotateIntegrationAccount(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.RotateAccountInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Integration.RotateAccountPassword(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// RetryIntegrationAccount 重试「建号 + 连接」：失败后不必重填整个表单。
//
// 带管理凭据 → 幂等重跑建号 SQL；不带 → 只测连接并重建 Exporter。
// 结果里同时给出 created / connected / message，便于前端直接展示"还差什么"。
func (h *Handler) RetryIntegrationAccount(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.RetryAccountInput
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.Integration.RetryAccount(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// ProbeIntegrationAccount 只做连接测试（不建号、不改配置）。
func (h *Handler) ProbeIntegrationAccount(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.AccountProbeInput
	if !bindJSON(c, &in) {
		return
	}
	result, err := h.deps.Integration.ProbeAccount(c.Request.Context(), id, in)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// DropIntegrationAccount 删除平台创建的监控账号（L2：破坏性写操作，需管理凭据）。
func (h *Handler) DropIntegrationAccount(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.DropAccountInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.Integration.DropAccount(c.Request.Context(), id, in, h.operator(c))
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
