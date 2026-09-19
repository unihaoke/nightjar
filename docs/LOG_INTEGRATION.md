# 日志集成（Filebeat → 平台 Kafka）

> 本文是「日志集成」的实现方案与运维契约。它取代了旧的「日志接入」——
> 旧方案由平台反查被管容器的 docker 配置、把同一个日志卷挂进平台自己的采集容器（`mwops-agent`）。
> 新方案的边界更清晰：**平台提供 Kafka 与消费链路，采集由目标服务器上的 Filebeat 完成**。

---

## 一、为什么重构

| | 旧方案：日志接入 | 新方案：日志集成 |
|---|---|---|
| 采集对象 | 只能是被管容器的 docker 卷 | 任意服务器（含物理机、K8s 节点、非容器化应用） |
| 平台需要 | 挂载 docker.sock、反查挂载点、起采集容器 | 只需要一个 Kafka 端口开放 |
| 目标机需要 | Docker，且日志必须落在被挂载的卷里 | Filebeat（平台用 Ansible 装） |
| 平台故障域 | 采集容器与平台同生共死，日志卷不可读即采集失败 | Filebeat 本地缓冲，平台重启/升级不影响采集 |
| 采集能力 | 只能按 glob 抓文本、单一 level 过滤 | Filebeat 全套：多行合并、字段解析、背压、断点续传 |

结论：旧方案把「采集」和「平台」耦合在一起，且只能看见容器日志。新方案让平台回到
「集成中心」的定位——**平台负责下发、接收、分析，采集在被管侧自洽运行**。

---

## 二、总体拓扑

```
              被管服务器 B                          平台（nightjar）
   ┌───────────────────────────────┐        ┌──────────────────────────────┐
   │ 应用日志 /var/log/app/*.log   │        │  kafka（KRaft 单节点）        │
   │        │                      │        │   EXTERNAL :9092 ← 被管机接入 │
   │        ▼                      │        │   INTERNAL :29092 ← 平台内部  │
   │  filebeat（Ansible 幂等部署） │───────▶│   topic: mwops-logs          │
   │   · inputs: filestream        │ 9092   │        │                     │
   │   · output.kafka（JSON 编码） │        │        ▼                     │
   └───────────────────────────────┘        │  backend 消费者组             │
                                            │   logpipe → LogAlertService  │
                                            │   （指纹/窗口/告警/诊断复用） │
                                            └──────────────────────────────┘
```

三条数据通路，按可靠性排序：

1. **Filebeat → Kafka（主路径）**：本方案，支持任何服务器，带本地缓冲与断点续传；
2. **应用直推 `POST /api/hooks/logs`（保留）**：零侵入，适合不能装 Filebeat 的场景；
3. ~~平台起采集容器~~（已移除）：见上表。

---

## 三、Kafka 部署（平台自带）

`docker-compose.yml` 新增 `kafka` 服务，**KRaft 单节点、无 ZooKeeper**（镜像 `apache/kafka:3.8.0`）：

| 监听器 | 地址 | 用途 |
|---|---|---|
| `CONTROLLER` | `kafka:9093` | KRaft 仲裁（仅容器网络） |
| `INTERNAL` | `kafka:29092` | 平台后端消费（容器网络） |
| `EXTERNAL` | `${KAFKA_ADVERTISED_HOST}:${KAFKA_PORT}` | **被管服务器上的 Filebeat 接入** |

**最容易配错、也最容易被忽略的一处**：`KAFKA_ADVERTISED_LISTENERS` 里的 EXTERNAL 地址必须是
**被管机能访问到的平台地址**（宿主 IP 或域名），不能写 `localhost`、也不能写容器名。
写错的典型现象是：Filebeat 能连上 9092 握手成功，但拿到 broker 元数据后**立刻断开并报
`dial tcp 127.0.0.1:9092: connect: connection refused`** —— 因为它被引导去了自己那台机器的 127.0.0.1。

因此：

