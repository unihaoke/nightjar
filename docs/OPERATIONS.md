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
                    └──┬──────┬─────┬───┘        └──────────────┘
                       │      │     │
          ┌────────────▼─┐ ┌──▼───────┐ ┌▼────────────────────────┐
          │ Redis 7      │ │Prometheus│ │ Kafka（KRaft 单节点）   │
          │ 缓存/队列    │ │ 指标存储 │ │ 日志总线 :9092 对外     │
          └──────────────┘ └────▲─────┘ └▲────────────────────────┘
                                │        │ Filebeat 推送
                     ┌──────────┴──┐     │（平台用 Ansible 装到目标机）
                     │官方 Exporter│─────┘
                     └─────────────┘
```

| 项目 | 最低 | 推荐 |
|------|------|------|
| CPU / 内存 | 2C / 4G | 4C / 8G |
| 磁盘 | 40G（含 Prometheus 15 天指标 + Kafka 日志总线） | 100G+ SSD |
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
| `kafka.enabled` | `true` | 日志总线开关（`MWOPS_KAFKA_ENABLED`）；`false` 时平台照常启动、日志集成不可用 |
| `kafka.brokers` | 空 | 平台侧消费地址（`MWOPS_KAFKA_BROKERS`），compose 注入 `kafka:29092`；留空 = 未启用 |
| `kafka.log_topic` | `mwops-logs` | 日志 topic（`MWOPS_KAFKA_LOG_TOPIC`），平台启动时幂等确保存在 |
| `kafka.group_id` | `mwops-log-ingest` | 平台消费组（`MWOPS_KAFKA_GROUP_ID`） |
| `kafka.external_host` / `external_port` | `127.0.0.1` / `9092` | **被管服务器上的 Filebeat 要连的地址**（`MWOPS_KAFKA_EXTERNAL_HOST`），必须与 compose 的 `KAFKA_ADVERTISED_HOST` 一致 |
| `kafka.filebeat_version` | `8.16.0` | 目标机安装的 Filebeat 版本（`FILEBEAT_VERSION`） |
| `log_alert.default_dedup_window` | `5` | **没命中任何日志告警规则时**的去重窗口（分钟）：窗口内同指纹只合并计数 |
| `log_alert.default_cooldown` | `10` | 默认冷却期（分钟）：冷却内不重复通知、不重复触发 AI（事件仍记录） |
| `log_alert.default_ai_enabled` | `true` | 默认是否对日志事件自动做 AI 代码分析（需该服务已配仓库映射） |
| `log_alert.default_notify_channels` | `[]` | 默认通知渠道（`feishu/wecom/dingtalk/email`）；空 = 用「通知渠道」里已启用的渠道 |
| `log_alert.worker_interval_seconds` | `15` | 日志告警**后处理**（通知 + AI）的扫描间隔（秒） |
| `log_alert.worker_batch` | `10` | 每轮最多处理的事件数（AI 很贵，靠它与间隔限流） |
| `log_alert.analyze_timeout` | `2m` | 单条事件的 AI 分析超时 |
| `code_repo.cache_dir` | `./data/repos` | 代码仓库本地缓存根目录（容器内落 `backend-data` 卷，每个服务一个子目录） |
| `code_repo.clone_timeout` | `10m` | **首次 clone** 的超时（仓库大就调大） |
| `code_repo.pull_timeout` | `2m` | 之后 `fetch`/`checkout`/`pull` 的超时 |
| `code_repo.refresh_interval_seconds` | `300` | 同一仓库两次拉取的**最小间隔**（秒），风暴期避免反复拉远端 |
| `code_repo.allow_outbound` | `true` | 平台**能否 `git clone/pull` 代码**；`false` 时不执行任何 git 命令（详见 5.10） |

> 环境变量名按同一规则拼：`log_alert.worker_interval_seconds` → `MWOPS_LOG_ALERT_WORKER_INTERVAL_SECONDS`，
> `code_repo.cache_dir` → `MWOPS_CODE_REPO_CACHE_DIR`。
> **这两组已支持环境变量覆盖**（后端启动时显式读取，不依赖 viper 对嵌套键的自动覆盖），
> 且 `.env.example` 与 compose 都提供了对应的短名（如 `LOG_ALERT_COOLDOWN` → `MWOPS_LOG_ALERT_DEFAULT_COOLDOWN`）：
> 改 `.env` 后 `docker compose up -d backend` 即可生效，不必重建镜像。
> 注意**只有**写进 `docker-compose.yml` 的那几项能这样传（`default_notify_channels`、`clone_timeout`
> 等未注入的项请直接改 `middleware-ops/configs/config.yaml`）。
> 日常调参应优先在页面「日志告警规则」里改（保存即生效，不需要重启）；这一组只是"没命中规则时的兜底"。

---

## 四、接入真实组件

### 4.1 接入 Prometheus

```yaml
prometheus:
  base_url: http://prometheus:9090
  exporter_job_prefix: middleware-exporter   # job 名约定：<前缀>-<中间件类型>
