# 接入 nightjar · 操作指南（平台托管监控 · 被管项目零配置）

> 架构已定：**监控栈统一在 nightjar**。被管项目（被管项目）只跑业务，不装 Exporter、不装 Agent、
> 不跑 Prometheus/Grafana；平台负责建只读账号、拉起 Exporter、自动接入对方网络、抓取、出大盘，
> 日志则由平台用 Ansible 在目标机部署 Filebeat 推到平台自带 Kafka（见 [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md)）。
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
│ mwops-kafka        日志总线：Filebeat 推送日志            │
└──────────┬─────────────────────────────────────────────┘
           │ 平台用 Docker API 自己发现目标容器所在网络并接入（被管项目零改动）
┌──────────┴─ 被管项目（零监控配置）───────────────────────────┐
│ mysql / redis / backend / frontend                     │
│ 只提供：容器名（app-mysql / app-redis）      │
│        + backend-logs 卷                                │
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

---

## 2. 被管项目侧：什么都不用做

被管项目侧只有一个日常动作：把业务起起来。

```bash
cd <被管项目>
cp .env.example .env        # 必改：DB_PASS、REDIS_PASSWORD、JWT_SECRET
./start.sh                  # 等价于 docker compose up -d --build
./start.sh check            # 可选自检：容器 / 日志卷 / 日志目录挂载
```

被管项目的 `.env` 只有这些变量：`DB_NAME/DB_USER/DB_PASS`、`REDIS_PASSWORD`、
`AGENT_INTERVIEW_REDIS_ENABLED`、`JWT_SECRET`、`SPRING_PROFILES_ACTIVE`、
`BACKEND_PORT/WEB_PORT`、`AI_PROVIDER`。
（`PROMETHEUS_PORT`/`GRAFANA_*`/`MYSQL_EXPORTER_PASSWORD`/`NIGHTJAR_HOOK_TOKEN`/网络名 都已不再需要。）

自检通过的标准：

| 检查 | 期望 |
|---|---|
| `app-mysql` / `app-redis` / `app-backend` | running（并打印它们真实所在的网络，供平台自动接入） |
| `app-frontend` | running（不参与监控，失败只提示） |
| `backend` 的 `/app/data/logs` | 挂的是**命名卷**且已有 `.log` 文件 |

---

## 3. 平台侧：一条命令 + 三次点击

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
| ④ 集成 Redis | 名称 `legacy-redis`、地址 `app-redis:6379`、口令填 `.env` 的 `REDIS_PASSWORD`（Redis 不需要建号） |
| ⑤ 日志集成 | 集成中心 → **日志 / Filebeat** 选 `log` 类型 → 集成名 `order-app-log`、目标机 `app-backend` 所在主机、日志路径 `/app/data/logs/*.log`、服务名 `app-review-api`、级别 `ERROR` → 保存后点**自检**，三段环节（平台 → Kafka / 被管机接入地址 / 日志是否真的进来了）全绿 |

日志接入之后还有两步**可选但强烈建议**的配置（都在「日志告警」分组里，不配也能跑通）：

