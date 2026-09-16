# jd 项目接入本平台 · 操作文档

> 接入对象：**jd（Java 高级开发面试复习工具）** —— Spring Boot 3.2 / Java 21 / MySQL 8 / Redis 7 / 自带 Prometheus + Grafana。
>
> 本文基于两个仓库的实际代码编写，涉及平台侧的契约都标注了源码位置，便于核对。

---

## 1. 接入前必须知道的四件事

| # | 结论 | 依据 |
|---|------|------|
| 1 | **平台会用 TCP 直连被管实例** | `internal/service/middleware.go:probe()` 对 `host:port` 做 `DialContext`，超时 3s。连不通 → `status=0`、`LastMessage="连接失败…"`，但**不影响指标查询与 AI 诊断** |
| 2 | **指标靠 `instance_name` 标签定位** | `internal/monitor/profile.go:buildSelector()`：填了 `prom_instance` 就用 `instance="..."`，否则用 `instance_name="<实例名>"`。**两个字段不要同时填**，否则 `instance_name` 会被忽略 |
| 3 | **`instance_name` 必须由 Prometheus relabel 产生** | `mysqld_exporter` 不带实例名；`oliver006/redis_exporter` **没有 `SERVICE_NAME` 这个 flag**（只有 `REDIS_ADDR`/`REDIS_PASSWORD`/…），早期靠该环境变量上报 `instance_name` 的做法是无效的。两个 job 都必须写 `relabel_configs` |
| 4 | **`mw_type` 只能是 7 类之一** | `internal/service/middleware.go`：redis / kafka / mysql / pg / es / nginx / rabbitmq。jd 能纳管的是 `mysql` 与 `redis` |

---

## 1.5 先分清两条链路：指标 ≠ 日志（回答"为什么新增实例后没有输出"）

这是接入时最大的认知陷阱。平台里「中间件实例」和「日志事件」**没有从属关系**：

| | 指标链路 | 日志链路 |
|---|----------|----------|
| 数据流 | Exporter → jd 的 Prometheus → 平台按 PromQL **拉取** | 应用日志文件 → jd-log-agent → `POST /api/hooks/logs` → 平台**接收** |
| 平台侧对象 | 中间件实例（`jd-mysql` / `jd-redis`） | 服务器 + 服务名（`jd-host` / `interview-review-backend`） |
| 页面 | 中间件纳管 / 统一监控 | 日志告警 → 事件、服务器 |
| 「新增实例」会带来它吗 | **会**（实例是唯一入口） | **不会**。纳管 MySQL/Redis 实例不会产生任何日志事件 |

因此：

- **"我新增了 jd-redis 实例，但日志告警页什么都没有"** —— 这是预期行为，不是故障。
  日志要在 jd 侧以 `logs` profile 启动 Agent（`./start.sh nightjar-logs`），
  并让 `NIGHTJAR_HOOK_TOKEN` 与平台 `HOOK_TOKEN` 一致。
- **"我新增了实例，但监控页全是 0 / unknown"** —— 才是指标链路问题，
  按 §4.9 的排查树处理（最快的方式：实例详情页 → 「接入自检」按钮，
  或 `GET /api/metrics/<id>/diagnose`）。

---

## 2. 网络隔离模型（核心）

两个栈各有自己的 compose 网络，接入时**不共享整张网**，只开一个专用的 internal 互联网络。

```
┌──────────────────────── 宿主 ────────────────────────┐
│  :8542 前端   :3000 Grafana          :8000 nightjar Web│
└───────┬───────────────────────────────────────┬───────┘
        │                                       │
┌───────┴────────────── jd ─────────────────────┼────────────────────┐
│                                               │                    │
│  jd-edge (bridge)      jd-app (bridge)        │   jd-obs (bridge)  │
│   frontend              frontend ↔ backend ───┼──▶ prometheus:9090 │
│   grafana ────────────────────────────────────┼──▶ grafana         │
│                         backend 经此出网调 LLM │   (仅 127.0.0.1)   │
│                              │                │        │           │
│                              ▼                │        │           │
│  jd-data (internal，无出网、无宿主端口)         │        │           │
│   mysql:3306   redis:6379   backend            │        │           │
│   mysqld-exporter   redis-exporter ◀───────────┼────────┘           │
│        │                                       │                    │
└────────┼───────────────────────────────────────┼────────────────────┘
         │                                       │
         │   jd-nightjar（internal，跨项目专用）   │
         │   jd 侧：jd-mysql / jd-redis / jd-prometheus / jd-log-agent
         │                                       │
         └───────────────▶ mwops-backend ◀───────┘
                           （nightjar，见 deploy/compose.jd-link.yml）
```

