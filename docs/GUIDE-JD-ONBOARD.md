# jd 接入 nightjar · 操作指南（从零到可用）

> **适用场景**：单台 Linux 主机 + Docker Compose，把 **jd（Java 高级开发面试复习工具：Spring Boot 3.2 / Java 21 / MySQL 8 / Redis 7 / 自带 Prometheus + Grafana）**
> 接入 **nightjar（中间件智能问题解决平台）**，实现 Redis、MySQL 指标监控 + 应用日志告警 + 阈值告警 + AI 诊断闭环。
>
> **目录约定**：`<jd>` 指 jd 项目根目录，`<nightjar>` 指 nightjar 项目根目录。
> **配套文档**：字段级排查见 [`COLLECTOR-JD.md`](COLLECTOR-JD.md)，集成中心设计见 [`INTEGRATION.md`](INTEGRATION.md)，接口清单见 [`API.md`](API.md)。

---

## 0. 先读这三条，能省掉 80% 的返工

### 0.1 指标和日志是两条独立链路

| | 指标链路 | 日志链路 |
|---|---|---|
| 数据流 | Exporter → Prometheus → 平台按 PromQL **拉取** | 应用日志文件 → jd-log-agent → `POST /api/hooks/logs` → 平台**接收** |
| 平台侧对象 | 中间件实例（`jd-mysql` / `jd-redis`） | 服务器 + 服务名（`jd-host` / `interview-review-backend`） |
| 页面 | 集成中心 / 中间件纳管 / 统一监控 | 日志告警 → 事件、服务器与仓库 |

**结论：纳管了 MySQL/Redis 实例，日志页不会自动出现任何东西。** 日志只取决于 jd 侧有没有以 `logs` profile 启动 Agent（第 5 节）。

### 0.2 跨栈只开一个 internal 网络，只暴露三个端点

```
┌────────────── jd ──────────────┐          ┌──────── nightjar ────────┐
│ jd-data (internal)             │          │ mwops (bridge)           │
│   mysql:3306  redis:6379       │          │   mwops-backend          │
│   mysqld-exporter  redis-exporter         │   mwops-prometheus       │
│ jd-obs                         │          │   mwops-frontend         │
│   prometheus:9090 (仅 127.0.0.1)│          │                          │
└───────────┬────────────────────┘          └───────────┬──────────────┘
            │        jd-nightjar (internal，跨项目专用)  │
            └───────────────────────────────────────────┘
     可见面仅三条：nightjar→jd:9090 查指标 / nightjar→jd:3306,6379 探活 / jd→nightjar:8080 推日志
```

- `jd-nightjar` **必须是 internal 网络**（否则 jd 的库会重新获得出网路由，隔离失效）；由 jd 侧创建，nightjar 侧以 `external` 引用。
- jd 的 MySQL / Redis **不发布宿主端口**，这是刻意的（改造前是 `0.0.0.0` 全暴露）。

### 0.3 两种接入方式，先选一种

| | **方式一：集成中心（推荐）** | **方式二：复用 jd 自带 Prometheus** |
|---|---|---|
| Exporter 由谁起 | 平台页面一键拉起（需挂 docker.sock） | jd 的 overlay 起（`./start.sh nightjar`） |
| 指标由谁抓 | **平台自带的 Prometheus**（HTTP 服务发现，保存即生效） | **jd 自带的 Prometheus**（改抓取配置需重建容器） |
| 平台 `MWOPS_PROMETHEUS_BASE_URL` | `http://prometheus:9090`（默认值，不用改） | `http://jd-prometheus:9090` |
| 平台操作 | 集成中心点几下，自动纳管 + 自动告警规则 | 中间件纳管 → 新增实例，字段要手工填对 |
| 适合 | 只想让 nightjar 看 | jd 自己也要用 Grafana 看同一份指标 |
| 本章节 | 第 4.1 节 | 第 4.2 节 |

> 两种方式可以并存（不同实例走不同来源），但**同一个实例不要两边都抓**，否则同一 `instance_name` 会出现重复序列。

---

## 1. 前置条件

| 项 | 要求 | 检查命令 |
|---|---|---|
| 主机 | Linux，2C4G 起，磁盘 ≥ 20G | `uname -a; free -h; df -h` |
| Docker | 24+ 与 compose v2 | `docker version --format '{{.Server.Version}}'; docker compose version` |
| 网络 | 两栈同机或同网；跨机需放通 9090/8000 | — |
| PowerShell（可选） | 若要用 `verify-nightjar.ps1` / `smoke-test.ps1` | `pwsh -v`（无则用本文的 bash 版校验） |

### 1.1 端口占用（同机部署必看）

