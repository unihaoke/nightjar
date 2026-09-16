# jd 接入 nightjar · 操作指南（平台托管监控·jd 零配置）

> 架构已定：**监控栈统一在 nightjar**。被管项目（jd）只跑业务，不装 Exporter、不装 Agent、
> 不跑 Prometheus/Grafana；平台负责建只读账号、拉起 Exporter、自动接入对方网络、抓取、出大盘、采日志。
>
> 设计细节见 [`INTEGRATION.md`](INTEGRATION.md)，接口见 [`API.md`](API.md)，运维见 [`OPERATIONS.md`](OPERATIONS.md)。

---

## 1. 职责边界

```
┌──────── nightjar（唯一监控栈）──────────────────────────┐
│ mwops-prometheus   抓取全部 Exporter                    │
│ mwops-grafana      大盘（数据源已自动配好）              │
│ mwops-backend      纳管/查询/告警/AI 诊断/日志接收        │
│ mwops-exporter-*   按「集成」创建，自动接入两张网         │
│ mwops-*-logs       日志采集容器，挂载被管项目的日志卷      │
└──────────┬─────────────────────────────────────────────┘
           │ 平台用 Docker API 自己发现目标容器所在网络并接入（jd 零改动）
┌──────────┴─ jd（零监控配置）───────────────────────────┐
│ mysql / redis / backend / frontend                     │
│ 只提供：容器名（interview-mysql / interview-redis）      │
│        + backend-logs 卷                                │
└────────────────────────────────────────────────────────┘
```

| 事项 | 谁负责 | jd 需要做什么 |
|---|---|---|
| 只读监控账号 | 平台（勾选后自动建号，prod 走审批） | 集成时填一次管理凭据 |
| Exporter | 平台（Docker API 创建） | 无 |
| 网络接入 | 平台（`ResolveTarget` 反查容器所在网络并自动接入） | **无**（不建网络、不加别名、不改 compose） |
| 指标抓取 | 平台自带 Prometheus | 无 |
| 大盘 | 平台自带 Grafana | 无（可选：按编号导入官方大盘） |
| 日志采集 | 平台侧采集容器读同一日志卷 | 无 |
| 告警规则 | 平台按模板自动创建 | 无 |

---

## 2. jd 侧：什么都不用做

jd 侧只有一个日常动作：把业务起起来。

```bash
cd <jd>
cp .env.example .env        # 必改：DB_PASS、REDIS_PASSWORD、JWT_SECRET
./start.sh                  # 等价于 docker compose up -d --build
./start.sh check            # 可选自检：容器 / 日志卷 / 日志目录挂载
```

jd 的 `.env` 只有这些变量：`DB_NAME/DB_USER/DB_PASS`、`REDIS_PASSWORD`、
`AGENT_INTERVIEW_REDIS_ENABLED`、`JWT_SECRET`、`SPRING_PROFILES_ACTIVE`、
`BACKEND_PORT/WEB_PORT`、`AI_PROVIDER`。
（`PROMETHEUS_PORT`/`GRAFANA_*`/`MYSQL_EXPORTER_PASSWORD`/`NIGHTJAR_HOOK_TOKEN`/网络名 都已不再需要。）

自检通过的标准：

| 检查 | 期望 |
|---|---|
| `interview-mysql` / `interview-redis` / `interview-backend` | running（并打印它们真实所在的网络，供平台自动接入） |
| `interview-frontend` | running（不参与监控，失败只提示） |
| `backend` 的 `/app/data/logs` | 挂的是**命名卷**且已有 `.log` 文件 |

---

## 3. 平台侧：一条命令 + 三次点击

```bash
cd <nightjar>
./scripts/setup-jd-link.sh --dry-run    # ① 预览：打印将要改的 .env 与启动命令
./scripts/setup-jd-link.sh              # ② 执行：写 .env → 起平台 → 自检 → 打印下一步
```

脚本只动平台自己：生成缺失密钥（十六进制，免转义）、打开 `INTEGRATION_DOCKER_ENABLED`、
**自动放开 `docker-compose.yml` 里 docker.sock 的注释**（自动发现网络的前提）、
写 `MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090`，
然后用普通的 `docker compose up -d --build` 启动平台——**不再需要任何 overlay 或互联网络**。

然后在浏览器完成三次集成（**被管项目无需再改任何东西**）：

| 步骤 | 集成中心操作 |
|---|---|
| ③ 集成 MySQL | 名称 `jd-mysql`、地址 `interview-mysql:3306`、监控账号 `exporter`、口令留空；勾选 **由平台创建只读监控账号**（填 root 管理凭据）+ **一键拉起 Exporter** |
| ④ 集成 Redis | 名称 `jd-redis`、地址 `interview-redis:6379`、勾选一键拉起（Redis 无需建账号） |
| ⑤ 日志接入 | 目标容器名 `interview-backend`、服务名 `interview-review-backend`、级别 `ERROR` → 先「读取 docker 配置并预览」再「创建采集容器」 |

保存集成的瞬间，平台会自己完成网络接入：反查 `interview-mysql` / `interview-redis`
命中的容器 → 取得真实网络名 `jd_jd-data` → 把 Exporter 接成
「`middleware-ops_mwops`（Prometheus 抓它）+ `jd_jd-data`（它连数据库）」两张网，
并把平台自身也接进 `jd_jd-data` 以便做 TCP 健康探测。这些都发生在平台侧，
jd 的网络、别名、compose 文件一个字节都不动（集成卡片会写明"已发现目标容器…所在网络…"）。