**跨栈可见面只有三个端点**：

| 方向 | 端点 | 用途 |
|------|------|------|
| nightjar → jd | `jd-prometheus:9090` | 指标查询（`prometheus.base_url`） |
| nightjar → jd | `jd-mysql:3306`、`jd-redis:6379` | TCP 健康探测 |
| jd → nightjar | `mwops-backend:8080/api/hooks/logs` | 日志上报 |

nightjar **看不到** jd 的 frontend / grafana / backend 出网链路；jd 的 MySQL / Redis **不向宿主发布任何端口**（改造前是 `0.0.0.0:3306` / `:6379` 全暴露）。

> ⚠️ `jd-nightjar` **必须是 internal 网络**。若它是普通桥接网，jd 的 mysql / redis 会因为多了一张非 internal 网卡而重新获得默认路由，数据面隔离即失效。
> `jd/start.sh` 会自动以 `docker network create --internal` 创建，并在检测到非 internal 时告警。

---

## 3. jd 侧改动清单

### 3.1 两个仓库要改的文件总览

**jd 侧（业务系统 = 被监控方）**

| 文件 | 作用 | 改了会怎样 |
|------|------|------------|
| `.env`（由 `.env.example` 复制） | 统一口令与接入参数：`REDIS_PASSWORD`、`MYSQL_EXPORTER_PASSWORD`、`JD_NIGHTJAR_NETWORK`、`NIGHTJAR_URL`、`NIGHTJAR_HOOK_TOKEN`、`SERVER_NAME` | 密码不一致 → Exporter 认证失败；令牌不一致 → 日志 401 |
| `docker-compose.yml` | 四网分区（jd-edge / jd-app / jd-data[internal] / jd-obs）；MySQL、Redis 取消宿主端口；Redis 开启 `requirepass`；后端日志落盘到 `backend-logs` 卷；Prometheus 只绑 `127.0.0.1` | 不分区则数据库对宿主暴露；不挂日志卷则 Agent 无日志可采 |
| `deploy/jd-exporters/docker-compose.jd.yml` | overlay：两个 Exporter + `jd-nightjar` 网络别名 + `mysql-monitor-user`（initdb profile）+ `jd-log-agent`（logs profile）+ `LOG_TARGETS` | 缺它则 Prometheus 里没有 `middleware-exporter-*` job，平台永远查不到指标 |
| `deploy/jd-exporters/prometheus-jd.yml` | 抓取配置：两个 job 用 **relabel_configs 写 `instance_name`** | 缺它则平台按 `instance_name` 匹配不到任何时序 |
| `deploy/jd-exporters/init/01-monitor-user.sql` | MySQL 只读监控账号 `exporter`（`PROCESS, REPLICATION CLIENT, SELECT`） | 缺它则 `mysql_up = 0` |
| `deploy/jd-exporters/log-shipper.sh` / `Dockerfile.logagent` | 简易日志上报（alpine + curl）；失败会写 stderr 便于排查 | 缺它则日志链路不通 |
| `backend/src/main/resources/logback-spring.xml` | `interview-review.log` 全量 + `error.log` 仅 ERROR | Agent 采集的目标文件 |
| `backend/Dockerfile` | GC 日志路径 `/app/data/logs/gc.log`（落进 `backend-logs` 卷） | 路径不对则 gc.log 采集不到 |
| `start.sh` | `nightjar` / `nightjar-logs` / `nightjar-initdb` / `link` / `exporters-only` 子命令 | — |

