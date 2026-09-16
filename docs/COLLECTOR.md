# 中间件接入与采集操作文档

> 面向「如何把**其他项目/其他机器**上的中间件接入本平台，并让平台持续采集、监控、AI 分析」的实操手册。
> 适用版本：v1.0（实现基线见仓库 `middleware-ops/`）。

---

## 0. 先明确边界：平台做什么、不做什么

这一节请务必先读。设计上平台**刻意不做**中间件协议级采集器，避免重复造轮子并与 Prometheus 生态重复建设。

| 能力 | 平台是否具备 | 真实实现方式 | 代码位置 |
|------|--------------|--------------|----------|
| 中间件指标采集 | ❌ 平台不直连中间件取指标 | 由 **官方 Exporter** 暴露，平台通过 **PromQL 查询 Prometheus** | `internal/monitor/prometheus.go` |
| 连接可用性探测 | ⚠️ 仅 **TCP 端口连通性** | `net.DialTimeout` 探测 host:port，非协议级握手 | `internal/service/middleware.go: probe()` |
| 应用日志采集 | ✅ | HTTP Hook 推送 / 轻量 Agent tail 文件 / 复用 Filebeat 等 | `/api/hooks/logs`、`cmd/agent` |
| 中间件运行态配置 | ⚠️ 读的是**平台侧登记的配置**，不是从中间件实时读取 | 诊断时读取实例的 `config` 字段（纳管时填写） | `internal/service/diagnose.go: collectConfig()` |
| 阈值告警 | ✅ | 平台按 PromQL 取当前值 → 比对规则阈值 → 指纹收敛 → 通知/触发 AI | `internal/service/alert.go` |
| AI 根因分析 | ✅ | 采集上下文（指标摘要 + 日志指纹 + 登记配置 + 知识库）→ 一次 LLM 调用 → 结构化报告 | `internal/service/diagnose.go` |

**因此有一条硬性前置条件：被管中间件必须能通过 Exporter 暴露指标，并被某个 Prometheus 抓到。**
若某中间件没有官方 Exporter（例如自研组件），平台无法凭空获得它的指标——可按第 7 节方案自行适配。

> 关于「连接测试」的准确表述：当前 `POST /api/middlewares/:id/test` 只验证**TCP 可达性与耗时**，
> 不会用账号密码做协议级登录校验。密码已按 AES-256-GCM 加密存储（`internal/utils/crypto.go`），
> 但目前仅用于存储，尚未用于协议级连接——这一点在验收时请勿按「已校验凭据」理解。

---

## 1. 数据流总览：五类数据从哪里来

```text
                    ┌─────────────── ① 指标（主链路） ───────────────┐
其他项目的中间件 ──▶ 官方 Exporter ──▶ Prometheus ──PromQL──▶ 平台监控/告警
 (Redis/Kafka/…)      :9121/:9308/…      :9090                  │
                                                                ▼
应用服务 ──② 日志(HTTP Hook / Agent)──▶ /api/hooks/logs ──▶ 指纹收敛 ──▶ 日志告警
                                                                │
平台纳管记录 ──③ 登记配置(config 字段)──────────────────────────┼──▶ AI 诊断上下文
知识库 ──────④ 历史案例(向量检索)───────────────────────────────┤   （六道护栏约束）
平台侧 ──────⑤ 平台自身指标(/metrics，由平台 Prometheus 抓)─────┘
```

| 编号 | 数据 | 来源 | 采集方式 | 是否需要 Exporter |
|------|------|------|----------|-------------------|
| ① | 性能/资源/可靠性指标 | 中间件官方 Exporter | 平台拉 Prometheus（PromQL） | **需要** |
| ② | 应用 ERROR 日志、堆栈、GC | 应用自身 | 应用推送 Hook 或 Agent tail 文件 | 不需要 |
| ③ | 连接信息、关键配置项 | 纳管表单 | 人工登记（`config` 字段） | 不需要 |
| ④ | 历史相似故障案例 | 平台知识库 | 诊断沉淀 + 人工录入 | 不需要 |
| ⑤ | 平台自检指标 | 平台后端 | Prometheus 抓 `/metrics` | 不需要 |

---

## 2. 部署场景与前置条件

### 2.1 三种典型拓扑