- `.env` 里显式提供 `KAFKA_ADVERTISED_HOST`（默认 `127.0.0.1`，仅单机自测可用）；
- `scripts/onboard.sh` 会尝试自动探测宿主 IP 并写入；
- **平台会主动拒绝"必然失败"的组合**（INC-028）：日志集成的目标是**远程**机器、而 Kafka 对外地址
  还是回环（`127.0.0.1`/`localhost`/`::1`）或容器内服务名（`kafka`）时，**预览、部署、重新应用
  三条会渲染 `filebeat.yml` 的路径**都会在写出产物之前直接失败（保存表单本身不渲染，
  因此不会因为平台配置不全而存不下集成点），错误信息给出改哪个配置项与重建哪两个容器；
  自检第 2 段也会把这一环判红而不是笼统地"不通"。
  本机目标（集成就填平台自己）用回环地址是**正确**的：EXTERNAL 端口已发布到宿主。
- 日志集成的**自检**会把"平台侧能否连上 Kafka"与"被管机接入地址是否可用"分开判定（见 §六），
  而"目标机视角能否连上"由部署时的 playbook 在目标机上探测（那里才有 SSH，见 §六末段）。

Topic：`mwops-logs`（可配），`KAFKA_AUTO_CREATE_TOPICS_ENABLE=true`，平台后端启动时也会
显式确保 topic 存在（幂等），避免"第一条日志因 topic 不存在而被丢弃"。

数据保留：`KAFKA_LOG_RETENTION_HOURS`（默认 72 小时）——日志是**事件源**而非归档，
长期留存由平台的日志事件表/告警记录承担，Kafka 只做削峰与解耦。

---

## 四、集成中心：新增 `log` 类型

集成类型分成两类（模板新增 `category` 字段）：

- `monitor`：redis / mysql / pg / kafka / es / nginx / node —— Exporter + Prometheus + 告警；
- `log`：**log（日志 / Filebeat）** —— Filebeat + 平台 Kafka，**不涉及 Exporter 与 Prometheus**。

日志集成的表单字段：

| 字段 | 说明 |
|---|---|
| 名称 | 集成名，同时作为日志事件里的服务标识与 Filebeat 配置目录名 |
| 目标服务器 | 被管机地址（本机填 `127.0.0.1`，远程填 IP/域名） |
| 部署位置 | 本机 / 远程服务器（远程走 Ansible + SSH，与 Exporter 集成同一套凭据机制） |
| 日志路径 | 多个 glob，如 `/var/log/app/*.log`、`/data/logs/**/*.log` |
| 服务名 / 环境 | 写入事件字段（`service` / `environment`），用于日志页归集与筛选 |
| 最低级别 | ERROR / WARN / INFO（写入 Filebeat 处理器，降低噪音） |
| 多行合并 | 是否把 Java/Python 堆栈合并成一条事件（默认开，pattern 按语言给默认值） |
| 安装方式 | `package`（**默认**，deb/rpm + systemd）/ `auto`（已装则复用 → docker → 包安装）/ `docker`（官方镜像容器） |
| 覆盖 Filebeat | **默认关闭**（幂等：已装就复用、镜像已在本地就不重新拉取；缺失才安装）。打开则忽略"已存在"，重新下载 deb/rpm 并强制重装，或重新 `docker pull` 镜像并重建容器 |
| Filebeat 版本 | 默认 `8.16.0` |
| Kafka Topic | 默认取平台配置，只读展示，便于对齐排障 |

### 「覆盖 Filebeat」开关：管的是**程序本体**，不是配置

这个开关只影响"要不要动目标机上那个 Filebeat"：

| 安装方式 | 关闭（默认） | 打开（覆盖） |
|---|---|---|
| `package` | 已安装 → 不重装；缺失 → 下载 deb/rpm 安装 | 强制重新下载（`get_url force`）→ `apt-get --reinstall` / `dnf reinstall`（兜底 `rpm -Uvh --replacepkgs`） |
| `docker` | 本地已有该 tag 的镜像 → 不 `pull`；容器按既有逻辑重建 | 无条件 `docker pull`（重新获取镜像）→ 重建容器 |
| `auto` | 已安装 → 复用；否则 docker → package | 跳过"复用"，按 **有 docker → docker，否则 package** 重装 |