**nightjar 侧（平台 = 监控方）**

| 文件 | 作用 | 关键值 |
|------|------|--------|
| `.env`（由 `.env.example` 复制） | 平台密钥与接入参数 | `MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090`、`HOOK_TOKEN=<与 jd 的 NIGHTJAR_HOOK_TOKEN 相同>`、`JD_NIGHTJAR_NETWORK=jd-nightjar` |
| `deploy/compose.jd-link.yml` | 把 `mwops-backend` 加入 jd 创建的 internal 网络 `jd-nightjar` | 不加则解析不到 `jd-prometheus` / `jd-mysql` / `jd-redis` |
| `docker-compose.yml` | 已注入 `MWOPS_HOOK_TOKEN`、`MWOPS_PROMETHEUS_BASE_URL` 等环境变量 | 一般不用改 |
| `middleware-ops/configs/config.yaml` | `prometheus.exporter_job_prefix`（默认 `middleware-exporter`）、`scheduler.*`、`server.rate_limit_per_minute` | 只有换 job 命名约定时才改 |
| `deploy/prometheus/prometheus.yml` / `prometheus.with-exporters.yml` | **仅用于平台自带 Exporter 的方案**（jd 场景走 jd 自己的 Prometheus，不用改这里） | 见 §7 的已知修复 |

> 路径约束：overlay 里的 bind mount 是相对**项目目录**解析的，资产必须落在 `<jd项目根>/deploy/jd-exporters/`。
>
> 网络重命名影响：`interview-net` 已拆成 `jd-*`，已有部署请先 `docker compose down` 再启动。

### 3.2 本轮修复的 4 个真实缺陷

| # | 缺陷 | 现象 | 修复 |
|---|------|------|------|
| 1 | `monitor/factory.go` 的历史查询把「空结果」也当作失败并回退模拟器 | 选择器写错时趋势图仍有曲线，看着"接上了"其实是假数据 | 只在查询**报错**时回退模拟器；空结果如实返回 |
| 2 | `monitor/prometheus.go` 的快照把「全空」和「不可达」都吞成 `unknown/0` | 页面全是 0，无任何线索；且 Prometheus 挂了也不会降级 | 引入 `errEmptyResult` 哨兵；区分三种结果，新增 `selector`/`matched`/`job_up` 与 `note` |
| 3 | `deploy/prometheus/prometheus.with-exporters.yml` 的 redis job 依赖 `SERVICE_NAME`（redis_exporter 没有该 flag） | 平台自带 Exporter 方案下，redis 实例纳管后指标恒为空 | 与 jd 变体一致，改用 `relabel_configs` 写 `instance_name` |
| 4 | `/api/hooks/*` 与用户接口共用 600 次/分钟的 IP 限流 | 一次日志刷屏就把额度打满，后续上报被 403 静默丢弃 | Hook 路径豁免人机限流（由 `X-Hook-Token` 鉴权） |

另外修复：`cmd/agent` 的 `level_filter` 被解析后从未生效（`gc.log` 配了却永远采不到），且上报体的 `level` 被硬编码为 `ERROR`；`log-shipper.sh` 用 `-o /dev/null` 吞掉了 401/403/连不上，现在会写 stderr。

---


## 4. 操作步骤

### 4.0 前置

```bash
cd <jd 项目目录>
cp .env.example .env
# 必改：JWT_SECRET、DB_PASS、REDIS_PASSWORD、MYSQL_EXPORTER_PASSWORD
# 且 MYSQL_EXPORTER_PASSWORD 必须与 deploy/jd-exporters/init/01-monitor-user.sql 里的 BY '<口令>' 一致
```

### 4.1 jd 侧（方案 A，推荐）

```bash
# 首次启动（会自动创建 internal 网络 jd-nightjar）
./start.sh nightjar
# 等价于
docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml up -d --build

# MySQL 数据卷已存在（非首次启动）时，补建只读监控账号
./start.sh nightjar-initdb

# 可选：启用日志上报
./start.sh nightjar-logs
```