| 场景 | 适用 | Exporter 部署位置 | Prometheus | 说明 |
|------|------|-------------------|------------|------|
| **A. 同机共栈**（最快验证） | 单机/测试 | 与平台同一 compose | 平台自带 | 用 `deploy/compose.middleware-exporters.yml` 直接起，见 2.3 |
| **B. 中间件在别的机器**（最常见） | 生产 | 部署在**中间件所在主机**（或能访问它的机器） | 平台自带 Prometheus 抓取 Exporter | 只需确保 Prometheus 能访问 Exporter 的 `:9121` 等端口 |
| **C. 已有 Prometheus**（推荐生产） | 已建成监控体系 | 复用既有 Exporter | **复用既有 Prometheus** | 平台只需把 `prometheus.base_url` 指向它，无需新起 |

### 2.2 网络与端口要求

| 方向 | 源 → 目标 | 端口 | 用途 |
|------|-----------|------|------|
| 出站 | Prometheus → Exporter | Exporter 端口（9121/9308/9104/9187/9114/9113） | 拉取指标 |
| 出站 | Exporter → 中间件 | 中间件端口（6379/9092/3306/5432/9200/80） | Exporter 采集 |
| 入站 | 平台后端 → Prometheus | Prometheus `:9090` | 平台执行 PromQL |
| 入站 | 应用 → 平台后端 | 平台 `:8080` | 日志 Hook 上报 |
| 出站 | 平台后端 → LLM（可选） | 443 | AI 诊断/代码分析（受出站白名单约束） |

平台**不需要**直连中间件端口（除了 TCP 健康探测，可关）。

### 2.3 场景 A：一条命令起齐全套 Exporter

```bash
# 前提：平台已用根目录 compose 起好
docker compose -f docker-compose.yml -f deploy/compose.middleware-exporters.yml up -d
```

该 override 会拉起 redis / mysql / pg / kafka / es / nginx 六个官方 Exporter，
并把平台自带的 Prometheus 换成含这些抓取任务的配置。
被管中间件地址通过环境变量指定，例如：

```bash
cat > .env <<'EOF'
REDIS_TARGET_ADDR=redis://10.0.0.11:6379
REDIS_TARGET_PASSWORD=your-readonly-password
REDIS_INSTANCE_NAME=redis-dev-01        # 会作为 instance_name 标签，务必与纳管实例名一致
MYSQL_... 见文件注释
EOF
docker compose -f docker-compose.yml -f deploy/compose.middleware-exporters.yml up -d
```

### 2.4 场景 B/C：只准备 Exporter 与抓取配置

在**中间件所在主机**部署对应 Exporter（不要暴露到公网）。以 Redis 为例：

```bash
docker run -d --name redis-exporter --restart unless-stopped \
  -p 9121:9121 \
  -e REDIS_ADDR=redis://127.0.0.1:6379 \
  -e REDIS_PASSWORD='<只读账号密码>' \
  -e SERVICE_NAME=redis-prod-order \        # 作为 instance_name 上报
  oliver006/redis_exporter:v1.66.0
```

然后在 Prometheus 侧增加抓取任务（场景 B 用平台自带 Prometheus 时，改 `deploy/prometheus/prometheus.yml`；
场景 C 在你自己的 Prometheus 里加）：

```yaml
  - job_name: middleware-exporter-redis       # 命名约定：<prometheus.exporter_job_prefix>-<类型>
    static_configs:
      - targets: ['10.0.0.11:9121']
        labels:
          instance_name: redis-prod-order      # 平台按此匹配实例
```

两个可选但推荐的 Exporter 参数：

- `oliver006/redis_exporter`：加 `--redis-only-metrics` 让集合贴近平台画像（非必需，不影响正确性）；
- `danielqsj/kafka-exporter`：**不提供** `instance_name` 标签，必须在 Prometheus 里用
  `relabel_configs` 补上，否则平台匹配不到该实例（`deploy/prometheus/prometheus.with-exporters.yml` 已给出示例）。

---

## 3. 在平台上纳管实例（关键：标签必须对上）

「纳管」= 告诉平台「这个中间件叫什么、在哪、指标在 Prometheus 里怎么找」。

**操作路径**：登录 → 中间件纳管 → 新增实例。表单字段与用途：