```

被管实例需在纳管时填写 `prom_job` / `prom_instance` 以便 PromQL 标签匹配；未填写时按 `job="<前缀>-<类型>"` + `instance_name="<实例名>"` 匹配。抓取配置见 `deploy/prometheus/prometheus.yml`。

### 4.2 配置 AI（在平台内完成，不再改 .env）

**登录平台 →「AI 设置」**：选策略（third_party / self_hosted / hybrid）、填提供方的协议、base_url、
**API Key**、模型、max_tokens 与价格（元/千 token），保存即时生效（`Factory.Reload()` 原子重建引擎，
不需要重建容器）。

- **密钥安全**：整个配置以 AES-256-GCM 加密后存在 `platform_settings`（复用平台主密钥）；
  接口只回显掩码（`sk-****cdef`），日志与审计里不含密钥；输入框留空＝不修改，点「清除」才清空。
- **额度**：日额度 / 每人日额度也在这一页配置，超限即触发平台的配额护栏（`CodeQuotaExceeded`）。
- **消费与剩余额度**：同一页展示今日已用 tokens、剩余额度、今日调用次数、按天趋势、
  按来源（诊断 / 代码分析）与按用户 Top10 —— 数据来自诊断记录表与代码分析表（真实表名
  `a_idiagnoses` / `ai_code_analyses`，由 `model.TableNameOf` 推导，见 INC-019）的
  `cost_tokens`，都是真实调用记录，不做估算。
- **一键验证**：页面上有「测试连接」，用当前生效配置发一次最小请求并返回耗时。

`.env` / `configs/config.yaml` 里的 `ai_engine.*` **仅作首次启动的一次性导入**：平台启动时若
`platform_settings` 里还没有 `ai` 记录，会把当前非空值导入并写一条启动日志；此后以平台内的配置为准，
改环境变量不会再生效（想跳过导入就留空）。

部署拓扑、熔断与降级链（连续失败 2 次 → self_hosted → 规则引擎）仍由配置决定：

```yaml
ai_engine:
  fallback: { failure_threshold: 2, open_duration: 60s, rules_engine_enabled: true }
  third_party:
    timeout: { connect: 5s, first_byte: 15s, total: 60s, tool_call: 10s, task_deadline: 120s }
```

### 4.3 接入本地 LLM（Ollama / vLLM）

同样在「AI 设置」里填 `self_hosted`：协议 `openai`、base_url `http://ollama.internal:11434/v1`、
模型 `qwen2.5:7b`、启用即可；API Key 一般留空。

### 4.4 启用 pgvector 原生向量

```bash
# 1) 构建镜像时开启构建标签
GO_BUILD_TAGS=pgvector docker compose build backend && docker compose up -d backend
# 2) 后端迁移时会执行：CREATE EXTENSION / ALTER COLUMN TYPE vector(768) / HNSW 索引
```

未启用时向量以文本存储，检索走应用层余弦相似度（复用同一份 768 维本地嵌入），知识库规模可控时性能足够。

### 4.5 配置通知渠道（在平台内完成，不再改 .env）

**登录平台 →「通知渠道」**：四张卡片（飞书 / 企微 / 钉钉 / 邮件）各自启用并填 webhook、
签名密钥、@成员（邮件填 SMTP 主机/端口/账号/口令/收发件人），每个渠道都有「发送测试」按钮。

| 渠道 | 平台内配置项 | 说明 |
|------|------|------|
| 飞书 | webhook + 签名密钥（可选）+ @成员 | 交互式消息卡片，含「查看详情」按钮 |
| 企业微信 | webhook + @成员 | Markdown 消息 |
| 钉钉 | webhook + 签名密钥 + @成员 | Markdown 消息（备选） |
| 邮件 | SMTP host/port/账号/口令/from/to + TLS | 低优先级 |