验证 Exporter 已被抓取（Prometheus 只绑 127.0.0.1，需在容器内 curl）：

```bash
docker run --rm --network jd_jd-data curlimages/curl:8.10.1 \
  -s http://redis-exporter:9121/metrics | grep -E '^redis_(up|memory_used_bytes)'
docker run --rm --network jd_jd-data curlimages/curl:8.10.1 \
  -s http://mysqld-exporter:9104/metrics | grep -E '^mysql_(up|global_status_threads_connected)'
```

### 4.2 jd 侧（方案 B，不动原生 Prometheus 配置）

```bash
./start.sh exporters-only
# 需要 TCP 探测时叠加网络 overlay
docker compose -f docker-compose.yml \
  -f deploy/jd-exporters/docker-compose.jd-link.yml \
  -f deploy/jd-exporters/docker-compose.exporters-only.yml up -d
```

方案 B 的 Prometheus 别名为 `jd-prometheus`，端口 `9190`。

### 4.3 nightjar 侧

```bash
cd <nightjar 项目目录>
cp .env.example .env
# 必改：JWT_SECRET、ADMIN_PASSWORD、DB_PASSWORD、REDIS_PASSWORD、HOOK_TOKEN
# 接入相关：JD_NIGHTJAR_NETWORK=jd-nightjar、MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090

docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build
```

> 启动顺序：先起 jd（它负责创建 `jd-nightjar`），再起 nightjar。
> 若 nightjar 报 `network jd-nightjar not found`，说明 jd 侧还没执行 `./start.sh nightjar`。

### 4.4 平台纳管两个实例

> 字段速查表见下；**可直接照抄的 curl + 自检命令见 §4.8**，
> 「新增实例后没有监控/日志」的原因速查见 §4.9。

**中间件纳管 → 新增实例**，按下表填写：

| 字段 | jd-mysql | jd-redis |
|------|----------|----------|
| 实例名称 | `jd-mysql` | `jd-redis` |
| 中间件类型 | `mysql` | `redis` |
| 连接地址 | `jd-mysql` | `jd-redis` |
| 端口 | `3306` | `6379` |
| 监控账号 / 密码 | `exporter` / 与 `MYSQL_EXPORTER_PASSWORD` 一致 | 留空或填 `REDIS_PASSWORD`（探测只做 TCP，口令仅存档） |
| 环境 / 分组 | `dev` / `interview` | `dev` / `interview` |
| Prometheus job | `middleware-exporter-mysql` | `middleware-exporter-redis` |
| **Prometheus instance** | **留空** | **留空** |
| 配置（config） | 见 4.5 | 见 4.5 |

> **`Prometheus instance` 务必留空**。填了之后 `buildSelector` 会改用 `instance="..."` 而忽略 `instance_name`，导致查不到数据。

`jd-mysql` 的 config：

```json
{
  "profile": "mysql",
  "ddl-auto": "update",
  "database": "interview_review",
  "connection_url_hint": "useUnicode=true&characterEncoding=utf8&serverTimezone=Asia/Shanghai",
  "note": "jd 项目演练库；沙箱场景 MySqlSlowScenario 会制造慢查询"
}
```

`jd-redis` 的 config：

```json
{
  "maxmemory-policy": "allkeys-lru",
  "appendonly": "yes",
  "requirepass": "enabled",
  "usage": "Agent 面试会话缓存（AgentInterviewSessionService）；AGENT_INTERVIEW_REDIS_ENABLED=false 可关闭",
  "note": "沙箱场景 RedisFaultScenario 会制造 Redis 异常"
}
```

### 4.5 平台侧：数据源与令牌

```bash
# <nightjar>/.env
MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090
HOOK_TOKEN=<随机串>
```

对应 jd 侧 `.env` 的 `NIGHTJAR_HOOK_TOKEN` 必须与 `HOOK_TOKEN` 相同。
若 `HOOK_TOKEN` 留空，平台侧 Hook 不鉴权（仅测试用）。