| 字段 | 是否必填 | 作用 |
|------|----------|------|
| 实例名称 | ✅ | 也是默认的 `instance_name` 匹配值，**建议与 Exporter 的 `SERVICE_NAME` 完全一致** |
| 中间件类型 | ✅ | 决定用哪套指标画像（PromQL 模板） |
| 连接地址 / 端口 | ✅ | 健康探测（TCP）用；Exporter 若在别处，此处仍填**中间件自身**地址 |
| 监控账号 / 密码 | 建议填 | 密码 AES-256-GCM 加密存储；当前仅存储，未用于协议级连接 |
| 环境 | ✅ | dev/staging/prod，决定数据权限隔离与 L2 审批强制 |
| 分组 | 建议填 | 与数据权限配合（如 payment/order） |
| Prometheus job | 可选 | 填了就**精确按该 job 匹配**；不填则按 `<前缀>-<类型>` 兜底 |
| Prometheus instance | 推荐 | 填了就按 `instance="..."` 精确匹配，最稳（如 `10.0.0.11:6379`） |
| 配置（config） | 建议填 | **这部分会进入 AI 诊断上下文**，见第 5 节 |

### 匹配规则的权威说明

平台构造 PromQL 标签匹配串的逻辑（`internal/monitor/profile.go: buildSelector`）：

| 你填写的字段 | 生成的匹配条件 | 结果 |
|---|---|---|
| job + instance | `job="你的job",instance="10.0.0.11:6379"` | 最精确 |
| 只填 instance | `job="<前缀>-<类型>",instance="..."` | 精确（job 走默认） |
| 只填 job | `job="你的job",instance_name="<实例名>"` | 依赖实例名一致 |
| 都没填 | `job="<前缀>-<类型>",instance_name="<实例名>"` | 依赖 Exporter 上报 `SERVICE_NAME` |

**排查口诀**：监控页面显示「内置模拟器」= 没连上 Prometheus；
显示指标但全是 `unknown`/`0` = 连上了 Prometheus 但**标签没匹配到序列**，去 Prometheus 里用同样的标签查一次。

### 配置平台的 Prometheus 地址

```yaml
# configs/config.yaml 或环境变量 MWOPS_PROMETHEUS_BASE_URL
prometheus:
  base_url: http://prometheus:9090        # 留空则使用内置确定性模拟器
  exporter_job_prefix: middleware-exporter
  cache_ttl: 15s
```

---

## 4. 指标采集：类型 → Exporter → 平台指标契约

**平台按固定指标名与 PromQL 模板查询**，Exporter 需提供下表中的指标名。

<details>
<summary><b>Redis</b>（oliver006/redis_exporter）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `memory_usage_percent` | 内存使用率 | `redis_memory_used_bytes / redis_memory_max_bytes * 100` | 越高越差（警 70/严 85） |
| `connected_clients` | 连接数 | `redis_connected_clients` | 越高越差（800/1000） |
| `instantaneous_ops_per_sec` | QPS | `rate(redis_commands_processed_total[5m])` | 越高越差（20000/40000） |
| `keyspace_hit_rate` | 命中率 | `rate(hits)/(rate(hits)+rate(misses))*100` | 越低越差（<90 警 / <80 严） |
| `db_keys` | key 数量 | `sum(redis_db_keys)` | 越高越差（500w/1000w） |
| `slowlog_length` | 慢查询数 | `redis_slowlog_length` | 越高越差（10/50） |
| `evicted_keys` | 淘汰 key 数 | `rate(redis_evicted_keys_total[5m])` | 越高越差（1/20） |
| `blocked_clients` | 阻塞客户端 | `redis_blocked_clients` | 越高越差（1/5） |
</details>

<details>
<summary><b>Kafka</b>（danielqsj/kafka-exporter）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `broker_up` | Broker 状态 | `up` | 正常/异常（0=down 判严重） |
| `partition_count` | Topic 分区数 | `sum(kafka_topic_partitions)` | 纯观测 |
| `consumer_lag` | 消费者组 Lag | `sum(kafka_consumergroup_lag)` | 越高越差（1w/10w） |
| `produce_rate` | 生产速率 | `sum(rate(kafka_topic_partition_current_offset[5m]))` | 纯观测 |
| `consume_rate` | 消费速率 | `sum(rate(kafka_consumergroup_current_offset[5m]))` | 纯观测 |
| `under_replicated_partitions` | ISR 副本不足分区 | `sum(kafka_topic_partition_under_replicated_partition)` | 越高越差（1/10） |
</details>

