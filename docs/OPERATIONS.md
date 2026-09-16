# 部署与运维说明

## 一、部署拓扑与最小要求

```text
              ┌──────────────────────────────┐
   浏览器 ───▶│ Nginx（静态资源 + /api 反代） │
              └───────────────┬──────────────┘
                              ▼
                    ┌───────────────────┐        ┌──────────────┐
                    │ 平台后端（Go 单体）│───────▶│ PostgreSQL 15│
                    │  :8080            │        │  + pgvector  │
                    └────┬─────────┬────┘        └──────────────┘
                         │         │
              ┌──────────▼──┐  ┌───▼─────────┐   ┌─────────────────────┐
              │ Redis 7      │  │ Prometheus  │◀──│ 各中间件官方 Exporter│
              │ 缓存/队列    │  │  指标存储   │   └─────────────────────┘
              └──────────────┘  └─────────────┘
```

| 项目 | 最低 | 推荐 |
|------|------|------|
| CPU / 内存 | 2C / 4G | 4C / 8G |
| 磁盘 | 40G（含 Prometheus 15 天指标） | 100G+ SSD |
| 部署方式 | 单机 Docker Compose | 单机 + 定时 pg_dump 备份 |
| 规模上限（一期） | 纳管 ≤200 实例、采集 ≤50 QPS、诊断并发 ≤4 | — |

> 演进条件（设计文档 3.3）：纳管实例 >500 或出现独立团队多租户时，才按「诊断引擎 / 采集 / 通知」拆分微服务；在此之前保持单进程 + 纵向扩容。

---

## 二、首次部署清单

1. **生成密钥材料**
   ```bash
   # JWT 密钥（≥32 位）
   openssl rand -base64 48
   # 主密钥（可选；留空则由容器内密钥文件自动生成，0600 权限）
   openssl rand -base64 32
   ```
2. **填写 `.env`**：`JWT_SECRET`、`ADMIN_PASSWORD`、`DB_PASSWORD`、`REDIS_PASSWORD` 必改。
3. **启动**：`docker compose up -d --build`。
4. **验证**：`curl -fsS http://127.0.0.1:8000/healthz`，随后运行端到端冒烟
   （PowerShell 7 用 `pwsh -File scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8000`；
   Windows 自带 PowerShell 5.1 用 `powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:8000`）。
5. **登录并改密**：浏览器访问 `http://<host>:8000`，使用管理员账号登录后立即修改密码。

---

## 三、配置项速查（`configs/config.yaml` / `MWOPS_*` 环境变量）

环境变量命名规则：前缀 `MWOPS_`，层级以下划线连接，例如 `MWOPS_DATABASE_HOST`。

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `app.mode` | `debug` | `release` 下强制校验 `jwt.secret` 长度 ≥32 |
| `server.port` | `8080` | 后端监听端口 |
| `server.write_timeout` | `0` | 必须为 0（SSE 流式诊断依赖长连接） |
| `server.rate_limit_per_minute` | `600` | 应用层限流（Nginx 为第一层） |
| `database.auto_migrate` | `true` | 启动时自动迁移表结构 |
| `redis.addr` | 空 | 为空 → 进程内缓存与队列（单机模式） |
| `prometheus.base_url` | 空 | 为空 → 内置确定性指标模拟器 |
| `ai_engine.strategy` | `hybrid` | `third_party` / `self_hosted` / `hybrid` |
| `ai_engine.third_party.enabled` | `false` | 需同时提供 `api_key` |
| `ai_engine.self_hosted.kind` | `mock` | `mock`（进程内）/ `openai` / `ollama` / `anthropic` |
| `guardrail.input_token_budget` | `8192` | 单次诊断输入预算 |
| `guardrail.output_token_budget` | `2048` | 输出预算 |
| `guardrail.max_concurrency` | `4` | 诊断并发上限（防打爆外部配额） |
| `guardrail.cache_ttl` | `24h` | 确定性缓存有效期 |
| `guardrail.daily_token_quota` | `2000000` | 平台日 token 预算 |
| `guardrail.per_user_daily_token_quota` | `200000` | 单用户日预算 |
| `guardrail.sql_default_limit` / `sql_max_limit` | `100` / `1000` | SQL 强制 LIMIT 与上限 |
| `scheduler.approval_expire_interval` | `1m` | 审批超时扫描周期（工单 TTL 30 分钟） |
| `scheduler.audit_snapshot_cron` | `0 10 0 * * *` | 每日 00:10 生成审计哈希链快照 |
| `security.outbound_whitelist` | 空 | 空 = **禁止**任何第三方 AI 分析 |