| 端口 | 属于 | 说明 |
|---|---|---|
| 8000 | nightjar 前端 | `WEB_PORT` |
| **9090** | **两边都要用** ⚠️ | jd 的 Prometheus 绑 `127.0.0.1:9090`；nightjar 的 Prometheus 默认绑 `0.0.0.0:9090` → **同机会冲突** |
| 3000 | jd Grafana | `GRAFANA_PORT` |
| 8542 | jd 前端 | `WEB_PORT` |
| 8541 | jd 后端 | 仅绑 `127.0.0.1` |
| 9121 / 9104 | Exporter | 只在容器网络内，不发布宿主端口 |

**冲突处理**：把 nightjar 的 Prometheus 挪走即可（方式一必做；方式二下 nightjar 的 Prometheus 可以不用）：

```bash
# <nightjar>/.env
PROMETHEUS_PORT=9091
```

### 1.2 口令清单（三处必须一致的项）

| 口令 | 用在哪 | 必须一致的地方 |
|---|---|---|
| `MYSQL_EXPORTER_PASSWORD` | MySQL 只读监控账号 | jd `.env` ↔ `jd/deploy/jd-exporters/init/01-monitor-user.sql` 的 `BY '<口令>'`（当前示例值 `exporter_change_me`） |
| `REDIS_PASSWORD` | Redis 鉴权 | jd `.env` ↔ jd 后端 ↔ redis-exporter（同一份 `.env` 自动注入） |
| `NIGHTJAR_HOOK_TOKEN` / `HOOK_TOKEN` | 日志上报鉴权 | jd `.env` ↔ nightjar `.env` |
| `JD_NIGHTJAR_NETWORK` | 跨项目网络名 | jd `.env` ↔ nightjar `.env`（默认都是 `jd-nightjar`） |

生成随机口令：

```bash
openssl rand -base64 24     # 口令
openssl rand -hex 32        # JWT_SECRET / HOOK_TOKEN
```

### 1.3 执行顺序（有强依赖）

```
① jd 侧改 .env → ② 起 jd（创建 jd-nightjar 网络）→ ③ 补建只读账号 → ④ 校验指标进 Prometheus
                                                              ↓
                        ⑥ 平台侧集成实例 ← ⑤ 起 nightjar（必须带 jd-link overlay）
                                                              ↓
                                              ⑦ 日志链路（logs profile）→ ⑧ 告警/诊断
```

> 顺序不能反：nightjar 启动时若 `jd-nightjar` 网络不存在，compose 会直接报
> `network jd-nightjar declared as external, but could not be found`。

---

## 2. jd 侧：改造与启动

### 2.1 准备 `.env`

```bash
cd <jd>
cp .env.example .env
```

**必改项**：

```bash
# ---------- 数据库 ----------
DB_NAME=interview_review
DB_USER=root
DB_PASS=<MySQL root 口令>            # 必改

# ---------- Redis ----------
REDIS_PASSWORD=<Redis 口令>          # 必改，必须非空（空口令会让 Spring 报 NOAUTH）
AGENT_INTERVIEW_REDIS_ENABLED=true

# ---------- 应用 ----------
JWT_SECRET=<≥32 位随机串>            # 必改
BACKEND_PORT=8541
WEB_PORT=8542

# ---------- 监控 ----------
PROMETHEUS_PORT=9090                 # 同机部署 nightjar 时按 1.1 节处理冲突
GRAFANA_PORT=3000
GRAFANA_USER=admin
GRAFANA_PASSWORD=<Grafana 口令>

# ---------- 接入 nightjar ----------
MYSQL_EXPORTER_PASSWORD=<监控账号口令>   # 必改，且要同步改 01-monitor-user.sql
JD_NIGHTJAR_NETWORK=jd-nightjar
NIGHTJAR_URL=http://mwops-backend:8080   # 同机同网络直接用容器名
NIGHTJAR_HOOK_TOKEN=<与 nightjar 的 HOOK_TOKEN 相同>
```

同步改监控账号口令（这一步最容易漏）：

```bash
cd <jd>
set -a; . ./.env; set +a          # 把 .env 载入当前 shell（与 start.sh 的做法一致）
sed -i "s/BY 'exporter_change_me'/BY '${MYSQL_EXPORTER_PASSWORD}'/g" \
  deploy/jd-exporters/init/01-monitor-user.sql
grep "BY '" deploy/jd-exporters/init/01-monitor-user.sql   # 确认两行都已替换
```

> 可选：`SERVER_NAME`（默认 `jd-host`，日志里显示的服务器名）、`LOG_TARGETS`（默认 `/logs/error.log:error:ERROR`）——这两个不在 `.env.example` 里，需要时自行追加。

### 2.2 启动 jd

