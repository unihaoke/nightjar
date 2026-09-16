-- 平台数据库初始化（容器首次启动时自动执行）。
--
-- 说明：
--   1. 表结构由后端 AutoMigrate 维护（database.auto_migrate=true）；
--   2. 本脚本只负责「扩展与索引」这类 DDL，保证 pgvector 可用；
--   3. 若使用 GO_BUILD_TAGS=pgvector 构建，后端会再次确认向量列类型与 HNSW 索引。

-- 向量扩展（pgvector）：知识库与告警语义聚类使用
CREATE EXTENSION IF NOT EXISTS vector;
-- 用于 UUID 生成与字符串摘要（部分查询辅助）
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- 平台元数据表结构由应用迁移，此处仅建立扩展与通用设置。
ALTER DATABASE middleware_ops SET timezone TO 'UTC';