<details>
<summary><b>MySQL</b>（prom/mysqld-exporter）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `qps` | QPS | `rate(mysql_global_status_queries[5m])` | 越高越差（8000/15000） |
| `tps` | TPS | `rate(mysql_global_status_questions[5m])` | 纯观测 |
| `threads_connected` | 连接数 | `mysql_global_status_threads_connected` | 越高越差（400/500） |
| `slow_queries` | 慢查询数 | `rate(mysql_global_status_slow_queries[5m])` | 越高越差（5/30） |
| `buffer_pool_hit_rate` | 缓冲池命中率 | `(1 - reads/read_requests)*100` | 越低越差（<95 警 / <90 严） |
| `replication_lag_seconds` | 主从延迟 | `mysql_slave_status_seconds_behind_master` | 越高越差（5/30） |
</details>

<details>
<summary><b>PostgreSQL</b>（prometheuscommunity/postgres-exporter，需 <code>pg_monitor</code> 角色）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `qps` | QPS | `rate(pg_stat_database_xact_commit[5m])` | 越高越差（6000/12000） |
| `tps` | TPS | `rate(pg_stat_database_xact_rollback[5m])` | 纯观测 |
| `connections` | 连接数 | `pg_stat_activity_count` | 越高越差（150/200） |
| `slow_queries` | 慢查询数 | `rate(pg_stat_statements_mean_time_seconds_count[5m])` | 越高越差（5/30） |
| `cache_hit_rate` | 缓存命中率 | `blks_hit/(blks_hit+blks_read)*100` | 越低越差（<95 警 / <90 严） |
| `lock_waits` | 锁等待 | `pg_locks_count` | 越高越差（5/20） |
</details>

<details>
<summary><b>Elasticsearch</b>（prometheuscommunity/elasticsearch-exporter）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `cluster_status` | 集群健康状态 | `elasticsearch_cluster_health_status` | 越低越差（≤1 警 / ≤0 严），2=green 正常 |
| `node_count` | 节点数 | `elasticsearch_cluster_health_number_of_nodes` | 越低越差（≤1 警 / ≤0 严） |
| `index_count` | 索引数量 | `elasticsearch_cluster_health_number_of_indices` | 纯观测 |
| `search_rate` | 搜索速率 | `rate(elasticsearch_indices_search_query_total[5m])` | 纯观测 |
| `indexing_rate` | 索引速率 | `rate(elasticsearch_indices_indexing_index_total[5m])` | 纯观测 |
| `jvm_heap_used_percent` | JVM 堆使用率 | `jvm_memory_used_bytes{area="heap"}/max*100` | 越高越差（75/85） |
| `unassigned_shards` | 未分配分片 | `elasticsearch_cluster_health_unassigned_shards` | 越高越差（1/20） |
</details>

<details>
<summary><b>Nginx</b>（nginx/nginx-prometheus-exporter，需开启 stub_status；仅监控不含 AI 诊断）</summary>

| 平台指标名 | 展示名 | PromQL 模板 | 状态方向 |
|---|---|---|---|
| `active_connections` | 活跃连接数 | `nginx_connections_active` | 越高越差（3000/5000） |
| `request_rate` | 请求速率 | `rate(nginx_http_requests_total[5m])` | 纯观测 |
| `error_rate_5xx` | 错误率(5xx) | `5xx 速率 / 总速率 * 100` | 越高越差（0.5/2） |
| `upstream_response_time` | 响应时间(P95) | `histogram_quantile(0.95, rate(..._bucket[5m]))` | 越高越差（500/2000） |
</details>

### 状态判定语义（很重要，配规则时会用到）

平台按 **阈值模式（`threshold_mode`）** 判定指标状态，而不是简单比较：

