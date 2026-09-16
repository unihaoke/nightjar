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

| 文件 | 作用 |
|------|------|
| `docker-compose.yml` | **改造**：四网分区（jd-edge / jd-app / jd-data[internal] / jd-obs）；MySQL、Redis 取消宿主端口映射；Redis 开启 `requirepass`；后端日志落盘到 `backend-logs` 卷；Prometheus 只绑 `127.0.0.1` |
| `.env.example` | 新增：统一环境变量（含 `REDIS_PASSWORD`、`MYSQL_EXPORTER_PASSWORD`、`JD_NIGHTJAR_NETWORK`、`NIGHTJAR_URL`、`NIGHTJAR_HOOK_TOKEN`） |
| `backend/src/main/resources/logback-spring.xml` | 新增：`interview-review.log` 全量 + `error.log` 仅 ERROR，供平台日志 Agent 采集 |
| `backend/Dockerfile` | GC 日志路径改为 `/app/data/logs/gc.log`（落进 `backend-logs` 卷） |
| `deploy/jd-exporters/docker-compose.jd.yml` | **改造**：Exporter 只入数据面 `jd-data`；mysql/redis/prometheus 追加 `jd-nightjar` 与别名；新增 `jd-log-agent`（profile=`logs`）；mysqld-exporter 改用 `DATA_SOURCE_NAME` 注入口令 |
| `deploy/jd-exporters/prometheus-jd.yml` | **修复**：两个 job 统一用 `relabel_configs` 写 `instance_name` |
| `deploy/jd-exporters/docker-compose.jd-link.yml` | 新增：只做网络打通的最小 overlay |
| `deploy/jd-exporters/log-shipper.sh`、`Dockerfile.logagent`、`agent.yaml` | 新增：日志上报（简易 shell 版 + 镜像构建 + 官方 Agent 配置模板） |
| `start.sh` | 新增 `nightjar` / `nightjar-logs` / `nightjar-initdb` / `link` / `restore` / `exporters-only` |

> 路径约束：overlay 里的 bind mount 是相对**项目目录**解析的，资产必须落在 `<jd项目根>/deploy/jd-exporters/`。
>
> 网络重命名影响：`interview-net` 已拆成 `jd-*`，已有部署请先 `docker compose down` 再启动。

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

---

## 7. 一句话总结

jd 已经有 Prometheus 与后端指标，缺的是**中间件 Exporter、网络分区和日志落盘**。
本轮补上之后，两个栈之间只留一条 internal 专用网和三个必要端点：
nightjar 查指标、探活、收日志；jd 的数据库零宿主暴露、数据面零出网。