> 地址填**容器名**最稳（`interview-mysql`）；填 compose 服务名（`mysql`）平台也会换算成容器名。
> 填一个 docker 里不存在的名字时，报错会列出候选容器名。

集成保存后平台会自动核验：抓取目标 `up` 就标记「已应用」；失败则把 Prometheus 的
`lastError` 翻译后写回列表（不必再去 `/targets` 页面翻）。

验收：

```bash
cd <nightjar>
./scripts/doctor-jd-link.sh     # 平台容器 / docker.sock / 目标容器网络 / Exporter 网络 / 抓取状态
```

页面验收：**统一监控**能看到 `jd-redis` / `jd-mysql` 的曲线（趋势图 Y 轴按数据自适应）；
**日志告警 → 事件**能看到 `interview-review-backend` 的 ERROR 事件；
**Grafana**（`http://<主机>:3000`）数据源已就绪，大盘按集成卡片给的编号导入。

---

## 4. 排障对照表

| 现象 | 原因 | 处理 |
|---|---|---|
| 集成报「无法确定目标所在网络」 | 地址里的名字与 docker 里的容器名/服务名/别名都不匹配，或平台没挂 docker.sock | 报错里会列出候选容器名；`./scripts/setup-jd-link.sh` 会自动放开 docker.sock |
| 集成报「解析不了 jd-mysql / server misbehaving」 | 用的是**旧架构的人工别名**（jd 侧已不再提供） | 把地址改成容器名 `interview-mysql:3306` / `interview-redis:6379`，重新保存即可（平台会自动接入 `jd_jd-data`） |
| 集成保存成功但指标为空（`job_up=0`） | Exporter 连不上目标：账号没建 / 口令不一致 / 目标容器没运行 | 列表里的「待处理」已写明 lastError；勾选代建账号可自动解决认证类问题 |
| 报 `Access denied for user 'exporter'` | 只读账号不存在或口令不一致 | 重新保存并勾选「由平台创建只读监控账号」（等效手工：见 `INTEGRATION.md` 的模板 SQL） |
| 报 `invalid DSN` | 旧版 Exporter 配置在拼 `DATA_SOURCE_NAME` | 已被官方方式取代（`--mysqld.username` + `MYSQLD_EXPORTER_PASSWORD`）；重建 Exporter 即可 |
| Exporter 起来了但 up=0，且 lastError 是 `connection refused` | Exporter 不在目标网络上（例如平台没有 docker.sock，无法自动接网） | 挂上 docker.sock 后点该集的「重新应用」；或按 `INTEGRATION.md` §6 手工把网络写进 `INTEGRATION_EXPORTER_NETWORK` |
| 日志接入报「未从 docker 配置中发现日志位置」 | 目标容器没有日志环境变量，也没有像日志的挂载 | 在 jd 的 compose 里保留 `backend-logs:/app/data/logs`（已有）并重启后端；**平台不会猜路径** |
| 日志页没有事件 | 采集容器没起来 / 令牌不一致 / 级别过滤太严 | `docker logs mwops-*-logs`；`doctor-jd-link.sh` 会检查采集容器 |
| 趋势图是一条直线 | 指标本身波动极小（如内存使用率 1%~3%） | Y 轴已改为按数据自适应；仍不动说明确实没变化 |
| 点某指标报 `Cannot read properties of undefined (reading 'series')` | 修复前 NaN 会破坏 JSON 编码（见 `POSTMORTEM.md` INC-004） | 升级后端 + 前端产物即可；该指标此后显示「暂无采样数据」 |

---

## 5. 回滚

```bash
# 平台侧：删掉集成（同时移除抓取目标与 Exporter/采集容器）
#   「集成中心 → 该行 → 删除」；日志采集容器可用 docker rm -f 删除
cd <nightjar> && docker compose down            # 平台下线（保留数据卷）

# jd 侧：本来就什么都没改，停掉业务即可
cd <jd> && ./start.sh stop
```

> 平台的自动接入只体现在"平台容器多了一张网卡"上；`docker compose down` 后即消失，
> 被管项目的网络、容器配置、compose 文件都不存在需要还原的改动。
>
> jd 的 Prometheus/Grafana 服务与相关资产已从仓库移除（`monitoring/`、`deploy/jd-exporters/`）。
> 需要历史版本请用 git 回退到移除前的提交。

---

## 6. 相关文档

| 文档 | 内容 |
|---|---|
| [`INTEGRATION.md`](INTEGRATION.md) | 集成中心：字段对照、自动化矩阵、日志接入原理、安全边界 |
| [`API.md`](API.md) | 接口清单（集成、日志接入、服务发现、接入自检） |
| [`COLLECTOR.md`](COLLECTOR.md) | 接入契约与标签约定（自建 Prometheus/Exporter 场景） |
| [`OPERATIONS.md`](OPERATIONS.md) | 平台运维（备份、升级、审计校验） |
| [`POSTMORTEM.md`](POSTMORTEM.md) | 交付期真实故障记录（含指标 NaN、白屏、建表冲突） |