**故意不覆盖配置文件**：`filebeat.yml` 是平台自己的产物，内容由平台配置推导（Kafka 对外地址、
日志路径、级别）。若"不勾选就不覆盖配置"，就会出现"平台上改了地址、界面一切正常、目标机还在用旧配置"
——正是 INC-016 / INC-028 反复出现的那类"看起来成功了但链路是断的"。
因此配置**始终按内容同步**（Ansible `copy` 的 checksum 语义：内容没变连重启都不会发生），
只有"程序本体"（安装包 / 镜像）由这个开关决定是否重新拉取。**改日志路径或 Kafka 地址不需要打开它。**

判定顺序里还有一条实现时修掉的缺陷（INC-029）：显式的 `package` / `docker` **优先于"复用"**。
早期版本一律"已安装 → reuse"，于是"显式选 docker + 目标机恰好也装了包版 Filebeat"会算出 reuse，
配置变化时 handler 去 `systemctl restart filebeat` 而不是重建容器，容器静默地用着旧配置。

### 幂等部署规则（用户要求："已存在则不需要部署"）

**默认是 `package`（deb/rpm + systemd），不是 `auto`**——这是刻意的取舍：
`auto` 在"目标机有 Docker"时会走容器模式，而容器模式的活动部件最多，
真实环境里连着暴露了四轮问题（INC-024 / INC-026）：容器的运行用户与宿主数据目录属主、
配置必须挂到镜像约定的 `/usr/share/filebeat/filebeat.yml`、Docker 会把缺失的绑定源创建成目录。
`package` 由系统包管理器安装，配置路径与数据目录都由 deb/rpm 按正确属主落好，最不容易出错。
需要容器化采集（例如不准在宿主装包）时，显式改成 `auto`/`docker` 即可。

`auto` 模式的判定顺序（全部用 Ansible 完成，结果写回集成备注）：

1. 目标机已有 `filebeat` 且 `systemctl is-active filebeat` → **复用**，只下发/校验配置；
2. 目标机有 `docker` → 用官方 `docker.elastic.co/beats/filebeat:8.16.0` 容器
   （`--user=root`、配置挂到容器内 `/usr/share/filebeat/filebeat.yml`、数据目录用平台专属的
   `/var/lib/mwops-filebeat`（0775 / uid 1000）——与系统 filebeat 的 `/var/lib/filebeat` 刻意分开）；
3. 都没有 → 用官方仓库安装 deb/rpm 并启用 systemd 单元。

显式选了 `package` / `docker` 时**不走上面这套**：使用者选定的方式优先，
"复用"只在 `auto` 模式下才可能出现（见上面 INC-029）。
勾选「覆盖 Filebeat」时第 1 步被跳过：需要安装 → 按"有 docker 用容器、否则用包"重装。

配置的"是否变更"用**渲染后的 filebeat.yml 内容哈希**判定（Ansible `copy` 的 `checksum` 语义）：
内容一致 → 不重启（避免每次重放都抖动采集）；内容变化 → `systemctl restart filebeat` 或容器重建。
检测与安装都只在"需要"时执行，**重复点击集成不会重复安装**。

容器模式还有三道用现场故障换来的自愈与闸门（顺序固定，改动前先看 INC-024 / INC-026）：
`stat 配置路径` → 非普通文件则 `absent`（清掉历史 docker 挂载留下的目录）→ **`docker rm -f` 旧容器**
（打断"Docker 每次重启重建绑定源目录"的循环）→ 下发配置 → **重新 `stat` 之后**用两条闸门断言
（存在 / 是普通文件；两条的失败提示分别指向"目录权限与磁盘"和"目录需删除"）→ `docker run`。
最后那次 stat 必须重新探测：Ansible 的注册变量是**快照**，拿写配置之前的状态做断言必然误报。

### 目标机出网问题

`package` 模式默认走官方仓库（`artifacts.elastic.co`），要求目标机能出网。内网机器常见不能出网，
此时有两种做法（文档给出命令）：

