# jd 项目接入本平台 · 操作文档

> 接入对象：[gitee.com/unihao/jd](https://gitee.com/unihao/jd)（面试题库 / AI 模拟面试演练系统）
> 本文档基于该仓库 `master` 分支的实际文件内容编写（`docker-compose.yml`、`backend/src/main/resources/application*.yml`、`backend/Dockerfile`、`monitoring/prometheus/prometheus.yml`）。

---

## 1. 该项目现状（先读，决定接入策略）

### 1.1 技术栈与端口（来自其 `docker-compose.yml`）

| 服务 | 镜像 | 容器名 | 宿主机端口 | 说明 |
|------|------|--------|-----------|------|
| mysql | mysql:8.0 | interview-mysql | 3306 | 库名 `interview_review`，root/`DB_PASS` |
| redis | redis:7-alpine | interview-redis | 6379 | **无密码**；`--maxmemory-policy allkeys-lru`；仅 Agent 面试会话缓存用，可用 `AGENT_INTERVIEW_REDIS_ENABLED=false` 关闭 |
| backend | 自建（Temurin 21） | interview-backend | 8541 → 8080 | Spring Boot；健康检查 `/api/health` |
| frontend | 自建 Nginx | interview-frontend | 8542 → 80 | Vue 3 |
| prometheus | prom/prometheus:v2.54.1 | interview-prometheus | **9090** | 抓后端 `/actuator/prometheus` |
| grafana | grafana/grafana:11.2.2 | interview-grafana | 3000（compose 中定义） | 可视化 |

网络：`interview-net`（bridge）。

### 1.2 关键结论：三件事已经具备、两件事缺失

| 结论 | 依据 |
|------|------|
| ✅ **已有 Prometheus** | compose 已含 `prometheus:9090`，可直接作为平台的指标数据源 |
| ✅ **后端已暴露应用指标** | `application.yml` 中 `management.endpoints.web.exposure.include: health,info,prometheus`、`management.prometheus.metrics.export.enabled: true`，抓取路径 `/actuator/prometheus`，job 名 `interview-backend` |
| ✅ **GC 日志已落盘** | Dockerfile 的 `JAVA_OPTS` 含 `-Xlog:gc*,gc+age=trace:file=/app/data/gc.log:...` |
| ❌ **没有中间件 Exporter** | `monitoring/prometheus/prometheus.yml` 只有 `interview-backend` 与 `prometheus` 两个 job，**没有 redis_exporter / mysqld_exporter** |
| ❌ **没有应用错误日志文件** | 只有 `logging.level` 配置，日志走 stdout；compose 未挂载日志目录 |

**因此本平台目前能直接采到的是应用指标，采不到 MySQL / Redis 的中间件指标。** 接入的核心工作就是**补齐两个 Exporter**（见第 3 节），其余（平台纳管、告警、AI 诊断）都是平台侧配置。

> 概念澄清：平台的「统一监控」管理**中间件指标**（Redis 内存、MySQL 连接数等）；
> jd 的 `/actuator/prometheus` 是**应用指标**（HTTP QPS、JVM 等）。两者互补，但**不能互相替代**——
> 想诊断「MySQL 慢查询」必须要有 mysqld-exporter。

### 1.3 可被纳管的中间件实例（2 个）

| 实例名（建议） | 类型 | 环境 | 分组 | 地址 |
|----------------|------|------|------|------|
| `jd-mysql` | mysql | dev | interview | `mysql:3306`（容器内）/ `<宿主机IP>:3306` |
| `jd-redis` | redis | dev | interview | `redis:6379` |

> 注意：jd 的 MySQL 用户是 `root`，平台要求**最小权限只读账号**。平台侧纳管时请填新建的
> `exporter` 账号，不要填 root（平台不会用它做协议级连接，但仍应遵循最小权限原则）。

### 1.4 顺带一个高价值用法

jd 自带**故障演练场景**（`sandbox/scenario/impl/`）：`RedisFaultScenario`、`MySqlSlowScenario`、
`GcLeakScenario`、`DeadlockScenario`、`DubboRpcScenario`。
这些场景会真实制造「Redis 异常 / MySQL 慢查询 / GC 泄漏 / 死锁」。
接入后你可以**用这些场景当作本平台的 AI 诊断验证集**：先触发场景 → 等平台采到异常指标并告警
→ 让 AI 诊断 → 与预期根因对比。这比人工造数据可靠得多。

---

## 2. 总体接入路径

```
jd 项目的 MySQL/Redis ──▶ 新增 Exporter ──▶ jd 的 Prometheus(9090) ──▶ 平台（纳管 + 告警 + AI 诊断）
                                              ▲
                                   本文档第 3 节补的就是这一段
```

三条路径，按你的偏好选一条：

| 方案 | 做法 | 适用 |
|------|------|------|
| **A. 复用 jd 的 Prometheus**（推荐） | 给 jd 的 compose 追加载两个 Exporter，并替换 Prometheus 抓取配置 | jd 用 MySQL profile 正常启动 |
| **B. 独立起 Exporter + 独立 Prometheus** | 不动 jd 任何文件，平台侧多一个数据源（9190） | 不想改 jd；或 jd 用 SQLite profile（无 mysql 服务） |
| **C. 完全独立自建** | 自己在别的机器上部署 Exporter 与 Prometheus | 生产环境、jd 与平台不同机 |

---

## 3. 操作步骤

### 3.0 前置：确认 jd 已启动

```bash
cd <jd 项目目录>
docker compose ps
curl -sf http://localhost:8541/api/health && echo "jd backend OK"
curl -sf http://localhost:9090/-/healthy && echo "jd prometheus OK"
```

### 3.1 方案 A：复用 jd 的 Prometheus（推荐）

**第 1 步**：把本仓库的 `deploy/jd-exporters/` 整个目录拷到 **jd 项目根目录的 `deploy/` 下**：

```bash
cp -r <本仓库>/deploy/jd-exporters <jd 项目目录>/deploy/
```

> ⚠️ 路径位置有约束：compose 中的 bind mount 是相对**项目目录**解析的，两个 override 文件都
> 引用 `./deploy/jd-exporters/...`。因此必须落在 `<jd项目根>/deploy/jd-exporters/`，
> 否则会报 `bind source path does not exist`。
> 若你确实想放在别处，请相应修改 override 文件里的路径，或用软链接：
> `ln -s <实际路径> <jd项目根>/deploy/jd-exporters`。

**第 2 步**：为 MySQL 创建只读监控账号。二选一：

- **空数据卷首次启动**（最省事）：`01-monitor-user.sql` 会被 MySQL 自动执行，无需额外操作；
- **已有数据卷**（不能删数据）：手工执行一次

  ```bash
  docker exec -i interview-mysql mysql -uroot -p"$DB_PASS" -e "
    CREATE USER IF NOT EXISTS 'exporter'@'%' IDENTIFIED WITH mysql_native_password BY 'exporter_change_me';
    GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';
    FLUSH PRIVILEGES;"
  ```

  > MySQL 8.0 要求 `mysql_native_password`，否则 mysqld-exporter 会报认证失败。
  > 若你的 MySQL 是 8.4+（该插件默认禁用），改用 `caching_sha2_password` 并给 exporter 加
  > `--mysqld.username/--mysqld.password` 参数，或先 `SHOW VARIABLES LIKE 'default_authentication_plugin'` 确认支持情况。

**第 3 步**：追加启动（**不改动 jd 原有 service 定义，只追加**）：

```bash
docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml up -d
```

**第 4 步**：验证指标已进 Prometheus：

```bash
# Redis 指标（应有 redis_memory_used_bytes 等）
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=redis_memory_used_bytes{job="middleware-exporter-redis"}' | head -c 400

# MySQL 指标（应有 mysql_global_status_threads_connected 等）
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=mysql_global_status_threads_connected{job="middleware-exporter-mysql"}' | head -c 400
```

### 3.2 方案 B：独立启动（不动 jd 文件 / jd 用 SQLite）

```bash
cd <jd 项目目录>
# 0) 同样需要先把资产放到 <jd项目根>/deploy/jd-exporters/（见 3.1 第 1 步的路径约束）

# 1) 确认 jd 的网络名（默认按目录名推导为 jd_interview-net）
docker network ls | grep interview

# 2) 若网络名不同，通过环境变量覆盖
JD_NETWORK=<实际网络名> docker compose \
  -f deploy/jd-exporters/docker-compose.exporters-only.yml up -d

# 3) 创建 MySQL 只读监控账号（一次性容器，执行后退出）
docker compose -f deploy/jd-exporters/docker-compose.exporters-only.yml \
  --profile initdb run --rm mysql-monitor-user
```

该方案会起一个**独立的 Prometheus（端口 9190）**，平台侧把 `prometheus.base_url`
指向 `http://<宿主机IP>:9190` 即可（见 3.4），与 jd 自带的 9090 互不干扰。

> 若 jd 用 SQLite profile（没有 `mysql` 服务），步骤 3 会失败——此时跳过即可，只接入 Redis：
> 方案 B 的 Exporter 端口是映射到宿主机的，`mysqld-exporter` 会因连不上 MySQL 而重启，
> 可用 `docker compose -f ... up -d redis-exporter` 只启动需要的那个。

### 3.3 平台侧：纳管两个实例

登录平台 → **中间件纳管 → 新增实例**，按下表填写（**标签字段务必一致，否则查不到数据**）：

**实例一：jd-mysql**

| 字段 | 值 | 说明 |
|------|-----|------|
| 实例名称 | `jd-mysql` | 必须等于 Prometheus 中的 `instance_name` |
| 中间件类型 | MySQL | 决定用哪套指标画像 |
| 连接地址 / 端口 | `mysql` / `3306`（或宿主机 IP） | 仅用于 TCP 健康探测 |
| 监控账号 / 密码 | `exporter` / `exporter_change_me` | 只读账号，AES-256-GCM 加密存储 |
| 环境 | `dev` | 决定数据权限与审批强制 |
| 分组 | `interview` | 建议按项目分组 |
| Prometheus job | `middleware-exporter-mysql` | 精确匹配 |
| Prometheus instance | `mysqld-exporter:9104` | 可选，最精确 |
| **配置（config）** | 见 3.5 | **直接影响 AI 诊断质量** |

**实例二：jd-redis**

| 字段 | 值 |
|------|-----|
| 实例名称 | `jd-redis` |
| 中间件类型 | Redis |
| 连接地址 / 端口 | `redis` / `6379` |
| 监控账号 / 密码 | 留空（jd 的 Redis 无密码） |
| 环境 / 分组 | `dev` / `interview` |
| Prometheus job | `middleware-exporter-redis` |
| Prometheus instance | `redis-exporter:9121` |

### 3.4 平台侧：指向数据源

```yaml
# <本平台>/configs/config.yaml，或环境变量 MWOPS_PROMETHEUS_BASE_URL
prometheus:
  base_url: http://<jd宿主机IP>:9090     # 方案 A
  # base_url: http://<jd宿主机IP>:9190   # 方案 B
  exporter_job_prefix: middleware-exporter
```

```bash
docker compose up -d backend     # 重启平台后端使配置生效
```

验证：平台「统一监控」页应能看到 `jd-mysql` / `jd-redis` 的指标；
若显示「内置模拟器」= 没连上；若有指标但值异常 = 标签没匹配上（回到 3.3 核对）。

### 3.5 平台侧：填写 config（让 AI 有据可依）

在实例的「配置」字段填入 jd 的实际运行参数（这些会进入 AI 诊断上下文，使其能引用证据而非推测）：

**jd-mysql 的 config**：

```json
{
  "profile": "mysql",
  "ddl-auto": "update",
  "database": "interview_review",
  "connection_url_hint": "useUnicode=true&characterEncoding=utf8&serverTimezone=Asia/Shanghai",
  "note": "jd 项目演练库；沙箱场景 MySqlSlowScenario 会制造慢查询"
}
```

**jd-redis 的 config**：

```json
{
  "maxmemory-policy": "allkeys-lru",
  "appendonly": "yes",
  "requirepass": "none",
  "usage": "Agent 面试会话缓存（AgentInterviewSessionService）；可用 AGENT_INTERVIEW_REDIS_ENABLED=false 关闭",
  "note": "沙箱场景 RedisFaultScenario 会制造 Redis 异常"
}
```

### 3.6 平台侧：配置告警规则

jd 是开发/演练环境，建议用能触发但不过度打扰的阈值：

| 实例 | 指标 | 操作符 | 阈值 | 级别 | 说明 |
|------|------|--------|------|------|------|
| jd-mysql | `threads_connected` | `>` | 50 | warning | 演练环境连接数偏低，50 即可预警 |
| jd-mysql | `slow_queries` | `>` | 1 | warning | 触发 MySqlSlowScenario 后应立刻命中 |
| jd-redis | `memory_usage_percent` | `>` | 60 | warning | 演练环境内存占用小，60% 足够敏感 |
| jd-redis | `evicted_keys` | `>` | 0 | warning | 触发淘汰即说明容量有问题 |

> 平台规则的阈值是你自己填的，与指标画像的默认阈值（面向生产）无关。

### 3.7 平台侧：日志与 GC 接入（可选但推荐）

分三种情况，**没有一种能同时拿到应用错误日志且零侵入**，请按需选择：

| 目标 | 是否零侵入 | 做法与限制 |
|------|-----------|-----------|
| **GC 日志** | ✅ 零侵入 | GC 日志已写到容器内 `/app/data/gc.log`（`VOLUME /app/data`）。但它是**命名卷**不是宿主目录，Agent 无法直接 tail；需在 compose 里把 `./backend/data:/app/data` 改成 bind mount，再让宿主机上的 Agent 采集该目录 |
| **应用 ERROR 日志 + 堆栈** | ❌ 需改 jd | 默认日志只输出 stdout。要拿到异常堆栈需二者之一：① 给 jd 加 file appender 写 `/app/data/app.log` 后按上一条采集；② 在 jd 的全局异常处理器里 POST 到平台 `/api/hooks/logs`（jd 已有 `AgentInterviewController` 等入口，可仿照） |
| **不修改 jd 的折中** | ✅ 零侵入 | 用 `docker logs` 定时抓取：写个 cron 脚本把 `docker logs --since` 的输出投给平台 Hook。**只能做规则/关键词级聚合，无法拿到完整上下文**，适合先用起来 |

日志上报命令（参见平台 `docs/COLLECTOR.md` 第 6 节，或用现成脚本）：

```bash
# 用平台提供的示例脚本把 jd 容器日志投给平台
docker logs --since 5m interview-backend 2>&1 \
  | grep -E 'ERROR|Exception' > /tmp/jd-error.log
powershell -ExecutionPolicy Bypass -File scripts\hook-log-report.ps1 \
  -LogFile /tmp/jd-error.log -Service interview-backend -BaseUrl http://<平台IP>:8000
```

### 3.8 平台侧：（可选）登记仓库映射与出网白名单

若要使用「AI 代码分析」（把堆栈定位到源码），在平台 **服务器与仓库** 中登记：

```json
{
  "service_name": "interview-review-backend",
  "repo_url": "https://gitee.com/unihao/jd.git",
  "branch": "master",
  "local_path": "/data/repos/jd",
  "language": "java",
  "allow_third_party": false
}
```

出网白名单默认**关闭**（合规优先）。仅当你确认「允许把堆栈与代码片段发送到第三方 AI」时，
再把 `interview-review-backend` 加入平台 `security.outbound_whitelist` 并置 `allow_third_party=true`。
否则代码分析走本地检索 + 本地 LLM（若已配置）。

---

## 4. 验证清单

按顺序执行，每步都应有明确输出：

```bash
# ① Exporter 自身有数据
curl -s http://localhost:9121/metrics | grep -E '^redis_(up|memory_used_bytes)'
curl -s http://localhost:9104/metrics | grep -E '^mysql_(up|global_status_threads_connected)'

# ② Prometheus 抓到了（工作 A/B 分别用 9090 / 9190）
curl -s 'http://localhost:9090/api/v1/targets' | grep -o '"job":"middleware-exporter-[a-z]*"'
curl -s 'http://localhost:9090/api/v1/query' \
  --data-urlencode 'query=redis_memory_used_bytes{instance_name="jd-redis"}'

# ③ 平台采到了（换成你的 JWT 与实例 ID）
curl -s -H "Authorization: Bearer $TOKEN" http://<平台IP>:8000/api/metrics/<实例ID> | head -c 500

# ④ 触发演练场景，验证告警与 AI 诊断
#    在 jd 前端「沙箱」页选择 RedisFault / MySqlSlow 场景运行
#    随后在平台「告警中心」应看到告警，点「AI 诊断」查看结构化结论
```

平台侧全链路冒烟：

```powershell
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:8000
```

---

## 5. 常见问题

| 现象 | 原因 | 处理 |
|------|------|------|
| mysqld-exporter 启动即退出 / 报认证失败 | MySQL 8 认证插件或账号未创建 | 用 `mysql_native_password` 建号；确认 `GRANT PROCESS, REPLICATION CLIENT, SELECT` |
| `Can't connect to MySQL server on 'mysql'` | Exporter 与 MySQL 不在同一网络，或用了 SQLite profile（无 mysql 服务） | 方案 A 需 MySQL profile；否则改用方案 B（host 用容器名 `jd-mysql-db`） |
| 平台显示「内置模拟器」 | 平台 `prometheus.base_url` 未配置或不通 | 配置并重启平台后端；确认平台容器能访问 jd 宿主机 9090/9190 |
| 有指标但值为空 / `unknown` | `instance_name` 与纳管实例名不一致 | 实例名必须等于 `jd-redis` / `jd-mysql`（见 `prometheus-jd.yml` 的标签与 relabel） |
| Redis 指标全无 | jd 的 Redis 被关闭（`AGENT_INTERVIEW_REDIS_ENABLED=false`） | 打开该开关，或该实例确实不纳管 |
| 平台查询超时 | 跨主机访问 Prometheus 被防火墙拦截 | 放通平台 → jd 宿主机 9090/9190 |
| 找不到 `interview-net` | jd 的网络名前缀是目录名 | `docker network ls` 查实际名，用 `JD_NETWORK=` 覆盖 |

---

## 6. 一句话总结

jd 已经帮你把 Prometheus 和后端指标准备好了，**你只需要补两个 Exporter（Redis、MySQL）+ 建一个只读监控账号**，
然后在平台纳管 `jd-redis` / `jd-mysql` 两个实例、把 `prometheus.base_url` 指过去即可。
之后可直接用 jd 自带的故障演练场景（Redis 故障 / MySQL 慢查询 / GC 泄漏 / 死锁）来验证平台的告警与 AI 诊断效果。