---

## 四、接入真实组件

### 4.1 接入 Prometheus

```yaml
prometheus:
  base_url: http://prometheus:9090
  exporter_job_prefix: middleware-exporter   # job 名约定：<前缀>-<中间件类型>
```

被管实例需在纳管时填写 `prom_job` / `prom_instance` 以便 PromQL 标签匹配；未填写时按 `job="<前缀>-<类型>"` + `instance_name="<实例名>"` 匹配。抓取配置见 `deploy/prometheus/prometheus.yml`。

### 4.2 接入第三方 LLM（DeepSeek / Claude / Codex 兼容网关）

```yaml
ai_engine:
  strategy: hybrid
  third_party:
    enabled: true
    kind: openai            # anthropic 走兼容网关时改为 anthropic
    base_url: https://api.deepseek.com/v1
    api_key: sk-xxxx        # 建议用环境变量 MWOPS_AI_ENGINE_THIRD_PARTY_API_KEY 注入
    model: deepseek-chat
    price_per_k_token: 0.002
    timeout: { connect: 5s, first_byte: 15s, total: 60s, tool_call: 10s, task_deadline: 120s }
  fallback: { failure_threshold: 2, open_duration: 60s, rules_engine_enabled: true }
```

连续失败 2 次触发熔断并降级到 `self_hosted`，再失败则降级规则引擎（输出半自动结论 + AI 不可用提示）。

### 4.3 接入本地 LLM（Ollama / vLLM）

```yaml
ai_engine:
  self_hosted:
    enabled: true
    kind: openai
    base_url: http://ollama.internal:11434/v1
    model: qwen2.5:7b
```

### 4.4 启用 pgvector 原生向量

```bash
# 1) 构建镜像时开启构建标签
GO_BUILD_TAGS=pgvector docker compose build backend && docker compose up -d backend
# 2) 后端迁移时会执行：CREATE EXTENSION / ALTER COLUMN TYPE vector(768) / HNSW 索引
```

未启用时向量以文本存储，检索走应用层余弦相似度（复用同一份 768 维本地嵌入），知识库规模可控时性能足够。

### 4.5 接入通知渠道

| 渠道 | 配置 | 说明 |
|------|------|------|
| 飞书 | `notify.feishu.webhook` + `secret`（可选签名） | 交互式消息卡片，含「查看详情」按钮 |
| 企业微信 | `notify.wecom.webhook` | Markdown 消息 |
| 钉钉 | `notify.dingtalk.webhook` + `secret` | Markdown 消息（备选） |
| 邮件 | `notify.email.*` | 低优先级，SMTP |

**边界**：卡片仅支持「查看详情 / 确认 / 驳回」，**不支持一键执行**；高危操作统一回 Web 端执行（设计文档 6.2）。

配置完成后在「告警规则」页点击渠道标签即可发送自检消息。

### 4.6 部署日志采集 Agent

```bash
cd middleware-ops
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o mwops-agent ./cmd/agent
# 上传二进制与 agent.yaml（参考 cmd/agent/agent.example.yaml）
./mwops-agent -config agent.yaml            # 前台运行；生产用 systemd 托管
```

- 增量读取：偏移量写入 `position_file`，文件轮转（变小）时自动从头读取。
- 上报失败**不推进偏移**，下轮重读同一批数据，保证日志不丢。
- 也可完全不用 Agent：应用直接 POST `/api/hooks/logs`（零侵入）。

---

## 五、日常运维

### 5.1 备份与恢复

