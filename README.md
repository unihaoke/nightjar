# 中间件智能问题解决平台

> 轻量级、AI 驱动的中间件问题诊断与解决平台。覆盖 Redis / Kafka / MySQL / PostgreSQL / Elasticsearch / Nginx 的统一纳管、监控、告警治理、AI 诊断与分级执行，向上扩展到应用层日志告警与 AI 代码分析。

本仓库是《中间件智能问题解决平台 · 设计文档 v1.0》的可执行实现：**后端 Go（Gin + GORM）+ 前端 Vue 3（Element Plus，移动端适配）**，模块化单体，可单机一键部署。

---

## 一、交付范围

| 里程碑 | 内容 | 本仓库状态 |
|--------|------|------------|
| M1 基础闭环 | 纳管 + 监控 + 告警规则/通知 + RBAC + 审计 | ✅ 完整实现 |
| M2 AI 诊断 | AI 诊断中心 + 知识库 + 六道工程护栏 + 成本治理 | ✅ 完整实现 |
| M3 代码分析 | 日志告警 + AI 代码分析（第三方 + 本地兜底）+ 高危执行审批闭环 | ✅ 实现（执行器为预演实现，见下文「已知边界」） |

**一期核心能力（与设计文档 4.1 能力矩阵一致）**：Redis / MySQL / PostgreSQL / Kafka / Elasticsearch 支持纳管、监控、阈值告警与 AI 诊断；Nginx 支持纳管、监控与告警（不做 AI 诊断）；RabbitMQ 本版本仅纳管。

**集成中心（M3 增强）**：在页面上选组件、填地址与账号即可完成「Exporter 暴露 → Prometheus 抓取 → 实例纳管 → 推荐告警规则」，
对齐云厂商 Prometheus 控制台的「数据采集 → 集成中心」。抓取目标走 `file_sd`，新增集成无需重启 Prometheus；
可选挂载 `docker.sock` 由平台一键拉起 Exporter 容器。详见 [`docs/INTEGRATION.md`](docs/INTEGRATION.md)。

---

## 二、快速开始

### 2.1 一键部署（推荐）

```bash
cp .env.example .env
# 必改：JWT_SECRET（≥32 位随机）、ADMIN_PASSWORD、DB_PASSWORD、REDIS_PASSWORD
docker compose up -d --build
```

启动后访问 `http://<主机>:8000`，使用 `.env` 中的管理员账号登录（默认 `admin`），**登录后请立即修改密码**。

包含组件：PostgreSQL 15 + pgvector、Redis 7、Prometheus、后端（Go）、Nginx + 前端静态资源。

> 表结构由后端 `GORM AutoMigrate` 在启动时创建（`database.auto_migrate=true`）；`deploy/postgres/init/` 下的初始化脚本**只创建扩展与数据库参数，不建表**。请勿手工先建表：PostgreSQL 会把内联 `UNIQUE` 命名为 `users_username_key`，而 GORM 迁移列唯一性时期望 `uni_users_username`，`DropConstraint` 会报 `SQLSTATE 42704` 导致启动失败。需要手工建库的 DBA 场景请使用 `docs/SCHEMA.sql`（约束名已按 GORM 策略显式命名），并同时把 `auto_migrate` 置为 `false`。修复办法见 [`docs/OPERATIONS.md`](docs/OPERATIONS.md) 第 5.6 节。

### 2.2 本地开发

前置：Go 1.23+、Node 20+、PostgreSQL 15（Redis / Prometheus 可缺省）。

```bash
# 1) 准备数据库
createdb middleware_ops

# 2) 后端（默认读取 configs/config.yaml，未配置 Redis/Prometheus 时自动降级）
cd middleware-ops
go mod tidy
go run ./cmd/server -config configs/config.yaml     # 监听 :8080

# 3) 前端（代理 /api 到 127.0.0.1:8080）
cd ../middleware-ops-web
npm install
npm run dev                                          # 监听 :5173
```

### 2.3 零外部依赖的离线模式

平台刻意设计了「缺省即可运行」的降级策略，便于联调与演示：

