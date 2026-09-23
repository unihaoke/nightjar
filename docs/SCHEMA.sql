-- ===========================================================================
-- 平台表结构参考实现（docs/SCHEMA.sql）
-- ===========================================================================
--
-- 用途：
--   1. 供 DBA 评审与手工建库（当 database.auto_migrate=false 时自行执行）；
--   2. 作为表结构的事实说明文档，与设计文档 7.1 / 7.2 一一对应。
--
-- 【重要】本文件**不会**被 Docker 容器执行（容器只执行 deploy/postgres/init/ 下的脚本，
-- 且其中的初始化脚本已刻意不含建表语句）。
--
-- 【重要】默认部署请**不要**手工执行本文件：表结构的唯一权威是后端 GORM
-- AutoMigrate。若先手工建表，PostgreSQL 会为内联 UNIQUE 生成默认约束名
-- （如 users_username_key），与 GORM 命名策略期望的 uni_users_username 不一致，
-- AutoMigrate 执行 DROP CONSTRAINT 时会报 SQLSTATE 42704 导致后端启动失败。
--
-- 因此本文件中的唯一约束一律按 GORM 命名策略显式命名（uni_<表>_<列>），
-- 供确实需要手工建库的场景使用；此时请同时把配置项 database.auto_migrate 置为 false。
--
-- 执行顺序：先建表，再建索引；向量列在启用 pgvector 时使用 vector(768)。
--
-- 【表名警示】不要凭直觉写表名：GORM 命名策略会把常见缩写先改写再切词，
-- AIDiagnosis 落地为 a_idiagnoses、KnowledgeBase 落地为 knowledge_bases。
-- internal/model/naming_test.go 会校验本文件的表名与模型一致。

CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------------------
-- 用户与角色（6.1 RBAC）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ,
    username        VARCHAR(64)  NOT NULL,
    password_hash   VARCHAR(128) NOT NULL,
    nickname        VARCHAR(64),
    email           VARCHAR(128),
    role_code       VARCHAR(32)  NOT NULL,
    env_scope       TEXT,
    group_scope     TEXT,
    status          SMALLINT     DEFAULT 1,
    last_login      TIMESTAMPTZ,
    -- 约束名必须与 GORM 命名策略一致（uni_<表>_<列>）
    CONSTRAINT uni_users_username UNIQUE (username)
);
CREATE INDEX IF NOT EXISTS idx_users_role_code ON users(role_code);

CREATE TABLE IF NOT EXISTS roles (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ,
    code            VARCHAR(32)  NOT NULL,
    name            VARCHAR(64)  NOT NULL,
    description     VARCHAR(255),
    permissions     TEXT,
    levels          TEXT,
    builtin         BOOLEAN DEFAULT FALSE,
    CONSTRAINT uni_roles_code UNIQUE (code)
);

-- ---------------------------------------------------------------------------
-- 中间件纳管（4.1）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS middleware_instances (
    id                  BIGSERIAL PRIMARY KEY,
    created_at          TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ,
    name                VARCHAR(128) NOT NULL,
    mw_type             VARCHAR(16)  NOT NULL,
    host                VARCHAR(255) NOT NULL,
    port                INTEGER      NOT NULL,
    username            VARCHAR(128),
    password_encrypted  TEXT,
    environment         VARCHAR(16)  DEFAULT 'dev',
    group_name          VARCHAR(64),
    tags                TEXT,
    status              SMALLINT     DEFAULT 1,
    last_message        VARCHAR(255),
    last_check_at       TIMESTAMPTZ,
    config              TEXT,
    prom_job            VARCHAR(64),
    prom_instance       VARCHAR(128)
);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_name ON middleware_instances(name);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_mw_type ON middleware_instances(mw_type);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_environment ON middleware_instances(environment);
CREATE INDEX IF NOT EXISTS idx_middleware_instances_group_name ON middleware_instances(group_name);

