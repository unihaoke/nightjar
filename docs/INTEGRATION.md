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

## 4. 三种落地方式（按推荐度）

| 方式 | 适用 | 操作 |
|---|---|---|
| **http_sd（推荐，平台内置）** | 平台自带 Prometheus（`deploy/prometheus/prometheus.yml` 已内置 `middleware-integration` job） | 无：保存后 30 秒内自动生效 |
| **http_sd（外部 Prometheus）** | 复用 被管项目自带的 app-prometheus 等外部 Prometheus | 在其 `scrape_configs` 加一个 job，`http_sd_configs.url: http://mwops-backend:8080/api/sd/integrations`（该 Prometheus 需能访问 `mwops-backend`，例如与平台同网络） |
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

## 6. 与被管项目（跨主机 / 跨 compose）的组合

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

## 6.1 反向接网（可选项，默认关闭）

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

## 6.2 监控账号：默认由平台代建 + 账号管理

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

## 7. 权限、安全与审计
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

### 7.1 docker.sock：权限与收敛（部署必读）

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

## 8. 排查
| 现象 | 排查 |
|---|---|
| 集成保存成功但监控页没有数据 | 实例详情 → **接入自检**：`job_up=null` 说明没有该 job（检查 Prometheus 配置里是否有 `middleware-integration`）；`job_up=1` 且 `matched=0` 说明标签对不上（检查集成名称是否被改过） |
| 服务发现不生效 | 后端容器内 `curl -s http://127.0.0.1:8080/api/sd/integrations`；Prometheus 容器内 `wget -qO- http://backend:8080/api/sd/integrations`；Prometheus 的 /targets 页看 `middleware-integration` 下的 target（`refresh_interval` 默认 30s） |
| Exporter 容器起来了但 up=0 | Exporter 连不上被管实例：核对地址/账号口令、网络是否两张都挂上（`docker inspect <容器> | grep -A5 Networks`） |
| **Redis：Exporter 日志写 `Couldn't connect to redis instance (redis://<公网IP>:6379)`、`redis_up 0`** | Exporter 打的是自己的**公网 IP**：被管实例与 Exporter 同机时应填 `127.0.0.1`（公网 IP 要走 hairpin + 安全组，云上常被拦）。平台 r5 起会在「地址主机 == 目标机」时自动改用回环并写进部署说明；也可直接检查容器拿到的变量：`docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' <容器>` 里 `REDIS_ADDR` 应是 `127.0.0.1:6379` |
| **Redis：`redis_up 0` 且 `last_scrape_error` 形如 `dial redis: unknown network redis`** | **这是马甲，不是根因**：redis_exporter 先用 `redis://` 连接，失败后按 `://` 拆开重试（`Dial("redis", "host:port")`），于是把 scheme 当成了网络类型，**真正的失败原因被吞掉**。真实原因通常是：① 地址/端口不通（端口只绑在 `127.0.0.1` 却填了公网 IP）；② 口令不一致，或目标没设口令却传了口令（`ERR Client sent AUTH, but no password is set`）；③ 多填了 ACL 用户名。拿真实原因：给容器加 `REDIS_EXPORTER_DEBUG=true` 看日志里的 `DialURL() failed, err:`，或在被管机上 `redis-cli -h <地址> -p <端口> [-a <口令>] ping`。平台从 r4 起按不带 scheme 的 `host:port` 注入，兜底路径会走回 tcp 分支，真实错误不再被吞 |
| **容器在跑，但 `docker ps` 的 PORTS 列是空的** | **正常现象，不是故障**：只有做端口映射（`-p`/NAT）的容器才在这一列显示端口。远程安装与主机监控都用 `--network host`，容器直接使用宿主网络命名空间、Exporter 绑的就是宿主端口，因此没有映射可显示。按下面三条确认它真的在听：<br>`docker inspect -f '{{.HostConfig.NetworkMode}}' <容器>` → `host`<br>`ss -ltnp \| grep <Exporter端口>` → 看到 `redis_exporter`/`mysqld_exporter` 在听<br>`curl -s http://127.0.0.1:<Exporter端口>/metrics \| grep -E '^redis_up\|^mysql_up'` → `1` 才算真的通 |
| 远程安装 Ansible 成功但平台报"探测失败" | 平台从**平台侧**探目标机的 Exporter 端口：云主机要在安全组/防火墙对平台出口 IP 放通该端口（9121/9104/9187/9100） |
| 一键部署报错 | 集成列表里该行会显示「待处理」与失败原因；`INTEGRATION_DOCKER_ENABLED` 与 socket 挂载是否都就绪 |
| MySQL 某个采集项"关了没关掉" | 开关是否渲染成 `--no-collect.xxx`（抽屉里看 compose 片段） |
| 想彻底重来 | 列表行 →「重新应用」（重写服务发现 + 重建容器），或删除后重新集成 |
| **不确定到底是哪一环的问题** | 列表行 →「**自检**」：按环节给出结论，不用翻日志（见下） |

