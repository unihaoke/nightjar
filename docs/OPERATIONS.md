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
4. **验证**：`curl -fsS http://127.0.0.1:8000/healthz`，随后 `pwsh -File scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8000`。
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