```bash
# 方式一（集成中心）：只要网络 + 数据面，不需要 jd 自己起 Exporter
./start.sh link
#   → 等价于 docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd-link.yml up -d

# 方式二（复用 jd 的 Prometheus）：原生服务 + 两个 Exporter
./start.sh nightjar
```

`start.sh` 会自动以 `docker network create --internal jd-nightjar` 创建互联网络，并在检测到已存在的网络不是 internal 时告警。

手工等价命令（无 `start.sh` 时）：

```bash
docker network create --internal jd-nightjar 2>/dev/null || true
docker network inspect jd-nightjar --format '{{.Internal}}'   # 期望 true
```

### 2.3 补建 MySQL 只读监控账号

```bash
cd <jd>
# 数据卷为空时（首次启动）会自动执行 init/01-monitor-user.sql，可跳过
# 已有数据卷时执行：
./start.sh nightjar-initdb
# 期望输出：>> monitor user ready
```

手工核对账号是否建好：

```bash
cd <jd>
set -a; . ./.env; set +a      # 载入 DB_PASS
docker compose exec -T mysql mysql -uroot -p"$DB_PASS" -e \
  "SELECT user,host,plugin FROM mysql.user WHERE user='exporter';"
# 期望一行 exporter / % / mysql_native_password
```

### 2.4 启动状态自检

```bash
docker ps --format '{{.Names}}\t{{.Status}}' | grep -E 'interview-(mysql|redis|backend|frontend|prometheus|grafana)|jd-(redis-exporter|mysqld-exporter|log-agent)'

# 网络必须是 internal
docker network inspect jd-nightjar --format '{{.Internal}}'
```

### 2.5 指标链路校验（三选一）

**A. 有 PowerShell 时（jd 仓库自带）**

```bash
pwsh -File scripts/verify-nightjar.ps1
# 方式二独立 Prometheus：pwsh -File scripts/verify-nightjar.ps1 -Standalone
```

**B. 纯 bash + curl（Linux 推荐）** —— 把下面整段存成 `verify-jd-metrics.sh` 后 `bash verify-jd-metrics.sh`：

```bash
#!/usr/bin/env bash
# jd 侧指标链路校验：容器 → 抓取 → 连通 → 标签，逐层收敛。
set -u
PROM="${PROM:-http://127.0.0.1:9090}"   # jd 的 Prometheus 只绑 127.0.0.1
fail=0

q() { # q <promql>  → 命中条数
  curl -sG --max-time 10 --data-urlencode "query=$1" "$PROM/api/v1/query" \
    | grep -o '"metric"' | wc -l | tr -d ' '
}
check() { # check <期望> <实际> <说明>
  if [ "$1" = "$2" ]; then echo "  [PASS] $3（$2）"; else echo "  [FAIL] $3：期望 $1，实际 $2"; fail=$((fail+1)); fi
}

echo "== 1/4 容器层 =="
for c in interview-mysql interview-redis jd-redis-exporter jd-mysqld-exporter; do
  state=$(docker inspect -f '{{.State.Status}}' "$c" 2>/dev/null || echo missing)
  [ "$state" = "running" ] && echo "  [PASS] $c running" || { echo "  [FAIL] $c = $state"; fail=$((fail+1)); }
done

echo "== 2/4 抓取层 =="
for job in middleware-exporter-redis middleware-exporter-mysql; do
  n=$(curl -sG --max-time 10 --data-urlencode "query=up{job=\"$job\"}" "$PROM/api/v1/query" \
      | grep -o '"1"' | wc -l | tr -d ' ')
  check 1 "$n" "up{job=$job}"
done

echo "== 3/4 连通层（Exporter → 中间件）=="
check 1 "$(q 'redis_up')"                'redis_up'
check 1 "$(q 'mysql_up')"                'mysql_up'

echo "== 4/4 标签层（平台靠 instance_name 定位实例）=="
check 1 "$(q 'redis_memory_used_bytes{instance_name="jd-redis"}')" 'redis_memory_used_bytes{instance_name="jd-redis"}'
check 1 "$(q 'mysql_global_status_threads_connected{instance_name="jd-mysql"}')" 'mysql_global_status_threads_connected{instance_name="jd-mysql"}'

echo
[ "$fail" -eq 0 ] && echo "✅ 指标已进入 Prometheus，可以开始平台侧集成" || echo "❌ 失败 $fail 项，按 FAIL 行逐条处理"
exit $([ "$fail" -eq 0 ] && echo 0 || echo 1)
```

**C. 临时直连容器内查（Prometheus 未绑宿主 9090 时）**

```bash
docker exec interview-prometheus wget -qO- \
  'http://localhost:9090/api/v1/query?query=redis_memory_used_bytes{instance_name="jd-redis"}'
```