1. 平台侧开启包分发：把 filebeat 的 deb/rpm 放到 `deploy/filebeat/packages/`，平台提供
   `GET /api/hooks/filebeat/pkg`（受 hook token 保护），Ansible 用 `get_url` 从平台拉取后本地安装
   —— 目标机无需出网，只需要能访问平台的 8000 端口；
2. 运维自行把包放到目标机（`filebeat.install_source=preinstalled`），平台只下发配置。

---

## 五、后端：Kafka 消费链路与后处理编排

新增 `internal/logpipe`（不与 HTTP handler 耦合，便于单测）：

```
kafka-go Reader（consumer group=mwops-log-ingest）
   → 解析 Filebeat JSON 事件（@timestamp/message/fields.*/log.file.path/host.name）
   → 映射为 service.LogReport{ServerName, Service, Environment, Level, Message, Stacktrace, Timestamp}
   → LogAlertService.Ingest：按规则做窗口去重 + 冷却抑制 → 落库 log_alert_events
   → 成功后 CommitMessages
```

- **不丢**：只有 Ingest 成功才提交位点；Kafka 不可用/Ingest 报错时重试并退避；
- **不卡死**：解析失败的消息计入 `dropped` 指标并**照常提交**（否则一条脏消息会堵住整个分区），
  同时把它写入平台日志（含 offset 标识）；
- 平台启动时若 `kafka.brokers` 为空 → 消费者不启动，日志页面显示「未启用 Kafka 接入」；
- Kafka 连接异常不影响平台其余功能（与监控数据源同一原则：**降级只减少展示，不伪造数据**）。

### 5.1 规则：去重窗口与冷却期（页面可配）

`log_alert_rules` 决定"多久打扰人一次"，没有命中任何规则时用 `log_alert.default_*` 兜底：

| 概念 | 语义 | 默认 |
|---|---|---|
| 去重窗口 `dedup_window` | 窗口内**同指纹合并为一条事件**（计数累加），不新增记录 | 5 分钟 |
| 冷却期 `cooldown` | 冷却内**不重复通知、不重复触发 AI**，但**事件照常记录**（列表里标「冷却中」） | 10 分钟 |
| 通知渠道 `notify_channels` | 留空用平台「通知渠道」里已启用的渠道 | 空 |
| AI 开关 `ai_enabled` | 关掉就不做代码分析（省额度；也可只对少量服务开） | 开 |
| 优先级 `priority` | 数字**小**的优先；多条命中取第一条，便于"特例压过通用" | 100 |

匹配输入是**服务名 + 日志消息原文 + 级别**（`signature_pattern` 支持普通文本子串与 `/正则/`；
匹配的是上报的 `message`，与屏蔽项同一套写法，不是错误指纹）。
冷却过后再次合并时只**重新通知**、不会重复跑 AI（同一条事件已经有结论了）。

### 5.2 后处理：通知 + AI 代码分析（`LogAlertWorker`）

Ingest 只做"记录与判定"，**通知与 AI 分析放在定时任务里异步执行**（默认 15 秒扫一轮）：

- 为什么不同步做：AI 分析要拉代码、调 LLM，耗时几十秒；放在采集路径上会把 Kafka 消费拖慢
  （位点积压 → 整条链路延迟）；
- 为什么不丢：状态落在 `log_alert_events.analysis_state`（pending → running → done/failed/disabled），
  重启后由定时任务按队列继续，**不依赖内存里的 goroutine**；
- 为什么不重复：多副本部署时用「带条件的 UPDATE 抢占」（`pending→running`，影响行数=1 才算抢到）；
- 限流：每轮批量 `worker_batch`（默认 10） + 事件之间错开 + 单条 `analyze_timeout`（默认 2 分钟）；
- 页面可以直接看到"为什么没有结论"：`analysis_state=disabled/failed` 时 `analysis_error` 写明原因
  （规则关了 AI / 服务没配代码仓库 / 拉代码失败 / LLM 报错）。

### 5.3 代码仓库本地缓存（首次 clone，之后只做更新）

AI 要回答"这条日志对应哪一行代码"，就必须先有代码。`internal/repo` 负责这件事：

- **首次** `git clone`（按 `CodeRepo.branch` 指定分支；不指定则用远端默认分支）到
  `code_repo.cache_dir`（默认 `./data/repos`，落在 `backend-data` 卷里，容器重建不丢）；