```bash
# 每日 pg_dump（建议 crontab）
docker exec mwops-postgres pg_dump -U mwo middleware_ops | gzip > backup-$(date +%F).sql.gz

# 恢复
gunzip -c backup-2026-09-16.sql.gz | docker exec -i mwops-postgres psql -U mwo -d middleware_ops
```

指标数据保留 15 天（Prometheus `--storage.tsdb.retention.time=15d`），无需备份；审计日志长期保留，其哈希链快照落盘于容器内 `/app/data/audit-snapshots/`（挂载在 `backend-data` 卷），建议定期导出。

**月度恢复演练**：至少每季度用备份在隔离环境做一次恢复演练并记录结果。

### 5.2 审计完整性核查

```bash
TOKEN=<登录后的 JWT>
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8000/api/audit/verify | jq
```

```json
{ "code": 0, "data": { "verified": true, "broken_id": 0, "message": "哈希链校验通过，审计日志未被篡改" } }
```

`verified=false` 时 `broken_id` 给出首个断链日志 ID，需立即排查数据库直改行为。每日 00:10 会自动生成快照并校验，快照失败会以 ERROR 级别写入平台日志。

### 5.3 密钥轮换（90 天）

1. 生成新主密钥并更新 `MWOPS_SECURITY_MASTER_KEY`。
2. 由于平台不保存明文，轮换后需**重新录入**被管实例连接密码（旧密文无法用新密钥解密）。
   建议：先在维护窗口更新 `security.master_key`，再通过「中间件纳管 → 编辑 → 填写密码」批量重录。
3. 轮换 JWT 密钥会使全部会话失效，请在低峰期执行。

### 5.4 平台自身可观测性

| 观测项 | 位置 |
|--------|------|
| 结构化日志 | 容器 stdout（JSON）+ `./data/logs/middleware-ops.log`（轮转，默认 64MB × 10） |
| 自身指标 | `GET /metrics`（存活、引擎可用性、缓存降级、护栏并发） |
| 诊断质量/成本 | 「AI 诊断 → 质量与成本」页面；`GET /api/ai/quality` |
| 任务与调度 | 平台日志中以 `component` 字段区分；规则评估、聚类、健康巡检均输出统计 |
| 审计 | 「审计日志」页面 + 哈希链校验 |

### 5.5 常见问题

| 现象 | 排查方向 |
|------|----------|
| 监控页面显示「内置模拟器」 | 未配置或连不上 `prometheus.base_url`；检查 Prometheus 容器与 `/-/healthy` |
| AI 诊断为「规则引擎」结论 | 第三方/本地引擎均不可用；检查 `ai_engine.*` 配置、API Key、出网连通性、熔断状态（`/api/system/info`） |
| 诊断报 4033（需要审批） | 目标动作是 L2；到「审批管理」创建/审批工单后凭 `ticket_id` 执行 |
| 代码分析提示「不在出网白名单」 | 需在「服务器与仓库」中为该服务开启 `allow_third_party`，并在 `security.outbound_whitelist` 中加入服务名 |
| 上报 Hook 返回 401 | `X-Hook-Token` 与后端 `MWOPS_HOOK_TOKEN` 不一致 |
| 前端刷新 404 | Nginx 未启用 history 回退（确认 `try_files $uri $uri/ /index.html`） |
| SSE 诊断被截断 | 反向代理开启了缓冲；确认 `proxy_buffering off;` 且 `server.write_timeout=0` |
| 审计校验失败 | 数据库被直接修改；用备份恢复并排查访问来源，`broken_id` 即首个异常位置 |
| 后端启动报 `password authentication failed for user "mwo" (SQLSTATE 28P01)` | 改过 `.env` 的 `DB_PASSWORD`，但数据卷早已初始化过（Postgres 只在空卷时应用该口令）。见 5.8 |
| 后端启动报 `role "xxx" does not exist` | `DB_USER` 与数据卷初始化时不一致。见 5.8 |
| 后端启动报 `constraint "uni_users_username" ... does not exist (SQLSTATE 42704)` | 数据库里的唯一约束来自**非 GORM 来源**（手工执行过建表 SQL，或使用过早期版本的初始化脚本）。PostgreSQL 把内联 `UNIQUE` 命名为 `users_username_key`，而 GORM 迁移列唯一性时期望 `uni_users_username`，`DropConstraint` 因找不到约束而报错。修复见下方 5.6 |

