-- 平台数据库初始化 · 扩展创建（容器首次启动时自动执行）。
--
-- 说明：
--   1. 表结构由后端 GORM AutoMigrate 维护（database.auto_migrate=true），
--      本目录下的脚本一律**不建表**，避免唯一约束命名冲突（详见 02-extensions-and-settings.sql）；
--   2. 本脚本只创建扩展，保证 pgvector 可用；
--   3. 以 GO_BUILD_TAGS=pgvector 构建时，后端 postMigrate 会把向量列改写为
--      vector(768) 并创建 HNSW 索引，此处无需重复处理。

-- 向量扩展（pgvector）：知识库与告警语义聚类使用
CREATE EXTENSION IF NOT EXISTS vector;

-- 模糊匹配辅助扩展（知识库关键词检索、实例名模糊查询）
CREATE EXTENSION IF NOT EXISTS pg_trgm;