| 外部依赖 | 缺省行为 | 如何接入真实组件 |
|----------|----------|------------------|
| Redis | `redis.addr` 为空 → 进程内缓存 + 内存队列 | 填写 `redis.addr`（支持密码/DB） |
| Prometheus | `prometheus.base_url` 为空 → 内置**确定性**指标模拟器 | 填写 `prometheus.base_url` |
| 第三方 LLM | 未启用 → 规则引擎生成结构化半自动结论 | 配置 `ai_engine.third_party.*` |
| 本地 LLM | `kind: mock` → 进程内确定性引擎 | `kind: ollama` + `base_url` |
| pgvector | 未启用 → 向量以文本存储，检索走应用层余弦相似度 | 以 `-tags pgvector` 构建并按需 `CREATE EXTENSION vector` |

> 指标模拟器是**确定性**的（同一实例 + 同一指标 + 同一时间桶 → 同一数值），因此趋势图、规则评估与 AI 上下文三者始终自洽，可完整跑通告警与诊断链路。

---

## 三、仓库结构

```text
.
├── middleware-ops/                 # 后端（Go，模块化单体）
│   ├── cmd/server/                 # 入口：配置→日志→DB→缓存→引擎→服务→路由→调度
│   ├── cmd/agent/                  # 轻量日志采集 Agent（零依赖，可交叉编译投放）
│   ├── configs/config.yaml         # 默认配置（含全部注释说明）
│   └── internal/
│       ├── config/                 # viper 配置 + 环境变量覆盖 + 启动期校验
│       ├── model/                  # 实体（含 feedback/hash_prev/hash_self 等 v0.2 字段）
│       ├── db/                     # GORM 连接、迁移、pgvector 构建标签适配
│       ├── repository/             # 数据访问层（审计表仅追加，无 Update/Delete）
│       ├── engine/                 # AI 引擎抽象 + 降级链
│       │   ├── engine.go           # 引擎接口（Chat/ChatStream/Embed/Status）
│       │   ├── factory.go          # 策略装配（third_party/self_hosted/hybrid）
│       │   ├── http_provider.go    # OpenAI 兼容协议（含超时/熔断）
│       │   ├── rule_engine.go      # 规则引擎（降级链末端，结构化输出）
│       │   ├── hybrid.go           # 降级链实现
│       │   └── guardrail/          # 六道护栏
│       │       ├── budget.go       # ① 上下文预算 + 时序降采样摘要
│       │       ├── loop_guard.go   # ② 防死循环（步数/指纹白名单/决策卡）
│       │       ├── timeout.go      # ③ 超时熔断 + 并发额度 + 指数退避
│       │       ├── permscope.go    # ④ 权限隔离 + 只读工具集 + SQL 规则校验
│       │       ├── quality.go      # ⑤ 质量护栏（结构化/证据/推测标注/评测集）
│       │       └── cost.go         # ⑥ 成本治理（确定性缓存/配额/熔断）
│       ├── monitor/                # Prometheus 查询封装（不含自研采集器）+ 模拟器
│       ├── integration/            # 集成中心：组件模板 + 采集配置渲染（纯函数，可单测）
│       ├── docker/                 # Docker Engine API 最小客户端（一键拉起 Exporter）
│       ├── pkg/cache/              # 缓存与任务队列抽象（Redis / 内存双实现）
│       ├── service/                # 业务服务（域：resource/ai/control）
│       │   ├── ai/                 # 上下文组装与固定 Prompt 模板
│       │   ├── diagnose.go         # AI 诊断编排（六道护栏落点）
│       │   ├── alert.go            # 告警收敛（窗口去重/冷却/语义聚类）
│       │   ├── fix.go / approval.go# 操作分级、审批链路、结果回填
│       │   ├── audit.go            # 审计哈希链与每日快照
│       │   └── codeanalysis.go     # 三点式代码分析 + 出网合规
│       ├── middleware/             # Gin 中间件（追踪/恢复/限流/认证/数据权限）
│       ├── handler/ · router/      # 接口层（权限点与操作级别在路由显式声明）
│       └── scheduler/…             # 定时任务（健康巡检/规则评估/聚类/快照/审批超时）
├── middleware-ops-web/             # 前端（Vue 3 + Vite + TS）
│   ├── src/api/                    # 接口封装 + 统一响应处理 + SSE 客户端
│   ├── src/components/             # StatCard / MetricChart(ECharts) / DiagnosisReport / LevelTag
│   ├── src/layouts/                # AppShell（桌面侧栏 ↔ 移动抽屉）
│   ├── src/views/                  # 16 个页面（大盘/纳管/监控/AI/告警/知识库/…）
│   └── src/styles/                 # 设计令牌 + 全局基础样式（深浅双主题）
├── deploy/                         # Postgres 初始化（仅扩展/参数）、Prometheus 抓取与告警规则
│   ├── postgres/init/              # 只建扩展与数据库参数，不建表
│   ├── prometheus/                 # prometheus.yml / prometheus.with-exporters.yml / rules
│   ├── exporters/                  # Exporter 凭据模板（my.cnf）
│   ├── jd-exporters/               # 「jd 面试演练系统」接入用的 Exporter override 与抓取配置
│   └── compose.middleware-exporters.yml  # override：一键起 6 个官方 Exporter
├── docker-compose.yml              # 一键部署编排
├── scripts/smoke-test.ps1          # 端到端冒烟验证（含权限越权与护栏用例）
├── scripts/setup-jd-link.ps1       # 一键接入/修复：只维护 .env，其余（口令派生、网络、启动、体检）全自动
├── scripts/doctor-jd-link.ps1      # 跨栈网络体检（对照期望拓扑逐条判定并给修复命令）
└── Makefile                        # 常用开发/部署命令
```

