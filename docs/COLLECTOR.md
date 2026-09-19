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
| 应用日志集成 | ✅ | **Filebeat（平台用 Ansible 部署到目标机）→ 平台 Kafka**；应用也可 HTTP Hook 直推 | `internal/logpipe`、`internal/service/logpipeline.go`、`/api/hooks/logs` |
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
应用服务 ──② 日志(Filebeat → Kafka / HTTP Hook)──▶ 平台消费/接收 ──▶ 指纹收敛 ──▶ 日志告警
                                                    │  按「日志告警规则」做窗口去重 + 冷却抑制
                                                    └─▶ 后处理：通知渠道 + 拉代码 + AI 三点式结论
                                                                │
平台纳管记录 ──③ 登记配置(config 字段)──────────────────────────┼──▶ AI 诊断上下文
知识库 ──────④ 历史案例(向量检索)───────────────────────────────┤   （六道护栏约束）
平台侧 ──────⑤ 平台自身指标(/metrics，由平台 Prometheus 抓)─────┘
```

| 编号 | 数据 | 来源 | 采集方式 | 是否需要 Exporter |
|------|------|------|----------|-------------------|
| ① | 性能/资源/可靠性指标 | 中间件官方 Exporter | 平台拉 Prometheus（PromQL） | **需要** |
| ② | 应用 ERROR 日志、堆栈、GC | 应用所在服务器上的日志文件 | 集成中心「日志集成」用 Ansible 部署 Filebeat → 平台 Kafka；或应用 HTTP Hook 直推 | 不需要 |
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
| 入站 | 目标机 Filebeat → 平台 Kafka | 平台 `:9092`（`KAFKA_PORT`） | **日志集成主链路**：平台用 Ansible 装 Filebeat，它主动把日志推到平台 Kafka |
| 入站 | 平台后端 → 目标机 | 目标机 SSH（22） | 平台用 Ansible 幂等部署 Filebeat 与 Exporter |
| 出站 | 平台后端 → LLM（可选） | 443 | AI 诊断/代码分析（受出站白名单约束） |

平台**不需要**直连中间件端口（除了 TCP 健康探测，可关）。日志集成连的 `:9092` 是**平台自带的
Kafka**（`docker compose` 里的 `kafka` 服务），不是被管项目的 Kafka 端口——两者不要混淆。

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

**排查口诀**：监控页面显示「无数据源」或指标显示「无数据」= 没连上 Prometheus；
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

## 6. 日志集成：Filebeat → 平台 Kafka（主路径）

指标只能回答「资源/性能是否异常」，定位到**代码级根因**需要日志与堆栈。

日志采集已改为**日志集成**：平台在集成中心提供一个 `log` 类型模板，用 **Ansible 在目标服务器上幂等部署 Filebeat**，
Filebeat 把日志推到**平台自带的 Kafka**（`docker compose` 的 `kafka` 服务，KRaft 单节点），
平台后端按消费组 `mwops-log-ingest` 消费 topic `mwops-logs`，复用既有日志事件链路。

```
集成中心「日志集成」→ Ansible 装/复用 Filebeat（目标机）
        │  日志路径 glob + 级别过滤 + 多行合并
        ▼
目标机 Filebeat ──output.kafka(JSON)──▶ 平台 Kafka :9092
        ▼
