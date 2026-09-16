// Package repository 实现数据访问层（GORM）。
//
// 约定：
//   - 所有查询使用 GORM 参数化接口，禁止字符串拼接 SQL（6.6 注入防护）；
//   - 审计日志只追加：本包不提供更新/删除审计记录的方法（6.4）；
//   - 敏感字段（连接密码、AI key）在 service 层加解密，本包只存密文。
package repository

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("record not found")

// wrap 把 gorm.ErrRecordNotFound 规范化为 ErrNotFound。
func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Base 保存共享的 DB 句柄，供各仓储内嵌。
type Base struct {
	db *gorm.DB
}

// DB 返回底层句柄（仅限仓储内部与迁移使用）。
func (b Base) DB() *gorm.DB { return b.db }

// withCtx 为查询附加 context。
func (b Base) withCtx(ctx context.Context) *gorm.DB {
	if ctx == nil {
		return b.db
	}
	return b.db.WithContext(ctx)
}