| 模式 | 语义 | 示例 |
|------|------|------|
| `higher_worse` | `≥ 严重线` → critical；`≥ 警戒线` → warning | 内存使用率、连接数、延迟 |
| `lower_worse` | `≤ 警戒线` → warning；`≤ 严重线` → critical（**含边界**） | 命中率（警 90/严 80）、ES 集群状态（警 1/严 0） |
| `bool_down` | `≤ 严重线` → critical，否则 ok | Broker up（严 0） |
| 空 | 纯观测，不判定 | QPS、TPS、速率类 |

判定结果可在「实例详情 / 统一监控」看到；指标目录接口 `GET /api/metrics/catalog?mw_type=redis`
会返回每项的 `threshold_mode`、`warning_threshold`、`critical_threshold`。

### 阈值告警的两种落地方式

1. **平台内规则**（推荐，默认）：新增「告警规则」→ 选实例 + 指标 + 操作符 + 阈值，
   平台按 `scheduler.metric_rule_eval_interval`（默认 30s）评估，命中后指纹收敛并通知/触发 AI。
   注意：平台规则用**你填的操作符**比较当前值，与画像阈值无关，所以可以直接写
   `keyspace_hit_rate < 90` 这类语义。
2. **Prometheus 规则**：用 `deploy/prometheus/rules/middleware.yml` 里的规则在指标侧先拦截，
   适合已有 Alertmanager 体系的团队；两套共用同一收敛策略，不会重复告警。

---

## 5. 让 AI 诊断「有据可依」：注入关键配置

AI 诊断的上下文**只有四个来源**（受上下文预算约束）：指标摘要、日志指纹、**登记的 config**、知识库参考案例。
其中指标与日志自动获得，而「中间件关键配置」需要你在纳管时填写——这直接决定根因分析的准确度。

**操作路径**：中间件纳管 → 编辑实例 → 配置（JSON）。建议按类型填写：

```jsonc
// Redis：诊断内存/淘汰类问题必需
{
  "maxmemory": "4gb",
  "maxmemory-policy": "allkeys-lru",
  "appendonly": "yes",
  "save": "900 1 300 10",
  "cluster-enabled": "no"
}

// Kafka：诊断积压/副本问题必需
{
  "num.partitions": "12",
  "default.replication.factor": "3",
  "min.insync.replicas": "2",
  "auto.offset.reset": "latest",
  "log.retention.hours": "168"
}

// MySQL / PostgreSQL
{
  "max_connections": "500",
  "innodb_buffer_pool_size": "8G",
  "slow_query_log": "ON",
  "long_query_time": "1"
}
```

> 为什么重要：AI 的每条结论必须引用证据（`evidence`），无证据的结论会被质量护栏标注为「推测」。
> 若 `config` 为空，像「maxmemory-policy 与业务写入模式不匹配」这类根因就只能给推测结论。

---

## 6. 日志采集：两种接入方式

指标只能回答「资源/性能是否异常」，定位到**代码级根因**需要日志与堆栈。

### 6.1 方式一：HTTP Hook（零侵入，推荐先跑通）

应用侧把 ERROR 上报到平台，只需一个 HTTP 请求：

```bash
curl -X POST http://<平台地址>/api/hooks/logs \
  -H 'Content-Type: application/json' \
  -H 'X-Hook-Token: <MWOPS_HOOK_TOKEN>' \
  -d '{
    "service": "order-service",
    "level": "ERROR",
    "message": "Order 10086 处理失败",
    "stacktrace": "java.lang.NullPointerException\n\tat com.demo.OrderService.process(OrderService.java:42)",
    "context_lines": "…错误前后 20 行…",
    "alert_type": "stack",
    "count": 1
  }'
```

响应中的 `signature` 为错误指纹，`merged=true` 表示与 5 分钟窗口内的既有事件合并。

可直接使用仓库内示例脚本（含单条、批量、扫描日志文件三种模式）：

```bash
pwsh -File scripts/hook-log-report.ps1 -BaseUrl http://127.0.0.1:8080
pwsh -File scripts/hook-log-report.ps1 -LogFile /var/log/order-service/error.log -TailLines 400
```

各语言框架的接入点（示意）：

| 技术栈 | 接入位置 |
|--------|----------|
| Java/Spring | 全局异常处理器 `@ExceptionHandler` / logback 自定义 Appender |
| Go | `recover()` 兜底 + `zap` 自定义 Core，ERROR 级别触发异步上报 |
| Python | `logging.Handler` 子类，过滤 `levelno >= ERROR` |
| Node.js | `process.on('uncaughtException')` + 日志库 transport |
| 通用兜底 | 复用 Filebeat/Fluentd，用平台 Agent 或 hook 转发 |