| 步骤 | 操作 | 为什么 |
|---|---|---|
| ⑥ **配规则（可选）** | 日志告警 → **日志告警规则** → 新建：`service_name` 填 `app-review-api`（留空 = 任意服务）、`signature_pattern` 填**错误指纹**的一段子串（从事件详情页的 `error_signature` 复制；留空 = 任意指纹，写 `/正则/` 则按正则匹配）、`min_severity` 选 `ERROR`，再定 **去重窗口**（默认 5 分钟）、**冷却期**（默认 10 分钟）、**通知渠道**（留空 = 平台已启用的渠道）与 **AI 分析**开关 | 决定"多久打扰人一次"以及要不要自动出代码结论。匹配按「priority 数字小者优先、同优先级按 id 升序取第一条命中的**启用**规则」；**一条都没命中**时用平台默认值（`log_alert.default_*`，该页顶部卡片会显示具体取值）。建完规则记得确认它是**启用**状态 |
| ⑦ **配服务→代码仓库映射（要 AI 代码结论就必填）** | 日志告警 → **服务器与仓库** → 新增映射：服务名填 `app-review-api`（**必须与日志事件里的 `service` 完全一致**）、仓库地址（HTTPS + 只读令牌或 SSH 部署密钥）、分支（默认 `main`）、语言 | AI 要回答"这条错误对应哪一行代码"，平台必须先在本地有一份与服务当前版本一致的代码。首次分析 **clone** 到 `code_repo.cache_dir`（默认 `./data/repos`，落在 `backend-data` 卷里），之后**只做更新**（分支非空时强制重置到远端）。没配映射时事件是 `analysis_state=disabled`，原因写在 `analysis_error`。要真出结论还需按服务开启 `allow_third_party` 并把服务名加入 `security.outbound_whitelist`；`code_repo.allow_outbound=false` 会让平台**完全不执行 git 命令** |

账号相关补充：

- **不需要提前建号**：MySQL/PostgreSQL 默认由平台代建（`CREATE USER IF NOT EXISTS` + `ALTER USER` + 最小授权），
  口令 24 字节十六进制、加密存储；未填管理凭据时集成照常保存，只在备注里提示"填凭据后点重新应用"；
- **账号管理**：集成中心右上角「监控账号」可查看来源/权限/最近轮换，
  支持**轮换口令**（账号改自己的口令，不需要管理员凭据）与**删除账号**（破坏性，需管理员凭据；prod 转审批工单）；
- 生产环境（`env=prod`）建号/删号只创建审批工单，不直接执行。

保存集成的瞬间，平台会自己完成网络接入：反查 `app-mysql` / `app-redis`
命中的容器 → 取得真实网络名 `app_data` → 把 Exporter 接成
「`middleware-ops_mwops`（Prometheus 抓它）+ `app_data`（它连数据库）」两张网，
并把平台自身也接进 `app_data` 以便做 TCP 健康探测。这些都发生在平台侧，
被管项目的网络、别名、compose 文件一个字节都不动（集成卡片会写明"已发现目标容器…所在网络…"）。

> 地址填**容器名**最稳（`app-mysql`）；填 compose 服务名（`mysql`）平台也会换算成容器名。
> 填一个 docker 里不存在的名字时，报错会列出候选容器名。

集成保存后平台会自动核验：抓取目标 `up` 就标记「已应用」；失败则把 Prometheus 的
`lastError` 翻译后写回列表（不必再去 `/targets` 页面翻）。

验收：

```bash
cd <nightjar>
./scripts/doctor.sh     # 平台容器 / docker.sock / 目标容器网络 / Exporter 网络 / 抓取状态
```

页面验收：**统一监控**能看到 `legacy-redis` / `legacy-mysql` 的曲线（趋势图 Y 轴按数据自适应）；
**日志告警 → 事件**能看到 `app-review-api` 的 ERROR 事件；
**Grafana**（`http://<主机>:3000`）数据源已就绪，大盘按集成卡片给的编号导入。

---

## 4. 排障对照表

