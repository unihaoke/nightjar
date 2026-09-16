//go:build !pgvector

// Package db 提供数据库连接、方言适配与迁移入口（默认构建：无 pgvector 依赖）。
package db

import "gorm.io/gorm"

// VectorType 返回向量列的 DDL 类型。
//
// 默认构建不依赖 pgvector 扩展，向量以 JSON 文本落库，
// 相似度检索由 service/ai 的应用层余弦实现承接。
const VectorType = "text"

// VectorEnabled 表示当前构建是否使用 pgvector 原生类型。
const VectorEnabled = false

// postMigrate 在默认构建下补建普通索引（设计文档 7.2 的非向量部分）。
func postMigrate(gdb *gorm.DB) error {
	stmts := []string{
		`CREATE INDEX IF NOT EXISTS idx_alert_instance_time ON alerts(instance_id, triggered_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnosis_instance ON ai_diagnoses(instance_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_user_time ON audit_logs(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_instance ON audit_logs(instance_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_log_event_sig ON log_alert_events(error_signature, last_seen_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_log_event_srv ON log_alert_events(server_id, last_seen_at DESC)`,
	}
	for _, stmt := range stmts {
		// 索引为性能优化项，失败不阻塞启动（例如方言差异）。
		_ = gdb.Exec(stmt).Error
	}
	return nil
}