- **之后**只做更新：分支非空时 `git fetch --prune` + `git checkout -B <branch> origin/<branch>`
  （等价于把缓存重置到远端——它是**缓存不是工作区**，手工改动不该被保留），
  分支为空时 `git pull --ff-only`；
- 同一服务并发分析只会跑一次 git（进程内互斥），并有**最小拉取间隔**
  （`code_repo.refresh_interval_seconds`，默认 300 秒）避免风暴期反复拉远端；
- 拉取结果写回 `code_repos.local_path` 与 `last_pull_at`（页面上看得到"什么时候拉的、拉到哪了"）；
- 错误翻译成可操作的中文（认证失败 / 分支不存在 / 域名解析失败 / 磁盘满），
  URL 里的凭据（userinfo、token 参数）在日志与错误里一律脱敏；
- **凭据与地址分开存放**：入库的 `repo_url` 不含任何凭据，令牌单独加密保存在
  `code_repos.credential_encrypted`，只在执行 git 的那一刻拼回 URL；克隆/更新后会把缓存里的
  `origin` 改写成干净地址，因此 `.git/config` 里没有令牌。页面与接口只回 `has_credential` 布尔量，
  表单里的令牌框**永不回显**（留空=不修改、`clear_credential`=清除）。旧数据里内嵌的凭据
  在第一次读取时自动迁移（见 OPERATIONS §5.10）。
- **轮换令牌立即生效**：缓存里的 `origin` 会对齐成配置里的地址，不必进容器删缓存目录。
- **重启不重复拉取**：最小间隔同时看进程内记忆与数据库 `last_pull_at`，
  容器重建后不会把所有仓库重新 fetch 一遍。
- **定位用的候选来自 git 索引**（`git ls-files`），并按堆栈的路径线索打分
  （依赖/产物/测试目录扣分，不跟随软链指向仓库外的文件）；结果里记录
  `repo_revision`（本次所用提交），便于事后复核。
- **合规上有两个开关，别混**：`code_repo.allow_outbound` 管"平台能否 git clone/pull"；
  `CodeRepo.allow_third_party` 管"能否把代码片段发给第三方 AI"（6.5 出网白名单）。
  合成一个开关的后果是：不想用第三方 AI 的团队会连内网仓库都拉不下来，整个功能形同虚设。
  注意后者是**硬拦截**：平台引擎若是第三方提供方，未勾选/不在白名单时**不会调用引擎**，
  该事件记为 `analysis_state=disabled` 并在原因里写明三种出路（见 OPERATIONS §5.10）。

### 5.4 字段映射（Filebeat JSON → 平台日志事件）

| Filebeat 字段 | 平台字段 | 说明 |
|---|---|---|
| `message` | Message | 单行/多行合并后的正文（首行进正文，其余作为堆栈） |
| `@timestamp` | Timestamp | Filebeat 打的采集时间 |
| `fields.service` | Service | 集成时写入（留空则回落集成名） |
| `fields.environment` | Environment | 集成时写入 |
| `fields.server` | ServerName / ServerIP | **目标机地址**（不是集成名）。平台按它反查/登记 `server_instances`，因此必须填地址 |
| `fields.integration` | —（仅排障用） | 集成名。平台暂不消费，但排查"这条日志来自哪个集成"时直接可读 |
| `log.file.path` | LogPath | 落到 `log_alert_events.log_path`，回答"哪个文件在报错" |
| `log.level`（若解析到） | Level | 解析不到时按内容关键字推断，最终兜底 ERROR |

---

## 六、自检（日志集成专用）

`POST /api/integrations/:id/selfcheck` 对 `log` 类型走另一条链路，逐环节给结论与动作。

**刻意不需要 SSH**：自检是只读的、随时可点，而"目标机上 Filebeat 到底起没起来"这件事，
最终一定会体现在**数据面**上（日志有没有进来）。与其隔着 SSH 猜，不如直接看数据——
判断依据全部是平台自己能看到的事实：

