package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
	"middleware-ops/internal/service"
)

// ListKnowledge 知识库列表。
func (h *Handler) ListKnowledge(c *gin.Context) {
	pageNo, pageSize, offset := page(c)
	filter := repository.KnowledgeFilter{
		Keyword: c.Query("keyword"),
		MWType:  c.Query("mw_type"),
		Status:  c.Query("status"),
		Source:  c.Query("source"),
		Tag:     c.Query("tag"),
	}
	if c.Query("only_published") == "true" {
		filter.OnlyPub = true
	}
	items, total, err := h.deps.KnowledgeSvc.List(c.Request.Context(), filter, pageSize, offset)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OKPage(c, items, total, pageNo, pageSize)
}

// GetKnowledge 知识条目详情。
func (h *Handler) GetKnowledge(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	item, err := h.deps.KnowledgeSvc.Get(c.Request.Context(), id)
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// CreateKnowledge 新增知识条目。
func (h *Handler) CreateKnowledge(c *gin.Context) {
	var in service.KnowledgeInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.KnowledgeSvc.Create(c.Request.Context(), in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// UpdateKnowledge 更新知识条目（草稿转正）。
func (h *Handler) UpdateKnowledge(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var in service.KnowledgeUpdateInput
	if !bindJSON(c, &in) {
		return
	}
	item, err := h.deps.KnowledgeSvc.Update(c.Request.Context(), id, in, h.operator(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, item)
}

// AdoptKnowledge 采纳知识条目（质量闭环）。
func (h *Handler) AdoptKnowledge(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.KnowledgeSvc.Adopt(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "已采纳，该条目权重已提升"})
}

// DeleteKnowledge 删除知识条目（L1）。
func (h *Handler) DeleteKnowledge(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.deps.KnowledgeSvc.Delete(c.Request.Context(), id, h.operator(c)); err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"message": "已删除"})
}

// KnowledgeStats 知识库统计。
func (h *Handler) KnowledgeStats(c *gin.Context) {
	stats, err := h.deps.KnowledgeSvc.Stats(c.Request.Context())
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, stats)
}

// KnowledgeOptions 返回知识库筛选选项。
func (h *Handler) KnowledgeOptions(c *gin.Context) {
	response.OK(c, gin.H{
		"statuses": knowledgeStatusOptions,
		"sources": []map[string]string{
			{"value": "manual", "label": "人工录入"},
			{"value": "auto", "label": "AI 诊断沉淀"},
		},
	})
}

// 知识条目状态常量透出（供前端筛选器）。
var knowledgeStatusOptions = []map[string]string{
	{"value": model.KnowledgeStatusDraft, "label": "草稿"},
	{"value": model.KnowledgeStatusPublished, "label": "已发布"},
	{"value": model.KnowledgeStatusDeprecated, "label": "已废弃"},
}