### 4.6 平台侧：告警规则

jd 是开发/演练环境，用能触发但不过度打扰的阈值：

| 实例 | 指标 | 操作符 | 阈值 | 级别 |
|------|------|--------|------|------|
| jd-mysql | `threads_connected` | `>` | 50 | warning |
| jd-mysql | `slow_queries` | `>` | 1 | warning |
| jd-redis | `memory_usage_percent` | `>` | 60 | warning |
| jd-redis | `evicted_keys` | `>` | 0 | warning |

### 4.7 平台侧：日志与代码分析（可选）

日志链路已打通：jd 的 `error.log` / `gc.log` → `jd-log-agent` → `POST /api/hooks/logs`。
契约见 `internal/service/logalert.go:LogReport`（`service` / `level` / `message` 必填，`timestamp` 需 RFC3339）。

若要使用「AI 代码分析」，在 **服务器与仓库** 中登记：

```json
{
  "service_name": "interview-review-backend",
  "repo_url": "<jd 仓库地址>",
  "branch": "master",
  "local_path": "/data/repos/jd",
  "language": "java",
  "allow_third_party": false
}
```

出网白名单默认关闭。确认允许把堆栈与代码片段发给第三方 AI 后，再把
`interview-review-backend` 加入 `security.outbound_whitelist` 并置 `allow_third_party=true`；
否则代码分析走本地检索 + 本地 LLM。

### 4.8 新增实例的完整案例（可直接照抄）

**第 0 步：先确认指标已经进了 jd 的 Prometheus**（否则平台再怎么配也查不到）

```bash
# 在 jd 仓库根目录执行；全绿再往下走
pwsh -File scripts/verify-nightjar.ps1
# 方案 B（独立 Prometheus）用：pwsh -File scripts/verify-nightjar.ps1 -Standalone
```

判定口径（由外到内）：Exporter 容器 running → job 存在且 target up → `redis_up`/`mysql_up` = 1 →
`instance_name` 为 `jd-redis`/`jd-mysql`。

**第 1 步：拿平台令牌**

```bash
TOKEN=$(curl -s -X POST http://127.0.0.1:8000/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"'"$ADMIN_PASSWORD"'"}' | jq -r '.data.token')
```

**第 2 步：新增两个实例**（页面「中间件纳管 → 新增实例」或下面的 curl，字段完全一致）

| 字段 | jd-mysql | jd-redis | 为什么这么填 |
|------|----------|----------|--------------|
| 实例名称 | `jd-mysql` | `jd-redis` | **必须**与 `prometheus-jd.yml` 里 `relabel_configs.replacement` 完全一致（平台未填 `prom_instance` 时用 `instance_name="<实例名>"` 查询） |
| 中间件类型 | `mysql` | `redis` | `mw_type` 只能是 7 类之一 |
| 连接地址 | `jd-mysql` | `jd-redis` | jd 在 `jd-nightjar` 网络上的别名，用于 TCP 健康探测 |
| 端口 | `3306` | `6379` | |
| 监控账号 / 密码 | `exporter` / 与 `MYSQL_EXPORTER_PASSWORD` 一致 | 留空 / 填 `REDIS_PASSWORD` | 探测只做 TCP，口令仅加密存档 |
| 环境 / 分组 | `dev` / `interview` | `dev` / `interview` | 数据权限按此隔离 |
| Prometheus job | 留空（或 `middleware-exporter-mysql`） | 留空（或 `middleware-exporter-redis`） | 留空即按 `<exporter_job_prefix>-<mw_type>` 兜底，正好等于 jd 的 job 名 |
| **Prometheus instance** | **留空** | **留空** | 填了就用 `instance="..."` 且**忽略** `instance_name`；jd 的时序只有 `instance_name`，填了必查不到 |

