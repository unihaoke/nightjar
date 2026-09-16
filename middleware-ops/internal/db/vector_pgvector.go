//go:build pgvector

// Package db 提供数据库连接、方言适配与迁移入口。
//
// 在 pgvector 构建标签下，向量列映射为原生 vector(768)，可创建 HNSW 索引；
// 未启用时向量以文本存储，检索在应用层完成（见 model.Vector 注释）。
package db

import "gorm.io/gorm"

// VectorType 返回向量列的 DDL 类型。
const VectorType = "vector(768)"

// VectorEnabled 表示当前构建是否使用 pgvector 原生类型。
const VectorEnabled = true

// postMigrate 在 pgvector 构建下把向量列改写为原生 vector(768) 并创建 HNSW 索引
// （设计文档 7.2）。
//
// 实体层的 embedding 列声明为 text 以保持方言可移植，本步骤负责升级为原生类型；
// 使用 USING 显式转换，重复执行安全（幂等）。
func postMigrate(gdb *gorm.DB) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		`ALTER TABLE alert_embeddings ALTER COLUMN embedding TYPE vector(768) USING embedding::vector`,
		`ALTER TABLE knowledge_base ALTER COLUMN embedding TYPE vector(768) USING embedding::vector`,
		`CREATE INDEX IF NOT EXISTS idx_alert_embedding ON alert_embeddings USING hnsw (embedding vector_cosine_ops)`,
		`CREATE INDEX IF NOT EXISTS idx_knowledge_embedding ON knowledge_base USING hnsw (embedding vector_cosine_ops)`,
		`CREATE INDEX IF NOT EXISTS idx_alert_instance_time ON alerts(instance_id, triggered_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_diagnosis_instance ON ai_diagnoses(instance_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_user_time ON audit_logs(user_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_instance ON audit_logs(instance_id, created_at DESC)`,
	}
	for _, stmt := range stmts {
		if err := gdb.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

// VectorSearchSQL 返回基于 pgvector 余弦距离的检索 SQL（? 为向量字面量）。
const VectorSearchSQL = `SELECT id, 1 - (embedding <=> ?::vector) AS similarity
	FROM knowledge_base
	WHERE status = ? AND embedding IS NOT NULL
	ORDER BY embedding <=> ?::vector
	LIMIT ?`