- **密钥安全**：与 AI 设置同一套机制——整体加密落库、接口只回显掩码、输入框留空＝不修改。
- `.env` 里的 `FEISHU_*` / `WECOM_*` 等同样只作**首次启动的一次性导入**，之后以平台内配置为准。

**边界**：卡片仅支持「查看详情 / 确认 / 驳回」，**不支持一键执行**；高危操作统一回 Web 端执行（设计文档 6.2）。

### 4.6 日志集成（Filebeat → 平台 Kafka）

日志采集不再由平台侧完成（旧的「平台起采集容器」实现与那个采集二进制已整体删除，见 [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md)）。
现在的做法是：集成中心新建 **`log` 类型集成**（模板「日志 / Filebeat」），
平台用 **Ansible 在目标服务器上幂等部署 Filebeat**，Filebeat 把日志推到**平台自带的 Kafka**，
后端按消费组 `mwops-log-ingest` 消费 topic `mwops-logs`，复用既有日志事件链路。

**完整说明（拓扑、字段、幂等部署规则、自检环节、配置项）见
[`LOG_INTEGRATION.md`](LOG_INTEGRATION.md)**，这里只列运维要点：

- **平台侧组件**：`docker-compose.yml` 里的 `kafka` 服务（`apache/kafka:3.8.0`，KRaft 单节点、无 ZooKeeper），
  容器名 `mwops-kafka`；平台后端用 INTERNAL 监听器 `kafka:29092` 消费；
- **目标机侧**：不要求有 Docker（`auto` 模式会在 deb/rpm + systemd 与官方容器之间自动选择）、
  不要求出网（可用平台包分发）、也**不需要平台挂载 `docker.sock`**——
  它走 SSH + Ansible，所以关掉 `INTEGRATION_DOCKER_ENABLED` 后日志集成依然可用；
- **核对目标机**（自检报红时在目标机上执行，按安装方式选）：

  ```bash
  systemctl status filebeat              # docker 模式：docker ps | grep mwops-filebeat
  filebeat test config                   # 配置文件是否合法
  filebeat test output                   # 能否连上平台 Kafka（抓 advertised 地址配错）
  nc -vz <KAFKA_ADVERTISED_HOST> 9092    # 平台对外地址是否可达
  timedatectl                            # 时钟是否同步（偏移过大被 Kafka 拒收）
  ```

- **重复点集成不会重复安装**：`auto` 模式先探测已装的 Filebeat 并复用，只在渲染后的
  `filebeat.yml` 内容变化时才重启（`systemctl restart filebeat` / 容器重建）。
- **「覆盖 Filebeat」开关（默认关闭）**：关闭时目标机上已装的 Filebeat 一律不动（不重新下载安装包、
  本地已有镜像也不重新 `docker pull`），只有缺失时才安装；打开后会重新拉取安装包/镜像并**强制覆盖安装**
  （`apt-get --reinstall` / `dnf reinstall` / 重新 `docker pull` + 重建容器），用于升级版本或修复装坏的 Filebeat。
  注意两点：① 配置文件与这个开关**无关**，始终按平台渲染内容同步（内容没变连重启都不会发生），
  所以改日志路径或 Kafka 地址不必打开它；② 显式选了 `package` / `docker` 时以选定方式为准，"复用"只在
  `auto` 模式下才可能出现（INC-029）。

`POST /api/hooks/logs`（应用 HTTP 直推）作为零侵入兜底保留，字段与 Filebeat 路径统一映射：
`service` / `level` / `message` 必填，`log_path` 记录日志来自哪个文件。

### 4.7 平台自管设置的接口与权限

AI 设置与通知渠道共用一个后端分组（密钥整段加密存在 `platform_settings` 的 `payload_encrypted`）：