```bash
curl -s -X POST http://127.0.0.1:8000/api/middlewares \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "name": "jd-redis",
    "mw_type": "redis",
    "host": "jd-redis",
    "port": 6379,
    "username": "",
    "password": "'"$REDIS_PASSWORD"'",
    "environment": "dev",
    "group_name": "interview",
    "tags": ["jd", "redis"],
    "prom_job": "",
    "prom_instance": "",
    "config": {
      "maxmemory-policy": "allkeys-lru",
      "appendonly": "yes",
      "requirepass": "enabled",
      "usage": "Agent 面试会话缓存（AgentInterviewSessionService）"
    }
  }'

curl -s -X POST http://127.0.0.1:8000/api/middlewares \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{
    "name": "jd-mysql",
    "mw_type": "mysql",
    "host": "jd-mysql",
    "port": 3306,
    "username": "exporter",
    "password": "'"$MYSQL_EXPORTER_PASSWORD"'",
    "environment": "dev",
    "group_name": "interview",
    "tags": ["jd", "mysql"],
    "prom_job": "",
    "prom_instance": "",
    "config": {
      "database": "interview_review",
      "connection_url_hint": "useUnicode=true&characterEncoding=utf8&serverTimezone=Asia/Shanghai"
    }
  }'
```

**第 3 步：立刻验证（不要靠肉眼猜）**

```bash
# 实例 ID 从创建响应里取，或在「中间件纳管」列表里看
ID=1

curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8000/api/metrics/$ID" \
  | jq '{selector:.data.selector, matched:.data.matched, total:.data.total, job_up:.data.job_up, source:.data.source, note:.data.note}'

curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8000/api/metrics/$ID/diagnose" \
  | jq '{selector:.data.selector, job_up:.data.job_up, hints:.data.hints}'
```

`jd-redis` 的期望结果：

```json
{
  "selector": "job=\"middleware-exporter-redis\",instance_name=\"jd-redis\"",
  "matched": 8,
  "total": 8,
  "job_up": 1,
  "source": "prometheus",
  "note": ""
}
```

页面上的等价操作：**实例详情页 → 「接入自检」按钮**，会直接把选择器、`job_up`、
逐条 PromQL 命中情况和排查建议列出来。

**第 4 步：配告警规则**（§4.6 的阈值表），再跑一次沙箱场景验证端到端。

### 4.9 「新增实例后没有监控 / 没有日志」的排查树

先回答一句：**指标和日志是两条链路，症状要分开看**（见 §1.5）。

#### A. 没有监控（监控页全 0 / unknown）

打开「实例详情 → 接入自检」，按它的输出直接定位：

| 自检输出 | 根因 | 处理 |
|----------|------|------|
| `monitor_kind = simulator`（页面标签显示"内置模拟器"） | `prometheus.base_url` 为空，或平台没带 `deploy/compose.jd-link.yml` 启动 | 平台 `.env` 设 `MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090`，用 `docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build` |
| `prometheus_healthy = false` | 地址/网络别名不通 | 平台后端容器内 `getent hosts jd-prometheus`；确认 jd 侧已建 `jd-nightjar` 网络 |
| `job_up = null` | Prometheus 里根本没有这个 job | 抓取配置没生效：确认启动带了 `-f deploy/jd-exporters/docker-compose.jd.yml`，且 **Prometheus 容器已重建**（换配置文件必须重启该容器）；再核对 `prom_job` 与 job 名是否一字不差 |
| `job_up = 0` | target 抓取失败（Exporter 没起或连不上被管中间件） | `docker ps` 看 `jd-redis-exporter` / `jd-mysqld-exporter`；MySQL 看 `mysql_up` 与 exporter 账号口令（`./start.sh nightjar-initdb`）；Redis 看 `REDIS_PASSWORD` 是否与 jd 一致 |
| `job_up = 1` 且 `matched = 0` | 选择器对不上 | 把「实例名称」改成与 `instance_name` 一致；**清空**「Prometheus instance」 |
| `matched < total` 且 `note` 提到部分指标无数据 | 该实例没有暴露那几项指标（如单机 MySQL 没有主从延迟） | 正常现象，可忽略 |
| `total = 0` | 该类型没有指标画像（rabbitmq 仅纳管） | 正常 |

手工分层验证（与自检等价，用于没有 UI 时）：