### 8.1 集成自检（一次点击，按环节给结论）

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

## 8.5 集成后"直接能跑"的边界（自动化矩阵）

一次集成要真正产出指标，需要四个环节都成立。下表说明每个环节今天由谁完成、
以及全自动化的前置条件——**跨栈接入时，"平台能否写被管系统"决定了自动化上限**。

| 环节 | 今天的状态 | 能否全自动 | 前置条件 / 风险 |
|---|---|---|---|
| ① Exporter 容器拉起 | ✅ 平台调 Docker Engine API 创建（默认关闭，可开） | 能 | 需挂载 `docker.sock`（等价宿主机 root 权限） |
| ② 网络接入（监控面 + 数据面） | ✅ 按 `integration.exporter_network` 多网络接入；`onboard.sh` 会自动写进 `.env` | 能 | 需 `docker.sock`；也可手工把网络名写进 `.env` |
| ③ 抓取目标注册（`instance_name` 标签） | ✅ http_sd，保存后 30s 内生效，无需重启 | 能 | 无 |
| ④ 实例纳管 / 命名一致 / 告警规则 | ✅ 自动（集成即纳管；relabel 由 `实例名相关环境变量` 同步） | 能 | 无 |
| ⑤ 凭据注入（口令含特殊字符） | ✅ 改为官方 flag + 环境变量（`--mysqld.username` / `MYSQLD_EXPORTER_PASSWORD`），不拼 DSN | 能 | 无（本轮修复） |
| ⑥ 「到底跑没跑起来」的核验 | ✅ 保存后**异步核验**：抓取目标 up 则标记已应用，失败则把 Prometheus 的 `lastError` 翻译后写回集成 | 能 | 无（本轮新增） |
| **⑦ 只读监控账号的创建** | ✅ **平台可代劳**：集成表单勾选「由平台创建/更新只读监控账号」并填一次管理凭据 | 能 | 属写操作 → 需显式授权；平台只执行内置模板 SQL（幂等 + 最小权限 + `MAX_USER_CONNECTIONS`），口令由平台生成，审计不含口令 |
| ⑧ 账号存在性预检 | ✅ 由 ⑦ 覆盖（建号后立刻核验；未建号时核验阶段会报 `Access denied`） | 能 | 无 |

**结论**：①②③④⑤⑥⑦⑧ 现在都能在平台上一次配置完成——**对被管项目零侵入**：

- 账号：平台用一次性 client 容器执行固定模板 SQL 建号，不需要登录被管库手工建；
- 网络：平台按**别名自动发现**目标所在的 docker 网络（跨 compose 项目也行），
  使用者只需填 `legacy-mysql` 这样的名字，不必知道它属于哪个项目、哪张网；