**判定口径（由外到内，逐层排除）**：

| 层 | 命令 | 期望 | 不通过时的方向 |
|---|---|---|---|
| 容器 | `docker ps` | Exporter running | 看 `docker compose logs`，多为口令或依赖未就绪 |
| 抓取 | `up{job="middleware-exporter-*"}` | `1` | job 不存在 → 抓取配置没生效（方式二要重建 prometheus 容器） |
| 连通 | `redis_up` / `mysql_up` | `1` | 认证失败：Redis 口令 / MySQL 账号（跑 `nightjar-initdb`） |
| 标签 | `{instance_name="jd-*"}` | 有数据 | relabel 未生效（`prometheus-jd.yml` 是否被挂载） |

---

## 3. nightjar 侧：启动

### 3.1 准备 `.env`

```bash
cd <nightjar>
cp .env.example .env
```

```bash
# ---------- 必改 ----------
JWT_SECRET=<openssl rand -hex 32>
ADMIN_USER=admin
ADMIN_PASSWORD=<平台管理员口令>
DB_PASSWORD=<平台 PG 口令>
REDIS_PASSWORD=<平台自身 Redis 口令>
HOOK_TOKEN=<openssl rand -hex 32>        # 必须等于 jd 的 NIGHTJAR_HOOK_TOKEN
PROMETHEUS_PORT=9091                     # 见 1.1 端口冲突

# ---------- 接入 jd ----------
JD_NIGHTJAR_NETWORK=jd-nightjar

# 方式一（集成中心，推荐）：用平台自带的 Prometheus
MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090

# 方式二（复用 jd 的 Prometheus）：
# MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090

# ---------- 集成中心（方式一需要）----------
INTEGRATION_ENABLED=true
INTEGRATION_DOCKER_ENABLED=true                      # 想「一键拉起 Exporter 容器」才需要
INTEGRATION_EXPORTER_NETWORK=middleware-ops_mwops,jd-nightjar
```

> `INTEGRATION_EXPORTER_NETWORK` 是**逗号分隔的多网络**：第一个是监控面（平台 Prometheus 所在网络），
> 其余是数据面（jd 的 internal 网络）。这样 Exporter 既被抓得到，又连得上 jd 的库。

若开启 `INTEGRATION_DOCKER_ENABLED=true`，还要放开 docker.sock：

```yaml
# <nightjar>/docker-compose.yml · backend.volumes
      - /var/run/docker.sock:/var/run/docker.sock
```

> ⚠️ 挂载 docker.sock 等于把宿主机 root 权限交给平台容器。生产环境建议保持关闭，
> 改用「集成中心 → 配置 → 复制 compose 片段/docker run 命令」人工执行。

### 3.2 启动平台

```bash
cd <nightjar>
docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build
```

> `deploy/compose.jd-link.yml` 做两件事：把 `mwops-backend` 加入 `jd-nightjar`（用于 TCP 探测 + 收日志），
> 并给 `MWOPS_PROMETHEUS_BASE_URL` 一个兜底默认值（`.env` 里显式设置的值优先）。

### 3.3 平台可用性自检

```bash
# ① 后端健康
curl -s http://127.0.0.1:8000/healthz

# ② 平台能看到 jd 的别名（关键：说明互联网络通了）
docker exec mwops-backend sh -c 'getent hosts jd-mysql && getent hosts jd-redis'

# ③ 浏览器登录 http://<主机>:8000 ，进入「系统信息」页确认：
#    - 监控数据源：Prometheus（不是「内置模拟器」）
#    - 集成中心可用性提示
```

---

## 4. 接入实例

### 4.1 方式一：集成中心一键集成（推荐）

**第 1 步**：左侧 **资源 → 集成中心**，卡片区应显示 6 个组件（Redis / MySQL / PostgreSQL / Kafka / Elasticsearch / Nginx）。
若顶部提示「仅生成配置」，说明 `INTEGRATION_DOCKER_ENABLED` 或 docker.sock 没就绪（见 3.1）。

**第 2 步**：点 **Redis** 卡片的「集成」，按下表填写：