```bash
# ① jd 侧：Exporter 是否抓到
docker exec interview-prometheus wget -qO- \
  'http://localhost:9090/api/v1/query?query=up{job="middleware-exporter-redis"}'
# ② jd 侧：Exporter 是否连上中间件
docker exec interview-prometheus wget -qO- 'http://localhost:9090/api/v1/query?query=redis_up'
# ③ jd 侧：标签是否正确（这一步过了，平台按实例名就一定能查到）
docker exec interview-prometheus wget -qO- \
  'http://localhost:9090/api/v1/query?query=redis_memory_used_bytes{instance_name="jd-redis"}'
# ④ 平台侧：与自己拼出来的选择器对比
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8000/api/metrics/$ID/diagnose" | jq -r .data.selector
```

> 常见误判：**历史趋势图有曲线 ≠ 接入成功**。本轮修复前，选择器查不到数据时
> `History` 会回退内置模拟器，前端画出一条很正常的假曲线；现在空结果会如实返回空图，
> 并且 `source` 与实际数据源一致。

#### B. 没有日志

日志**不需要**新增中间件实例，也不需要平台侧配任何东西，只需要 jd 侧把 Agent 跑起来：

```bash
cd <jd 项目目录>
./start.sh nightjar-logs            # 或 docker compose ... --profile logs up -d
docker logs -f jd-log-agent         # 现在能看到 endpoint/targets/失败原因
```

逐项核对：

1. **Agent 起了吗**：`docker ps | grep jd-log-agent`。默认它在 `logs` profile 下，**普通 `./start.sh nightjar` 不会启动它**（这是"没有日志"最常见的原因）。
2. **令牌一致吗**：平台 `.env` 的 `HOOK_TOKEN` 必须等于 jd `.env` 的 `NIGHTJAR_HOOK_TOKEN`。不一致时 `docker logs jd-log-agent` 会出现 `上报失败 #1 HTTP=401`。
3. **地址对吗**：`NIGHTJAR_URL` 用容器名 `http://mwops-backend:8080`（不带路径）。出现 `HTTP=000` 说明连不上，检查 Agent 是否在 `jd-nightjar` 网络内。
4. **日志文件存在且可读吗**：Agent 只读挂载 `backend-logs` 卷。`/logs/error.log` 由 logback 启动即创建；`gc.log` 由 JVM `-Xlog` 写入。若脚本打印"等待 xxx 超时（10 分钟）"，说明后端从未写过该文件。
5. **采集范围**：默认只采 `error.log`（`LOG_TARGETS` 可覆盖）。要采 GC 需要显式加上 `/logs/gc.log:gc:INFO`，并注意 `-Xlog:gc*,gc+age=trace` 的日志量会把告警列表刷满。
6. **看得到吗**：日志事件在「日志告警 → 事件」，按错误指纹聚合；服务器是自动注册的 `jd-host`。列表按 `service`/`level`/`status` 过滤，先清空筛选条件。

---

## 5. 验证清单

```bash
# ① 互联网络存在且是 internal
docker network inspect jd-nightjar --format '{{.Internal}}'   # 期望 true

# ② nightjar 能解析到 jd 的别名并连通
docker exec mwops-backend sh -c 'getent hosts jd-prometheus && getent hosts jd-mysql'

# ③ Prometheus 抓到了（在 jd 的 prometheus 容器里查）
docker exec interview-prometheus wget -qO- \
  'http://localhost:9090/api/v1/query?query=redis_memory_used_bytes{instance_name="jd-redis"}'

# ④ 平台采到（换上你的 JWT 与实例 ID）
curl -s -H "Authorization: Bearer $TOKEN" http://<平台>:8000/api/metrics/<实例ID>

# ⑤ 端到端：jd 前端「沙箱」跑 RedisFault / MySqlSlow 场景
#    → 平台「告警中心」出现告警 → 点「AI 诊断」看结构化结论

# 平台侧全链路冒烟
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:8000
```

---

## 6. 常见问题