平台后端消费组 mwops-log-ingest → 错误指纹 / 规则化窗口去重与冷却抑制 / 通知 / AI 代码分析入口
```

平台只提供 Kafka 与消费链路，**采集在被管侧自洽运行**：Filebeat 有本地缓冲与断点续传，平台重启/升级不影响采集；
日志集成**不需要平台侧 `docker.sock`**（它走 SSH + Ansible 到目标机），也不装 Exporter、不经过 Prometheus。

| 采集方式 | 适用场景 | 入口 | 是否需要 Exporter |
|---|---|---|---|
| **日志集成：Filebeat（平台用 Ansible 部署到目标机）→ 平台 Kafka** | 主路径：任意能被 SSH 到的服务器（物理机 / 容器 / K8s 节点），需要多行合并、级别过滤、背压与断点续传 | 集成中心 → 日志集成（`mw_type=log`） | 不需要 |
| HTTP Hook 直推 | 兜底：不能装 Filebeat、或只想推关键错误 | `POST /api/hooks/logs` | 不需要 |

### 6.1 操作：在集成中心新建 `log` 类型集成

> 完整的字段说明、幂等部署规则（已装则跳过安装、配置内容变化才重启）与日志集成的三段自检环节见
> [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md)，这里只给最短路径。

1. 平台 → **集成中心 → 日志 / Filebeat** 卡片 → **集成**，填：

   | 字段 | 示例 | 说明 |
   |---|---|---|
   | 名称 | `order-app-log` | 集成名，同时作为日志事件的服务标识与目标机上的 Filebeat 配置目录名（`/opt/mwops/filebeat/<集成名>/`，同一个目标机上可并存多个集成） |
   | 目标服务器 | `10.0.0.21`（本机填 `127.0.0.1`） | **目标机自身地址**；日志集成统一走 SSH 安装 |
   | 部署位置 | 远程服务器 / 本机 | 远程走 Ansible + SSH，与 Exporter 集成同一套凭据机制 |
   | 日志路径 | `/var/log/order-service/*.log` | 多个 glob 用换行或逗号分隔；**必须写目标机上真实存在的路径**，写错不报错、只会采不到 |
   | 服务名 / 环境 | `order-api` / `prod` | 写入事件字段 `service` / `environment`，用于日志页归集与筛选；服务名留空时回落为集成名，环境留空为 `dev` |
   | 最低级别 | `ERROR` | ERROR 只收错误行，WARN 收 ERROR+WARN，INFO 不过滤；过滤在目标机完成，能显著降低负载 |
   | 合并多行堆栈 | 开（默认） | Java / Python 堆栈合并成一条事件 |
   | 安装方式 | `package`（默认） | `package` = deb/rpm + systemd（默认，活动部件最少）；`auto` = 已装且 `systemctl is-active` → 复用，有 docker → 官方镜像容器，都没有 → deb/rpm；`docker` = 显式用容器。容器模式要额外注意容器运行用户、宿主数据目录属主与镜像约定的配置路径（见 LOG_INTEGRATION.md 与 POSTMORTEM INC-024/026） |
   | Filebeat 版本 | `8.16.0` | 与 `.env` 的 `FILEBEAT_VERSION` 一致 |

2. 保存后点该集成的 **自检**，三段环节要全绿：
   **① 平台 → Kafka 日志总线**、**② 被管机接入地址（Kafka EXTERNAL）**、**③ 日志是否已进入平台**。

前置条件只有两条：目标机能被平台 SSH 到（与 Exporter 集成同一套凭据），
以及目标机**能访问平台 Kafka 的对外地址**（`.env` 的 `KAFKA_ADVERTISED_HOST` + `KAFKA_PORT`，默认 `:9092`）。

### 6.2 在目标机上核对（自检报红时逐条走）

```bash
# 1) Filebeat 在不在跑、配置是否合法
systemctl status filebeat                     # docker 安装方式改为：docker ps | grep mwops-filebeat
filebeat test config -c /etc/filebeat/filebeat.yml

# 2) 能不能连上平台 Kafka（这一步专抓 advertised 地址配错）
filebeat test output
# 期望：kafka: <KAFKA_ADVERTISED_HOST>:9092... talk to server... OK

# 3) 平台侧对外地址是否真的可达（在目标机上执行）
nc -vz <KAFKA_ADVERTISED_HOST> 9092

# 4) 端到端：往被采集的路径里塞一行错误日志，数秒内平台日志页应出现事件
echo '2024-01-01 00:00:00 ERROR demo: boom' >> /var/log/order-service/error.log
```

三种安装方式的自检命令不同，别混用：

| 安装方式 | 服务状态 | 自检命令 | 配置路径 |
|---|---|---|---|
| `package`（deb/rpm + systemd） | `systemctl is-active filebeat` | `filebeat test config; filebeat test output` | `/etc/filebeat/filebeat.yml` |
| `docker`（官方镜像容器） | `docker ps \| grep mwops-filebeat` | `docker exec mwops-filebeat sh -c 'filebeat test config; filebeat test output'` | 宿主同一路径只读挂载进容器 |
| 复用目标机已有的 Filebeat | `systemctl is-active filebeat` | 同 `package` | `/etc/filebeat/filebeat.yml`（官方 unit 写死该路径） |

**幂等是设计目标，不是副作用**：平台先 `command -v filebeat` + `systemctl is-active filebeat` 探测，
已安装就只校验/下发配置、不重装；配置内容用渲染后的 `filebeat.yml` 内容哈希判定，
内容不变时不重启 Filebeat（避免每次重放都抖动采集）。所以**重复点集成不会重复安装，也不会重启**。

内网目标机不能出网时，走 [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md) §四的两条路：
平台侧包分发（`deploy/filebeat/packages/` + 受 hook token 保护的 `GET /api/hooks/filebeat/pkg`，目标机只需能访问平台 8000 端口），
或运维自行把包放到目标机（`filebeat.install_source=preinstalled`），平台只下发配置。

### 6.3 方式二：HTTP Hook 直推（应用侧零侵入兜底）

不能装 Filebeat 的应用（或只想推关键错误）直接把 ERROR 上报到平台，只需一个 HTTP 请求。
**这是唯一保留的「应用侧零侵入」兜底通路**，字段与 Filebeat 路径统一映射，最终落到同一张事件表：

```bash
curl -X POST http://<平台地址>/api/hooks/logs \
  -H 'Content-Type: application/json' \
  -H 'X-Hook-Token: <MWOPS_HOOK_TOKEN>' \
  -d '{
    "server_name": "order-app-01",
    "service": "order-service",
    "level": "ERROR",
    "message": "Order 10086 处理失败",
    "stacktrace": "java.lang.NullPointerException\n\tat com.demo.OrderService.process(OrderService.java:42)",
    "context_lines": "…错误前后 20 行…",
    "log_path": "/var/log/order-service/error.log",
    "alert_type": "stack",
    "count": 1
  }'
```

响应中的 `signature` 为错误指纹；`merged=true` 表示与**命中规则的去重窗口**（`dedup_window`，
默认 5 分钟）内的既有事件合并，`suppressed=true` 表示正处于冷却期（事件已记录但本次不通知、不触发 AI，
详见 6.5）。

脚本接入用 `curl` 即可（`X-Hook-Token` 取自平台 `.env` 的 `HOOK_TOKEN` / `MWOPS_HOOK_TOKEN`）：

```bash
# 单条上报（log_path 记录这条日志来自哪个文件）
curl -sS -X POST http://127.0.0.1:8080/api/hooks/logs \
  -H "X-Hook-Token: $MWOPS_HOOK_TOKEN" -H 'Content-Type: application/json' \
  -d '{"server_name":"order-app-01","service":"order-api","level":"ERROR",
       "message":"connection pool exhausted","log_path":"/var/log/order-service/error.log"}'

# 直接扫描日志文件尾部 400 行批量上报（每条一行 JSON 的数组）
tail -n 400 /var/log/order-service/error.log | while IFS= read -r line; do
  curl -sS -X POST http://127.0.0.1:8080/api/hooks/logs \
    -H "X-Hook-Token: $MWOPS_HOOK_TOKEN" -H 'Content-Type: application/json' \
    -d "$(printf '{"server_name":"order-app-01","service":"order-api","level":"ERROR","log_path":"/var/log/order-service/error.log","message":%s}' \
          "$(printf '%s' "$line" | python3 -c 'import json,sys;print(json.dumps(sys.stdin.read()))')")"
done
```

各语言框架的接入点（示意）：

| 技术栈 | 接入位置 |
|--------|----------|
| Java/Spring | 全局异常处理器 `@ExceptionHandler` / logback 自定义 Appender |
| Go | `recover()` 兜底 + `zap` 自定义 Core，ERROR 级别触发异步上报 |
| Python | `logging.Handler` 子类，过滤 `levelno >= ERROR` |
| Node.js | `process.on('uncaughtException')` + 日志库 transport |
| 通用 | 先考虑第 6.1 节的日志集成（Filebeat 读同一个文件），不要为了采集改应用代码 |

### 6.4 两条通路的字段映射（统一口径）

| 平台字段 | Filebeat 路径（日志集成） | HTTP Hook 直推 |
|---|---|---|
| `message` | `message`（多行合并后的"首行 + 堆栈"，平台拆开存 `message` 与 `stacktrace`） | `message`（必填） |
| `timestamp` | `@timestamp` | `timestamp`（缺省取当前时间） |
| `service` | `fields.service`（集成时写入；缺失依次退回 `fields.server`、`host.name`、`unknown`——指纹按服务归集，不能为空） | `service`（必填） |
| `environment` | `fields.environment`（集成时写入） | 服务器自动登记时固定为 `dev`（HTTP Hook 不携带环境字段） |
| `server_name` | `fields.server`（**目标机地址**，平台按它反查/登记 `server_instances`；缺失依次退回 `host.name`、服务名。另有 `fields.integration` = 集成名，平台暂不消费，仅供排障对号） | `server_name` / `server_id` / `server_ip` 至少一个 |
| `log_path` | `log.file.path`（**这条日志来自哪个文件**） | `log_path` |
| `level` | `log.level`（解析不到时按正文关键字推断，最终兜底 `ERROR`） | `level`（必填） |
| `stacktrace` | 多行合并正文里的堆栈部分（自动拆出） | `stacktrace` |
| `alert_type` | 固定为 `error` | `error` / `gc` / `stack` |

### 6.5 规则驱动的去重与告警风暴抑制

去重窗口与冷却期**不再写死在代码里**，而是由「日志告警规则」（页面：日志告警 → 日志告警规则，
路由 `log-alerts/rules`；表 `log_alert_rules`）逐条配置。这样做的原因很实际：
核心交易服务要立刻通知、批处理服务可以攒一攒——"多久打扰人一次"随服务重要性而变。

| 机制 | 说明 |
|------|------|
| 错误指纹 | 异常类名 + 错误消息模板（去时间戳/IP/数字/引号内容）+ 首个业务栈帧（`ErrorSignature`，与规则无关，始终先生成） |
| 规则匹配 | 按「服务 + 错误指纹 + 级别」匹配 `log_alert_rules`：**priority 数字小的优先，同优先级按 id 升序，取第一条命中的启用规则**（`MatchLogAlertRule`） |
| 命中不到规则 | 用平台默认值（`log_alert.default_*`）构造一条虚拟规则（`effectiveRuleFor`）——**零配置也能跑通**，不是"不告警" |
| 窗口去重 | 命中规则的 `dedup_window`（分钟，默认 5）内同指纹**合并计数**（累加 `error_count`、刷新 `last_seen_at`），**不新增事件**；窗口配 0 表示不合并 |
| 冷却抑制 | 命中规则的 `cooldown`（分钟，默认 10）内**不重复通知、不重复触发 AI**，但**事件照常记录**（列表里 `suppressed=true` + `cooldown_until`）；冷却过后再次合并时才重新通知 |
| 语义聚类 | 离线批处理，相似告警仅**合并展示**，不修改告警状态（可撤销） |

规则的字段与语义（`service/logalertrule.go`）：

| 字段 | 取值 | 说明 |
|------|------|------|
| `name` | 必填、唯一 | 规则名 |
| `description` | 可选 | 备注 |
| `service_name` | 空 = 任意服务 | 按服务名精确匹配（大小写不敏感） |
| `signature_pattern` | 空 = 任意指纹 | **普通文本 = 子串匹配**（大小写不敏感）；`/正则/` 形式 = 正则匹配（大小写不敏感；正则写错会退化为子串匹配，不会让链路挂掉） |
| `min_severity` | 空 / `INFO` / `WARN` / `ERROR` / `FATAL` | 低于该级别的日志不命中本条规则；留空表示不限 |
| `dedup_window` | 分钟，默认 5 | 0 = 不合并（每条都新增事件） |
| `cooldown` | 分钟，默认 10 | 0 = 不冷却（每条都通知/分析） |
| `notify_channels` | `feishu` / `wecom` / `dingtalk` / `email`，留空 | 留空 = 用平台「通知渠道」里已启用的渠道 |
| `ai_enabled` | 开/关 | 关掉后该规则命中的事件不自动做 AI 代码分析（`analysis_state=disabled`） |
| `enabled` | 开/关 | 关掉的规则完全不参与匹配（按 id 顺序看列表，最容易犯的错就是"规则建了但没启用"） |
| `priority` | 数字，默认 100 | **数字小的优先**；用它让"特例规则"压过"通用规则" |

**两条必须记住的语义**（列表页和详情页都按这个语义展示）：

1. **抑制 ≠ 丢弃**。冷却期内的日志只是不发通知、不触发 AI，事件仍然落库并累加计数，
   列表里标记 `suppressed=true`，`cooldown_until` 给出抑制到什么时候。
2. **冷却过后不会重跑 AI**。窗口内合并且冷却已过时，事件会被重新入队**只补一次通知**
   （`notified_at` 更新），AI 分析沿用已有结论（`analyzed=true` 的事件不会重跑，避免白烧 token）。
   确实要立刻重跑，用页面上的「重新分析」——它会**清掉冷却记录**并立即重跑通知与 AI。

三列排障字段（列表与详情都有）：`rule_id`（命中的规则，0 = 平台默认值）、
`suppressed` / `cooldown_until`（为什么没通知）、`analysis_state` / `analysis_error`（为什么没分析/分析失败）。

> **`signature_pattern` 比的是"错误指纹"，不是原始错误消息**。指纹由异常类名 + 消息模板（去变量）
> + 首个业务栈帧算出（`ErrorSignature`），是一条短串。最省事的做法：**先不配 `signature_pattern`
> （留空 = 任意指纹），只按服务名与级别过滤**，等事件进来后在事件详情页看到 `error_signature`，
> 再把它的一段（或 `/^前缀/` 正则）填进规则。

### 6.6 AI 代码分析：先有「服务 → 代码仓库」映射，再谈结论

日志的 AI 代码分析要回答的是「这条错误对应哪一行代码」，因此平台**必须先在本地有一份与服务
当前版本一致的代码**。整条链路是：

```text
日志事件落库（analysis_state=pending）
      │  定时任务（默认每 15s 扫一批，log_alert.worker_interval_seconds / worker_batch）
      ▼
① 外发通知（写 notified_at）─ 命中冷却则跳过（suppressed=true 的事件不重复打扰）
      ▼
② 按事件的服务名查「代码仓库」映射（CodeRepo.FindByService）
      │   没配 → analysis_state=disabled，analysis_error 写明"服务 X 未配置代码仓库映射"
      ▼
③ 拉代码：**首次 clone，之后只做更新**（internal/repo，见下）
      ▼
④ AI 三点式代码分析（定位文件行 + 根因 / 应急处置 / 修复建议）
```

**怎么配**：日志告警 → **服务器与仓库**（`GET/POST /api/log-alerts/code-repos`）添加一条
「服务名 → 仓库地址 + 分支（默认 `main`）+ 语言」。服务名必须与日志事件里的 `service`
（Filebeat 路径下是 `fields.service`）**完全一致**，否则永远匹配不到。

**缓存行为（`internal/repo`，首次 clone、之后更新到远端）**：

| 项 | 取值 / 行为 |
|---|---|
| 缓存根目录 | `code_repo.cache_dir`（默认 `./data/repos`，容器内落在 `backend-data` 卷里，重建容器不丢） |
| 目录布局 | `<cache_dir>/<净化后的服务名>`，每个服务一个子目录；服务名非 `[A-Za-z0-9._-]` 的字符会被替换成 `-` |
| 首次 | `git clone`（配了分支就 `--branch <分支>`），超时 `code_repo.clone_timeout`（默认 10m） |
| 之后（`branch` 非空） | `git fetch --prune origin` + `git checkout --force -B <branch> origin/<branch>`：**把缓存强制重置到远端**，本地手工改动会被丢弃（缓存不是工作区，脏缓存会让行号与线上堆栈对不上） |
| 之后（`branch` 为空） | `git pull --ff-only`（不改写历史；分叉时报错，由运维决定） |
| 最小拉取间隔 | `code_repo.refresh_interval_seconds`（默认 300s）：一次故障风暴里同服务的多条事件不会每条都去 pull 远端 |
| 并发 | 同一缓存目录上的 git 操作串行化，同目标的并发请求复用同一次执行结果 |
| 执行后写回 | `CodeRepo.local_path` 与 `CodeRepo.last_pull_at` 会更新（页面上能看到"什么时候拉过"） |
| 越界防护 | 服务名净化为单层安全目录名；显式指定的 `local_path` 必须严格落在 `cache_dir` 之内（含软链接解析），越界直接拒绝 |
| 目标目录已存在但不是 git 仓库 | **明确报错、绝不覆盖**（可能是别人的目录） |

**拉取失败时页面显示什么**：`analysis_state=failed`，`analysis_error` 是一句可操作的中文结论，
格式为「拉取代码失败：repo: git <操作> 失败：<中文结论>（远端仓库 <脱敏后的地址>）；git 输出：<脱敏片段>」。
翻译表覆盖最常见的几类（见 `internal/repo/errors.go`）：

| 现象 | 页面上的中文结论 |
|---|---|
| 令牌过期 / 不给交互式输入 | 认证失败：检查凭据是否有效（HTTPS 用访问令牌、SSH 用部署密钥）及令牌对该仓库的读权限 |
| `Repository not found` / 404 | 仓库不存在或当前凭据无权访问：核对 `RepoURL` 拼写与令牌授权 |
| 403 | 服务端拒绝访问：检查令牌权限范围与代码平台的 IP 白名单 |
| `couldn't find remote ref` | 分支或引用不存在：核对 `Branch` 拼写，或该仓库还没有任何提交 |
| `No space left on device` | 磁盘空间不足：清理仓库缓存目录后重试 |
| DNS / 连接超时 | 无法访问远端：检查平台出网白名单、代理与网络连通性 |
| clone/pull 超时 | `git ... 超时（超时上限 Xs…）`：仓库过大或网络过慢时调大 `clone_timeout` / `pull_timeout` |

**凭据不会进日志**：`RepoURL` 里的 `user:token@`（以及 `?token=` / `?access_token=` 这类查询参数）
一律脱敏成 `***` 后才进平台日志、错误信息与页面（`internal/repo/redact.go`）。
但**建议仍用最小权限的只读令牌**，并纳入轮换。

**两个出网开关不要混**（这是本功能最容易配错的地方）：

| 开关 | 管什么 | 默认 | 关掉后的现象 |
|---|---|---|---|
| `code_repo.allow_outbound` | 平台**能不能 `git clone/pull` 代码**（内网 GitLab 也该允许） | `true` | 平台**不执行任何 git 命令**，事件 `analysis_state=failed`，`analysis_error` 写明出网许可未开启（通知不受影响） |
| `CodeRepo.allow_third_party`（页面上的「出网白名单开关」，按服务维度） + `security.outbound_whitelist` | 能不能把**代码片段发给第三方 AI**（合规上更敏感） | `false` | AI 代码分析降级为本地分析/标注「不在出网白名单」 |

关掉 `allow_outbound` 仍然能**拉取动作之外的一切**：错误指纹、去重合并、冷却抑制、多渠道通知、
以及「没配仓库映射 / 规则关了 AI」这类明确结论照旧产生。

拉取失败**不会丢结论链路**：事件照旧入库、照旧通知、照旧可以在页面上重试（「重新分析」）。
进程重启也不丢——`analysis_state` 在数据库里，后处理是定时任务按队列重扫，不靠内存里的 goroutine。

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
| 监控页显示「无数据源」/指标显示「无数据」 | `prometheus.base_url` 为空或连不通 | 配置平台 Prometheus 地址；确认平台容器能访问 `:9090` |
| 有指标但值为 0 / `unknown` | PromQL 标签没匹配到序列 | 用第 2 步的查询核对 `job` / `instance` / `instance_name` 是否与纳管字段一致 |
| 只有部分指标有值 | Exporter 未暴露该指标 | 核对第 4 节表格的指标名；`curl exporter/metrics` 搜索 |
| Kafka 全部无数据 | kafka-exporter 无 `instance_name` 标签 | 在 Prometheus 抓取配置里用 `relabel_configs` 补标签 |
| ES 显示 warning | 集群为 yellow（1）属正常告警 | 若期望只在 red 告警，改用平台规则 `cluster_status < 1` |
| 日志页无事件（走 Hook 直推） | Hook 401 或字段不合规 | 核对 `X-Hook-Token` 与 `MWOPS_HOOK_TOKEN`；`service`/`level`/`message` 必填 |
| 日志页无事件（走日志集成） | 目标机没采到 / 连不上 Kafka / 时钟偏移 | ① 集成中心对该集成点**自检**看是哪一段红；② 目标机 `systemctl status filebeat` + `filebeat test output`；③ `nc -vz <KAFKA_ADVERTISED_HOST> <KAFKA_PORT>`；④ 目标机 `timedatectl` 核对时钟（偏移过大被 Kafka 以 `InvalidTimestampException` 拒收） |
| **日志收到了但不通知 / 不分析** | 见 6.5 与 6.6：规则没命中 / 冷却抑制 / 规则关了 AI / 没配仓库映射 | ① 先看「日志告警规则」页顶部卡片给出的**默认值**（`GET /api/log-alerts/rules/defaults`）：没命中任何规则时就是按它处理；② 事件列表看「抑制 / 通知」列，`cooldown_until` 有值说明在冷却中（事件已记录，想立刻看结论点「重新分析」）；③ 看「AI 分析」列：`disabled` 表示规则关了 AI 或没配仓库映射、`failed` 表示拉代码/调用 AI 失败，原因都在 `analysis_error` 里 |
| 日志事件 `analysis_state=disabled`，原因写「服务 X 未配置代码仓库映射」 | 该服务在「服务器与仓库」里没有映射（服务名必须与日志的 `service` 完全一致） | 加一条映射后对该条事件点「重新分析」 |
| 日志事件 `analysis_state=failed`，原因写「拉取代码失败：repo: git …」 | 平台拉代码失败（认证/分支/网络/磁盘，或 `code_repo.allow_outbound=false`） | 按 6.6 的错误翻译表逐条处理；凭据用只读令牌（URL 内嵌 token 在日志里会被脱敏） |
| 代码分析提示「不在出网白名单」 | 合规默认禁止第三方分析 | 「服务器与仓库」中为服务开启 `allow_third_party`，并把服务名加入 `security.outbound_whitelist` |
| 连接测试通过但监控无数据 | 两者独立：前者是 TCP 探测，后者依赖 Exporter | 正常现象，按上表排查指标链路 |

### 8.2 端到端冒烟

```powershell
# 后端全链路（含纳管、指标、诊断、告警、审计）
pwsh -File scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8080        # PowerShell 7+
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:8080   # PS 5.1
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
- [ ] 应用日志已通过**日志集成（Filebeat → 平台 Kafka）**接入并自检通过，或已用 Hook 直推，日志页能看到事件
- [ ]（可选）在「日志告警规则」页为不同重要性的服务配规则（去重窗口 / 冷却期 / 通知渠道 / AI 开关）；
      不配也能跑通——没命中规则时用平台默认值（页面上能看到具体取值）
- [ ] 想要 AI 代码结论，就在「服务器与仓库」登记服务→仓库映射（服务名与日志的 `service` 完全一致），
      必要时开启出网白名单；否则事件会是 `analysis_state=disabled`
- [ ] 造一条错误日志验证：事件出现 → 数秒内「AI 分析」列从 pending 变成 done / disabled / failed，
      且 `analysis_error` 能解释原因；冷却期内的重复日志只累加 `error_count` 且 `suppressed=true`
