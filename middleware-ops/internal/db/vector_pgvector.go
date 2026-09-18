//go:build pgvector

// Package db 提供数据库连接、方言适配与迁移入口。
//
// 在 pgvector 构建标签下，向量列映射为原生 vector(768)，可创建 HNSW 索引；
// 未启用时向量以文本存储，检索在应用层完成（见 model.Vector 注释）。
package db

import (
	"fmt"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// VectorType 返回向量列的 DDL 类型。
const VectorType = "vector(768)"

// VectorEnabled 表示当前构建是否使用 pgvector 原生类型。
const VectorEnabled = true

// postMigrate 在 pgvector 构建下把向量列改写为原生 vector(768) 并创建 HNSW 索引
// （设计文档 7.2）。
//
// 实体层的 embedding 列声明为 text 以保持方言可移植，本步骤负责升级为原生类型；
// 使用 USING 显式转换，重复执行安全（幂等）。
//
// 表名一律用 model.TableNameOf 推导（INC-019）：这里曾经按直觉写 `knowledge_base`
// 与 `ai_diagnoses`，而 GORM 实际生成的是 `knowledge_bases` 与 `a_idiagnoses`——
// 由于本函数的错误会向上返回，启用 pgvector 的部署会**直接启动失败**。
func postMigrate(gdb *gorm.DB) error {
	alertEmbedding := model.TableNameOf(&model.AlertEmbedding{})
	knowledge := model.TableNameOf(&model.KnowledgeBase{})
	alert := model.TableNameOf(&model.Alert{})
	diagnosis := model.TableNameOf(&model.AIDiagnosis{})
	audit := model.TableNameOf(&model.AuditLog{})

	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN embedding TYPE vector(768) USING embedding::vector`, alertEmbedding),
		fmt.Sprintf(`ALTER TABLE %s ALTER COLUMN embedding TYPE vector(768) USING embedding::vector`, knowledge),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_alert_embedding ON %s USING hnsw (embedding vector_cosine_ops)`, alertEmbedding),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_knowledge_embedding ON %s USING hnsw (embedding vector_cosine_ops)`, knowledge),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_alert_instance_time ON %s(instance_id, triggered_at DESC)`, alert),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_diagnosis_instance ON %s(instance_id, created_at DESC)`, diagnosis),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_audit_user_time ON %s(user_id, created_at DESC)`, audit),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_audit_instance ON %s(instance_id, created_at DESC)`, audit),
	}
	for _, stmt := range stmts {
		if err := gdb.Exec(stmt).Error; err != nil {
			// 这里必须硬失败：向量列没升级成 vector(768)，后续检索语义就完全不同。
			return fmt.Errorf("pgvector 迁移失败（%s）：%w", stmt, err)
		}
	}
	return nil
}

// VectorSearchSQL 返回基于 pgvector 余弦距离的检索 SQL（? 为向量字面量）。
//
// 注意：表名来自模型推导，不要改回字面量（knowledge_bases 才是真实表名）。
func VectorSearchSQL() string {
	return fmt.Sprintf(`SELECT id, 1 - (embedding <=> ?::vector) AS similarity
	FROM %s
	WHERE status = ? AND embedding IS NOT NULL
	ORDER BY embedding <=> ?::vector
	LIMIT ?`, model.TableNameOf(&model.KnowledgeBase{}))
}