| 字段 | 填写值 | 为什么 |
|---|---|---|
| 集成名称 | `jd-redis` | **必须**与 Prometheus 的 `instance_name` 一致；平台按它定位指标 |
| 连接地址 | `jd-redis:6379` | jd 在 `jd-nightjar` 上的别名，不是宿主机 IP（库没发布宿主端口） |
| 用户名 | 留空 | 探测只做 TCP，口令仅用于 Exporter 认证 |
| 密码 | `$REDIS_PASSWORD` | AES-256 加密存储，生成的配置里只出现 `${MONITOR_PASSWORD}` 占位 |
| 自定义标签 | `team=interview` | 会写进指标 label，可按标签筛选 |
| Exporter 参数 | 云数据库集群架构才需要打开「跳过 SLOWLOG / LATENCY HISTOGRAM」 | jd 是自建单机 Redis，保持默认即可 |
| 环境 / 分组 | `dev` / `interview` | 数据权限按环境与分组隔离 |
| 一键拉起 Exporter | 勾选 | 由平台创建容器并接入两张网络 |
| 自动创建推荐告警规则 | 勾选 | 自动生成内存率 / 命中率 / 淘汰 三条规则 |

**第 3 步**：点 **MySQL** 卡片，同样填写：

| 字段 | 填写值 |
|---|---|
| 集成名称 | `jd-mysql` |
| 连接地址 | `jd-mysql:3306` |
| 用户名 | `exporter` |
| 密码 | `$MYSQL_EXPORTER_PASSWORD` |
| 环境 / 分组 | `dev` / `interview` |

保存前可点 **预览生成的配置**，确认服务发现 JSON、compose 片段、`docker run` 命令与核对步骤符合预期。

**第 4 步**：验证（服务发现 `refresh_interval` 默认 30 秒）

```bash
# 平台侧：登录拿令牌
TOKEN=$(curl -s -X POST http://127.0.0.1:8000/api/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"'"$ADMIN_PASSWORD"'"}' | grep -o '"token":"[^"]*' | cut -d'"' -f4)

# 集成列表（确认 Exporter 容器状态 running / 已应用；instance_id 从这里取）
curl -s -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8000/api/integrations | head -c 800

# 接入自检（把 1 换成上面返回的 instance_id）：matched 应大于 0，job_up=1，source=prometheus
curl -s -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:8000/api/metrics/1/diagnose" | head -c 800
```

页面等价操作：**集成中心 → 该行「监控/自检」→ 实例详情 →「接入自检」**。

**该方式的数据流**：

```
平台页面保存 → 渲染服务发现目标（instance_name=jd-redis）
            → 平台拉起 mwops-exporter-jd-redis（网络：mwops + jd-nightjar）
            → GET /api/sd/integrations 暴露 targets+labels
            → mwops-prometheus 每 30s http_sd 拉取 → 抓取 http://mwops-exporter-jd-redis:9121/metrics
            → 平台查询 job="middleware-integration",instance_name="jd-redis"
```

排查用的两条命令：

```bash
# 平台侧：服务发现文档（应能看到两个目标，含 instance_name）
curl -s http://127.0.0.1:8000/api/sd/integrations

# Prometheus 视角：该 job 下的 target 是否 up
curl -s 'http://127.0.0.1:9091/api/v1/targets' | grep -o 'middleware-integration' | head -1
```

### 4.2 方式二：复用 jd 自带 Prometheus

**第 1 步**：确认 jd 侧已 `./start.sh nightjar` 且 2.5 节校验全绿。

**第 2 步**：nightjar `.env` 指向 jd 的 Prometheus：

```bash
MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090
```

**第 3 步**：**中间件纳管 → 新增实例**（不走集成中心，因为 Exporter 已由 jd 提供）：

| 字段 | jd-redis | jd-mysql |
|---|---|---|
| 实例名称 | `jd-redis` | `jd-mysql` |
| 中间件类型 | `redis` | `mysql` |
| 连接地址 | `jd-redis` | `jd-mysql` |
| 端口 | `6379` | `3306` |
| 监控账号 / 密码 | 留空 / `$REDIS_PASSWORD` | `exporter` / `$MYSQL_EXPORTER_PASSWORD` |
| 环境 / 分组 | `dev` / `interview` | `dev` / `interview` |
| Prometheus job | 留空（或 `middleware-exporter-redis`） | 留空（或 `middleware-exporter-mysql`） |
| **Prometheus instance** | **留空** | **留空** |

> **`Prometheus instance` 必须留空**：填了之后平台改用 `instance="..."` 匹配并**忽略** `instance_name`，
> 而 jd 的时序只有 `instance_name`，结果就是永远查不到数据。

**第 4 步**：验证（实例详情 →「接入自检」，或 4.1 第 4 步同样的 curl）。

**进阶**：若要让 jd 的 Prometheus 也抓平台拉起的 Exporter，把同一份宿主机目录挂给两边
（见 `jd/deploy/jd-exporters/prometheus-jd.yml` 顶部注释），或直接用集成中心抽屉里的「显式 scrape job」片段。

---

## 5. 日志接入

### 5.1 同步令牌

```bash
# jd/.env
NIGHTJAR_HOOK_TOKEN=<与 nightjar 的 HOOK_TOKEN 完全相同>

# nightjar/.env
HOOK_TOKEN=<同一个值>
```

