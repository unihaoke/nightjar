-- 平台数据库初始化（容器首次启动时自动执行）。
--
-- 重要：本脚本**不创建任何业务表**。
--
-- 原因（避免两套建表来源冲突）：
--   1. 表结构的唯一权威是后端 GORM AutoMigrate（database.auto_migrate=true）；
--   2. 若在此处先建表，PostgreSQL 会为内联 UNIQUE 生成默认约束名
--      （例如 users_username_key），而 GORM 期望其命名策略下的 uni_users_username，
--      于是 AutoMigrate 执行 DROP CONSTRAINT "uni_users_username" 时会报
--      SQLSTATE 42704（constraint does not exist），导致后端启动失败；
--   3. 表结构参考实现（仅用于 DBA 评审与手工建库）见仓库 docs/SCHEMA.sql，
--      该文件放在 docs/ 下，不会被容器执行。
--
-- 本文件的职责：设置数据库级参数。扩展创建见 01-extensions.sql。

-- 统一时间口径：所有 TIMESTAMPTZ 以 UTC 存储与比较（与后端 model.NowFunc 一致）
ALTER DATABASE middleware_ops SET timezone TO 'UTC';

-- 审计日志与告警表以时间倒序检索为主，适度提高随机页成本估计，避免顺序扫描误选
ALTER DATABASE middleware_ops SET random_page_cost TO 1.1;