1. **平台侧 Kafka 可达**：用平台配置的 `brokers` 列 topic，确认 `mwops-logs` 存在；
2. **被管机接入地址（advertised）**：把 `KAFKA_ADVERTISED_HOST:KAFKA_PORT` 摆出来并从平台侧探一次，
   同时明确标注"被管机视角仍需在目标机执行 `nc -vz <host> <port>` 验证"——
   这一步专门用来抓 §三 里那个"advertised 地址配成 localhost"的经典问题。
   **这一环会区分"地址本身就不可能对"与"地址对但端口不通"**：目标是远程机器、而地址是回环或容器内服务名时
   直接判红并指出改哪个配置项（INC-028），不再笼统报"不通"——否则现场会去查一个并不存在的网络问题；
3. **数据面**：按 `server_instances.last_seen_at` 给出"最近一条日志是多久以前"，
   超过 30 分钟判为警告，并给出目标机上的三条自查动作（`systemctl status filebeat` /
   `filebeat test output` / 路径 glob 是否匹配）。

**目标机视角的连通性由部署时的 playbook 负责**（那里有 SSH、就在目标机上执行）：
`RenderFilebeatInstall` 会渲染一个"目标机 → 平台 Kafka:端口"的 TCP 探测任务
（`/dev/tcp`，无 bash 时回退 `nc -z`），结论写进 ansible 输出与集成备注。
这样部署那一刻的现场证据与页面上的长期结论各司其职，不会互相冒充。

---

## 七、对旧能力的处置

| 旧能力 | 处置 |
|---|---|
| `service/logcollect.go`（docker 卷反查 + 采集容器） | **删除** |
| `internal/integration/logs.go`（DiscoverLogSource 等） | **删除**（含测试） |
| `cmd/agent`（`mwops-agent` 二进制） | **删除**，并从 Dockerfile 移除构建与入口 |
| `POST /api/hooks/logs`（应用直推） | **保留**：零侵入兜底，字段与 Filebeat 路径统一映射 |
| 前端「日志接入」卡片 | 改为「日志集成」（新建集成时选择 Filebeat 类型） |

`docs/INTEGRATION.md` 与前端文案同步更新，`POSTMORTEM.md` 记录本次重构（含旧方案的失效场景）。

---

## 八、配置项速查

`configs/config.yaml` / 环境变量：

```yaml
kafka:
  enabled: true                 # MWOPS_KAFKA_ENABLED
  brokers: ["kafka:29092"]      # MWOPS_KAFKA_BROKERS（平台内部用 INTERNAL 监听器）
  log_topic: mwops-logs         # MWOPS_KAFKA_LOG_TOPIC
  group_id: mwops-log-ingest    # MWOPS_KAFKA_GROUP_ID
  client_id: mwops-backend      # MWOPS_KAFKA_CLIENT_ID
  # 被管服务器接入用的地址（写进 Filebeat 的 hosts，也是自检第 3 步的探测目标）
  external_host: <平台可达地址>  # MWOPS_KAFKA_EXTERNAL_HOST
  external_port: 9092           # MWOPS_KAFKA_EXTERNAL_PORT
  filebeat_version: 8.16.0      # MWOPS_KAFKA_FILEBEAT_VERSION
```

`.env` 侧对应 `KAFKA_ADVERTISED_HOST` / `KAFKA_PORT`，两者必须一致（compose 用它渲染
`KAFKA_ADVERTISED_LISTENERS`，后端用它渲染 Filebeat 的 `hosts` 并把地址交给自检）。

**集成表单参数**（`MWOPS_LOG_*`，存在 `middleware_instances.options` 里，逐集成独立）：

| 键 | 取值 | 说明 |
|---|---|---|
| `MWOPS_LOG_PATHS` | 多行 glob | 目标机上的日志路径（必填） |
| `MWOPS_LOG_SERVICE` / `MWOPS_LOG_ENVIRONMENT` | 字符串 | 写入事件的 `service` / `environment` |
| `MWOPS_LOG_LEVEL` | ERROR / WARN / INFO | 目标机侧过滤 |
| `MWOPS_LOG_MULTILINE` / `MWOPS_LOG_MULTILINE_PATTERN` | bool / 正则 | 多行堆栈合并 |
| `MWOPS_LOG_INSTALL_MODE` | `package`（默认）/ `auto` / `docker` | 安装方式 |
| `MWOPS_LOG_OVERWRITE` | `true` / `false`（默认） | 覆盖 Filebeat：重新拉取安装包/镜像并强制重装 |
| `MWOPS_LOG_FILEBEAT_VERSION` | 版本号 | 默认 `8.16.0`（≥ 7.15） |

