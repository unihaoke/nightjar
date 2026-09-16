// Package response 提供统一响应封装。
package response

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
)

// Body 是统一响应结构（设计文档 8.1）。
type Body struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
	// Truncated 在上下文预算触发截断时告知用户被截断的维度（5.2）。
	Truncated []string `json:"truncated,omitempty"`
}

// Page 是统一分页结构。
type Page struct {
	List     any   `json:"list"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"page_size"`
}

// PageQuery 是分页查询参数（默认 20，上限 100）。
type PageQuery struct {
	Page     int `form:"page"`
	PageSize int `form:"page_size"`
}

// Normalize 归一化分页参数。
func (q *PageQuery) Normalize() (page, size, offset int) {
	page = q.Page
	if page < 1 {
		page = 1
	}
	size = q.PageSize
	if size < 1 {
		size = 20
	}
	if size > 100 {
		size = 100
	}
	return page, size, (page - 1) * size
}

// OK 返回成功响应。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Body{Code: 0, Message: "ok", Data: data})
}

// OKPage 返回分页成功响应。
func OKPage(c *gin.Context, list any, total int64, page, size int) {
	OK(c, Page{List: list, Total: total, Page: page, PageSize: size})
}

// Fail 按业务错误返回失败响应，并终止后续处理。
func Fail(c *gin.Context, err error) {
	appErr := apperr.From(err)
	c.AbortWithStatusJSON(appErr.HTTPStatus(), Body{
		Code:    int(appErr.Code),
		Message: appErr.Message,
	})
}
