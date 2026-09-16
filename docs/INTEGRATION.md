# 集成中心 · 使用与设计说明

> 对应云厂商 Prometheus 监控服务的「数据采集 → 集成中心」：
> [Redis Exporter 接入](https://cloud.tencent.com/document/product/1416/111839) ·
> [MySQL Exporter 接入](https://cloud.tencent.com/document/product/1416/111841)
>
> 差别只有一处：云上由厂商托管 Exporter 容器与抓取配置，本平台把这两件事
> **在本机 docker 环境内自己完成**——页面点选、填参数、保存即接入。

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
| 地址 | `address` | 实例 `host:port`（TCP 探测）+ Exporter 抓取目标 | 端口可省略（取模板默认值）；URL 型组件支持 `http://host:port/path` |
| 用户名 | `username` | Exporter 的 `REDIS_USER` / DSN / `--es.username` | MySQL/PG 需预建只读账号并授权 |
| 密码 | `password` | Exporter 的 `REDIS_PASSWORD` / DSN 口令 | AES-256-GCM 加密存储；生成的配置里只出现 `${MONITOR_PASSWORD}` 占位 |
| 标签 | `labels` | 服务发现的 labels（自定义指标标签） | 不能覆盖 `job`/`instance`/`instance_name`/`mw_type` |
| 环境变量（Redis）| `options`（`target=env`）| Exporter 容器环境变量 | 白名单：仅模板声明的键可透传 |
| Exporter 配置（MySQL）| `options`（`target=arg`）| Exporter 命令行开关 `--collect.*` | 上游默认开启的项被关闭时输出 `--no-xxx`（否则"关了没关掉"） |
| 查看监控 | 实例详情 / 统一监控 | 平台监控页 + 组件对应 Grafana 大盘 ID | 卡片上给出大盘编号 |
| 配置告警 | 自动创建推荐规则 | `alert_rules` | 保存时勾选「自动创建推荐告警规则」 |

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
   | 集成名称 | `jd-redis` |
   | 连接地址 | `jd-redis:6379`（容器名或 `10.0.0.11:6379`） |
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

## 4. 三种落地方式（按推荐度）

| 方式 | 适用 | 操作 |
|---|---|---|
| **http_sd（推荐，平台内置）** | 平台自带 Prometheus（`deploy/prometheus/prometheus.yml` 已内置 `middleware-integration` job） | 无：保存后 30 秒内自动生效 |
| **http_sd（外部 Prometheus）** | 复用 jd 自带的 interview-prometheus 等外部 Prometheus | 在其 `scrape_configs` 加一个 job，`http_sd_configs.url: http://mwops-backend:8080/api/sd/integrations`（该 Prometheus 需与 `mwops-backend` 同网，jd 场景已通过 `jd-nightjar` 打通） |
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

## 5. 组件模板矩阵

| 组件 | Exporter 镜像 | 端口 | 参数形态 | 推荐告警 | Grafana 大盘 |
|---|---|---|---|---|---|
| Redis | `oliver006/redis_exporter:v1.66.0` | 9121 | 环境变量 `REDIS_EXPORTER_*` | 内存率 > 85%、命中率 < 90%、淘汰 > 0 | 763 |
| MySQL | `prom/mysqld-exporter:v0.15.1` | 9104 | 命令行 `--collect.*` + `DATA_SOURCE_NAME` | 连接数 > 400、慢查询 > 5 | 7362 |
| PostgreSQL | `prometheuscommunity/postgres-exporter:v0.16.0` | 9187 | `DATA_SOURCE_NAME` + `--auto-discover-databases` 等 | 连接数 > 150、主从延迟 > 30s | 9628 |
| Kafka | `danielqsj/kafka-exporter:v1.7.0` | 9308 | 命令行 `--kafka.server` 等 | 消费 Lag > 1 万、ISR 不足 > 0 | 7589 |
| Elasticsearch | `prometheuscommunity/elasticsearch-exporter:v1.7.0` | 9114 | 命令行 `--es.uri` / `--es.username` | 非 green、堆 > 85% | 2322 |
| Nginx | `nginx/nginx-prometheus-exporter:1.3.0` | 9113 | 命令行 `--nginx.scrape-uri` | 5xx > 2% | 9614 |

各组件的踩坑点（会显示在集成弹窗里）：

- **Redis**：`REDIS_ADDR` 用 `redis://host:port`，账号口令分别用 `REDIS_USER`/`REDIS_PASSWORD`；
  云数据库集群架构必须打开两个 EXCLUDE 开关（不支持 `SLOWLOG`/`LATENCY HISTOGRAM`）；
  `redis_exporter` 没有 `SERVICE_NAME` 之类的实例名开关，`instance_name` 由平台写入。
- **MySQL**：先建只读账号 `GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';`；
  低于 5.6 部分指标采集不到属预期；口令含 `@ ( ) /` 建议改用 `--config.my-cnf`。
- **PostgreSQL**：`GRANT pg_monitor TO exporter;`。
- **Nginx**：被管 Nginx 必须开 `stub_status` 并对 Exporter 放通。

---

## 6. 与 jd（跨栈被管系统）的组合

jd 的 MySQL / Redis 在 `jd-data`（internal）网络里，不发布宿主端口。要让平台
拉起的 Exporter 同时「被平台 Prometheus 抓到」且「连得上 jd 的库」，Exporter
需要接两张网：

```bash
# .env —— 第一个是监控面（Prometheus 所在网络），第二个是数据面
INTEGRATION_EXPORTER_NETWORK=middleware-ops_mwops,jd-nightjar
```

- 容器的第一个网络在创建时指定，其余通过 `POST /networks/{id}/connect` 追加；
- `jd-nightjar` 由 jd 侧创建（internal），Exporter 因此能解析 `jd-redis` / `jd-mysql`；
- 集成地址填 **容器别名**（`jd-redis:6379` / `jd-mysql:3306`），与
  `docs/COLLECTOR-JD.md` 的纳管口径一致；
- 若更希望由 **jd 自己的 Prometheus** 抓取，用第 4 节的「显式 scrape job」方式，
  或把共享目录同时挂给两边（见 `jd/deploy/jd-exporters/prometheus-jd.yml` 顶部注释）。

---

## 7. 权限、安全与审计

- **权限**：与「中间件纳管」共用 `middleware:read`（查看/预览）与 `middleware:write`（增删改/应用）——
  集成产物本身就是一个纳管实例，不额外引入权限点。
- **口令**：AES-256-GCM 加密存储（复用平台主密钥）；**生成的任何配置里都不出现明文**，
  统一替换为 `${MONITOR_PASSWORD}` 占位；一键部署时才解密并按容器环境变量注入。
- **参数白名单**：只允许模板声明的 Exporter 参数，拒绝透传任意 env/flag；
  镜像与端口来自内置模板，不接受用户自定义。
- **审计**：`integration_create` / `integration_update` / `integration_apply` / `integration_delete`
  四条动作全部落审计（含地址、标签、是否部署，不含口令）。
- **操作级别**：L1（低危，直接执行并留痕）。生产环境若要更严的管控，
  可把 `deploy` 关掉，只允许平台渲染配置、由人工执行。

---

## 8. 排查

| 现象 | 排查 |
|---|---|
| 集成保存成功但监控页没有数据 | 实例详情 → **接入自检**：`job_up=null` 说明没有该 job（检查 Prometheus 配置里是否有 `middleware-integration`）；`job_up=1` 且 `matched=0` 说明标签对不上（检查集成名称是否被改过） |
| 服务发现不生效 | 后端容器内 `curl -s http://127.0.0.1:8080/api/sd/integrations`；Prometheus 容器内 `wget -qO- http://backend:8080/api/sd/integrations`；Prometheus 的 /targets 页看 `middleware-integration` 下的 target（`refresh_interval` 默认 30s） |
| Exporter 容器起来了但 up=0 | Exporter 连不上被管实例：核对地址/账号口令、网络是否两张都挂上（`docker inspect <容器> | grep -A5 Networks`） |
| 一键部署报错 | 集成列表里该行会显示「待处理」与失败原因；`INTEGRATION_DOCKER_ENABLED` 与 socket 挂载是否都就绪 |
| MySQL 某个采集项"关了没关掉" | 开关是否渲染成 `--no-collect.xxx`（抽屉里看 compose 片段） |
| 想彻底重来 | 列表行 →「重新应用」（重写服务发现 + 重建容器），或删除后重新集成 |

---

## 9. 已知边界

1. **一键部署依赖 docker.sock**：默认关闭；平台不会（也无法）在无 Docker 的环境里拉起容器，
   此时只渲染配置。
2. **容器重建策略**：每次「应用」都会删除并重建同名 Exporter 容器（保证 env/参数与页面一致），
   因此容器内的历史状态不会保留——Exporter 本身无状态，这是有意的取舍。
3. **Kafka SASL/ACL、ES 自签证书、MySQL `my.cnf` 挂载** 等复杂场景未做成表单字段，
   请用「配置」抽屉复制片段后手工补充。
4. **Grafana 大盘**只提供大盘编号，需要 Grafana 自行导入（平台不托管 Grafana）。
5. **告警规则**为模板预置阈值，落库后可在「告警规则」页继续调整。