| 接口 | 权限点 | 说明 |
|------|--------|------|
| `GET /api/settings/ai` | `system:config` | 读 AI 设置：密钥只回 `api_key_set` / `api_key_masked` |
| `PUT /api/settings/ai` | `system:config:write` | 保存并**立即生效**（重建引擎 + 热更新护栏配额） |
| `GET /api/settings/ai/usage?days=30` | `system:config` | token 消费/剩余额度（`days` 上限 90；`remaining_today=-1` 表示不限额） |
| `POST /api/settings/ai/test` | `system:config:write` | 发一次最小请求自检；失败返回 200 + `ok=false` + 原因（不是 500） |
| `GET /api/settings/notify` | `system:config` | 读通知渠道：webhook 只回掩码、签名密钥与 SMTP 口令只回 `*_set` |
| `PUT /api/settings/notify` | `system:config:write` | 保存并立即生效（通知服务原子换配置，不重启） |
| `POST /api/settings/notify/test` | `system:config:write` | `{"channel":"feishu/wecom/dingtalk/email"}` 发测试消息 |

写法约定（三条，改接口时不要破坏）：

1. **空串＝不修改**：`api_key` / `webhook` / `secret` / `password` 留空表示保持原值——
   界面不回显明文，若把"没动输入框"当成清空，只改模型名就会把密钥删掉；要清空必须显式传
   `clear_api_key` / `clear_webhook` / `clear_secret` / `clear_password`；
2. **密钥永不出现在响应、日志与审计里**：审计只记 `key_changed` / `key_cleared` 这类布尔；
   `base_url` 里的 userinfo（`https://user:pass@host`）会先脱敏再入库；
3. **额度与消费都是真实数据**：直接聚合诊断记录表与代码分析表的 `cost_tokens`（真实表名
   `a_idiagnoses` / `ai_code_analyses`，由 `model.TableNameOf` 推导，见 INC-019），
   没有数据就是 0，不做估算也不补数（同 INC-016 的原则）。

`postgres` 侧只有一张表 `platform_settings`（`name` 唯一，值为整段 AES-256-GCM 密文）：
主密钥（`security.master_key` / `master.key`）一旦更换，旧密文解不开，接口会**显式报错**
而不是悄悄回退 `.env`——避免"设置看着没了，再保存一次把旧值覆盖掉"。

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
| 监控页面显示「无数据源」/指标显示「无数据」 | 未配置或连不上 `prometheus.base_url`；检查 Prometheus 容器与 `/-/healthy` |
| AI 诊断为「规则引擎」结论 | 第三方/本地引擎均不可用；检查 `ai_engine.*` 配置、API Key、出网连通性、熔断状态（`/api/system/info`） |
| 诊断报 4033（需要审批） | 目标动作是 L2；到「审批管理」创建/审批工单后凭 `ticket_id` 执行 |
| 代码分析提示「不在出网白名单」 | 需在「服务器与仓库」中为该服务开启 `allow_third_party`，并在 `security.outbound_whitelist` 中加入服务名 |
| 日志收到了但**不通知 / 不分析** | 见 5.10：① 规则没命中 → 看「日志告警规则」页顶部给出的平台默认值；② 冷却中 → 看事件的 `cooldown_until` / `suppressed`；③ `analysis_state=disabled/failed` → 看 `analysis_error` |
| 日志事件 `analysis_state=failed`，`analysis_error` 以「拉取代码失败：repo: git …」开头 | 平台拉代码失败：认证过期 / 分支不存在 / 出网或 DNS 不通 / 磁盘满 / `code_repo.allow_outbound=false`。中文结论就在 `analysis_error` 里（见 5.10） |
| 日志事件 `analysis_state=disabled`，`analysis_error` 写「服务 X 未配置代码仓库映射」 | 该服务没在「服务器与仓库」里登记映射（服务名要与日志的 `service` 完全一致）；补上后对该条点「重新分析」 |
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

`scripts/onboard.sh` 第 5 步已经内建了 ①：检测到数据卷已存在时**不会轮换** `DB_PASSWORD`，
并在启动前用 `pg_isready` + TCP `scram` 认证核对口径，不一致就自动 `ALTER USER` 对齐。

> 同理，`DB_USER` / `DB_NAME` 也是**卷初始化时定死的**：改了 `DB_USER` 会报
> `role "xxx" does not exist`，此时只能改回原值或重建数据卷。

### 5.9 日志总线 Kafka 的启停、健康检查与排障

日志集成（目标机 Filebeat → 平台 Kafka → 平台消费）依赖 compose 里的 `kafka` 服务。
平台后端**没有 Kafka 也能启动**：未配置 `kafka.brokers`（或 `kafka.enabled=false`）时不启动消费者，
日志页的「Kafka 采集链路」卡片给出「未配置 Kafka：日志集成不可用 / 日志总线已关闭」并附原因，
其余功能不受影响（降级只减少展示，不伪造数据）。