| 现象 | 原因 | 处理 |
|---|---|---|
| Redis 指标全无、`redis_up` 查询出来是 0（而 `up=1`、标签全对） | Exporter 进程正常但**认证失败**：最常见是集成里填了「用户名」，而 被管项目的 Redis 只配了 `requirepass`（default 用户），并没有该 ACL 用户 → `AUTH <user> <pass>` 报 WRONGPASS | 编辑该集成，把**「用户名」清空**、只填口令（= 被管项目的 `REDIS_PASSWORD`）；保存后平台后台重建 Exporter。验证：`docker exec app-redis redis-cli -a "$REDIS_PASSWORD" ACL LIST` 只会看到 `user default` |
| Redis 指标全无、Exporter 报 `no such host` | Exporter 没接到 `app_data`，或地址填的是旧别名 `legacy-redis` | 点「重新应用」（平台会重新发现目标容器并接入其网络）；地址用容器名 `app-redis:6379` |
| 集成报 `dial unix /var/run/docker.sock: connect: no such file or directory` | 平台容器里**没有** docker.sock：compose 里的挂载还是注释状态，或平台容器早于该改动启动 | ① `docker inspect mwops-backend --format '{{range .Mounts}}{{.Source}}{{"\n"}}{{end}}' \| grep docker.sock` 确认；② 重跑 `./scripts/onboard.sh`（它会自动放开注释）；③ `docker compose up -d --force-recreate backend`。rootless Docker 请把 `INTEGRATION_DOCKER_HOST` 指向 `/run/user/<uid>/docker.sock` 并挂载该路径 |
| 集成报 `dial unix /var/run/docker.sock: connect: permission denied` | socket 已挂载，但平台以非 root 用户（`mwops`）运行，不在宿主 docker 组里 | `stat -c '%g' /var/run/docker.sock` 取 GID → 写进平台 `.env` 的 `DOCKER_GID=<GID>` → `docker compose up -d --force-recreate backend`。校验：`docker exec mwops-backend curl -s --unix-socket /var/run/docker.sock http://localhost/_ping` 应输出 `OK`（setup 脚本会自动探测并写入 DOCKER_GID） |
| 集成报「Prometheus 已配置 job 但 target 抓取失败（up=0）」 | Exporter 容器根本没被创建（上一条 docker.sock 报错的连锁结果） | 先解决 docker.sock，再点该集的「重新应用」 |
| 后端启动失败：`password authentication failed for user "mwo" (SQLSTATE 28P01)` | **Postgres 只在数据卷为空时应用 `POSTGRES_PASSWORD`**；卷早就初始化过，`setup` 脚本又轮换了 `.env` 的 `DB_PASSWORD`（旧版脚本的行为） | 重跑 `./scripts/onboard.sh`：第 5 步会用容器内 trust socket 把库内口令对齐到 `.env`（零数据损失）。详见 `OPERATIONS.md` §5.8 |
| 集成报「无法确定目标所在网络」 | 地址里的名字与 docker 里的容器名/服务名/别名都不匹配，或平台没挂 docker.sock | 报错里会列出候选容器名；`./scripts/onboard.sh` 会自动放开 docker.sock |
| 集成报「解析不了 legacy-mysql / server misbehaving」 | 用的是**旧架构的人工别名**（被管项目侧已不再提供） | 把地址改成容器名 `app-mysql:3306` / `app-redis:6379`，重新保存即可（平台会自动接入 `app_data`） |
| 集成保存成功但指标为空（`job_up=0`） | Exporter 连不上目标：账号没建 / 口令不一致 / 目标容器没运行 | 集成列表下方「待处理项」旁的 **「去重试 / 测试连接」**：带 root 凭据点「重试建号」（幂等重跑建号 SQL），或先点「测试连接」看账号到底能不能连 |
| 提示"job 已抓取（up=1）但匹配不到时序" | **`up{job=…}` 是 job 级判定**：同 job 里 MySQL 正常就会返回 1，即使本实例的 Exporter 已挂 | 新版自检会按**本实例那条 target** 给结论：① target 不是 up → 直接给 lastError（多半是 Exporter 没起来/连不上目标）；② target up=1 但只有抓取元指标、没有 `redis_*` → Exporter 起来了但连不上中间件（看 `redis_up`/`mysql_up`）；③ 真的 `instance_name` 不一致 → 提示改成实际取值 |
| 建号失败（权限不足、实例只读、凭据不对） | 建号 SQL 需要 CREATE USER / GRANT 权限 | 修好外部原因后**不必重填集成表单**：集成中心右上角「监控账号」→「重试建号」，失败原因就地显示；建号语句幂等，可反复重试 |
| 报 `Access denied for user 'exporter'` | 只读账号不存在或口令不一致 | 重新保存并勾选「由平台创建只读监控账号」（等效手工：见 `INTEGRATION.md` 的模板 SQL） |
| 报 `invalid DSN` | 旧版 Exporter 配置在拼 `DATA_SOURCE_NAME` | 已被官方方式取代（`--mysqld.username` + `MYSQLD_EXPORTER_PASSWORD`）；重建 Exporter 即可 |
| Exporter 起来了但 up=0，且 lastError 是 `connection refused` | Exporter 不在目标网络上（例如平台没有 docker.sock，无法自动接网） | 挂上 docker.sock 后点该集的「重新应用」；或按 `INTEGRATION.md` §6 手工把网络写进 `INTEGRATION_EXPORTER_NETWORK` |
| 日志集成：Filebeat **连上又断开**，报 `dial tcp 127.0.0.1:9092: connect: connection refused` | `.env` 的 `KAFKA_ADVERTISED_HOST` 填成了 `localhost`/`127.0.0.1`（或容器名）。Filebeat 先连上平台 9092 握手成功，随后被 broker 元数据引导去连**它自己那台机器**的 127.0.0.1，于是立刻断开 | 把 `KAFKA_ADVERTISED_HOST` 改成**被管机能访问到的平台宿主机 IP 或域名**（`ip route get 1 \| awk '{print $7; exit}'`），重建 kafka 容器（`docker compose up -d kafka`）；然后在目标机 `nc -vz <KAFKA_ADVERTISED_HOST> 9092` + `filebeat test output` 复验。集成自检第 2 段「被管机接入地址」专门抓这个问题 |
| 日志集成：Filebeat 显示已发送，但平台一条日志都没有 | 目标机时钟偏移过大（NTP 未同步）。Kafka 3.6+ 默认校验 `message.timestamp.difference.max.ms`，超出容忍窗口的消息以 `InvalidTimestampException` 被直接丢弃 | 目标机执行 `timedatectl` / `chronyc tracking` 确认已同步；偏差大的先修 NTP 再重启 Filebeat。自检第 3 段「日志是否已进入平台」会持续显示"还没有收到日志" |
| 担心重复点集成会重复安装 Filebeat | 不需要担心：`auto` 模式先 `command -v filebeat` + `systemctl is-active filebeat` 探测，**已装 Filebeat 不会被重复安装**，只校验/下发配置；配置内容用渲染后的 `filebeat.yml` 内容哈希判定，**内容不变就不会重启** Filebeat | 直接再点一次「保存并集成」即可；想核实就在目标机跑 `systemctl status filebeat`（或 `docker ps \| grep mwops-filebeat`）与 `filebeat test output` |
| 日志集成自检第 1 段红「平台 → Kafka 日志总线」 | 平台侧 `kafka` 容器没起来，或 `KAFKA_BROKERS` 被改错（平台侧要填容器网络的 `kafka:29092`，不是宿主端口） | `docker compose ps kafka`；`docker exec mwops-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:29092 --list` 应列出 `mwops-logs`；恢复 `.env` 的 `KAFKA_BROKERS=kafka:29092` 后重建 backend |
| 日志页没有事件 | 目标机 Filebeat 没起来 / 目标机连不上平台 Kafka / 日志路径 glob 写错 / 级别过滤太严 | 目标机 `systemctl status filebeat`（docker 模式 `docker ps \| grep mwops-filebeat`）与 `filebeat test output`；再在集成中心点**自检**，按红色那一段定位；`doctor.sh` 会检查平台 Kafka 与日志链路 |
| **日志收到了但不通知、也不分析** | ① 规则没命中：日志页上的规则与事件的服务名/指纹/级别对不上（或规则建了但没启用），于是走了平台默认值；② 冷却中：同指纹在冷却期内只合并计数；③ 规则关了 AI，或该服务没配代码仓库映射 / 拉代码失败 | ① 打开**日志告警 → 日志告警规则**，页顶卡片就是"没命中任何规则时平台用的默认值"（去重窗口/冷却期/AI 开关/通知渠道）；再核对规则的 `service_name`、`signature_pattern`、`min_severity`、`enabled` 与 `priority`（数字小者优先，取第一条命中）；② 看事件列表的「抑制 / 通知」列：`suppressed=true` 且有 `cooldown_until` 即处于冷却（**事件已记录，只是不重复打扰**），要立刻拿结论就点该条的「**重新分析**」（会清掉冷却记录立即重跑通知与 AI）；③ 看「AI 分析」列 `analysis_state` 与 `analysis_error`：`disabled` = 规则关了 AI 或没配仓库映射（去「服务器与仓库」补映射后点「重新分析」）；`failed` = 拉代码或调用 AI 失败，原因就在 `analysis_error` 里（认证/分支不存在/网络/磁盘/出网许可未开启） |
| 日志事件 `analysis_state=failed`，`analysis_error` 以「拉取代码失败：repo: git …」开头 | 平台拉代码失败：凭据过期、分支写错、DNS/网络不通、磁盘满，或 `code_repo.allow_outbound=false` | 按 `analysis_error` 里的中文结论处理（平台已把 git 的英文 stderr 翻译成"该怎么办"；URL 内嵌 token 会被脱敏成 `***`）；缓存目录满了就清 `code_repo.cache_dir`（默认 `./data/repos`）下的对应服务子目录，下次分析会自动重新 clone；修完点「重新分析」。详见 `COLLECTOR.md` 6.6 与 `OPERATIONS.md` 5.10 |
| 趋势图是一条直线 | 指标本身波动极小（如内存使用率 1%~3%） | Y 轴已改为按数据自适应；仍不动说明确实没变化 |
| 点某指标报 `Cannot read properties of undefined (reading 'series')` | 修复前 NaN 会破坏 JSON 编码（见 `POSTMORTEM.md` INC-004） | 升级后端 + 前端产物即可；该指标此后显示「暂无采样数据」 |