---

## 四、核心设计落点

### 4.1 AI 能力六道工程护栏（设计文档第五章）

| 护栏 | 实现位置 | 关键行为 |
|------|----------|----------|
| ① 上下文预算 | `engine/guardrail/budget.go` | 输入 8K / 输出 2K 可配；超预算**告知被截断的维度**；时序指标降采样为「均值/P95/斜率/拐点/异常片段」 |
| ② 防死循环 | `loop_guard.go` | 步数上限、同工具同参数指纹重复 2 次即终止、工具白名单、每步决策卡；**工具失败不自动重试** |
| ③ 超时与降级 | `timeout.go` | 工具 10s / 任务 120s；超时放弃数据源并标注缺失维度；第三方→本地→规则引擎三级降级 + 熔断 |
| ④ 权限隔离 | `permscope.go` | AI 仅挂载**只读工具集**（类型层面无写方法）；数据权限在仓储查询上强制生效；AI 生成 SQL 强制只读 + 强制 LIMIT + 禁多语句/注释 + 表白名单 |
| ⑤ 质量护栏 | `quality.go` | 强制结构化输出（根因/证据/置信度/建议/影响/待确认）；无硬证据标注「推测」；24 条典型故障评测集 |
| ⑥ 成本治理 | `cost.go` | 同实例 + 同问题签名 24h 确定性缓存；按用户/平台日预算；并发 ≤4；异常突增自动熔断 |

### 4.2 安全设计（第六章）

- **RBAC**：admin / ops / dev / readonly 四内置角色 + 自定义角色；权限点在路由层显式声明，服务端强制校验。
- **数据权限**：按环境（dev/staging/prod）与分组隔离，直接落到仓储查询条件，**不依赖 Prompt 约束**。
- **操作分级**：L0 只读 / L1 低危 / L2 高危；L2 一律走审批（生产环境强制，30 分钟超时自动拒绝），且**申请人与审批人不得为同一人**。
- **审计可验证**：`audit_logs` 从 API 到仓储层均无更新/删除方法；哈希链 `hash_self = SHA256(hash_prev | 规范化内容)`，每日快照落盘并校验，断链可定位。
- **凭据管理**：连接密码 AES-256-GCM 加密存储，主密钥来自环境变量或 0600 权限密钥文件（首次启动自动生成），接口永不回传密文。
- **出网合规**：默认**禁止**任何第三方分析；需按服务显式开启白名单，堆栈/代码强制脱敏（IP/手机号/请求 ID/路径/邮箱/凭据）且 ≤200 行/文件。

### 4.3 事件驱动闭环

```text
埋点/阈值 → 去重收敛（指纹 + 窗口 + 冷却） → AI 按需介入 → 结构化诊断 → 多渠道通知 → 知识草稿沉淀 → 人工确认转正
```

AI 不做苦力活：日志 tail、指标采集、规则评估全部由采集管线与调度器完成（`scheduler` 承载健康巡检、规则评估、语义聚类、审计快照、审批超时五类任务）。

---

## 五、接口与认证

- 统一响应：`{code, message, data}`；分页 `page` / `page_size`（默认 20，上限 100）。
- 错误码分段：400x 参数、401x 认证、403x 权限、404x 不存在、500x 系统。
- 认证：`Authorization: Bearer <JWT>`，同时写入 `SameSite` Cookie；每次请求从数据库重建权限，角色变更即时生效。
- 流式：`POST /api/ai/diagnose` 走 SSE，事件类型 `meta` / `data` / `done` / `error`（前端用 fetch + ReadableStream 以便携带认证头）。
- 上报 Hook：`POST /api/hooks/logs`、`POST /api/hooks/alerts`，使用 `X-Hook-Token` 与服务令牌，与用户 JWT 分离。