**启停与健康检查**

```bash
cd <nightjar>

# 启动/重启日志总线
docker compose up -d kafka
docker compose restart kafka

# 健康检查：容器状态 + topic 列表（健康检查用的就是同一条命令）
docker compose ps kafka
docker exec mwops-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:29092 --list
# 期望输出里含 mwops-logs；没有就说明 topic 还没被创建（平台后端启动时会幂等创建）

# 平台侧 brokers 是否真的连得上（容器内没有 nc，直接问 Kafka 要元数据）
docker exec mwops-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:29092 --list >/dev/null \
  && echo "INTERNAL kafka:29092 OK"

# 页面/接口视角：消费链路状态与探测
TOKEN=<登录后的 JWT>
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8000/api/log-alerts/pipeline | jq
curl -s -X POST -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8000/api/log-alerts/pipeline/probe | jq
```

**topic 与保留时长**

| 项 | 取值 | 说明 |
|---|---|---|
| topic | `mwops-logs`（`KAFKA_LOG_TOPIC`） | 平台后端启动时幂等确保存在，`KAFKA_AUTO_CREATE_TOPICS_ENABLE=true` 是双保险 |
| 分区数 | `KAFKA_NUM_PARTITIONS`（默认 3） | 也是平台消费的并行度上限 |
| 保留时长 | `KAFKA_LOG_RETENTION_HOURS`（默认 72） | 改完需重建容器生效：`docker compose up -d kafka` |

```bash
# 核对 topic 的生效配置（含 retention.ms）
docker exec mwops-kafka /opt/kafka/bin/kafka-configs.sh --bootstrap-server localhost:29092 \
  --entity-type topics --entity-name mwops-logs --describe

# 查看消费组积压（Lag 持续不降 = 平台消费跟不上或消费者没起来）
docker exec mwops-kafka /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:29092 \
  --describe --group mwops-log-ingest
```

**`KAFKA_ADVERTISED_HOST` 配错的现象与修法**

- 现象：目标机上 `systemctl status filebeat` 正常、Filebeat 也能连上 9092 握手成功，但**随后立刻断开**，
  日志里报 `dial tcp 127.0.0.1:9092: connect: connection refused`；平台日志页始终没有该 server 的事件。
  根因是 broker 把客户端引导去了它自己被通告的地址——通告成 `localhost`/`127.0.0.1`（或容器名 `kafka`）时，
  别的机器上的 Filebeat 就会去连**它自己**的 127.0.0.1。
- 修法：

  ```bash
  # 取宿主机 IP（被管机能访问到的那个地址）
  ip route get 1 | awk '{print $7; exit}'      # 或 hostname -I | awk '{print $1}'
  # 写进 .env
  #   KAFKA_ADVERTISED_HOST=<上面的 IP 或域名>
  docker compose up -d kafka                   # 重建 kafka，让 advertised listeners 生效
  # 在**目标机**上复验
  nc -vz <KAFKA_ADVERTISED_HOST> 9092
  filebeat test output
  ```

  集成中心对该日志集成点**自检**，第 2 段「被管机接入地址（Kafka EXTERNAL）」就是查这个地址。
  只有平台与被管机在同一台机器时，`localhost` 才成立。

- **平台现在会在执行前就拒绝这个必然失败的组合**（INC-028）：目标是**远程**被管机、而 EXTERNAL 地址仍是
  回环（`127.0.0.1` / `localhost` / `::1`）或容器内服务名（`kafka`）时，**预览 / 部署 / 重新应用**
  会在渲染 `filebeat.yml` 之前直接失败并提示改 `KAFKA_ADVERTISED_HOST` 后
  `docker compose up -d kafka backend`。
  这样做的理由是：这类配置**必然**失败，但失败点在对方机器的 Filebeat 上（报错在 `dial tcp 127.0.0.1`
  这种极具误导性的形态），让它在平台侧当场拒绝比事后到目标机排障便宜得多。
  **本机目标不需要也不应该改成公网地址**——此时回环地址是正确配置（EXTERNAL 端口已发布到宿主）。

**磁盘占用与清理**