### 6.2 方式二：轻量 Agent（生产推荐）

Agent 常驻在**应用服务器**上，增量 tail 日志文件并上报：

```bash
# 1) 编译（可在任意机器交叉编译）
cd middleware-ops
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags '-s -w' -o mwops-agent ./cmd/agent

# 2) 配置（参考 cmd/agent/agent.example.yaml）
cat > agent.yaml <<'EOF'
platform_url: http://10.0.0.5:8080
hook_token: "<MWOPS_HOOK_TOKEN>"
service: order-service
environment: prod
server_name: order-app-01
position_file: /var/lib/mwops-agent/position.json
context_lines: 20
files:
  - path: /var/log/order-service/error.log
    alert_type: error
  - path: /var/log/order-service/gc.log
    alert_type: gc
EOF

# 3) systemd 托管
sudo tee /etc/systemd/system/mwops-agent.service >/dev/null <<'EOF'
[Unit]
Description=Middleware-Ops Log Agent
After=network-online.target
[Service]
ExecStart=/usr/local/bin/mwops-agent -config /etc/mwops-agent/agent.yaml
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
EOF
sudo systemctl enable --now mwops-agent
```

Agent 的两个可靠性设计（`cmd/agent/main.go`）：

- **偏移量持久化**：每个文件的读取位置写入 `position_file`，进程重启不重复上报；文件轮转（变小）自动从头读；
- **上报失败不推进偏移**：失败时下一轮重读同一批数据，保证日志不丢。

### 6.3 去重与告警风暴抑制

| 机制 | 说明 |
|------|------|
| 错误指纹 | 异常类名 + 错误消息模板（去时间戳/IP/数字/引号内容）+ 首个业务栈帧 |
| 窗口去重 | 5 分钟内同指纹合并为一条，累加 `error_count` |
| 冷却静默 | 告警发送后 `cooldown` 分钟内不再重复通知 |
| 语义聚类 | 离线批处理，相似告警仅**合并展示**，不修改告警状态（可撤销） |

---

## 7. 没有官方 Exporter 怎么办

平台靠「Prometheus 里存在符合约定的指标名」获取数据，因此你有三条路：

1. **自研 Exporter**（推荐）：按第 4 节表格里的指标名暴露 `/metrics`。
   实现要点：命名与单位一致；带 `job`/`instance_name` 标签；`instance_name` 与平台纳管实例名一致。
2. **Prometheus recording rules 做映射**：已有其他指标名时，用 recording rule 计算出平台期望的名字，例如
   ```yaml
   groups:
     - name: mwops-mapping
       rules:
         - record: my_redis_memory_usage_percent
           expr: (my_redis_used / my_redis_limit) * 100
   ```
   注意：平台指标名是固定的（如 `memory_usage_percent` 对应的 **PromQL 模板**是内置的），
   若无法产生原始指标名，需按第 3 条改画像。
3. **扩展指标画像**（需改代码）：在 `internal/monitor/profile.go` 的 `profiles` 中为该类型
   增加 `MetricSpec`（含 `Expr`、`Mode`、阈值、模拟器参数）。这是设计上预留的扩展点，
   改完 `go test ./internal/monitor/` 会校验阈值方向是否自洽。

---

## 8. 验证与排障

### 8.1 三步确认链路是否打通

```bash
# 第 1 步：Exporter 自己有数据吗？
curl -s http://<exporter-host>:9121/metrics | grep -E '^redis_(memory_used_bytes|connected_clients)'

# 第 2 步：Prometheus 抓到了吗？（用平台的标签约定查）
curl -s 'http://<prometheus>:9090/api/v1/query' \
  --data-urlencode 'query=redis_memory_used_bytes{job="middleware-exporter-redis",instance_name="redis-prod-order"}'

# 第 3 步：平台查得到吗？
TOKEN=<登录后的 JWT>
curl -s -H "Authorization: Bearer $TOKEN" 'http://<平台>/api/metrics/1' | jq '.data.source, .data.metrics[0]'
```