完整接口清单见 [`docs/API.md`](docs/API.md)，接入与运维说明见 [`docs/OPERATIONS.md`](docs/OPERATIONS.md)，
**「把某个具体项目接进来」的端到端操作指南见 [`docs/GUIDE-JD-ONBOARD.md`](docs/GUIDE-JD-ONBOARD.md)（以 jd 为例）**，
**「集成中心」的字段对照与落地方式见 [`docs/INTEGRATION.md`](docs/INTEGRATION.md)**，
**「如何把其他项目的中间件接进来」请看 [`docs/COLLECTOR.md`](docs/COLLECTOR.md)**；
针对具体项目的接入范例见 **[`docs/COLLECTOR-JD.md`](docs/COLLECTOR-JD.md)**（面试演练系统 jd：Spring Boot + MySQL + Redis + 自带 Prometheus）。

---

## 六、验证

```bash
# 后端：格式 / 静态检查 / 单元测试
cd middleware-ops && gofmt -l . && go vet ./... && go test ./...

# 前端：类型检查 + 构建
cd middleware-ops-web && npm run build
# 前端运行时冒烟（无头浏览器加载产物，捕获白屏/TDZ 这类只在运行时暴露的问题）
cd middleware-ops-web && npm run smoke

# 端到端冒烟（需后端已在 8080 运行；PS 7 用 pwsh，Windows 自带 PS 5.1 用 powershell）
pwsh -File scripts/smoke-test.ps1
```

`scripts/smoke-test.ps1` 覆盖：健康检查、登录与会话、未认证访问拦截、实例纳管与连接测试、指标采集、
AI 同步/流式诊断与结构化输出校验、确定性缓存命中、告警规则评估与确认、日志 Hook 与错误指纹归并、
代码分析出网合规、修复预览 L2 判定、SQL 只读校验（含拒绝写操作）、审计哈希链校验、大盘与能力矩阵。

单元测试重点覆盖**越权用例**（RBAC 权限点/级别、数据权限过滤、只读工具集强制）、**告警去重指纹**、
**六道护栏**（预算截断、防死循环、SQL 校验、质量推测标注、成本缓存与配额）、**出网脱敏**
以及**数据库 schema 不变量**（唯一约束命名必须与 GORM 命名策略一致、模型列名不得命中 PostgreSQL
保留关键字，防止 AutoMigrate 建表/迁移在部署时失败回归）。

---

## 七、已知边界（避免误解）

1. **修复执行器为预演实现**：`service/dryRunExecutor` 只返回影响预览与参数校验，**不对被管中间件产生副作用**；接入真实执行需实现 `service.Executor` 接口并注入，L2 动作在审批通过后按预演方案人工执行并回填结果。
2. **一期不做自研代码索引**：代码分析采用「第三方 API + 本地简单检索（堆栈定位文件行 → 上下文切片）」，AST/知识图谱/向量 Rerank 为二期项。
3. **因果收敛不做**：告警收敛仅实现规则级（实时）与语义聚类（离线、仅合并展示），依赖服务拓扑的因果收敛列为二期。
4. **通知渠道需外部配置**：飞书/企微/钉钉/邮件在未配置 webhook 时仅记录通知日志，不影响主链路。
5. **默认构建未启用 pgvector 原生类型**：向量以文本存储并在应用层做余弦检索，规模 ≤200 实例可接受；需要 ANN 索引时以 `-tags pgvector` 构建。
6. **前端不做手工分包**：`vite.config.ts` 刻意不使用 `manualChunks`。element-plus 与 dayjs 互相引用，强行分包会形成 chunk 循环依赖并触发 ES module TDZ（表现为白屏），详见 [`docs/POSTMORTEM.md`](docs/POSTMORTEM.md) INC-003。

---

## 八、故障记录

交付过程中在部署阶段真实暴露的 3 个问题（两套建表来源、保留字列名、前端白屏）的现象、根因、
修复与防复发措施，记录在 [`docs/POSTMORTEM.md`](docs/POSTMORTEM.md)，并各配有回归测试或冒烟脚本。

---

## 九、许可与致谢

内部技术方案实现。指标采集依赖 Prometheus 生态与各中间件官方 Exporter（本仓库不包含采集器）。