| 现象 | 原因 | 处理 |
|------|------|------|
| `network jd-nightjar not found` | jd 侧未执行 `./start.sh nightjar` | 先起 jd，再起 nightjar |
| 平台显示「内置模拟器」 | `MWOPS_PROMETHEUS_BASE_URL` 不通 | 确认填的是 `http://jd-prometheus:9090`；在 nightjar 后端容器内 `getent hosts jd-prometheus` |
| 有实例但指标为空 | `instance_name` 标签缺失 | 两个 job 都已用 relabel 写入；确认 `prom_job` 精确匹配且 **`prom_instance` 留空** |
| Redis 指标全无 | Redis 已鉴权但 Exporter 没传口令 | `docker-compose.jd.yml` 的 `REDIS_PASSWORD` 需与 jd 的 `REDIS_PASSWORD` 一致 |
| 实例状态「连接失败」 | TCP 探测不通 | 方案 A 填 `jd-mysql` / `jd-redis`；方案 B 需叠加 `docker-compose.jd-link.yml` |
| mysqld-exporter 认证失败 | MySQL 8 认证插件或账号未建 | 用 `mysql_native_password` 建号并 `GRANT PROCESS, REPLICATION CLIENT, SELECT` |
| 日志上报 401 | `HOOK_TOKEN` 与 jd 的 `NIGHTJAR_HOOK_TOKEN` 不一致 | 两边改成同一串 |
| 找不到 `jd_jd-data` 网络 | compose 项目名前缀不同 | `docker network ls \| grep jd-data`，用 `JD_DATA_NETWORK=` 覆盖 |
| 后端起不来，报 Redis `NOAUTH` | `REDIS_PASSWORD` 为空 | `--requirepass ""` 与 Spring 空口令冲突，口令必须非空 |
| 日志 Agent 起不来 / 报 curl 缺失 | 镜像构建期没装上依赖 | 用 `Dockerfile.logagent` 构建（`./start.sh nightjar-logs`）；容器只连 internal 网，运行期无法 `apk add` |
| 日志 Agent 读不到日志 | 日志文件权限 | Agent 以非 root 运行，依赖 logback 生成文件的 644 权限；必要时临时去掉 `Dockerfile.logagent` 的 `USER mwops` |
| **新增实例后日志页完全没有事件** | 日志与中间件实例是两条链路 | 纳管实例本来就不产生日志。按 §4.9-B 启动 `./start.sh nightjar-logs` 并核对令牌（见 §1.5） |
| **新增实例后监控全 0，但趋势图有曲线** | 修复前 `History` 用模拟器补齐空结果 | 已修复；先看实例详情页的「接入自检」，确认 `source=prometheus` 且 `matched>0` |
| **自检显示 `job_up=1` 但 `matched=0`** | 实例名与 `instance_name` 不一致，或误填了「Prometheus instance」 | 清空 `prom_instance`，把实例名改成 `jd-redis`/`jd-mysql` |
| **自检显示 `job_up=null`** | Prometheus 未加载抓取配置 | 确认启动带了 overlay，且 `interview-prometheus` 容器已重建（改配置文件必须重启容器） |
| **日志时有时无、中间丢一段** | 修复前 `/api/hooks/*` 与用户接口共用 600 次/分钟 IP 限流，超限被 403 静默丢弃 | 已修复（Hook 路径豁免限流）；升级后 `docker logs jd-log-agent` 不再出现 `HTTP=403` |
| **配了 `gc.log` 却永远没有 GC 日志** | 修复前 `cmd/agent` 的 `level_filter` 被解析后忽略 | 已修复；gc.log 需 `level_filter: INFO`，且建议把 JVM 参数收敛为 `-Xlog:gc` 以控制日志量 |

---

## 7. 一句话总结

jd 已经有 Prometheus 与后端指标，缺的是**中间件 Exporter、网络分区和日志落盘**。
本轮补上之后，两个栈之间只留一条 internal 专用网和三个必要端点：
nightjar 查指标、探活、收日志；jd 的数据库零宿主暴露、数据面零出网。