- Exporter：平台拉起（同时接入监控面与数据面）；
- 抓取与大盘：平台自带的 Prometheus + Grafana，被管项目**不再需要自带监控栈**。

> 唯一仍需被管项目配合的是**日志链路**：日志文件在被管容器里，
> 平台侧的采集需要共享日志卷（被管项目的 `docker-compose.yml` 里那一行 `backend-logs` 挂载）。
> 这是"读对方文件"的物理前提，与监控栈无关。

**结论**：①②③④⑤⑥ 已经做到"配置即接入"；**唯一的硬缺口是 ⑦**——
没有只读账号，mysqld_exporter 必然 `up=0`（表现为 `Access denied`）。
这正是 `docs/GUIDE-ONBOARD.md` §8.4 里最常见的那一类。

给使用者的两条路（**现已默认走平台托管**）：

- **路径 A（平台托管，推荐）**：集成表单勾选「由平台创建/更新只读监控账号」，
  填一次管理凭据 → 平台执行固定模板 SQL 建号（详见上表 ⑦），
  口令留空则由平台生成十六进制随机串。**被管项目零配置**。
- **路径 B（自行预置）**：不勾选该选项，账号由被管系统侧预置
  （模板 SQL 见组件说明；被管项目的 `onboard.sh` 也会执行 initdb）。
  适合不便提供管理凭据的环境（如生产库由 DBA 管控）。

---

## 8.6 日志接入（平台侧采集，被管项目零改动）

集成中心顶部有 **日志接入** 入口，用于采集被管项目的应用日志。与中间件集成的区别是
**它不止配置，还会真的去读对方的 docker 配置**：

```
① 你填「目标容器名」（如 app-backend）
        ↓
② 平台 docker inspect 该容器，读 env 与 Mounts
        ↓
③ 发现日志位置（按可信度）：
     a) 环境变量 LOG_PATH/LOG_DIR/... 指向的目录，且该目录被某个卷/宿主目录覆盖；
     b) 挂载点的容器内路径或卷名/宿主路径含 "log"；
     c) 都没有 → **拒绝配置**（不猜路径）
        ↓
④ 用**平台自身镜像**创建一个采集容器（只覆盖 Entrypoint=mwops-agent）：
     挂载同一份存储（命名卷按名字 / 宿主目录按路径）→ /logs:ro
     挂载 mwops-log-agent-state:/data（偏移量，重启不重复上报）
     接入平台网络 → 环境变量注入 platform_url / hook_token / service / files=/logs/*.log
        ↓
⑤ 日志事件进入「日志告警 → 事件」，按服务名归集
```

要点：

- **为什么拒绝而不是猜**：猜错的后果是采集容器起来了、但日志页永远为空，
  比直接报错难排查得多。拒绝时会明确告诉你"环境变量指了目录但没被挂载"或
  "没有任何像日志的挂载"。
- **为什么不需要额外镜像**：Agent 二进制已打进平台镜像（`middleware-ops/Dockerfile`
  同时构建 `cmd/server` 与 `cmd/agent`），采集容器复用它并覆盖 Entrypoint，
  因此不存在"另一个镜像要构建/分发/对版本"的问题。
- **通配采集**：默认采集 `<挂载点>/*.log` 里符合级别的行——平台不需要知道具体文件名，
  logback 轮转出的新文件也会被采到；`INFO` 级别会连 GC 这类无级别日志一起采（可选）。
- **对生产环境的写操作走审批**：`environment=prod` 时，由平台创建只读监控账号
  （见 §8.5 ⑦）不会立即执行，而是**创建审批工单**（工单里带将执行的固定 SQL，不含口令），
  审批通过后再点「重新应用」由平台建号。

> 备选方案（被管项目侧自建 Agent）仍保留：`cmd/agent` 支持 YAML 与**纯环境变量**两种配置，
> 既可以由平台代管，也可以在被管项目里自己跑一个容器；后者适合网络不允许平台访问
> docker.sock 的环境。

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