### 5.6 数据库初始化职责划分（重要）

**为什么会报 42704（已核对 GORM v1.25.x 源码）**

- 模型用 `uniqueIndex` 标签声明唯一约束，AutoMigrate 通过 `NamingStrategy.IndexName` 创建唯一索引，名字是 `idx_<表>_<列>`；
- 但迁移「列唯一性」时会走 `migrateColumnUnique`，用 `NamingStrategy.UniqueName` 生成 `uni_<表>_<列>` 并执行 `DropConstraint`；
- 若数据库里的唯一约束由初始化脚本的内联 `UNIQUE` 建出，实际名字是 `<表>_<列>_key`，于是 `DROP CONSTRAINT "uni_users_username"` 直接报 42704 并使后端启动失败。

**结论：表结构只能有一个来源。** 初始化脚本不建表，全部交给 GORM。

| 来源 | 职责 | 是否建表 |
|------|------|----------|
| `deploy/postgres/init/01-extensions.sql` | 创建 pgvector / pg_trgm 扩展 | 否 |
| `deploy/postgres/init/02-extensions-and-settings.sql` | 数据库级参数（时区、random_page_cost） | 否 |
| 后端 `GORM AutoMigrate` | **全部业务表与索引（唯一权威）** | 是 |
| `docs/SCHEMA.sql` | DBA 参考 / 手工建库（约束名已按 GORM 的 `uni_*` 策略显式命名） | 仅手工执行时 |

**修复方式（二选一）**

1. 开发/全新环境（推荐）：清掉旧数据卷重建，让 GORM 从零建表。
   ```bash
   docker compose down -v      # 注意：会删除数据库数据
   docker compose up -d --build
   ```
2. 保留数据的生产环境：把 5 处唯一约束重命名成 GORM 期望的名字（`DBA` 操作，先在备库演练）。
   ```sql
   -- 先确认实际约束名（以 users 为例）
   SELECT conname FROM pg_constraint
   WHERE conrelid = 'users'::regclass AND contype = 'u';

   ALTER TABLE users             RENAME CONSTRAINT users_username_key TO uni_users_username;
   ALTER TABLE roles             RENAME CONSTRAINT roles_code_key     TO uni_roles_code;
   ALTER TABLE approvals         RENAME CONSTRAINT approvals_ticket_id_key TO uni_approvals_ticket_id;
   ALTER TABLE log_alert_events  RENAME CONSTRAINT log_alert_events_event_id_key TO uni_log_alert_events_event_id;
   ALTER TABLE audit_snapshots   RENAME CONSTRAINT audit_snapshots_snapshot_date_key TO uni_audit_snapshots_snapshot_date;
   ```
   全部唯一约束的实际名字可一次查清：
   ```sql
   SELECT conrelid::regclass AS table_name, conname
   FROM pg_constraint
   WHERE contype = 'u' AND connamespace = 'public'::regnamespace
   ORDER BY 1;
   ```

> 若选择手工建库（执行 `docs/SCHEMA.sql`），务必同时把配置项 `database.auto_migrate` 置为 `false`，避免 AutoMigrate 再去维护这套约束。

### 5.7 保留关键字与 DDL 引号

早期版本的初始化脚本使用未加引号的建表语句，`alert_rules.window INTEGER` 会直接失败：

```
ERROR: syntax error at or near "window"
LINE 12: window INTEGER DEFAULT 5,
```

原因：`WINDOW` 属 PostgreSQL **reserved** 关键字（`reserved_keywords`），不能作为裸列名；而 `COUNT` / `RESULT` / `LEVEL` / `STATUS` / `KEY` / `VALUE` / `SOURCE` 等属 **non-reserved**，可以裸写——所以只有 `window` 这一处有问题。

处理方式：