两侧不一致时，日志上报会被 **401** 拒绝（Agent 日志里能看到 `HTTP=401`）。

### 5.2 启动日志 Agent

```bash
cd <jd>
./start.sh nightjar-logs      # 等价：docker compose ... --profile logs up -d --build
```

> 普通 `./start.sh nightjar` **不会**启动 jd-log-agent（它在 `logs` profile 下），这是"没有日志"最常见的原因。

### 5.3 验证

```bash
# Agent 视角：endpoint / targets / 上报失败原因
docker logs -f jd-log-agent

# 生产日志：制造一条 ERROR
docker compose exec backend sh -c 'echo "$(date "+%F %T") ERROR [jd-smoke] hook 自检" >> /app/data/logs/error.log'

# 平台视角：事件列表
curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:8000/api/log-alerts/events?page=1&page_size=5' | head -c 800
# 服务器自动注册为 jd-host
curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:8000/api/log-alerts/servers' | head -c 400
```

页面：**日志告警 → 事件**（按错误指纹聚合）、**应用 → 服务器与仓库**。

### 5.4 采什么、不采什么

| 文件 | 默认 | 说明 |
|---|---|---|
| `/logs/error.log`（logback 的 ERROR_FILE） | ✅ 采集 | 默认 `LOG_TARGETS=/logs/error.log:error:ERROR` |
| `/logs/interview-review.log` | ❌ | 全量日志量太大，逐行上报不适用；需要时改用官方 Agent |
| `/logs/gc.log` | ❌ | 逐行上报会把告警列表刷满。需要时先把 JVM 参数收敛为 `-Xlog:gc`，再设 `LOG_TARGETS="/logs/error.log:error:ERROR;/logs/gc.log:gc:INFO"` |

改用官方 Agent（带偏移量、上下文行、批量上报，生产推荐）：

```bash
cd <nightjar>/middleware-ops && go build -o mwops-agent ./cmd/agent
# 配置模板：<jd>/deploy/jd-exporters/agent.yaml（platform_url / hook_token / files 已按 jd 填好）
# 注意 gc.log 必须写 level_filter: INFO，否则采不到（该字段早期版本被忽略，已修复）
```

---

## 6. 告警、通知与 AI 诊断

### 6.1 告警规则

- 方式一：集成时勾选「自动创建推荐告警规则」，保存后规则出现在 **治理 → 告警规则**；
- 手工补充建议阈值（与 [`COLLECTOR-JD.md`](COLLECTOR-JD.md) §4.6 一致）：

| 实例 | 指标 | 操作符 | 阈值 | 级别 |
|---|---|---|---|---|
| jd-mysql | `threads_connected` | `>` | 50 | warning |
| jd-mysql | `slow_queries` | `>` | 1 | warning |
| jd-redis | `memory_usage_percent` | `>` | 60 | warning |
| jd-redis | `evicted_keys` | `>` | 0 | warning |

### 6.2 通知渠道

```bash
# nightjar/.env
FEISHU_ENABLED=true
FEISHU_WEBHOOK=<webhook>
```

在 **告警中心 → 通知自检** 发一条测试消息；未配置 webhook 时只记录通知日志，不影响主链路。

### 6.3 AI 诊断

实例详情 → **AI 诊断**，或在 **智能 → AI 诊断** 里带实例提问。
出网合规默认关闭：需显式把服务加入 `security.outbound_whitelist` 并置 `allow_third_party=true`，否则走本地检索 + 本地 LLM（`SELF_HOSTED_KIND=mock` 时用内置规则引擎）。

---

## 7. 端到端验收（约 10 分钟）

| # | 步骤 | 命令 / 操作 | 期望 |
|---|---|---|---|
| 1 | jd 容器 | `docker ps` | mysql/redis/backend/frontend/prometheus 全 running |
| 2 | 互联网络 | `docker network inspect jd-nightjar --format '{{.Internal}}'` | `true` |
| 3 | 指标进库 | 2.5 节脚本 | 全 PASS |
| 4 | 平台可达 | `docker exec mwops-backend getent hosts jd-mysql` | 解析出 IP |
| 5 | 平台登录 | `http://<主机>:8000` | 能登录，系统信息显示监控数据源 Prometheus |
| 6 | 实例指标 | 集成中心/实例详情 → 接入自检 | `matched>0`、`job_up=1` |
| 7 | 日志事件 | 5.3 节制造 ERROR | 事件列表出现该指纹，服务器 `jd-host` |
| 8 | 告警闭环 | jd 前端「沙箱」跑 **RedisFault** / **MySqlSlow** | 平台告警中心出现告警 |
| 9 | AI 诊断 | 告警 → 「AI 诊断」 | 返回结构化结论（根因/证据/置信度/建议/影响） |
| 10 | 平台冒烟 | `pwsh -File <nightjar>/scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8000` | 全绿（覆盖权限、护栏、审计链、SQL 只读等） |