---

## 5. 回滚

```bash
# 平台侧：删掉集成（同时移除抓取目标与 Exporter 容器）
#   「集成中心 → 该行 → 删除」
# 日志集成：删除集成**不会**卸载目标机上的 Filebeat，需要就手工收尾——
#   systemctl disable --now filebeat && rm -f /etc/filebeat/filebeat.yml   （package 模式）
#   docker rm -f mwops-filebeat                                            （docker 模式）
cd <nightjar> && docker compose down            # 平台下线（保留数据卷）

# 被管项目侧：本来就什么都没改，停掉业务即可
cd <被管项目> && ./start.sh stop
```

> 平台的自动接入只体现在"平台容器多了一张网卡"上；`docker compose down` 后即消失，
> 被管项目的网络、容器配置、compose 文件都不存在需要还原的改动。
>
> 被管项目的 Prometheus/Grafana 服务与相关资产已从仓库移除（旧版自带的 Prometheus/Grafana 与 Exporter 资产目录）。
> 需要历史版本请用 git 回退到移除前的提交。

---

## 6. 相关文档

| 文档 | 内容 |
|---|---|
| [`INTEGRATION.md`](INTEGRATION.md) | 集成中心：字段对照、自动化矩阵、日志集成原理、安全边界 |
| [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md) | 日志集成权威说明：Kafka 拓扑、Filebeat 幂等部署、自检与配置项 |
| [`API.md`](API.md) | 接口清单（集成、日志集成、服务发现、接入自检） |
| [`COLLECTOR.md`](COLLECTOR.md) | 接入契约与标签约定（自建 Prometheus/Exporter 场景） |
| [`OPERATIONS.md`](OPERATIONS.md) | 平台运维（备份、升级、审计校验） |
| [`POSTMORTEM.md`](POSTMORTEM.md) | 交付期真实故障记录（含指标 NaN、白屏、建表冲突） |
