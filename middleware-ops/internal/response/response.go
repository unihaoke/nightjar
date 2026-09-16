// Package response 提供统一响应封装。
package response

import (
	"encoding/json"
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

// Encode 序列化统一响应体。
//
// 单列出来是为了让"响应体能否编码"成为可测试、可前置判断的一件事：
// gin 的 c.JSON 是**先写状态码、再 Marshal**，一旦数据里含 NaN/±Inf 这类
// 不可编码的值，客户端会收到「HTTP 200 + 空响应体」——前端解包得到 undefined，
// 最终报出与真实原因完全无关的错误（典型：Cannot read properties of undefined
// (reading 'series')）。这里改成先编码，失败就走标准错误响应。
func Encode(data any) ([]byte, error) {
	return json.Marshal(Body{Code: 0, Message: "ok", Data: data})
}

// OK 返回成功响应；数据不可编码时返回 500 并说明原因。
func OK(c *gin.Context, data any) {
	payload, err := Encode(data)
	if err != nil {
		Fail(c, apperr.Newf(apperr.CodeInternal,
			"响应序列化失败：%v（数据中可能包含 NaN/Inf 等不可编码的值）", err))
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", payload)
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