---

## 8. 常见问题速查

| 现象 | 原因 | 处理 |
|---|---|---|
| `network jd-nightjar not found` | 先起了 nightjar | 先 `cd <jd> && ./start.sh link`（或 `nightjar`），再起平台 |
| 平台显示「内置模拟器」 | `MWOPS_PROMETHEUS_BASE_URL` 空或不通 | 按接入方式设为 `http://prometheus:9090` 或 `http://jd-prometheus:9090` |
| 有实例但指标全 0 / unknown | `job_up=null`：job 未配置；`job_up=0`：Exporter 抓取失败；`job_up=1` 且 `matched=0`：标签对不上 | 依次看「接入自检」的提示；名称必须等于 `instance_name` |
| 趋势图有曲线但数值很"整" | 修复前历史查询会用模拟器补齐空结果 | 升级后用「接入自检」确认 `source=prometheus` 且 `matched>0` |
| Redis 指标全无（方式二） | Redis 开了 `requirepass` 但 Exporter 没拿到口令 | 核对 jd `.env` 的 `REDIS_PASSWORD` 与 redis-exporter 环境变量 |
| mysqld-exporter 认证失败 | 账号未建或口令不一致 | `./start.sh nightjar-initdb`；核对 `.env` 与 `01-monitor-user.sql` |
| 平台启动报端口占用 | 两边 Prometheus 都占 9090 | 见 1.1 节，给 nightjar 设 `PROMETHEUS_PORT=9091` |
| 实例状态「连接失败」 | TCP 探测不通 | 方式一/二都需 `jd-nightjar` 网络；`docker exec mwops-backend getent hosts jd-redis` |
| 日志页完全没有事件 | 没启用 logs profile，或令牌不一致 | `./start.sh nightjar-logs`；两侧 `HOOK_TOKEN` 对齐；`docker logs jd-log-agent` |
| 日志上报 `HTTP=000` | Agent 连不上平台 | 确认 Agent 在 `jd-nightjar` 内、`NIGHTJAR_URL` 为 `http://mwops-backend:8080` |
| Agent 报「等待 /logs/error.log 超时」 | 后端从未写过该文件 | 确认 `backend-logs` 卷挂载与 `LOG_PATH=/app/data/logs` |
| 集成中心提示「仅生成配置」 | 未开 `INTEGRATION_DOCKER_ENABLED` 或没挂 docker.sock | 按 3.1 节开启，或直接用渲染出的 compose/`docker run` 人工执行 |
| 集成保存成功但没指标 | 服务发现未到期 / Prometheus 拉不到平台接口 | 等 30s；`curl -s http://127.0.0.1:8000/api/sd/integrations`；`docker exec mwops-prometheus wget -qO- http://backend:8080/api/sd/integrations` |
| Exporter 起来了但 `up=0`（方式一） | Exporter 连不上 jd 的库 | 核对 `INTEGRATION_EXPORTER_NETWORK` 是否含 `jd-nightjar`；`docker inspect mwops-exporter-jd-redis \| grep -A5 Networks` |

---

## 9. 回滚与下线

```bash
# 只摘掉某个实例（方式一）
#   集成中心 → 该行「删除」：同时移除抓取目标与 Exporter 容器，历史告警/诊断保留

# 只摘掉某个实例（方式二）
#   中间件纳管 → 删除

# 平台整体下线（保留数据卷）
cd <nightjar> && docker compose down

# jd 侧退回「未接入 nightjar」的原生形态
cd <jd> && ./start.sh restore      # docker compose -f docker-compose.yml up -d --remove-orphans

# 彻底清理（含数据卷，慎用）
cd <jd> && ./start.sh clean        # docker compose down -v
cd <nightjar> && docker compose down -v
```

> 下线顺序建议：先删平台实例 → 再停平台 → 最后 `restore` jd，避免平台侧留下"探测失败"的历史噪音。

---

## 10. 附录

### A. 变量总表

**jd 侧（`<jd>/.env`）**