- Kafka 只做**削峰与解耦**，不承担长期留存：日志的长期留存由平台的事件表
  （`log_alert_events`，含 `log_path`）与告警记录负责，所以保留时长可以设得很短（默认 72h）。
- 磁盘大头是 `kafka-data` 卷；占用异常时按顺序处理：

  ```bash
  docker system df -v | grep -i kafka          # 看卷占用
  # 1) 缩短保留时长（治本）：.env 里调小 KAFKA_LOG_RETENTION_HOURS → docker compose up -d kafka
  # 2) 让清理立刻发生：Kafka 的日志段清理是周期性的，缩短后等一个 log.retention.check.interval
  # 3) 确认没有消费积压再动手（见上面的 kafka-consumer-groups.sh）
  ```

  平台侧的事件表清理按平台保留策略执行；**不要**用 `docker compose down -v` 清 Kafka——
  那会连同 PostgreSQL 数据卷一起删掉（见 5.6 / 5.8）。

**日志集成不需要 `docker.sock`**

- 日志集成走 **SSH + Ansible** 到目标机装 Filebeat，既不起平台侧采集容器、也不读对方的 docker 配置，
  因此平台**不需要挂载 `/var/run/docker.sock`**，`INTEGRATION_DOCKER_ENABLED=false` 也不影响它。
- 这与「指标集成的一键拉起 Exporter」是两条独立通道：后者需要 `docker.sock`，前者不需要。
  只有平台侧仍要一键拉起 Exporter 时才需要挂 socket（见 `.env.example` 的 `INTEGRATION_DOCKER_ENABLED` 说明）。

### 5.10 日志告警后处理与代码仓库缓存

日志事件的落库只是第一步：**通知渠道与 AI 代码分析由后处理（`LogAlertWorker`）完成**，
它由定时任务驱动（间隔 `log_alert.worker_interval_seconds`，默认 15s；每轮 `log_alert.worker_batch`，默认 10），
扫描 `analysis_state=pending` 的事件，先外发通知（写 `notified_at`），再按事件的服务名拉代码 + 做 AI 三点式分析。

**为什么是定时任务而不是采集路径上同步做**：AI 要拉代码、调 LLM，耗时几十秒；放同步路径会把 Kafka
消费拖慢（位点积压 → 整条日志链路延迟）。状态落在数据库（`analysis_state`）里，所以**进程重启不丢**；
多副本部署时用「带条件的 UPDATE 抢占」（`pending → running`）保证一条事件只被处理一次。

#### 排查：日志收到了但不通知 / 不分析

按下面三条顺序定位（页面上就能看到，不用先翻数据库）：

| 现象 | 看哪里 | 含义与处理 |
|---|---|---|
| 没通知 | 「日志告警规则」页顶部的**平台默认值**卡片（`GET /api/log-alerts/rules/defaults`） | 日志没命中任何启用规则时，按 `log_alert.default_*`（去重窗口 5 分钟 / 冷却 10 分钟 / AI 开 / 渠道 = 平台已启用渠道）处理。规则匹配是「priority 小者优先、同优先级按 id 升序、取第一条命中」——最常见的错是规则建了但 `enabled=false` |
| 没通知 | 事件列表的「抑制 / 通知」列：`suppressed=true` + `cooldown_until` | 处于**冷却期**：事件已记录，只是不重复打扰。想立刻拿到结论，对该条点「**重新分析**」（会清掉冷却记录，立即重跑通知与 AI） |
| 没分析 | 事件列表 / 详情的「AI 分析」列：`analysis_state` 与 `analysis_error` | `disabled` = 规则关了 AI、或该服务没配仓库映射（原因写在 `analysis_error`，照做即可）；`failed` = 拉代码或调用 AI 失败（原因写在 `analysis_error`）；`pending/running` 停留过久 = 后处理没在跑（见下）；`done` = 已有结论 |

数据库直查（排查"是不是只卡了某几条"）：

```bash
docker exec mwops-postgres psql -U mwo -d middleware_ops -c \
 "SELECT analysis_state, count(*) FROM log_alert_events GROUP BY 1 ORDER BY 2 DESC;"

# 最近 20 条异常：失败/跳过原因一目了然
docker exec mwops-postgres psql -U mwo -d middleware_ops -c \
 "SELECT id, service_name, analysis_state, analysis_error, cooldown_until, notified_at
    FROM log_alert_events WHERE analysis_state IN ('failed','disabled')
   ORDER BY id DESC LIMIT 20;"
```