> `MWOPS_LOG_OVERWRITE` 的"缺省即关闭"是刻意的：它是唯一会**重新下载并覆盖目标机上已有软件**的开关，
> 默认必须是"不动它"。因此对"没填/填 false"一律按关闭处理，只有显式的 `true`/`1`/`yes`/`on` 才算打开。

---

## 九、验证清单（交付时逐条走）

```bash
# 1. 组件起来且 topic 存在（29092 是**容器网络内**的 INTERNAL 监听器，只在容器里用）
docker compose up -d kafka
docker exec mwops-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:29092 --list

# 2. 被管机视角可达（在目标机上执行；这里用的是 **EXTERNAL** 端口 ${KAFKA_PORT}，默认 9092）
nc -vz <KAFKA_ADVERTISED_HOST> 9092

# 3. 端到端：目标机手工塞一条日志，平台日志页应在数秒内出现事件
echo '2024-01-01 ERROR demo: boom' >> /var/log/app/demo.log

# 4. 「覆盖 Filebeat」开关（可选）：仅在需要升级/修复目标机上已有的 Filebeat 时打开
#    未勾选时重放 playbook 不应出现下载/重装动作；勾选后应看到重新下载与 --reinstall/reinstall
#    产物里可直接核对：vars 段的 filebeat_overwrite 与各安装任务的 when 条件
```

> 端口别混：**29092 只在平台容器网络内**（后端消费用 `kafka:29092`），
> **9092 才是给被管机上的 Filebeat 用的**。两边看到的是同一个 Kafka，
> 但 advertised 地址不同——这正是 §三 那个经典问题的根源。

平台侧验证：集成中心 → 日志集成 → 自检（平台→Kafka / 被管机接入地址 / 日志是否已进入平台）；
日志监控页能看到该 server 的事件与指纹，并能看到「Kafka 采集链路」卡片上的消费计数。

> **未在本仓库环境中验证的部分**（如实说明）：本次交付只做了产物级验证——
> 渲染出的 `filebeat.yml` 与 playbook 通过 Go（yaml.v3）与 Python（pyyaml）**两种解析器**复核、
> 后端全量 `go build/vet/test` 通过、前端 `vue-tsc` 与 `vite build` 通过。
> INC-028 的地址校验（远程目标 + 回环/容器名 → 拒绝；本机目标 + 回环 → 放行）由
> `TestKafkaAddressUsableForTarget` / `TestLogInputOfRejectsMissingPieces` 覆盖，
> 但**"改成正确 advertised 地址后目标机是否真的能推上日志"仍未在真实链路上跑通**。
> 「覆盖 Filebeat」开关同理：产物里的分支、条件与决策语义由
> `TestRenderFilebeatInstallOverwriteIsDeclared` / `TestRenderFilebeatInstallImagePullFollowsOverwrite` /
> `TestRenderFilebeatInstallExplicitModeBeatsReuse` / `TestLogInputOfOverwriteOption` 覆盖，
> 并用独立的 Python/PyYAML 与一个极小的 Jinja 求值器复核（六种模式×覆盖组合、四种目标机状态），
> 但**`apt-get --reinstall` / `dnf reinstall` / `docker pull` 在真实目标机上的行为没有实跑过**
> （尤其 `dnf reinstall` 对本地 rpm 的可用性、以及内网机器重新下载是否会失败）。
> playbook **没有在真实目标机（真实 Debian/RHEL + Filebeat 8.16 + Kafka）上执行过**：
> `/dev/tcp` 探测、`nc -z` 回退、RPM 依赖解析、docker 挂载与 handler 重启都只有静态断言。