| 变量 | 必改 | 说明 |
|---|---|---|
| `DB_NAME` / `DB_USER` / `DB_PASS` | ✅ | MySQL 库名与 root 口令 |
| `REDIS_PASSWORD` | ✅ | 必须非空；Redis 鉴权与 redis-exporter 共用 |
| `JWT_SECRET` | ✅ | ≥32 位随机串 |
| `MYSQL_EXPORTER_PASSWORD` | ✅ | 只读监控账号口令，需与 `01-monitor-user.sql` 一致 |
| `JD_NIGHTJAR_NETWORK` |  | 跨项目网络名，默认 `jd-nightjar` |
| `NIGHTJAR_URL` |  | 日志上报地址，同机用 `http://mwops-backend:8080` |
| `NIGHTJAR_HOOK_TOKEN` | ✅ | 必须等于 nightjar 的 `HOOK_TOKEN` |
| `PROMETHEUS_PORT` / `GRAFANA_PORT` / `WEB_PORT` / `BACKEND_PORT` |  | 同机部署注意 9090 冲突 |
| `SERVER_NAME` / `LOG_TARGETS` |  | 可选，不在 `.env.example` 中，需要时追加 |

**nightjar 侧（`<nightjar>/.env`）**

| 变量 | 必改 | 说明 |
|---|---|---|
| `JWT_SECRET` / `ADMIN_PASSWORD` / `DB_PASSWORD` / `REDIS_PASSWORD` | ✅ | 平台自身密钥与口令 |
| `HOOK_TOKEN` | ✅ | 与 jd 的 `NIGHTJAR_HOOK_TOKEN` 相同 |
| `JD_NIGHTJAR_NETWORK` |  | 默认 `jd-nightjar` |
| `MWOPS_PROMETHEUS_BASE_URL` | ✅ | 方式一 `http://prometheus:9090`；方式二 `http://jd-prometheus:9090` |
| `PROMETHEUS_PORT` |  | 同机部署改 `9091` 避让 jd |
| `INTEGRATION_ENABLED` / `INTEGRATION_JOB_NAME` / `INTEGRATION_AUTO_RULES` |  | 集成中心开关与抓取任务名 |
| `INTEGRATION_DOCKER_ENABLED` / `INTEGRATION_DOCKER_HOST` |  | 一键拉起 Exporter（需挂 docker.sock） |
| `INTEGRATION_EXPORTER_NETWORK` |  | 多网络：`middleware-ops_mwops,jd-nightjar` |

### B. 文件总表（jd 侧改动）

| 文件 | 作用 |
|---|---|
| `docker-compose.yml` | 四网分区；MySQL/Redis 取消宿主端口；日志落 `backend-logs` 卷 |
| `deploy/jd-exporters/docker-compose.jd.yml` | overlay：两个 Exporter + `jd-nightjar` 别名 + initdb + 日志 Agent |
| `deploy/jd-exporters/docker-compose.jd-link.yml` | 只打通网络的最小 overlay |
| `deploy/jd-exporters/prometheus-jd.yml` | 抓取配置：两个 job 用 relabel 写 `instance_name` |
| `deploy/jd-exporters/init/01-monitor-user.sql` | MySQL 只读监控账号 |
| `deploy/jd-exporters/log-shipper.sh` / `Dockerfile.logagent` / `agent.yaml` | 日志上报（shell 版 / 镜像 / 官方 Agent 模板） |
| `backend/src/main/resources/logback-spring.xml` | `error.log`（仅 ERROR）+ 全量日志 |
| `backend/Dockerfile` | GC 日志路径 `/app/data/logs/gc.log` |
| `start.sh` | `nightjar` / `nightjar-logs` / `nightjar-initdb` / `link` / `restore` / `exporters-only` |

### C. 端点与端口总表

| 端点 | 方向 | 用途 |
|---|---|---|
| `jd-prometheus:9090` | nightjar → jd | 指标查询（方式二） |
| `jd-mysql:3306` / `jd-redis:6379` | nightjar → jd | TCP 健康探测 |
| `mwops-backend:8080/api/hooks/logs` | jd → nightjar | 日志上报 |
| `prometheus:9090`（平台自身） | 平台内部 | 集成中心 HTTP 服务发现 + 抓取（方式一） |
| `/api/sd/integrations` | Prometheus → 平台 | 服务发现（`http_sd_configs`，只含地址与标签） |
| `mwops-exporter-<名称>:<端口>` | 平台内部 | 集成中心拉起的 Exporter |

### D. 相关文档

| 文档 | 内容 |
|---|---|
| [`COLLECTOR-JD.md`](COLLECTOR-JD.md) | 接入契约、字段含义、无监控/无日志的完整排查树 |
| [`INTEGRATION.md`](INTEGRATION.md) | 集成中心的字段对照、落地方式、组件矩阵与安全边界 |
| [`API.md`](API.md) | 接口清单（含 `/api/integrations/*`、`/api/metrics/:id/diagnose`） |
| [`OPERATIONS.md`](OPERATIONS.md) | 平台运维（备份、升级、排障、审计校验） |