`pending` 数量长期不降 = 后处理没在跑或跑不动：`docker compose logs backend | grep -i "日志告警后处理"`
（正常每轮处理完会打一条 `日志告警后处理完成 processed=N`）；`processed=0` 且 `pending` 不减，
查数据库连通性与 `log_alert.worker_*` 配置；AI 分析慢就调大间隔/减小批量，别让它与诊断抢并发。

#### 代码仓库缓存目录：磁盘占用与清理

平台为了回答「这条日志对应哪一行代码」，必须持有一份**与服务当前版本一致**的代码：
首次分析 **clone** 到本地缓存，之后每次分析只**更新到远端**，不重复 clone
（`internal/repo`：分支非空走 `fetch --prune` + `checkout --force -B <branch> origin/<branch>`，
分支为空走 `pull --ff-only`）。

| 项 | 说明 |
|---|---|
| 目录 | `code_repo.cache_dir`（默认 `./data/repos`），容器内即 `/app/data/repos`，落在 **`backend-data` 卷**里 |
| 布局 | `<cache_dir>/<服务名>`，每个服务一个子目录（服务名会被净化为单层安全目录名） |
| 增长 | 与「仓库数 × 仓库体积 × 分支历史深度」成正比：缓存是**全量克隆**（不做浅克隆），大仓库很占空间 |
| 观察占用 | `docker system df -v \| grep -i backend-data`，或 `docker exec mwops-backend du -sh /app/data/repos/*` |
| 清理单个服务 | `docker exec mwops-backend rm -rf /app/data/repos/<服务名>`；**下次分析会自动重新 clone**（不影响数据库里的映射与历史事件） |
| 清理全部 | 删掉整个 `repos` 目录即可；注意**不要**用 `docker compose down -v`（会连库一起删，见 5.6 / 5.8） |
| 空间不足的报错 | `analysis_error` 里会出现「磁盘空间不足：请清理仓库缓存目录…」 |
| 目录已存在但不是 git 仓库 | 平台**拒绝覆盖**并报错（可能是别人的目录）：确认可清理后手工删除该子目录再重试 |

#### git 凭据与脱敏

- 私有仓库用 **HTTPS + 只读访问令牌**（URL 形如 `https://oauth2:<token>@gitlab.internal/group/repo.git`）
  或 **SSH 部署密钥**；令牌只需 `read_repository` 之类的读权限，并纳入轮换。
- **URL 内嵌的凭据会被脱敏**：userinfo 段（`user:token@`）与 `?token=` / `?access_token=` / `?private_token=`
  这类查询参数在平台日志、错误信息与页面上统一替换为 `***`（`internal/repo/redact.go`），
  连 git 回显的 stderr 也先脱敏再截断（≤400 字符）后才落库/打日志。
- 平台以服务方式运行、**关闭了交互式输入**：凭据不对时 git 不会弹窗等待，而是直接失败并给出
  「认证失败：请检查凭据是否有效/未过期…」的中文结论。
- 真实凭据在数据库里仍是明文存储（`code_repos.repo_url`），因此**数据库与备份同等敏感**，按 5.1 的备份策略保护。

#### `code_repo.allow_outbound=false` 的后果

这是"平台能否访问代码托管"的总开关（`CodeRepo.allow_third_party` 管的是另一件事：**能否把代码片段
发给第三方 AI**，两者刻意分开）。设为 `false` 后：

- 平台**不执行任何 git 命令**（连路径都不解析，零副作用），日志里会出现
  「拒绝拉取远端代码：出网许可未开启（合规开关）」；
- 命中的日志事件**照常记录、照常外发通知、照常累加计数**，只是 `analysis_state=failed`，
  `analysis_error` 写明「拉取代码失败：…未开启第三方代码出网许可…」；
- AI 代码定位**不可能产出结论**（没有代码就无法定位行）；修复建议/根因这类只在代码上下文里有意义的
  结论随之中断——但中间件指标诊断、告警、通知完全不受影响。

所以：**只有当合规上不允许平台访问代码托管时才关它**；只是不想用第三方 AI 时，
请保持 `allow_outbound=true` 并让 `allow_third_party` / `security.outbound_whitelist` 保持关闭
（此时走本地分析），否则内网仓库也拉不下来，AI 代码分析功能会整体形同虚设。

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
