//go:build !pgvector

// Package db 提供数据库连接、方言适配与迁移入口（默认构建：无 pgvector 依赖）。
package db

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// VectorType 返回向量列的 DDL 类型。
//
// 默认构建不依赖 pgvector 扩展，向量以 JSON 文本落库，
// 相似度检索由 service/ai 的应用层余弦实现承接。
const VectorType = "text"

// VectorEnabled 表示当前构建是否使用 pgvector 原生类型。
const VectorEnabled = false

// postMigrate 在默认构建下补建普通索引（设计文档 7.2 的非向量部分）。
//
// 表名一律用 model.TableNameOf 推导（INC-019）：这里曾经按直觉写 `ai_diagnoses`，
// 而 GORM 对 AIDiagnosis 实际生成的是 `a_idiagnoses`，于是这些建索引语句**每次都失败**，
// 又因为错误被忽略而完全无声——索引一直没建上，只在慢查询时才暴露。
// 现在表名解析失败会走下面的警告分支，不再静默。
func postMigrate(gdb *gorm.DB) error {
	alert := model.TableNameOf(&model.Alert{})
	diagnosis := model.TableNameOf(&model.AIDiagnosis{})
	audit := model.TableNameOf(&model.AuditLog{})
	logEvent := model.TableNameOf(&model.LogAlertEvent{})

	stmts := []string{
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_alert_instance_time ON %s(instance_id, triggered_at DESC)`, alert),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_diagnosis_instance ON %s(instance_id, created_at DESC)`, diagnosis),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_audit_user_time ON %s(user_id, created_at DESC)`, audit),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_audit_instance ON %s(instance_id, created_at DESC)`, audit),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_log_event_sig ON %s(error_signature, last_seen_at DESC)`, logEvent),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_log_event_srv ON %s(server_id, last_seen_at DESC)`, logEvent),
	}
	for _, stmt := range stmts {
		// 索引为性能优化项，失败不阻塞启动（例如方言差异）；
		// 但**必须留下日志**：静默失败等于索引永远建不上（INC-019）。
		if err := gdb.Exec(stmt).Error; err != nil {
			gdb.Logger.Warn(context.Background(),
				"补建索引失败（不影响启动，但该索引缺失会拖慢查询）：%s：%v", stmt, err)
		}
	}
	return nil
}