- 该列已重命名为 `time_window`（数据库列名、API 字段名、前端表单字段一致），模型侧用 `gorm:"column:time_window"` 固定列名；
- GORM 生成的 DDL 本身是带引号的（`QuoteTo` 内部无条件加 `"`），因此模型列名即使撞保留字也能建表成功——但初始化脚本、手工 SQL、BI 导出不会加引号，故统一从命名上规避；
- 已加入回归测试 `internal/model/schema_test.go`：`TestModelColumnsAvoidPostgresReservedKeywords` 与 `TestSchemaReferenceColumnsAvoidReserved` 会遍历全部模型列名与 `docs/SCHEMA.sql` 列名，禁止命中保留字表。新增模型字段时若误用保留字，`go test ./internal/model/` 会直接失败。

若已有环境建过旧表结构，`alert_rules` 可能因该语法错误而**整表缺失**（初始化脚本是按语句逐条执行的，失败后该表不会存在）。请按 5.6 节的第 1 种方式清库重建，或单独补建该表并确认其唯一约束命名符合 `uni_*` 规则。

### 5.8 数据库口令与数据卷的一致性（SQLSTATE 28P01）

**记住一条规则：PostgreSQL 的 `POSTGRES_PASSWORD` 只在数据卷为空时生效。**

数据卷 `middleware-ops_postgres-data` 一旦初始化完成，库里的口令就已经定死；
此后无论怎么改 `.env` 的 `DB_PASSWORD`，Postgres 都不会重新应用它，表现为后端启动即失败：

```
初始化数据库: open postgres: failed to connect to `host=postgres user=mwo database=middleware_ops`:
failed SASL auth (FATAL: password authentication failed for user "mwo" (SQLSTATE 28P01))
```

三种修法（**按推荐顺序**）：

```bash
cd <nightjar>

# ① 首选：把库内口令对齐成 .env 里的值（零数据损失）
#    容器内本地 socket 是 trust 认证，因此不需要原口令
docker exec mwops-postgres psql -U mwo -d middleware_ops \
  -c "ALTER USER mwo WITH PASSWORD '$(grep -E '^DB_PASSWORD=' .env | cut -d= -f2-)';"
docker compose up -d

# ② 或者：把 .env 改回卷初始化时用的旧口令（例如首次部署没改过默认值）
sed -i 's/^DB_PASSWORD=.*/DB_PASSWORD=mwo_change_me/' .env && docker compose up -d

# ③ 最后手段：重建平台数据卷（**会清空**集成/告警/诊断/知识/审计数据）
docker compose down -v && docker compose up -d --build
```

`scripts/setup-jd-link.sh` 第 5 步已经内建了 ①：检测到数据卷已存在时**不会轮换** `DB_PASSWORD`，
并在启动前用 `pg_isready` + TCP `scram` 认证核对口径，不一致就自动 `ALTER USER` 对齐。

> 同理，`DB_USER` / `DB_NAME` 也是**卷初始化时定死的**：改了 `DB_USER` 会报
> `role "xxx" does not exist`，此时只能改回原值或重建数据卷。

---

## 六、升级与回滚

```bash
# 升级
git pull
docker compose build
docker compose up -d            # 启动时 AutoMigrate 自动补齐新增字段/索引

# 回滚（保留数据卷）
docker compose down
git checkout <上一个版本 tag>
docker compose up -d --build
```

升级前务必 `pg_dump`；若新版本包含不可逆的表结构变更，回滚需从备份恢复数据库。

---

## 七、安全加固清单（生产必做）

- [ ] 全站 HTTPS + WSS（`middleware-ops-web/nginx.conf` 中已给出 443 配置模板）
- [ ] 替换全部默认口令，`jwt.cookie_secure=true`、`cookie_same_site=lax|strict`
- [ ] 主密钥通过环境变量或独立密钥文件（0600）注入，纳入 90 天轮换流程
- [ ] 被管中间件使用**最小权限只读监控账号**，不存明文密码，SSH 走密钥 + 跳板机
- [ ] `security.outbound_whitelist` 保持为空，确需第三方代码分析时逐服务开启并评审
- [ ] 审计快照导出到对象存储，开启定期校验告警
- [ ] 数据库与 Redis 不对公网暴露，仅容器网络内可达
- [ ] 平台账号按角色最小授权，生产环境数据权限显式限定到分组