-- ---------------------------------------------------------------------------
-- AI 诊断（4.3 / 7.1，含 v0.2 新增字段 feedback / engine_status）
--
-- 【表名注意】真实表名是 a_idiagnoses 而不是 ai_diagnoses：GORM 的命名策略会先把常见缩写
-- 改写（ID → Id）再切词，于是 AIDiagnosis → A_Idiagnosis → a_idiagnoses。
-- 手工建库 / 手写 SQL 时按直觉写成 ai_diagnoses 会报 SQLSTATE 42P01（见 INC-019）。
-- 本文件与模型的一致性由 internal/model/naming_test.go 守卫。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS a_idiagnoses (
    id                  BIGSERIAL PRIMARY KEY,
    created_at          TIMESTAMPTZ,
    updated_at          TIMESTAMPTZ,
    user_id             BIGINT NOT NULL,
    instance_id         BIGINT,
    mw_type             VARCHAR(16),
    user_query          TEXT   NOT NULL,
    collected_metrics   TEXT,
    diagnosis_result    TEXT,
    report              TEXT,
    suggestions         TEXT,
    feedback            VARCHAR(16),
    engine_status       VARCHAR(32),
    cost_tokens         INTEGER DEFAULT 0,
    engine_used         VARCHAR(32),
    duration_ms         BIGINT,
    cache_hit           BOOLEAN DEFAULT FALSE,
    truncated           TEXT
);
CREATE INDEX IF NOT EXISTS idx_diagnosis_instance ON a_idiagnoses(instance_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_diagnosis_user ON a_idiagnoses(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_diagnosis_feedback ON a_idiagnoses(feedback);

-- ---------------------------------------------------------------------------
-- 告警域（4.4 / 7.1）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS alert_rules (
    id               BIGSERIAL PRIMARY KEY,
    created_at       TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ,
    name             VARCHAR(128) NOT NULL,
    instance_id      BIGINT,
    mw_type          VARCHAR(16),
    metric_name      VARCHAR(128) NOT NULL,
    operator         VARCHAR(8)   NOT NULL,
    threshold        DOUBLE PRECISION,
    level            VARCHAR(16)  DEFAULT 'warning',
    -- 注意：列名刻意使用 time_window 而非 window。
    -- window 是 PostgreSQL 保留关键字，裸写会报 syntax error at or near "window"；
    -- GORM 会对标识符加引号，但初始化脚本 / 手工 SQL / BI 工具不会，故从命名规避。
    time_window      INTEGER DEFAULT 5,
    cooldown         INTEGER DEFAULT 10,
    notify_channels  TEXT,
    enabled          BOOLEAN DEFAULT TRUE,
    ai_enabled       BOOLEAN DEFAULT TRUE,
    description      VARCHAR(255)
);
CREATE INDEX IF NOT EXISTS idx_alert_rules_instance_id ON alert_rules(instance_id);

CREATE TABLE IF NOT EXISTS alerts (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ,
    instance_id     BIGINT NOT NULL,
    rule_id         BIGINT,
    mw_type         VARCHAR(16),
    alert_level     VARCHAR(16),
    alert_message   VARCHAR(512),
    metric_value    DOUBLE PRECISION,
    fingerprint     VARCHAR(64),
    status          VARCHAR(16),
    count           INTEGER DEFAULT 1,
    triggered_at    TIMESTAMPTZ,
    acked_by        BIGINT,
    acked_at        TIMESTAMPTZ,
    resolved_at     TIMESTAMPTZ,
    diagnosis_id    BIGINT,
    cluster_id      VARCHAR(64),
    suppressed      BOOLEAN DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_alert_instance_time ON alerts(instance_id, triggered_at DESC);
CREATE INDEX IF NOT EXISTS idx_alert_fingerprint ON alerts(fingerprint);
CREATE INDEX IF NOT EXISTS idx_alert_status ON alerts(status);
CREATE INDEX IF NOT EXISTS idx_alert_cluster_id ON alerts(cluster_id);

CREATE TABLE IF NOT EXISTS alert_embeddings (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    alert_id       BIGINT,
    alert_content  TEXT,
    -- 默认构建（无 pgvector 标签）为文本存储；启用 pgvector 构建时后端会改列为 vector(768)
    embedding      TEXT,
    clustered      BOOLEAN DEFAULT FALSE
);
CREATE INDEX IF NOT EXISTS idx_alert_embeddings_alert_id ON alert_embeddings(alert_id);
-- 启用 pgvector 后由后端 postMigrate 自动执行：
--   ALTER TABLE alert_embeddings ALTER COLUMN embedding TYPE vector(768) USING embedding::vector;
--   CREATE INDEX idx_alert_embedding ON alert_embeddings USING hnsw (embedding vector_cosine_ops);

-- ---------------------------------------------------------------------------
-- 知识库（4.5 质量闭环）
--
-- 【表名注意】真实表名是 knowledge_bases（GORM 复数化），不是设计文档里的 knowledge_base。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    title          VARCHAR(255) NOT NULL,
    content        TEXT,
    mw_type        VARCHAR(16),
    tags           TEXT,
    source         VARCHAR(16),
    status         VARCHAR(16),
    author_id      BIGINT,
    adopt_count    INTEGER DEFAULT 0,
    use_count      INTEGER DEFAULT 0,
    feedback       TEXT,
    diagnosis_id   BIGINT,
    embedding      TEXT
);
CREATE INDEX IF NOT EXISTS idx_knowledge_base_status ON knowledge_bases(status);
CREATE INDEX IF NOT EXISTS idx_knowledge_base_mw_type ON knowledge_bases(mw_type);
-- CREATE INDEX idx_knowledge_embedding ON knowledge_bases USING hnsw (embedding vector_cosine_ops);

-- ---------------------------------------------------------------------------
-- 审计（4.7 / 6.4：只追加 + 哈希链）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS audit_logs (
    id             BIGSERIAL PRIMARY KEY,
    user_id        BIGINT,
    username       VARCHAR(64),
    instance_id    BIGINT,
    action_type    VARCHAR(32),
    action_detail  TEXT,
    result         VARCHAR(16),
    level          VARCHAR(4),
    ip_address     VARCHAR(45),
    user_agent     VARCHAR(255),
    route          VARCHAR(255),
    hash_prev      VARCHAR(64),
    hash_self      VARCHAR(64),
    created_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_audit_user_time ON audit_logs(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_instance ON audit_logs(instance_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_action_type ON audit_logs(action_type);

CREATE TABLE IF NOT EXISTS audit_snapshots (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    snapshot_date  VARCHAR(16),
    last_log_id    BIGINT,
    log_count      BIGINT,
    chain_hash     VARCHAR(64),
    file_path      VARCHAR(255),
    verified       BOOLEAN DEFAULT FALSE,
    verified_at    TIMESTAMPTZ,
    CONSTRAINT uni_audit_snapshots_snapshot_date UNIQUE (snapshot_date)
);

-- ---------------------------------------------------------------------------
-- 审批与修复执行（4.6 / 6.2）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS approvals (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    ticket_id      VARCHAR(40),
    applicant_id   BIGINT,
    approver_id    BIGINT,
    instance_id    BIGINT,
    environment    VARCHAR(16),
    action_type    VARCHAR(64),
    action_detail  TEXT,
    preview        TEXT,
    reason         VARCHAR(512),
    status         VARCHAR(16),
    comment        VARCHAR(512),
    expires_at     TIMESTAMPTZ,
    decided_at     TIMESTAMPTZ,
    executed_at    TIMESTAMPTZ,
    exec_result    TEXT,
    CONSTRAINT uni_approvals_ticket_id UNIQUE (ticket_id)
);
CREATE INDEX IF NOT EXISTS idx_approvals_status ON approvals(status);
CREATE INDEX IF NOT EXISTS idx_approvals_applicant_id ON approvals(applicant_id);
CREATE INDEX IF NOT EXISTS idx_approvals_instance_id ON approvals(instance_id);

CREATE TABLE IF NOT EXISTS fix_records (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    approval_id    BIGINT,
    ticket_id      VARCHAR(40),
    user_id        BIGINT,
    instance_id    BIGINT,
    action_type    VARCHAR(64),
    level          VARCHAR(4),
    command        VARCHAR(512),
    action_detail  TEXT,
    status         VARCHAR(16),
    result         TEXT,
    dry_run        BOOLEAN DEFAULT FALSE,
    duration_ms    BIGINT
);
CREATE INDEX IF NOT EXISTS idx_fix_records_instance ON fix_records(instance_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_fix_records_ticket_id ON fix_records(ticket_id);

-- ---------------------------------------------------------------------------
-- 日志告警域（4.8：与应用域分离）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS server_instances (
    id             BIGSERIAL PRIMARY KEY,
    created_at     TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ,
    name           VARCHAR(128) NOT NULL,
    ip             VARCHAR(45),
    hostname       VARCHAR(128),
    environment    VARCHAR(16),
    group_name     VARCHAR(64),
    status         SMALLINT DEFAULT 1,
    tags           TEXT,
    last_seen_at   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_server_instances_ip ON server_instances(ip);
CREATE INDEX IF NOT EXISTS idx_server_instances_environment ON server_instances(environment);

-- 异步 AI 分析任务：提交 → 回调/轮询 → 结论（见 internal/service/ai_analysis_client.go）。
CREATE TABLE IF NOT EXISTS ai_analysis_tasks (
    id               BIGSERIAL PRIMARY KEY,
    created_at       TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ,
    event_id         BIGINT,
    service_name     VARCHAR(128),
    task_id          VARCHAR(128) NOT NULL,
    -- run_id：开放接口（openapi_v1）的运行 ID。回调对号用 task_id，
    -- 而轮询与报告地址按 run_id 组织（GET /api/v1/runs/{run_id}），两者都要存。
    run_id           VARCHAR(128),
    status           VARCHAR(16) DEFAULT 'submitted',
    question         TEXT,
    answer           TEXT,
    error            VARCHAR(512),
    deadline_at      TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    CONSTRAINT uni_ai_analysis_tasks_task_id UNIQUE (task_id)
);
CREATE INDEX IF NOT EXISTS idx_ai_analysis_tasks_event_id ON ai_analysis_tasks(event_id);
CREATE INDEX IF NOT EXISTS idx_ai_analysis_tasks_run_id ON ai_analysis_tasks(run_id);
CREATE INDEX IF NOT EXISTS idx_ai_analysis_tasks_service_name ON ai_analysis_tasks(service_name);
CREATE INDEX IF NOT EXISTS idx_ai_analysis_tasks_status ON ai_analysis_tasks(status);
CREATE INDEX IF NOT EXISTS idx_ai_analysis_tasks_deadline_at ON ai_analysis_tasks(deadline_at);

-- 日志消费链路的累计计数（按 topic + 消费组一行）：跨重启、跨副本累加。
CREATE TABLE IF NOT EXISTS kafka_consume_stats (
    id           BIGSERIAL PRIMARY KEY,
    created_at   TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ,
    topic        VARCHAR(128),
    group_id     VARCHAR(128),
    consumed     BIGINT DEFAULT 0,
    ingested     BIGINT DEFAULT 0,
    ignored      BIGINT DEFAULT 0,
    dropped      BIGINT DEFAULT 0,
    failed       BIGINT DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_kafka_consume_stats_topic ON kafka_consume_stats(topic);
CREATE INDEX IF NOT EXISTS idx_kafka_consume_stats_group_id ON kafka_consume_stats(group_id);

CREATE TABLE IF NOT EXISTS log_alert_events (
    id               BIGSERIAL PRIMARY KEY,
    created_at       TIMESTAMPTZ,
    updated_at       TIMESTAMPTZ,
    event_id         VARCHAR(40),
    server_id        BIGINT,
    service_name     VARCHAR(128),
    alert_type       VARCHAR(32),
    error_signature  VARCHAR(255),
    -- 错误消息原文（首条上报的 message）：指纹是哈希、不可读，通知与页面展示用它
    error_message    VARCHAR(1024),
    raw_stacktrace   TEXT,
    context_lines    TEXT,
    log_path         VARCHAR(512),
    error_count      INTEGER DEFAULT 1,
    severity         VARCHAR(16),
    status           VARCHAR(16),
    first_seen_at    TIMESTAMPTZ,
    last_seen_at     TIMESTAMPTZ,
    analyzed         BOOLEAN DEFAULT FALSE,
    suppressed       BOOLEAN DEFAULT FALSE,
    -- 规则化处理：命中的规则、本次窗口、冷却与通知、AI 分析状态
    -- （页面靠这几列回答"这条告警为什么没通知我"）
    rule_id          BIGINT DEFAULT 0,
    dedup_window     INTEGER,
    cooldown_until   TIMESTAMPTZ,
    notified_at      TIMESTAMPTZ,
    analysis_state   VARCHAR(16) DEFAULT 'pending',
    analysis_error   VARCHAR(512),
    CONSTRAINT uni_log_alert_events_event_id UNIQUE (event_id)
);
CREATE INDEX IF NOT EXISTS idx_log_event_sig ON log_alert_events(error_signature, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_log_event_srv ON log_alert_events(server_id, last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_log_alert_events_service_name ON log_alert_events(service_name);
CREATE INDEX IF NOT EXISTS idx_log_alert_events_status ON log_alert_events(status);

-- ---------------------------------------------------------------------------
-- 日志告警规则（日志集成的日志 → 去重窗口 / 冷却期 / 通知 / AI 分析）
--
-- 与指标告警规则（alert_rules）刻意分表：两者的判定输入不同——
-- 指标规则比数值（metric > threshold），日志规则比**服务 + 日志消息原文 + 级别**。
-- 但"去重窗口 / 冷却期 / 通知渠道 / AI 开关"四个概念同名同语义，使用者的心智模型一致。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS log_alert_rules (
    id                BIGSERIAL PRIMARY KEY,
    created_at        TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ,
    name              VARCHAR(128) NOT NULL,
    description       VARCHAR(255),
    service_name      VARCHAR(128),           -- 空 = 任意服务
    signature_pattern VARCHAR(255),           -- 匹配上报的 message 原文：普通文本=子串；/re/=正则；空 = 任意
    min_severity      VARCHAR(16),            -- 空 = 不限级别
    dedup_window      INTEGER DEFAULT 5,      -- 去重窗口（分钟）：窗口内同指纹只合并计数
    cooldown          INTEGER DEFAULT 10,     -- 冷却期（分钟）：冷却内不通知、不触发 AI
    notify_channels   TEXT,                   -- 逗号分隔；空 = 用平台默认渠道
    ai_enabled        BOOLEAN DEFAULT TRUE,
    enabled           BOOLEAN DEFAULT TRUE,
    priority          INTEGER DEFAULT 100,    -- 数字小优先（特例压过通用）
    CONSTRAINT uni_log_alert_rules_name UNIQUE (name)
);
CREATE INDEX IF NOT EXISTS idx_log_alert_rules_service_name ON log_alert_rules(service_name);
CREATE INDEX IF NOT EXISTS idx_log_alert_rules_enabled ON log_alert_rules(enabled);

-- ---------------------------------------------------------------------------
-- 日志告警屏蔽项（"这类错误不告警"）
--
-- 与规则的区别：规则回答"命中之后怎么处理"，屏蔽项回答"根本不该成为告警"
-- （典型是框架噪音，如 Request method 'GET' is not supported）。
-- 生效顺序也不同：屏蔽在所有规则**之前**，命中即丢弃（不入库、不通知、不分析）。
--
-- 匹配的是日志原文（message）而不是 error_signature：指纹是哈希值，
-- 使用者写不出"我想屏蔽的那句话"对应的哈希，只能写出日志里的文本。
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS log_alert_exclusions (
    id           BIGSERIAL PRIMARY KEY,
    created_at   TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ,
    name         VARCHAR(128),            -- 备注（为什么屏蔽）
    service_name VARCHAR(128),            -- 空 = 对任意服务生效
    pattern      VARCHAR(512) NOT NULL,   -- 普通文本=子串；/re/=正则；匹配日志原文
    enabled      BOOLEAN DEFAULT TRUE
);
CREATE INDEX IF NOT EXISTS idx_log_alert_exclusions_service_name ON log_alert_exclusions(service_name);
CREATE INDEX IF NOT EXISTS idx_log_alert_exclusions_enabled ON log_alert_exclusions(enabled);

CREATE TABLE IF NOT EXISTS ai_code_analyses (
    id              BIGSERIAL PRIMARY KEY,
    created_at      TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ,
    event_id        BIGINT,
    event_key       VARCHAR(40),
    service_name    VARCHAR(128),
    located_file    VARCHAR(512),
    located_line    INTEGER,
    code_snippet    TEXT,
    root_cause      TEXT,
    emergency_plan  TEXT,
    fix_suggestion  TEXT,
    impact_scope    TEXT,
    confidence      DOUBLE PRECISION,
    evidence        TEXT,
    engine_used     VARCHAR(32),
    engine_status   VARCHAR(32),
    cost_tokens     INTEGER,
    outbound_ok     BOOLEAN DEFAULT FALSE,
    -- 本次分析所用的代码版本（短 sha）：行号会随代码演进失效，事后复核必须能对上版本。
    repo_revision   VARCHAR(64),
    -- 外部 AI 服务的完整报告地址（开放接口的 reportUrl），平台只存结论摘要。
    report_url      VARCHAR(512)
);
CREATE INDEX IF NOT EXISTS idx_ai_code_analyses_event_id ON ai_code_analyses(event_id);
CREATE INDEX IF NOT EXISTS idx_ai_code_analyses_event_key ON ai_code_analyses(event_key);

CREATE TABLE IF NOT EXISTS notification_logs (
    id          BIGSERIAL PRIMARY KEY,
    created_at  TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ,
    event_id    BIGINT,
    alert_id    BIGINT,
    channel     VARCHAR(32),
    target      VARCHAR(255),
    content     TEXT,
    status      VARCHAR(16),
    error       VARCHAR(512),
    sent_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_notification_logs_alert_id ON notification_logs(alert_id);
CREATE INDEX IF NOT EXISTS idx_notification_logs_event_id ON notification_logs(event_id);

-- 平台自管配置（AI 提供方密钥、通知渠道）：payload 为 AES-256-GCM 密文，界面不回传明文。
CREATE TABLE IF NOT EXISTS platform_settings (
    id                BIGSERIAL PRIMARY KEY,
    created_at        TIMESTAMPTZ,
    updated_at        TIMESTAMPTZ,
    name              VARCHAR(64),
    payload_encrypted TEXT,
    updated_by        VARCHAR(64),
    CONSTRAINT uni_platform_settings_name UNIQUE (name)
);
