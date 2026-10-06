# 集成中心 · 使用与设计说明

> 对应云厂商 Prometheus 监控服务的「数据采集 → 集成中心」：
> [Redis Exporter 接入](https://cloud.tencent.com/document/product/1416/111839) ·
> [MySQL Exporter 接入](https://cloud.tencent.com/document/product/1416/111841)
>
> 差别只有一处：云上由厂商托管 Exporter 容器与抓取配置，本平台把这两件事
> **在本机 docker 环境内自己完成**——页面点选、填参数、保存即接入。
>
> 本文档同时覆盖：端到端接入教程（第四章）与「自备 Prometheus / 自建 Exporter」
> 接入专题（第十二章）。日志链路的权威说明见 [LOG_INTEGRATION.md](LOG_INTEGRATION.md)。

---

## 1. 它做了什么

一次「集成」= 四件事自动完成：

```
页面选组件 → 填地址/账号/标签/参数
      │
      ├─① Exporter 暴露   ：渲染官方 Exporter 的 env/开关；启用后可一键拉起容器
      ├─② Prometheus 抓取 ：暴露服务发现目标（instance_name=集成名称），无需重启
      ├─③ 实例纳管        ：自动创建纳管实例（prom_job=抓取任务名），监控/AI 诊断立即可用
      └─④ 告警规则        ：按组件模板自动创建推荐阈值规则
```

关键设计：**抓取目标走 Prometheus 的 `http_sd_configs`**（HTTP 服务发现）。
平台把全部集成渲染成一份 JSON（`targets` + `labels.instance_name`）并通过
`GET /api/sd/integrations` 暴露，Prometheus 每 30s 拉一次，所以新增/修改/删除集成
都不需要 reload 或重启 Prometheus——这就是云控制台「保存即可」体验的本地等价实现。

> 为什么不用 file_sd 共享卷：后端容器以非 root 用户运行，而命名卷的属主取决于
> 「哪个容器先初始化该卷」（backend 与 prometheus 谁先起不确定），顺序不可控时
> file_sd 会静默写不进去，表现为「集成保存成功但永远没有指标」。
> HTTP 通道没有属主概念，跨主机部署也能用。file_sd 文件仍会生成，作为副产物供人工核对。

```
集成中心（前端页面）
      │  POST /api/integrations
      ▼
IntegrationService ──渲染──▶ integration 包（纯函数，可单测）
      │                        ├─ 服务发现 JSON（→ GET /api/sd/integrations）
      │                        ├─ 显式 scrape job 片段
      │                        ├─ Exporter compose 片段
      │                        └─ docker run 命令 / 核对步骤
      ├─写入─▶ /app/data/integrations/integrations.json（副产物，落在 backend-data 卷内）
      ├─可选─▶ Docker Engine API：创建并启动 Exporter 容器
      ├─落库─▶ middleware_instances（Config.integration 保存集成元信息）
      └─可选─▶ alert_rules（模板推荐规则）

mwops-prometheus ──http_sd(30s)──▶ GET http://backend:8080/api/sd/integrations
```

---

## 2. 字段对照表

| 云控制台字段 | 本平台字段 | 落地位置 | 注意 |
|---|---|---|---|
| 名称 | `name` | 纳管实例名 + Prometheus `instance_name` 标签 + 容器名 | 唯一；正则 `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`，与云控制台一致 |
| 地址 | `address` | 实例 `host:port`（平台侧 TCP 探测）+ **Exporter 侧连接地址** | 端口可省略（取模板默认值）；URL 型组件支持 `http://host:port/path`。**远程部署时这个地址是「目标机视角」**——见下方说明 |
| 用户名 | `username` | Exporter 的 `REDIS_USER` / DSN / `--es.username` | MySQL/PG 需预建只读账号并授权 |
| 密码 | `password` | Exporter 的 `REDIS_PASSWORD` / DSN 口令 | AES-256-GCM 加密存储；生成的配置里只出现 `${MONITOR_PASSWORD}` 占位 |
| 标签 | `labels` | 服务发现的 labels（自定义指标标签） | 不能覆盖 `job`/`instance`/`instance_name`/`mw_type` |
| 环境变量（Redis）| `options`（`target=env`）| Exporter 容器环境变量 | 白名单：仅模板声明的键可透传 |
| Exporter 配置（MySQL）| `options`（`target=arg`）| Exporter 命令行开关 `--collect.*` | 上游默认开启的项被关闭时输出 `--no-xxx`（否则"关了没关掉"） |
| 查看监控 | 实例详情 / 统一监控 | 平台监控页 + 组件对应 Grafana 大盘 ID | 卡片上给出大盘编号 |
| 配置告警 | 自动创建推荐规则 | `alert_rules` | 保存时勾选「自动创建推荐告警规则」 |

### 2.1 「地址」的两种视角（远程部署必读）

同一个「地址」字段对两个角色含义不同：

| 角色 | 它需要什么 | 谁来用 |
|---|---|---|
| **平台** | 平台自己能连到它（TCP 探测） | 健康巡检、纳管列表状态 |
| **Exporter** | **Exporter 所在机器**能连到它 | 抓取指标（`redis_up` / `mysql_up` …） |

远程部署时 Exporter 跑在**目标机**上。如果被管实例也在这台机器上，那么：

- ❌ 填**公网 IP**（如 `203.195.191.75:6379`）：Exporter 打自己的公网 IP 要走 hairpin NAT
  并穿过安全组，云上常被拦；日志会写
  `Couldn't connect to redis instance (redis://203.195.191.75:6379)`，指标 `redis_up 0`；
- ✅ 填 **`127.0.0.1:6379`**：回环永远可用，也不经公网。

平台从 r5 起会自动处理这种情况：**当「地址」里的主机就是目标机本身时**，渲染给 Exporter 的
地址自动改成 `127.0.0.1`（部署说明里会写明这次改写），平台侧记录与你填的值保持不变。
另外，若你把地址填成回环（远程集成），平台**不再对它做 TCP 探测**——那会打到平台自己，
健康一律以 Exporter 指标为准（`redis_up` / `mysql_up` / `pg_up`）。

> 被管实例在**别的机器**上时，填那台机器的 IP/域名即可（Exporter 能直连，不涉及上面这些问题）。

---

## 3. 快速开始（单机）

```bash
cd <nightjar 项目目录>
cp .env.example .env      # 至少设置 JWT_SECRET / ADMIN_PASSWORD / HOOK_TOKEN
docker compose up -d --build
```

1. 浏览器打开 `http://<主机>:8000`，登录后进入左侧 **资源 → 集成中心**；
2. 点组件卡片（如 Redis）→ **集成**，填写：

   | 字段 | 示例 |
   |---|---|
   | 集成名称 | `legacy-redis` |
   | 连接地址 | `legacy-redis:6379`（容器名或 `10.0.0.11:6379`） |
   | 用户名 / 密码 | 留空 / `$REDIS_PASSWORD`（只做 TCP 探测与 Exporter 认证） |
   | 自定义标签 | `team=interview` |
   | Exporter 参数 | 云数据库 Redis 集群架构请打开「跳过 SLOWLOG / LATENCY HISTOGRAM 指标」 |
   | 一键拉起 Exporter | 已开启 Docker 时勾选 |
   | 自动创建推荐告警规则 | 勾选 |

3. 点 **保存并集成**（想先看产物可点 **预览生成的配置**）；
4. 30 秒内（服务发现的 `refresh_interval`）指标出现在 **统一监控**；
   实例详情页的 **接入自检** 可确认 `matched > 0` 且来源为 Prometheus。

### 3.1 让「一键拉起容器」可用（可选）

```yaml
# docker-compose.yml · backend
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock   # 取消注释
```

```bash
# .env
INTEGRATION_DOCKER_ENABLED=true
```

> ⚠️ 挂载 docker.sock 等于把宿主机 root 权限交给平台容器。
> 生产环境建议保持关闭，改用「渲染配置 + 人工执行」：
> 集成中心里点 **配置** 抽屉，复制 compose 片段或 `docker run` 命令执行即可。

---

## 4. 端到端接入教程（平台托管监控 · 被管项目零配置）

> 适用形态：**监控栈统一在 nightjar**。被管项目只跑业务，不装 Exporter、不装 Agent、
> 不跑 Prometheus/Grafana；平台负责建只读账号、拉起 Exporter、自动接入对方网络、抓取、出大盘，
> 日志则由平台用 Ansible 在目标机部署 Filebeat 推到平台自带 Kafka（见 [LOG_INTEGRATION.md](LOG_INTEGRATION.md)）。

### 4.1 职责边界

```
┌──────── nightjar（唯一监控栈）──────────────────────────┐
│ mwops-prometheus   抓取全部 Exporter                    │
│ mwops-grafana      大盘（数据源已自动配好）              │
│ mwops-backend      纳管/查询/告警/AI 诊断/日志接收        │
│ mwops-exporter-*   按「集成」创建，自动接入两张网         │
│ mwops-kafka        日志总线：Filebeat 推送日志            │
└──────────┬─────────────────────────────────────────────┘
           │ 平台用 Docker API 自己发现目标容器所在网络并接入（被管项目零改动）
┌──────────┴─ 被管项目（零监控配置）───────────────────────────┐
│ mysql / redis / backend / frontend                     │
│ 只需要提供：容器名（app-mysql / app-redis）             │
└────────────────────────────────────────────────────────┘
```

| 事项 | 谁负责 | 被管项目需要做什么 |
|---|---|---|
| 只读监控账号 | 平台（勾选后自动建号，prod 走审批） | 集成时填一次管理凭据 |
| Exporter | 平台（Docker API 创建） | 无 |
| 网络接入 | 平台（`ResolveTarget` 反查容器所在网络并自动接入） | **无**（不建网络、不加别名、不改 compose） |
| 指标抓取 | 平台自带 Prometheus | 无 |
| 大盘 | 平台自带 Grafana | 无（可选：按编号导入官方大盘） |
| 日志集成 | 平台用 Ansible 在目标机部署 Filebeat → 平台 Kafka（`mwops-kafka`） | 目标机能被平台 SSH 到、且能访问平台 Kafka 的对外地址 |
| 告警规则 | 平台按模板自动创建 | 无 |

### 4.2 被管项目侧：什么都不用做

被管项目侧只有一个日常动作：把业务起起来。旧版本要求被管项目自带
Prometheus/Grafana/Exporter 与各种监控环境变量，**现在全部不再需要**。

### 4.3 平台侧：一条命令 + 三次点击

```bash
cd <nightjar>
./scripts/onboard.sh --dry-run    # ① 预览：打印将要改的 .env 与启动命令
./scripts/onboard.sh              # ② 执行：写 .env → 起平台 → 自检 → 打印下一步
```

脚本只动平台自己：生成缺失密钥（十六进制，免转义）、打开 `INTEGRATION_DOCKER_ENABLED`、
**自动放开 `docker-compose.yml` 里 docker.sock 的注释**（自动发现网络的前提）、
写 `MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090`，
然后用普通的 `docker compose up -d --build` 启动平台——**不再需要任何 overlay 或互联网络**。

然后在浏览器完成三次集成（**被管项目无需再改任何东西**）：

| 步骤 | 集成中心操作 |
|---|---|
| ③ 集成 MySQL | 名称 `legacy-mysql`、地址 `app-mysql:3306`；**只读账号自动创建**（默认 `mwops_exporter`、口令平台生成），只需填一次 root 管理凭据 |
| ④ 集成 Redis | 名称 `legacy-redis`、地址 `app-redis:6379`、口令填被管项目的 Redis 密码（Redis 不需要建号） |
| ⑤ 日志集成 | 集成中心 → **日志 / Filebeat** 选 `log` 类型 → 集成名 `order-app-log`、目标机、日志路径 `/app/data/logs/*.log`、服务名 `app-review-api`、级别 `ERROR` → 保存后点**自检**，三段环节（平台 → Kafka / 被管机接入地址 / 日志是否真的进来了）全绿 |

日志接入之后还有两步配置（第 ⑥ 步不配也能跑通；第 ⑦ 步要 AI 代码结论才需要）：

| 步骤 | 操作 | 为什么 |
|---|---|---|
| ⑥ **配日志告警规则（可选）** | 日志告警 → **日志告警规则** → 新建：`service_name` 填 `app-review-api`（留空 = 任意服务）、`signature_pattern` 填**日志消息里的一段原文**（如 `NullPointerException`；留空 = 任意消息，写 `/正则/` 则按正则匹配）、`min_severity` 选 `ERROR`，再定 **去重窗口**（默认 5 分钟）、**冷却期**（默认 10 分钟）、**通知渠道**（留空 = 平台已启用的渠道）与 **AI 分析**开关 | 决定“多久打扰人一次”以及要不要自动出代码结论。匹配按「priority 数字小者优先、同优先级按 id 升序取第一条命中的**启用**规则」；**一条都没命中**时**不产生告警**（不入库、不通知、不分析）——平台没有默认规则兜底。建完规则记得确认它是**启用**状态 |
| ⑦ **配置外部 AI 分析服务（要 AI 代码结论才需要）** | 系统设置 → **AI 设置 → AI 代码分析**：启用并填写服务地址、API Key、对接协议（开放接口 v1 / 通用协议）、回调地址与回调令牌；再把服务名加入出网白名单 `MWOPS_SECURITY_OUTBOUND_WHITELIST`（默认空 = 禁止任何外发） | AI 代码分析由**外部 AI 分析服务**完成：平台把脱敏后的错误信息提交出去（事件置 `awaiting`），服务端分析完成后回调或由平台轮询取回三点式结论。**平台不 clone、不缓存任何业务代码**，也不需要配置“服务→仓库映射”。对接协议与验收清单见 [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md) 附录 B |

账号相关补充：

- **不需要提前建号**：MySQL/PostgreSQL 默认由平台代建（`CREATE USER IF NOT EXISTS` + `ALTER USER` + 最小授权），
  口令 24 字节十六进制、加密存储；未填管理凭据时集成照常保存，只在备注里提示“填凭据后点重新应用”；
- **账号管理**：集成中心右上角「监控账号」可查看来源/权限/最近轮换，
  支持**轮换口令**（账号改自己的口令，不需要管理员凭据）与**删除账号**（破坏性，需管理员凭据；prod 转审批工单）；
- 生产环境（`env=prod`）建号/删号只创建审批工单，不直接执行。

保存集成的瞬间，平台会自己完成网络接入：反查 `app-mysql` / `app-redis`
命中的容器 → 取得真实网络名 `app_data` → 把 Exporter 接成
「`middleware-ops_mwops`（Prometheus 抓它）+ `app_data`（它连数据库）」两张网，
并把平台自身也接进 `app_data` 以便做 TCP 健康探测。这些都发生在平台侧，
被管项目的网络、别名、compose 文件一个字节都不动（集成卡片会写明“已发现目标容器…所在网络…”）。

> 地址填**容器名**最稳（`app-mysql`）；填 compose 服务名（`mysql`）平台也会换算成容器名。
> 填一个 docker 里不存在的名字时，报错会列出候选容器名。

集成保存后平台会自动核验：抓取目标 `up` 就标记「已应用」；失败则把 Prometheus 的
`lastError` 翻译后写回列表（不必再去 `/targets` 页面翻）。

### 4.4 验收

```bash
cd <nightjar>
./scripts/doctor.sh     # 平台容器 / docker.sock / 目标容器网络 / Exporter 网络 / 抓取状态
```

页面验收：**统一监控**能看到 `legacy-redis` / `legacy-mysql` 的曲线（趋势图 Y 轴按数据自适应）；
**日志告警 → 事件**能看到 `app-review-api` 的 ERROR 事件；
**Grafana**（`http://<主机>:3000`）数据源已就绪，大盘按集成卡片给的编号导入。

### 4.5 回滚

```bash
# 平台侧：删掉集成（同时移除抓取目标与 Exporter 容器）
#   「集成中心 → 该行 → 删除」
# 日志集成：删除集成**不会**卸载目标机上的 Filebeat，需要就手工收尾——
#   systemctl disable --now filebeat && rm -f /etc/filebeat/filebeat.yml   （package 模式）
#   docker rm -f mwops-filebeat                                            （docker 模式）
cd <nightjar> && docker compose down            # 平台下线（保留数据卷）

# 被管项目侧：本来就什么都没改，停掉业务即可
```

> 平台的自动接入只体现在“平台容器多了一张网卡”上；`docker compose down` 后即消失，
> 被管项目的网络、容器配置、compose 文件都不存在需要还原的改动。

---

## 5. 三种落地方式（按推荐度）

| 方式 | 适用 | 操作 |
|---|---|---|
| **http_sd（推荐，平台内置）** | 平台自带 Prometheus（`deploy/prometheus/prometheus.yml` 已内置 `middleware-integration` job） | 无：保存后 30 秒内自动生效 |
| **http_sd（外部 Prometheus）** | 复用被管项目自带的 app-prometheus 等外部 Prometheus | 在其 `scrape_configs` 加一个 job，`http_sd_configs.url: http://mwops-backend:8080/api/sd/integrations`（该 Prometheus 需能访问 `mwops-backend`，例如与平台同网络） |
| **显式 scrape job** | 外部 Prometheus 无法访问平台接口（例如跨主机且未放通 8000） | 抽屉 →「Prometheus(显式 job)」→ 复制片段并入 `scrape_configs` → reload/重启 |
| **人工 compose** | 平台未启用 Docker 一键部署 | 抽屉 →「Exporter(compose)」→ 合并进 compose → `docker compose up -d` |

服务发现接口的鉴权：默认不鉴权（只暴露地址与标签，不含口令）；
设置 `INTEGRATION_SD_TOKEN` 后，Prometheus 侧 URL 要带上令牌：

```yaml
    http_sd_configs:
      - url: http://backend:8080/api/sd/integrations?token=<令牌>
        refresh_interval: 30s
```

三种方式对应的 `instance_name` 标签都由平台写入，因此平台侧查询选择器始终是：

```
job="middleware-integration",instance_name="<集成名称>"
```

---

## 6. 组件模板矩阵

| 组件 | Exporter 镜像 | 端口 | 参数形态 | 推荐告警 | Grafana 大盘 |
|---|---|---|---|---|---|
| Redis | `oliver006/redis_exporter:v1.66.0` | 9121 | 环境变量 `REDIS_EXPORTER_*` | 服务不可用、内存率 > 85%、命中率 < 90%、淘汰 > 0、主从链路断开 | 763 |
| MySQL | `prom/mysqld-exporter:v0.15.1` | 9104 | 命令行 `--collect.*` + `DATA_SOURCE_NAME` | 服务不可用、连接数 > 400、连接使用率 > 85%、慢查询 > 5 | 7362 |
| PostgreSQL | `prometheuscommunity/postgres-exporter:v0.16.0` | 9187 | `DATA_SOURCE_NAME` + `--auto-discover-databases` 等 | 连接数 > 150、主从延迟 > 30s | 9628 |
| Kafka | `danielqsj/kafka-exporter:v1.7.0` | 9308 | 命令行 `--kafka.server` 等 | 消费 Lag > 1 万、ISR 不足 > 0 | 7589 |
| Elasticsearch | `prometheuscommunity/elasticsearch-exporter:v1.7.0` | 9114 | 命令行 `--es.uri` / `--es.username` | 非 green、堆 > 85% | 2322 |
| Nginx | `nginx/nginx-prometheus-exporter:1.3.0` | 9113 | 命令行 `--nginx.scrape-uri` | 5xx > 2% | 9614 |

### 6.1 统一监控页可选指标（按画像顺序，**第一条是切换实例后的默认指标**）

| 组件 | 指标（下拉顺序，共 N 项） |
|---|---|
| Redis（18） | **服务可用 `redis_up`** · 已用内存 · 客户端连接使用率 · 过期 key 速率 · 入向流量 · 出向流量 · 拒绝连接速率 · 主从链路 · RDB 持久化状态 · 运行时长 · 内存使用率 · 连接数 · QPS · 命中率 · key 数量 · 慢查询数 · 淘汰 key 数 · 阻塞客户端 |
| MySQL（16） | **服务可用 `mysql_up`** · 运行中线程 · 连接使用率 · 缓冲池使用率 · 连接失败速率 · 行锁等待速率 · 磁盘临时表速率 · 复制 IO 线程 · 复制 SQL 线程 · 运行时长 · QPS · TPS · 连接数 · 慢查询数 · 缓冲池命中率 · 主从延迟 |
| PostgreSQL（7） | QPS · TPS · 连接数 · 慢查询数 · 缓存命中率 · 锁等待 · 主从延迟（`pg_replication_lag`） |
| 主机 Node（8） | 主机可达 · CPU · 内存 · 磁盘 · 负载 · inode · 入/出向流量 |
| Kafka（6） / ES（7） / Nginx（4） | 见 `internal/monitor/profile.go`（顺序即下拉顺序） |

> 指标名就是 PromQL 里那个指标（如 `redis_up`），因此怀疑数据不对时可以直接拿去 Prometheus 查。
> 「服务可用」这类 0/1 指标只有**该时序存在**时才有值；非从库实例不会有主从相关指标，
> 界面对缺失项显示「无数据」而不是 0，也不会误报。
>
> 补齐规则（改动指标时必须遵守）：**模板里推荐告警引用的指标必须存在于画像中**，
> 由 `TestTemplateAlertMetricsExistInProfiles` 守卫——它已经在补齐过程中抓出过
> 「PostgreSQL 主从延迟」这条永不触发的推荐规则。

各组件的踩坑点（会显示在集成弹窗里）：

- **Redis**：`REDIS_ADDR` 用 `<host>:<port>`（不带 scheme），账号口令分别用 `REDIS_USER`/`REDIS_PASSWORD`；
  云数据库集群架构必须打开两个 EXCLUDE 开关（不支持 `SLOWLOG`/`LATENCY HISTOGRAM`）；
  `redis_exporter` 没有 `SERVICE_NAME` 之类的实例名开关，`instance_name` 由平台写入。
- **MySQL**：先建只读账号 `GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';`；
  低于 5.6 部分指标采集不到属预期；口令含 `@ ( ) /` 建议改用 `--config.my-cnf`。
- **PostgreSQL**：`GRANT pg_monitor TO exporter;`。
- **Nginx**：被管 Nginx 必须开 `stub_status` 并对 Exporter 放通。

---

## 7. 与被管项目（跨主机 / 跨 compose）的组合

被管项目的 MySQL / Redis 在 `app-data`（internal）网络里，不发布宿主端口，也不为监控做任何改动。
平台创建 Exporter 时会**自己发现**目标容器所在的网络（`app_data`），然后把 Exporter
接成两张网：「平台网络（Prometheus 抓它）」+「目标网络（它连被管实例）」。
因此 被管项目侧不需要建互联网络、不需要加别名，`deploy/旧的跨栈 overlay` 已经删除。

```bash
# .env —— 只需平台网络；目标网络由集成时自动发现，不必手写
INTEGRATION_EXPORTER_NETWORK=middleware-ops_mwops
```

发现过程（`internal/docker.ResolveTarget`）：

1. 列容器 → 逐个 inspect，按 **容器名 → compose 服务名 → 网络别名** 顺序匹配用户填的地址；
2. 取命中容器的真实网络名（如 `app_data`），与 `INTEGRATION_EXPORTER_NETWORK` 求并集；
3. 把用户填的地址**换成容器名**（`app-redis`）——容器名在目标网络上一定能被内嵌 DNS 解析，
   而别名可能只存在于用户以为的那张网上；
4. 容器第一个网络在创建时指定，其余通过 `POST /networks/{id}/connect` 追加；
5. 同时把平台自身（`MWOPS_SELF_CONTAINER`，默认 `mwops-backend`）也接进目标网络，
   这样纳管实例的 TCP 健康探测不再需要任何人调整宿主机上的 compose。

> 平台看不到 docker（未挂 `docker.sock`）时退化为"只接配置里列出的网络"，
> 此时集成地址必须填**平台能解析**的名字，并且 Exporter 的抓取目标仍是平台容器
> （`mwops-exporter-<集成名>:<端口>`），不会去抓 MySQL 的 3306。

---

### 7.1 反向接网（可选项，默认关闭）

上面是默认方向：**平台动自己**。有些环境反过来更合适——例如目标容器在网络命名空间上受限，
或运维要求"所有被管容器都挂在平台网络上"。此时可在集成表单勾选
**「改为把目标容器接入平台网络」**（`join_platform_network=true`），平台会执行等价的：

```bash
docker network connect <平台网络> <目标容器>      # 例如 middleware-ops_mwops app-mysql
```

代价与注意事项：

- 这**修改了被管容器的网络配置**（默认方向不改被管容器）；
- 若目标容器原本只在 `internal` 网络里（被管项目的 `app-data` 就是刻意做成无出网的），
  接入非 internal 的平台网络后它会**多一条出网路径**，数据面隔离随之失效；
- 地址填的是外部地址（非容器名）时不需要、也不应该勾选——平台连容器都找不到；
- 勾选状态持久化在集成元信息里，「重新应用」会复现同样行为（取消勾选后保存即停止）。

---

### 7.2 监控账号：默认由平台代建 + 账号管理

**默认策略：需要账号的组件（MySQL / PostgreSQL）由平台自动建号**，使用者不必提前建号、
也不必自己想账号名与口令：

- 账号名留空时用模板默认值（`templates[].monitor_user`，当前为 `mwops_exporter`）；
- 口令由平台生成 24 字节十六进制随机串（无 `@ : / ?` 等字符，SQL/DSN/环境变量三层都不需要转义），
  AES-256-GCM 加密后随实例存储；
- 权限最小化：MySQL `PROCESS, REPLICATION CLIENT, SELECT` + `MAX_USER_CONNECTIONS 3`；
  PostgreSQL `pg_monitor`；语句幂等（`CREATE USER IF NOT EXISTS` + `ALTER USER`）；
- **需要一次管理员凭据**（建号是写被管库的操作）——未填写时**不会让集成保存失败**，
  只把「已跳过自动建号，填凭据后点重新应用」写进集成备注；
- 生产环境（`env=prod`）只创建审批工单（`integration_bootstrap`），不直接执行。

**账号管理（集成中心 → 监控账号）**：

表格里给的是**现状**；点任一操作按钮会先弹出一个「账号操作」窗口，
在里面填齐凭据后**由该窗口的主按钮触发请求**（口令/私钥、管理员账号口令都在同一处，
仅本次使用、不落库）。

| 能力 | 实现 | 是否需要管理员凭据 |
|---|---|---|
| 查看账号现状 | `GET /api/integrations/accounts`：账号名、来源（平台创建/外部账号/不需要）、权限摘要、最近轮换时间、集成当前错误 | 否 |
| **失败重试** | `POST /api/integrations/:id/account/retry`：带凭据 → 幂等重跑建号 SQL；不带 → 只测连接；两种情况都会**带上本次的 SSH 凭据**重建 Exporter 并核验。返回 `created` / `connected` / `message` | 可选 |
| 连接测试 | `POST /api/integrations/:id/account/probe`：用监控账号执行 `SELECT 1`（MySQL 另附 `SHOW GRANTS`），只读、不改配置 | 否 |
| 轮换口令 | `POST /api/integrations/:id/account/rotate`：账号**改自己的**口令（MySQL `ALTER USER USER()` / PG `ALTER ROLE CURRENT_USER`），随后自动重建 Exporter | **不需要**（平台持有该账号口令） |
| 删除账号 | `POST /api/integrations/:id/account/drop`：`DROP USER IF EXISTS` / `DROP ROLE IF EXISTS` | 需要；prod 转审批工单 |

> **「不需要账号」不是「未托管」**：Redis / Kafka / Nginx / Elasticsearch 等组件的模板
> `monitor_user` 为空——平台本来就不代管它们的账号，界面上显示「不需要」才是正常状态；
> 只有 MySQL / PostgreSQL 才有「平台创建 / 外部账号」之分。
>
> **远程部署的 Exporter 也不会显示「未托管」**：容器在目标机上、平台没有那台机器的 docker 通道，
> 因此界面直接标成「远程（目标机）」，运行状态看抓取指标（`redis_up` / `mysql_up`）或到目标机
> `docker ps`；只有「本机」部署才显示容器状态。

**失败可重试的设计要点**（为什么不需要重填整个集成表单）：

- 建号语句天然幂等：`CREATE USER IF NOT EXISTS` + `ALTER USER`（口令重置）+ 补 `GRANT`，
  因此"重试"永远是安全操作，不会产生半成品状态；
- 重试用的是**库里已存的那个口令**（加密存储），所以重建出的账号口令与 Exporter 注入的口令
  天然一致，不会出现"账号建好了但 Exporter 还在用旧口令"；
- 集成列表里的「待处理」提示旁**只给一个**推荐入口：按钮由后端的 `next_action` 决定
  （`reapply` = 重新应用；`retry_account` = 去重试建号），不再一律甩到账号弹窗；
- 「重新应用」只重建 Exporter（没有管理凭据、建不了号）。**远程集成会在弹窗里当场收一次
  SSH 凭据**（口令或私钥二选一，仅本次使用、不落库）——否则这个按钮在远程部署上必然失败，
  使用者只能绕到账号弹窗去重试，两个入口互相推诿；
- 保存/重新应用都是**异步**的：发起时会立刻清掉上一次的失败原因（只剩"⏳ 已开始…"），
  新的失败在后台任务结束时才写入。所以"刚填完凭据保存却先弹旧错误"这种情况不会再出现。

### 三个入口的分工（不是重复）

| | 「重新核验」 | 「重新应用」 | 「重试建号 / 连接」 |
|---|---|---|---|
| 做什么 | 只按 Prometheus 现状刷新状态 | 重写抓取配置 + 重装/重建 Exporter | **建号/重置口令** + 连接测试 + 重建 Exporter |
| 凭据 | **不需要** | 远程：SSH（口令或私钥） | 建号：管理员账号口令；远程另需 SSH |
| 副作用 | **无** | 会重建 Exporter | 会写被管库（仅幂等建号/授权语句） |
| 适用 | 外部原因已修好、状态没跟上（核验只在部署后跑几次，之后不会自己再核对） | 部署面问题：装了起不来、端口不通、容器被删、改了地址/端口 | 账号面问题：还没有账号、口令不一致（NOAUTH/WRONGPASS）、权限不足 |

**「待处理」消失了怎么办**：先点**重新核验**（零代价）。它不通再按原因选：
部署类 → 重新应用；账号类 → 重试建号。平台从 r6 起还会**周期自愈**——
每分钟对处于「待处理」的集成重新核对一次，明确看到抓取目标 `up` 就自动清除
（读不到 Prometheus 时保持原状，绝不凭空造错）。

> 轮换为什么不需要管理员凭据：SQL 标准与两个数据库都允许账号修改自己的口令，
> 因此"平台托管的账号"可以自助轮换，避免了每次轮换都要向用户再要一次 root 口令。

---

## 8. 权限、安全与审计

- **权限**：与「中间件纳管」共用 `middleware:read`（查看/预览）与 `middleware:write`（增删改/应用）——
  集成产物本身就是一个纳管实例，不额外引入权限点。
- **口令**：AES-256-GCM 加密存储（复用平台主密钥）；**生成的任何配置里都不出现明文**，
  统一替换为 `${MONITOR_PASSWORD}` 占位；一键部署时才解密并按容器环境变量注入。
- **参数白名单**：只允许模板声明的 Exporter 参数，拒绝透传任意 env/flag；
  镜像与端口来自内置模板，不接受用户自定义。
- **审计**：`integration_create` / `integration_update` / `integration_apply` / `integration_delete` /
  `integration_account_rotate` / `integration_account_retry` / `integration_account_drop`
  全部落审计（含地址、标签、是否部署、账号名，**不含口令**）。
- **操作级别**：L1（低危，直接执行并留痕）。生产环境若要更严的管控，
  可把 `deploy` 关掉，只允许平台渲染配置、由人工执行。

### 8.1 docker.sock：权限与收敛（部署必读）

平台容器以**非 root 用户**（镜像里的 `mwops`）运行，而宿主 socket 通常是 `root:docker 0660`，
因此只把 socket 挂进容器还不够，会报：

```
dial unix /var/run/docker.sock: connect: permission denied
```

三种处理方式，按推荐度：

| 方式 | 做法 | 代价 |
|---|---|---|
| **① 附加 docker 组 GID（默认）** | `stat -c '%g' /var/run/docker.sock` 取 GID → 平台 `.env` 写 `DOCKER_GID=<GID>`（`docker-compose.yml` 已配 `group_add`）→ `up -d --force-recreate backend` | 保持非 root；GID 随宿主不同需正确填写（`scripts/onboard.sh` 会自动探测写入） |
| ② 容器内以 root 运行 | 给 backend 加 `user: "0:0"` | 容器内进程获得 root；鉴于 socket 本身已等价宿主机 root，属"放弃纵深防御" |
| ③ docker-socket-proxy | 用 `tecnativa/docker-socket-proxy`，只放行 `containers`/`networks`/`volumes` 的 GET/POST 与 `exec`，平台连代理而非真实 socket | 多一个容器；最符合最小权限，生产强烈建议 |

> 平台在**启动时**就会探活 socket（`/_ping`）：一旦权限不对，集成中心页面顶部的
> `docker_note` 会直接显示上面这份修复步骤，`docker_ok` 为 false 时相关按钮会禁用，
> 不会等到你点保存才报一句 permission denied。

---

## 9. 排查

| 现象 | 排查 |
|---|---|
| 集成保存成功但监控页没有数据 | 实例详情 → **接入自检**：`job_up=null` 说明没有该 job（检查 Prometheus 配置里是否有 `middleware-integration`）；`job_up=1` 且 `matched=0` 说明标签对不上（检查集成名称是否被改过） |
| 服务发现不生效 | 后端容器内 `curl -s http://127.0.0.1:8080/api/sd/integrations`；Prometheus 容器内 `wget -qO- http://backend:8080/api/sd/integrations`；Prometheus 的 /targets 页看 `middleware-integration` 下的 target（`refresh_interval` 默认 30s） |
| Exporter 容器起来了但 up=0 | Exporter 连不上被管实例：核对地址/账号口令、网络是否两张都挂上（`docker inspect <容器> \| grep -A5 Networks`） |
| **Redis：指标全无、`redis_up=0`，Exporter 认证报 WRONGPASS** | 集成里填了「用户名」，而被管 Redis 只配了 `requirepass`（default 用户，没有该 ACL 用户）。编辑集成，把**用户名清空**、只填口令，保存后平台自动重建 Exporter |
| **Redis：Exporter 日志写 `Couldn't connect to redis instance (redis://<公网IP>:6379)`、`redis_up 0`** | Exporter 打的是自己的**公网 IP**：被管实例与 Exporter 同机时应填 `127.0.0.1`（公网 IP 要走 hairpin + 安全组，云上常被拦）。平台 r5 起会在「地址主机 == 目标机」时自动改用回环并写进部署说明；也可直接检查容器拿到的变量：`docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' <容器>` 里 `REDIS_ADDR` 应是 `127.0.0.1:6379` |
| **Redis：`redis_up 0` 且 `last_scrape_error` 形如 `dial redis: unknown network redis`** | **这是马甲，不是根因**：redis_exporter 先用 `redis://` 连接，失败后按 `://` 拆开重试（`Dial("redis", "host:port")`），于是把 scheme 当成了网络类型，**真正的失败原因被吞掉**。真实原因通常是：① 地址/端口不通（端口只绑在 `127.0.0.1` 却填了公网 IP）；② 口令不一致，或目标没设口令却传了口令（`ERR Client sent AUTH, but no password is set`）；③ 多填了 ACL 用户名。拿真实原因：给容器加 `REDIS_EXPORTER_DEBUG=true` 看日志里的 `DialURL() failed, err:`，或在被管机上 `redis-cli -h <地址> -p <端口> [-a <口令>] ping`。平台从 r4 起按不带 scheme 的 `host:port` 注入，兜底路径会走回 tcp 分支，真实错误不再被吞 |
| **容器在跑，但 `docker ps` 的 PORTS 列是空的** | **正常现象，不是故障**：只有做端口映射（`-p`/NAT）的容器才在这一列显示端口。远程安装与主机监控都用 `--network host`，容器直接使用宿主网络命名空间、Exporter 绑的就是宿主端口，因此没有映射可显示。按下面三条确认它真的在听：<br>`docker inspect -f '{{.HostConfig.NetworkMode}}' <容器>` → `host`<br>`ss -ltnp \| grep <Exporter端口>` → 看到 `redis_exporter`/`mysqld_exporter` 在听<br>`curl -s http://127.0.0.1:<Exporter端口>/metrics \| grep -E '^redis_up\|^mysql_up'` → `1` 才算真的通 |
| 远程安装 Ansible 成功但平台报"探测失败" | 平台从**平台侧**探目标机的 Exporter 端口：云主机要在安全组/防火墙对平台出口 IP 放通该端口（9121/9104/9187/9100） |
| 一键部署报错 `dial ... docker.sock: no such file or directory` | 平台容器里没有 docker.sock：compose 挂载还是注释状态，或容器早于该改动启动。重跑 `./scripts/onboard.sh`（会自动放开注释）后 `docker compose up -d --force-recreate backend`；rootless Docker 把 `INTEGRATION_DOCKER_HOST` 指向 `/run/user/<uid>/docker.sock` 并挂载该路径 |
| 一键部署报错（permission denied） | socket 已挂载但 GID 不对，见 8.1；`INTEGRATION_DOCKER_ENABLED` 与 socket 挂载是否都就绪 |
| 集成报「无法确定目标所在网络」 | 地址里的名字与 docker 里的容器名/服务名/别名都不匹配，或平台没挂 docker.sock；报错里会列出候选容器名，`./scripts/onboard.sh` 会自动放开 docker.sock |
| MySQL 某个采集项"关了没关掉" | 开关是否渲染成 `--no-collect.xxx`（抽屉里看 compose 片段） |
| MySQL 报 `Access denied for user 'exporter'` / `invalid DSN` | 账号不存在或口令不一致：用「监控账号 → 重试建号/测试连接」；`invalid DSN` 是旧版拼 `DATA_SOURCE_NAME` 的残留，重建 Exporter 即可（现已改为官方 flag `--mysqld.username` + `MYSQLD_EXPORTER_PASSWORD`） |
| 想彻底重来 | 列表行 →「重新应用」（重写服务发现 + 重建容器），或删除后重新集成 |
| 后端启动报 `password authentication failed for user "mwo" (SQLSTATE 28P01)` | Postgres 只在数据卷为空时应用 `POSTGRES_PASSWORD`；卷早就初始化过、`.env` 的 `DB_PASSWORD` 又被轮换。重跑 `./scripts/onboard.sh` 对齐库内口令，详见 [OPERATIONS.md](OPERATIONS.md) 5.8 |
| **日志集成：Filebeat 连上又断开**，报 `dial tcp 127.0.0.1:9092: connect: connection refused` | `.env` 的 `KAFKA_ADVERTISED_HOST` 填成了回环/容器名：Filebeat 握手成功后被 broker 元数据引导去连它自己那台机器。改成**被管机能访问到的平台宿主机 IP 或域名**，重建 kafka 容器；目标机 `nc -vz <host> 9092` + `filebeat test output` 复验 |
| **日志集成：Filebeat 显示已发送，但平台一条日志都没有** | 目标机时钟偏移过大，消息被 Kafka 以 `InvalidTimestampException` 丢弃。目标机 `timedatectl` / `chronyc tracking` 确认 NTP 已同步，偏差大的先修 NTP 再重启 Filebeat |
| **日志集成自检第 1 段红（平台 → Kafka）** | `kafka` 容器没起来，或 `KAFKA_BROKERS` 被改错（平台侧填容器网络的 `kafka:29092`，不是宿主端口）。`docker exec mwops-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:29092 --list` 应列出 `mwops-logs` |
| **日志收到了但不通知、也不分析** | ① 规则没命中或没启用（**平台没有默认规则**，未命中 = 不入库、不通知、不分析），屏蔽规则优先命中也会直接丢弃；② 冷却中（`suppressed=true` + `cooldown_until`，事件已记录）；③ 看「AI 分析」列：`disabled` = 规则关了 AI / 服务不在出网白名单 / 未配置外部 AI 服务；`awaiting` 久不结束 = 回调不通（见第十一章与 [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md) 附录 B）；`failed` = 外部服务错误或超时，原因都在 `analysis_error`。要立刻重跑点「**重新分析**」（清冷却立即重跑通知与 AI） |
| 趋势图是一条直线 / 某指标报 `Cannot read properties of undefined (reading 'series')` | 指标本身波动极小属正常（Y 轴已按数据自适应）；后一种是旧版 NaN 破坏 JSON 编码的问题（见 [POSTMORTEM.md](POSTMORTEM.md) INC-004），升级后端 + 前端产物即可 |
| **不确定到底是哪一环的问题** | 列表行 →「**自检**」：按环节给出结论，不用翻日志（见 9.1） |

### 9.1 集成自检（一次点击，按环节给结论）

点集成列表行或待处理横幅上的「自检」，平台按链路顺序检查四段，每段给 状态 + 依据 + 下一步：

| 环节 | 判据 | 失败时的动作 |
|---|---|---|
| ① 平台 → Exporter 端口 | 平台 TCP 探测抓取目标（本机：容器名:端口；远程：目标IP:端口） | 本机：重新应用；**远程：放通安全组/防火墙**（平台侧无动作可点） |
| ② Exporter 是否在位 | 本机：`docker inspect` 容器状态；**远程：如实说明平台看不到那台机器的容器**，以端口可达作为证据 | 重新应用 |
| ③ Prometheus 是否已抓取 | 按**本实例那条 target** 判 `health` / `lastError`（不是 job 级 up） | 重新应用（或等 30s 让 http_sd 刷新） |
| ④ 业务指标是否真的在流 | 直接查组件自身的 up（`redis_up` / `mysql_up` / `pg_up`；node 看抓取目标的 `up`）+ 命中了多少指标 | 测试连接 / 重试建号 |

结论行会写明「在『哪一环』断了」，并给出与待处理横幅一致的动作按钮。

> 为什么要有这个：以前排查一个集成要在四处各看一段 —— 平台端口、Prometheus target、`redis_up`、
> Exporter 日志 —— 而且"Exporter 是否还在托管""状态是否正确"没有一处能直接回答。
> 自检把这三件事分开回答：**在位**（②）、**被抓到**（③）、**真的有数据**（④）。

---

## 10. 集成后“直接能跑”的边界（自动化矩阵）

一次集成要真正产出指标，需要多个环节都成立。下表说明每个环节由谁完成、
以及全自动化的前置条件——**跨栈接入时，“平台能否写被管系统”决定了自动化上限**。

| 环节 | 今天的状态 | 能否全自动 | 前置条件 / 风险 |
|---|---|---|---|
| ① Exporter 容器拉起 | ✅ 平台调 Docker Engine API 创建（默认关闭，可开） | 能 | 需挂载 `docker.sock`（等价宿主机 root 权限） |
| ② 网络接入（监控面 + 数据面） | ✅ 按 `integration.exporter_network` 多网络接入；`onboard.sh` 会自动写进 `.env` | 能 | 需 `docker.sock`；也可手工把网络名写进 `.env` |
| ③ 抓取目标注册（`instance_name` 标签） | ✅ http_sd，保存后 30s 内生效，无需重启 | 能 | 无 |
| ④ 实例纳管 / 命名一致 / 告警规则 | ✅ 自动（集成即纳管；relabel 由 `实例名相关环境变量` 同步） | 能 | 无 |
| ⑤ 凭据注入（口令含特殊字符） | ✅ 改为官方 flag + 环境变量（`--mysqld.username` / `MYSQLD_EXPORTER_PASSWORD`），不拼 DSN | 能 | 无 |
| ⑥ 「到底跑没跑起来」的核验 | ✅ 保存后**异步核验**：抓取目标 up 则标记已应用，失败则把 Prometheus 的 `lastError` 翻译后写回集成 | 能 | 无 |
| ⑦ 只读监控账号的创建 | ✅ **平台可代劳**：集成表单勾选「由平台创建/更新只读监控账号」并填一次管理凭据 | 能 | 属写操作 → 需显式授权；平台只执行内置模板 SQL（幂等 + 最小权限 + `MAX_USER_CONNECTIONS`），口令由平台生成，审计不含口令 |

**结论**：①~⑦ 现在都能在平台上一次配置完成——**对被管项目零侵入**：

- 账号：平台用一次性 client 容器执行固定模板 SQL 建号，不需要登录被管库手工建；
- 网络：平台按**别名自动发现**目标所在的 docker 网络（跨 compose 项目也行），
  使用者只需填 `legacy-mysql` 这样的名字，不必知道它属于哪个项目、哪张网；
- Exporter：平台拉起（同时接入监控面与数据面）；
- 抓取与大盘：平台自带的 Prometheus + Grafana，被管项目**不再需要自带监控栈**。

给使用者的两条路（**现已默认走平台托管**）：

- **路径 A（平台托管，推荐）**：集成表单勾选「由平台创建/更新只读监控账号」，
  填一次管理凭据 → 平台执行固定模板 SQL 建号，口令留空则由平台生成十六进制随机串。**被管项目零配置**。
- **路径 B（自行预置）**：不勾选该选项，账号由被管系统侧预置（模板 SQL 见组件说明）。
  适合不便提供管理凭据的环境（如生产库由 DBA 管控）。

> 日志是**另一条独立链路**，且同样不需要被管项目配合：集成中心的「日志集成」
> （`mw_type=log`）由平台用 Ansible 在目标机装 Filebeat，推送到平台自带的 Kafka，
> 再由后端消费成日志事件并做规则化告警与外部 AI 代码分析，详见第十一章与
> [LOG_INTEGRATION.md](LOG_INTEGRATION.md)。它在**纳管与监控域之外**：不会出现在中间件列表、
> 统一监控下拉、指标告警的实例选择与大盘统计里。

---

## 11. 日志集成摘要（Filebeat → 平台 Kafka）

> 本节是摘要，**权威说明见 [LOG_INTEGRATION.md](LOG_INTEGRATION.md)**（拓扑、幂等部署规则、自检环节、配置项速查）。

集成中心除 `monitor` 类组件外，还有 `log` 类（模板 `type: "log"`、`category: "log"`，
名为「日志 / Filebeat」）。它与中间件集成的**根本区别**：不装 Exporter、不经过 Prometheus、
也不要求目标机上有 Docker——平台用 **Ansible 在目标服务器上幂等部署 Filebeat**，
Filebeat 把日志推到**平台自带的 Kafka**。

```
① 你在集成中心填：目标服务器 + 日志路径 glob + 服务名/环境 + 最低级别 + 多行合并
        ↓
② 平台渲染 filebeat.yml（inputs: filestream → output.kafka，JSON 编码）
   并渲染一份 Ansible playbook（与 Exporter 集成同一套 SSH 凭据机制）
        ↓
③ Ansible 在目标机上按 auto 顺序判定：
     已有 filebeat 且 systemctl is-active → 复用，只下发/校验配置
     有 docker                          → 官方 docker.elastic.co/beats/filebeat 容器
     都没有                             → 官方仓库装 deb/rpm + systemd 单元
        ↓
④ 配置内容变化才重启（渲染后的 filebeat.yml 内容哈希判定）；重复点集成不会重复安装
        ↓
⑤ Filebeat ──▶ 平台 Kafka（EXTERNAL :9092）──▶ 消费组 mwops-log-ingest 消费 topic mwops-logs
        ↓
⑥ 后端 internal/logpipe 解析事件 → LogAlertService.Ingest 按规则处理
   （去重窗口合并 + 冷却抑制）→ 落 log_alert_events
        ↓
⑦ 后处理（service/logalert_worker.go，定时任务）：外发通知 → 提交外部 AI 分析服务（事件置 awaiting）
   → 回调/轮询取回三点式结论（定位文件行 / 根因 / 应急处置 / 修复建议）→ 带结论通知；
   页面可对单条事件「重新分析」（会打破冷却抑制）
```

要点：

- **日志集成不属于「中间件纳管域」**（重要边界）：它虽然也记录在 `middleware_instances`
  （复用部署/尝试/自检那一套机制），但**没有指标画像、没有实例端口**，因此
  **不会**出现在「中间件纳管」列表、统一监控的实例下拉、指标告警规则的实例选择、
  大盘的实例统计与健康探测里。这个边界由 `service.MiddlewareDomainTypes()` 统一收口
  （白名单 = 手工纳管的中间件类型 ∪ 有指标画像的集成类型），并有守卫测试钉住。
- **目标机出网与接入地址**：被管机必须能访问 `.env` 里的 `KAFKA_ADVERTISED_HOST:KAFKA_PORT`。
  这个值写 `localhost`/`127.0.0.1` 时，Filebeat 会**握手成功、随后立刻断开**并报
  `dial tcp 127.0.0.1:9092: connect: connection refused`——它被 broker 元数据引导去了自己那台机器。
  集成自检第 2 段「被管机接入地址」专门检测这个地址（远程目标 + 回环/容器内地址会被直接判红）。
- **安装是幂等的，并提供一个「覆盖 Filebeat」开关**：默认（关闭）时目标机上已装的 Filebeat 一律不动
  ——不重新下载 deb/rpm、本地已有该 tag 的镜像也不重新 `docker pull`，只有缺失时才安装；
  打开后则重新拉取安装包/镜像并**强制覆盖安装**（`apt-get --reinstall` / `dnf reinstall` /
  重新 `docker pull` + 重建容器），用于升级版本或修复装坏的 Filebeat。
  `filebeat.yml` **不受这个开关控制**：它由平台配置推导，始终按渲染内容同步（内容没变不重启），
  所以改日志路径或 Kafka 地址不必打开它。显式选定的 `package`/`docker` 优先于“复用”（INC-029）。
- **不需要 `docker.sock`**：日志集成走 SSH + Ansible 到目标机，采集在被管侧自洽运行；
  平台侧即使关掉 Docker 通道（`INTEGRATION_DOCKER_ENABLED=false`）它依然可用。
- **平台重启不影响采集**：Filebeat 有本地缓冲与断点续传（注册表），
  消费位点只在 Ingest 成功后提交，因此不会因为平台升级而丢日志。
- **自检返回日志专用环节**（`POST /api/integrations/:id/selfcheck`）：
  ① 平台 → Kafka 日志总线；② 被管机接入地址（Kafka EXTERNAL）；③ 日志是否已进入平台。
  它不再检查 Exporter 端口与 Prometheus 抓取。
- **兜底通路**：`POST /api/hooks/logs`（应用直推）保留，用于不能装 Filebeat 的场景，
  字段与 Filebeat 路径统一映射（`log_path` ↔ `log.file.path`）。
- **AI 代码分析外移**：平台不持有代码副本，后处理把脱敏后的错误信息提交给外部 AI 分析服务
  （状态机 pending → running → awaiting → done/failed/disabled）；
  对接与验收见 [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md)。

> `environment=prod` 时的审批语义与中间件集成一致：写操作先建审批工单，审批通过后再由「重新应用」执行。

---

## 12. 专题：自备 Prometheus / 自建 Exporter 接入

> 集成中心覆盖的是「平台托管 Exporter」路径。如果中间件在别的机器、已有 Prometheus 体系，
> 或要接入自研组件，按本章操作。本章是原《中间件接入与采集操作文档》的收敛版，
> 去掉了与第三~十一章重复的部署细节。

### 12.1 平台边界：做什么、不做什么

平台**刻意不做**中间件协议级采集器，避免与 Prometheus 生态重复建设。

| 能力 | 平台是否具备 | 真实实现方式 | 代码位置 |
|------|--------------|--------------|----------|
| 中间件指标采集 | ❌ 平台不直连中间件取指标 | 由 **官方 Exporter** 暴露，平台通过 **PromQL 查询 Prometheus** | `internal/monitor/prometheus.go` |
| 连接可用性探测 | ⚠️ 仅 **TCP 端口连通性** | `net.DialTimeout` 探测 host:port，非协议级握手 | `internal/service/middleware.go: probe()` |
| 应用日志集成 | ✅ | **Filebeat（平台用 Ansible 部署到目标机）→ 平台 Kafka**；应用也可 HTTP Hook 直推 | `internal/logpipe`、`internal/service/logpipeline.go`、`/api/hooks/logs` |
| 中间件运行态配置 | ⚠️ 读的是**平台侧登记的配置**，不是从中间件实时读取 | 诊断时读取实例的 `config` 字段（纳管时填写） | `internal/service/diagnose.go: collectConfig()` |
| 阈值告警 | ✅ | 平台按 PromQL 取当前值 → 比对规则阈值 → 指纹收敛 → 通知/触发 AI | `internal/service/alert.go` |
| AI 根因分析 | ✅ | 采集上下文（指标摘要 + 日志指纹 + 登记配置 + 知识库）→ 一次 LLM 调用 → 结构化报告 | `internal/service/diagnose.go` |

**硬性前置条件：被管中间件必须能通过 Exporter 暴露指标，并被某个 Prometheus 抓到。**
若某中间件没有官方 Exporter（例如自研组件），平台无法凭空获得它的指标——可按 12.7 自行适配。

> 「连接测试」`POST /api/middlewares/:id/test` 只验证 **TCP 可达性与耗时**，
> 不会用账号密码做协议级登录校验。密码 AES-256-GCM 加密存储，但目前仅用于存储，
> 验收时请勿按「已校验凭据」理解。

### 12.2 五类数据从哪里来

```text
                    ┌─────────────── ① 指标（主链路） ───────────────┐
其他项目的中间件 ──▶ 官方 Exporter ──▶ Prometheus ──PromQL──▶ 平台监控/告警
 (Redis/Kafka/…)      :9121/:9308/…      :9090                  │
                                                                ▼
应用服务 ──② 日志(Filebeat → Kafka / HTTP Hook)──▶ 平台消费/接收 ──▶ 指纹收敛 ──▶ 日志告警
                                                    │  按「日志告警规则」做窗口去重 + 冷却抑制
                                                    └─▶ 后处理：通知渠道 + 提交外部 AI 分析服务
                                                                │
平台纳管记录 ──③ 登记配置(config 字段)──────────────────────────┼──▶ AI 诊断上下文
知识库 ──────④ 历史案例(向量检索)───────────────────────────────┤   （六道护栏约束）
平台侧 ──────⑤ 平台自身指标(/metrics，由平台 Prometheus 抓)─────┘
```

| 编号 | 数据 | 来源 | 采集方式 | 是否需要 Exporter |
|------|------|------|----------|-------------------|
| ① | 性能/资源/可靠性指标 | 中间件官方 Exporter | 平台拉 Prometheus（PromQL） | **需要** |
| ② | 应用 ERROR 日志、堆栈、GC | 应用所在服务器上的日志文件 | 日志集成（Ansible + Filebeat → 平台 Kafka）；或 HTTP Hook 直推 | 不需要 |
| ③ | 连接信息、关键配置项 | 纳管表单 | 人工登记（`config` 字段） | 不需要 |
| ④ | 历史相似故障案例 | 平台知识库 | 诊断沉淀 + 人工录入 | 不需要 |
| ⑤ | 平台自检指标 | 平台后端 | Prometheus 抓 `/metrics` | 不需要 |

### 12.3 部署拓扑与网络要求

| 场景 | 适用 | Exporter 部署位置 | Prometheus | 说明 |
|------|------|-------------------|------------|------|
| **A. 同机共栈**（最快验证） | 单机/测试 | 与平台同一 compose | 平台自带 | 用 `deploy/compose.middleware-exporters.yml` 直接起 |
| **B. 中间件在别的机器**（最常见） | 生产 | 部署在**中间件所在主机**（或能访问它的机器） | 平台自带 Prometheus 抓取 Exporter | 确保 Prometheus 能访问 Exporter 的 `:9121` 等端口 |
| **C. 已有 Prometheus**（推荐生产） | 已建成监控体系 | 复用既有 Exporter | **复用既有 Prometheus** | 平台只需把 `prometheus.base_url` 指向它，无需新起 |

场景 A 一条命令起齐全套 Exporter：

```bash
# 前提：平台已用根目录 compose 起好
docker compose -f docker-compose.yml -f deploy/compose.middleware-exporters.yml up -d
# 被管中间件地址通过 REDIS_TARGET_ADDR / REDIS_TARGET_PASSWORD / REDIS_INSTANCE_NAME 等环境变量指定
```

场景 B/C 在**中间件所在主机**部署 Exporter（不要暴露到公网），并在 Prometheus 增加抓取任务：

```bash
docker run -d --name redis-exporter --restart unless-stopped \
  -p 9121:9121 \
  -e REDIS_ADDR=redis://127.0.0.1:6379 \
  -e REDIS_PASSWORD='<只读账号密码>' \
  oliver006/redis_exporter:v1.66.0
```

```yaml
  - job_name: middleware-exporter-redis       # 命名约定：<prometheus.exporter_job_prefix>-<类型>
    static_configs:
      - targets: ['10.0.0.11:9121']
        labels:
          instance_name: redis-prod-order      # 平台按此匹配实例
```

- `oliver006/redis_exporter` 可加 `--redis-only-metrics` 让指标集合贴近平台画像；
- `danielqsj/kafka-exporter` **不提供** `instance_name` 标签，必须在 Prometheus 里用
  `relabel_configs` 补上（`deploy/prometheus/prometheus.with-exporters.yml` 有示例）。

网络与端口要求：Prometheus → Exporter（9121/9308/9104/9187/9114/9113）；
Exporter → 中间件（6379/9092/3306/5432/9200/80）；平台后端 → Prometheus `:9090`；
应用 → 平台 `:8080`（Hook）；目标机 Filebeat → 平台 Kafka `:9092`；平台 → 目标机 SSH 22（Ansible）。
平台**不需要**直连中间件端口（TCP 健康探测除外，可关）。

### 12.4 手工纳管：标签必须对上

「纳管」= 告诉平台「这个中间件叫什么、在哪、指标在 Prometheus 里怎么找」（中间件纳管 → 新增实例）。
关键字段：实例名称（默认 `instance_name` 匹配值，建议与 Exporter 上报名一致）、中间件类型、
连接地址/端口（TCP 探测用）、环境、分组、Prometheus job（可选，精确匹配）、
Prometheus instance（推荐填，最稳）、配置 config（进入 AI 诊断上下文，见 12.6）。

平台构造 PromQL 标签匹配串的逻辑（`internal/monitor/profile.go: buildSelector`）：

| 你填写的字段 | 生成的匹配条件 | 结果 |
|---|---|---|
| job + instance | `job="你的job",instance="10.0.0.11:6379"` | 最精确 |
| 只填 instance | `job="<前缀>-<类型>",instance="..."` | 精确（job 走默认） |
| 只填 job | `job="你的job",instance_name="<实例名>"` | 依赖实例名一致 |
| 都没填 | `job="<前缀>-<类型>",instance_name="<实例名>"` | 依赖 Exporter 上报名 |

**排查口诀**：监控页显示「无数据源」= 没连上 Prometheus；
显示指标但全是 `unknown`/`0` = 连上了但**标签没匹配到序列**，拿同样的标签去 Prometheus 查一次。

```yaml
# configs/config.yaml 或环境变量 MWOPS_PROMETHEUS_BASE_URL
prometheus:
  base_url: http://prometheus:9090        # 留空则使用内置确定性模拟器
  exporter_job_prefix: middleware-exporter
  cache_ttl: 15s
```

### 12.5 指标契约：类型 → Exporter → 平台指标

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

**状态判定语义**（配规则时会用到）：平台按 **阈值模式（`threshold_mode`）** 判定指标状态：

| 模式 | 语义 | 示例 |
|------|------|------|
| `higher_worse` | `≥ 严重线` → critical；`≥ 警戒线` → warning | 内存使用率、连接数、延迟 |
| `lower_worse` | `≤ 警戒线` → warning；`≤ 严重线` → critical（**含边界**） | 命中率（警 90/严 80）、ES 集群状态（警 1/严 0） |
| `bool_down` | `≤ 严重线` → critical，否则 ok | Broker up（严 0） |
| 空 | 纯观测，不判定 | QPS、TPS、速率类 |

指标目录接口 `GET /api/metrics/catalog?mw_type=redis` 会返回每项的
`threshold_mode`、`warning_threshold`、`critical_threshold`。

**阈值告警的两种落地方式**：

1. **平台内规则**（推荐，默认）：新增「告警规则」→ 选实例 + 指标 + 操作符 + 阈值，
   平台按 `scheduler.metric_rule_eval_interval`（默认 30s）评估。规则用**你填的操作符**比较当前值，
   可以直接写 `keyspace_hit_rate < 90` 这类语义。
2. **Prometheus 规则**：用 `deploy/prometheus/rules/middleware.yml` 在指标侧先拦截，
   适合已有 Alertmanager 体系的团队；两套共用同一收敛策略，不会重复告警。

### 12.6 让 AI 诊断「有据可依」：注入关键配置

AI 诊断的上下文**只有四个来源**（受上下文预算约束）：指标摘要、日志指纹、**登记的 config**、知识库参考案例。
其中指标与日志自动获得，而「中间件关键配置」需要你在纳管时填写——这直接决定根因分析的准确度。

**操作路径**：中间件纳管 → 编辑实例 → 配置（JSON）。建议按类型填写：

```jsonc
// Redis：诊断内存/淘汰类问题必需
{ "maxmemory": "4gb", "maxmemory-policy": "allkeys-lru", "appendonly": "yes",
  "save": "900 1 300 10", "cluster-enabled": "no" }

// Kafka：诊断积压/副本问题必需
{ "num.partitions": "12", "default.replication.factor": "3", "min.insync.replicas": "2",
  "auto.offset.reset": "latest", "log.retention.hours": "168" }

// MySQL / PostgreSQL
{ "max_connections": "500", "innodb_buffer_pool_size": "8G",
  "slow_query_log": "ON", "long_query_time": "1" }
```

> AI 的每条结论必须引用证据（`evidence`），无证据的结论会被质量护栏标注为「推测」。
> 若 `config` 为空，像「maxmemory-policy 与业务写入模式不匹配」这类根因就只能给推测结论。

### 12.7 没有官方 Exporter 怎么办

平台靠「Prometheus 里存在符合约定的指标名」获取数据，因此你有三条路：

1. **自研 Exporter**（推荐）：按 12.5 表格里的指标名暴露 `/metrics`。
   实现要点：命名与单位一致；带 `job`/`instance_name` 标签；`instance_name` 与平台纳管实例名一致。
2. **Prometheus recording rules 做映射**：已有其他指标名时，用 recording rule 计算出平台期望的名字。
   若无法产生平台画像依赖的原始指标名，需按第 3 条改画像。
3. **扩展指标画像**（需改代码）：在 `internal/monitor/profile.go` 的 `profiles` 中为该类型
   增加 `MetricSpec`（含 `Expr`、`Mode`、`threshold_mode`、阈值、模拟器参数）。这是设计上预留的扩展点，
   改完 `go test ./internal/monitor/` 会校验阈值方向是否自洽。

### 12.8 日志通路（简述）

- **主路径**：集成中心新建 `log` 类型集成（Filebeat → 平台 Kafka），字段、幂等部署与排障见
  [LOG_INTEGRATION.md](LOG_INTEGRATION.md) 与本文第十一章；
- **兜底路径 HTTP Hook 直推**（不能装 Filebeat 时）：

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
    "log_path": "/var/log/order-service/error.log"
  }'
```

`service` / `level` / `message` 必填。响应中 `signature` 为错误指纹，
`merged=true` 表示与去重窗口内既有事件合并，`suppressed=true` 表示处于冷却期。
两条通路字段统一映射（`log_path` ↔ `log.file.path`，`service` ↔ `fields.service`）。
规则匹配、去重与冷却语义见 [LOG_INTEGRATION.md](LOG_INTEGRATION.md) 5.1：
**平台没有默认规则，未命中已启用规则的日志不入库、不通知、不分析。**

### 12.9 验证：三步确认链路

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

端到端冒烟脚本：

```powershell
pwsh -File scripts/smoke-test.ps1 -BaseUrl http://127.0.0.1:8080        # PowerShell 7+
powershell -ExecutionPolicy Bypass -File scripts\smoke-test.ps1 -BaseUrl http://127.0.0.1:8080   # PS 5.1
```

| 现象 | 定位 | 处理 |
|------|------|------|
| 监控页「无数据源」/指标「无数据」 | `prometheus.base_url` 为空或连不通 | 配置平台 Prometheus 地址；确认平台容器能访问 `:9090` |
| 有指标但值为 0 / `unknown` | PromQL 标签没匹配到序列 | 用第 2 步查询核对 `job` / `instance` / `instance_name` 是否与纳管字段一致 |
| 只有部分指标有值 | Exporter 未暴露该指标 | 核对 12.5 的指标名；`curl exporter/metrics` 搜索 |
| Kafka 全部无数据 | kafka-exporter 无 `instance_name` 标签 | 在 Prometheus 抓取配置里用 `relabel_configs` 补标签 |
| 连接测试通过但监控无数据 | 两者独立：前者是 TCP 探测，后者依赖 Exporter | 正常现象，按上表排查指标链路 |

### 12.10 安全与权限要点

| 项 | 要求 |
|----|------|
| 中间件账号 | **只读最小权限**。MySQL：`PROCESS, REPLICATION CLIENT, SELECT`；PG：`pg_monitor`；Redis：ACL 只读 + 禁用 `FLUSHALL/KEYS` 等 |
| Exporter 暴露面 | 只监听内网，不要发布到公网（`deploy/compose.middleware-exporters.yml` 的端口映射仅便于验证，生产建议去掉 `ports`） |
| 连接密码 | 平台侧 AES-256-GCM 加密存储，主密钥来自环境变量或 0600 密钥文件 |
| 数据权限 | 纳管时填好「环境 + 分组」，平台在**仓储查询层**强制过滤，跨环境不可见 |
| 日志脱敏 | 上报前建议在应用侧先脱敏；平台提交外部 AI 前对错误信息/堆栈自带 IP/手机号/邮箱/口令脱敏 |
| 出网合规 | 默认**禁止**向外部 AI 分析服务外发；在 `security.outbound_whitelist` 按服务显式放行 |
| 高危操作 | L2（清理 key / 重启 / 改配置 / SQL 写）一律走审批，30 分钟未审批自动拒绝 |

### 12.11 接入检查清单

纳管一个中间件实例时，按此清单逐项确认：

- [ ] Exporter 已部署，且能 `curl :<exporter-port>/metrics` 看到指标
- [ ] Exporter 到中间件的网络与只读账号可用（Exporter 自身上报 up=1）
- [ ] Prometheus 抓取任务已加，`job` 命名与 `prometheus.exporter_job_prefix` 约定一致
- [ ] 抓取任务带 `instance_name` 标签（kafka-exporter 需 relabel）
- [ ] 平台 `prometheus.base_url` 指向正确的 Prometheus
- [ ] 平台纳管实例名 = Exporter 的 `instance_name` 上报名
- [ ] 纳管时填写了「Prometheus instance」（最稳）或确认 job 兜底可用
- [ ] 正确选择环境与分组（影响数据权限与审批强制）
- [ ] 填写了 `config`（关键配置项）供 AI 诊断使用
- [ ] 实例详情页能看到指标、状态与阈值判定
- [ ] 按需创建告警规则，并点通知渠道标签发送自检消息
- [ ] 应用日志已通过**日志集成（Filebeat → 平台 Kafka）**接入并自检通过，或已用 Hook 直推，日志页能看到事件
- [ ] 在「日志告警规则」页为不同重要性的服务配规则（去重窗口 / 冷却期 / 通知渠道 / AI 开关）；
      **没有默认规则，不配规则 = 不产生告警**
- [ ] 想要 AI 代码结论：在「AI 设置」配置外部 AI 分析服务（协议、密钥、回调）并把服务加入出网白名单；
      否则事件会是 `analysis_state=disabled`（对接验收见 [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md) 附录 B）
- [ ] 造一条错误日志验证：事件出现 → 数秒内「AI 分析」列从 pending 变成 awaiting 再到 done / disabled / failed，
      且 `analysis_error` 能解释原因；冷却期内的重复日志只累加 `error_count` 且 `suppressed=true`

---

## 13. 已知边界

1. **一键部署依赖 docker.sock**：默认关闭；平台不会（也无法）在无 Docker 的环境里拉起 Exporter 容器，
   此时只渲染配置。**日志集成不受此限制**：它走 SSH + Ansible（见第十一章），关掉 Docker 通道后依然可用。
2. **容器重建策略**：每次「应用」都会删除并重建同名 Exporter 容器（保证 env/参数与页面一致），
   因此容器内的历史状态不会保留——Exporter 本身无状态，这是有意的取舍。
3. **Kafka SASL/ACL、ES 自签证书、MySQL `my.cnf` 挂载** 等复杂场景未做成表单字段，
   请用「配置」抽屉复制片段后手工补充。
4. **Grafana 大盘**只提供大盘编号，需要 Grafana 自行导入（平台不托管 Grafana）。
5. **告警规则**为模板预置阈值，落库后可在「告警规则」页继续调整。

---

## 14. 相关文档

| 文档 | 内容 |
|---|---|
| [LOG_INTEGRATION.md](LOG_INTEGRATION.md) | 日志集成权威说明：Kafka 拓扑、Filebeat 幂等部署、自检与配置项 |
| [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md) | 外部 AI 分析服务 OpenAPI v1 契约、回调 HMAC 验签、平台侧接入验收 |
| [API.md](API.md) | 接口清单（集成、日志集成、服务发现、接入自检） |
| [OPERATIONS.md](OPERATIONS.md) | 平台运维（备份、升级、审计校验、后处理排查） |
| [POSTMORTEM.md](POSTMORTEM.md) | 交付期真实故障记录（含指标 NaN、白屏、建表冲突） |
