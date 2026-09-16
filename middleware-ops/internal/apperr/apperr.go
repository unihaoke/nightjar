// Package apperr 定义统一错误码与响应封装（设计文档 8.1）。
//
// 错误码分段：
//   - 400x 参数
//   - 401x 认证
//   - 403x 权限
//   - 404x 不存在
//   - 500x 系统
package apperr

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 是业务错误码。
type Code int

// 参数类错误（400x）。
const (
	CodeInvalidParam Code = 4000
	CodeInvalidBody  Code = 4001
	CodeInvalidQuery Code = 4002
	CodeUnsafeSQL    Code = 4003
)

// 认证类错误（401x）。
const (
	CodeUnauthorized    Code = 4010
	CodeTokenExpired    Code = 4011
	CodeTokenInvalid    Code = 4012
	CodeWrongCredential Code = 4013
	CodeUserDisabled    Code = 4014
)

// 权限类错误（403x）。
const (
	CodeForbidden        Code = 4030
	CodeScopeDenied      Code = 4031
	CodeLevelDenied      Code = 4032
	CodeApprovalRequired Code = 4033
	CodeOutboundDenied   Code = 4034
	CodeQuotaExceeded    Code = 4035
)

// 不存在类错误（404x）。
const (
	CodeNotFound Code = 4040
)

// 系统类错误（500x）。
const (
	CodeInternal     Code = 5000
	CodeUpstream     Code = 5001
	CodeEngineFailed Code = 5002
	CodeTimeout      Code = 5003
	CodeQueueFull    Code = 5004
)

var httpStatus = map[Code]int{
	CodeInvalidParam:     http.StatusBadRequest,
	CodeInvalidBody:      http.StatusBadRequest,
	CodeInvalidQuery:     http.StatusBadRequest,
	CodeUnsafeSQL:        http.StatusBadRequest,
	CodeUnauthorized:     http.StatusUnauthorized,
	CodeTokenExpired:     http.StatusUnauthorized,
	CodeTokenInvalid:     http.StatusUnauthorized,
	CodeWrongCredential:  http.StatusUnauthorized,
	CodeUserDisabled:     http.StatusForbidden,
	CodeForbidden:        http.StatusForbidden,
	CodeScopeDenied:      http.StatusForbidden,
	CodeLevelDenied:      http.StatusForbidden,
	CodeApprovalRequired: http.StatusForbidden,
	CodeOutboundDenied:   http.StatusForbidden,
	CodeQuotaExceeded:    http.StatusForbidden,
	CodeNotFound:         http.StatusNotFound,
	CodeInternal:         http.StatusInternalServerError,
	CodeUpstream:         http.StatusBadGateway,
	CodeEngineFailed:     http.StatusServiceUnavailable,
	CodeTimeout:          http.StatusGatewayTimeout,
	CodeQueueFull:        http.StatusServiceUnavailable,
}

var defaultMessage = map[Code]string{
	CodeInvalidParam:     "参数不合法",
	CodeInvalidBody:      "请求体解析失败",
	CodeInvalidQuery:     "查询条件不合法",
	CodeUnsafeSQL:        "SQL 未通过只读安全校验",
	CodeUnauthorized:     "未认证或登录已过期",
	CodeTokenExpired:     "登录已过期，请重新登录",
	CodeTokenInvalid:     "认证信息无效",
	CodeWrongCredential:  "用户名或密码错误",
	CodeUserDisabled:     "账号已禁用",
	CodeForbidden:        "无操作权限",
	CodeScopeDenied:      "超出数据权限范围（环境/分组）",
	CodeLevelDenied:      "该操作级别不允许直接执行",
	CodeApprovalRequired: "该操作需要审批",
	CodeOutboundDenied:   "出网白名单未开启，禁止第三方分析",
	CodeQuotaExceeded:    "Token 预算已用尽",
	CodeNotFound:         "资源不存在",
	CodeInternal:         "服务内部错误",
	CodeUpstream:         "上游依赖异常",
	CodeEngineFailed:     "AI 引擎不可用",
	CodeTimeout:          "请求超时",
	CodeQueueFull:        "任务队列已满，请稍后重试",
}

// Error 是带错误码的业务错误。
type Error struct {
	Code    Code
	Message string
	// Cause 保留底层错误，仅用于日志，不返回给前端。
	Cause error
}

// Error 实现 error 接口。
func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("[%d] %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// Unwrap 支持 errors.Is / errors.As 链式判断。
func (e *Error) Unwrap() error { return e.Cause }

// HTTPStatus 返回对应的 HTTP 状态码。
func (e *Error) HTTPStatus() int {
	if status, ok := httpStatus[e.Code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// New 构造业务错误。
func New(code Code, message string) *Error {
	if message == "" {
		message = defaultMessage[code]
	}
	return &Error{Code: code, Message: message}
}

// Newf 构造带格式化消息的业务错误。
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 包装底层错误。
func Wrap(code Code, cause error) *Error {
	return &Error{Code: code, Message: defaultMessage[code], Cause: cause}
}

// Wrapf 包装底层错误并自定义消息。
func Wrapf(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Cause: cause}
}

// From 把任意 error 规范化为 *Error。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr
	}
	return &Error{Code: CodeInternal, Message: defaultMessage[CodeInternal], Cause: err}
}