| 现象 | 定位 | 处理 |
|------|------|------|
| 监控页显示「内置模拟器」 | `prometheus.base_url` 为空或连不通 | 配置平台 Prometheus 地址；确认平台容器能访问 `:9090` |
| 有指标但值为 0 / `unknown` | PromQL 标签没匹配到序列 | 用第 2 步的查询核对 `job` / `instance` / `instance_name` 是否与纳管字段一致 |
| 只有部分指标有值 | Exporter 未暴露该指标 | 核对第 4 节表格的指标名；`curl exporter/metrics` 搜索 |
| Kafka 全部无数据 | kafka-exporter 无 `instance_name` 标签 | 在 Prometheus 抓取配置里用 `relabel_configs` 补标签 |
| ES 显示 warning | 集群为 yellow（1）属正常告警 | 若期望只在 red 告警，改用平台规则 `cluster_status < 1` |
| 日志页无事件 | Hook 401 或字段不合规 | 核对 `X-Hook-Token` 与 `MWOPS_HOOK_TOKEN`；`service`/`level`/`message` 必填 |
| AI 结论多为「推测」 | 上下文不足（config 为空 / 指标未采集） | 按第 5 节填写 config；确认指标链路已打通 |
| 代码分析提示「不在出网白名单」 | 合规默认禁止第三方分析 | 「服务器与仓库」中为服务开启 `allow_third_party`，并把服务名加入 `security.outbound_whitelist` |
| 连接测试通过但监控无数据 | 两者独立：前者是 TCP 探测，后者依赖 Exporter | 正常现象，按上表排查指标链路 |

### 8.2 端到端冒烟

```bash
# 后端全链路（含纳管、指标、诊断、告警、审计）
pwsh -File scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8080
```

---

## 9. 安全与权限要点

| 项 | 要求 |
|----|------|
| 中间件账号 | **只读最小权限**。MySQL：`PROCESS, REPLICATION CLIENT, SELECT`；PG：`pg_monitor`；Redis：ACL 只读 + 禁用 `FLUSHALL/KEYS` 等 |
| Exporter 暴露面 | 只监听内网，不要发布到公网（`deploy/compose.middleware-exporters.yml` 的端口映射仅便于验证，生产建议去掉 `ports`） |
| 连接密码 | 平台侧 AES-256-GCM 加密存储，主密钥来自环境变量或 0600 密钥文件 |
| 数据权限 | 纳管时填好「环境 + 分组」，平台在**仓储查询层**强制过滤，跨环境不可见 |
| 日志脱敏 | 上报前建议在应用侧先脱敏；平台的代码分析链路自带 IP/手机号/请求 ID/凭据脱敏 |
| 出网合规 | 默认**禁止**任何第三方 AI 分析；需按服务显式开启白名单（`security.outbound_whitelist` + 仓库映射开关） |
| 高危操作 | L2（清理 key / 重启 / 改配置 / SQL 写）一律走审批，30 分钟未审批自动拒绝 |

---

## 10. 接入检查清单

纳管一个中间件实例时，按此清单逐项确认：

- [ ] Exporter 已部署，且能 `curl :<exporter-port>/metrics` 看到指标
- [ ] Exporter 到中间件的网络与只读账号可用（Exporter 自身上报 up=1）
- [ ] Prometheus 抓取任务已加，`job` 命名与 `prometheus.exporter_job_prefix` 约定一致
- [ ] 抓取任务带 `instance_name` 标签（kafka-exporter 需 relabel）
- [ ] 平台 `prometheus.base_url` 指向正确的 Prometheus
- [ ] 平台纳管实例名 = Exporter 的 `SERVICE_NAME`
- [ ] 纳管时填写了「Prometheus instance」（最稳）或确认 job 兜底可用
- [ ] 正确选择环境与分组（影响数据权限与审批强制）
- [ ] 填写了 `config`（关键配置项）供 AI 诊断使用
- [ ] 实例详情页能看到指标、状态与阈值判定
- [ ] 按需创建告警规则，并点通知渠道标签发送自检消息
- [ ] 应用日志已通过 Hook 或 Agent 接入，日志页能看到事件
- [ ]（可选）在「服务器与仓库」登记服务→仓库映射，必要时开启出网白名单
