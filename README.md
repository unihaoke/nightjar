# nightjar · 中间件智能问题解决平台

<p align="center">
  <a href="https://github.com/unihaoke/nightjar/actions/workflows/ci.yml"><img src="https://github.com/unihaoke/nightjar/actions/workflows/ci.yml/badge.svg?branch=master" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-MIT-yellow.svg" alt="License"></a>
  <a href="https://go.dev"><img src="https://img.shields.io/badge/Go-1.23-00ADD8?logo=go&logoColor=white" alt="Go"></a>
  <a href="https://vuejs.org"><img src="https://img.shields.io/badge/Vue-3-4FC08D?logo=vue.js&logoColor=white" alt="Vue"></a>
  <a href="https://www.typescriptlang.org"><img src="https://img.shields.io/badge/TypeScript-strict-3178C6?logo=typescript&logoColor=white" alt="TypeScript"></a>
  <a href="CONTRIBUTING.md"><img src="https://img.shields.io/badge/PRs-welcome-brightgreen.svg" alt="PRs Welcome"></a>
</p>

<p align="center">
  简体中文 · <a href="README.en.md">English</a>
</p>

> 轻量级、AI 驱动的中间件问题诊断与解决平台。覆盖 Redis / Kafka / MySQL / PostgreSQL / Elasticsearch / Nginx 的统一纳管、监控、告警治理、AI 诊断与分级执行，并向上扩展到应用层日志告警与 AI 代码分析。

本仓库是《[中间件智能问题解决平台 · 设计文档](docs/DESIGN.md)》的可执行实现：**后端 Go（Gin + GORM）+ 前端 Vue 3（Element Plus，移动端适配）**，模块化单体，可单机一键部署。

---

## 一、功能特性

| 里程碑 | 内容 | 状态 |
|--------|------|------|
| M1 基础闭环 | 纳管 + 监控 + 告警规则/通知 + RBAC + 审计 | 已完整实现 |
| M2 AI 诊断 | AI 诊断中心 + 知识库 + 六道工程护栏 + 成本治理 | 已完整实现 |
| M3 日志与代码分析 | 日志集成（Filebeat → 平台 Kafka）+ 日志告警规则 + 外部 AI 分析服务对接 + 高危执行审批闭环 | 已实现（执行器为预演实现，见「已知边界」） |

一期核心能力：Redis / MySQL / PostgreSQL / Kafka / Elasticsearch 支持纳管、监控、阈值告警与 AI 诊断；Nginx 支持纳管、监控与告警（不做 AI 诊断）；RabbitMQ 当前仅纳管。

**集成中心**：在页面上选组件、填地址与账号即可完成「Exporter 暴露 → Prometheus 抓取 → 实例纳管 → 推荐告警规则」，对齐云厂商控制台的「数据采集 → 集成中心」。抓取目标走 Prometheus **HTTP 服务发现（http_sd，`GET /api/sd/integrations`，30 秒刷新）**，新增集成无需重启 Prometheus；可选挂载 `docker.sock` 由平台一键拉起 Exporter 容器。详见 [docs/INTEGRATION.md](docs/INTEGRATION.md)。

**日志集成**：集成中心提供日志类型集成——平台用 **Ansible 在目标服务器幂等部署 Filebeat**（已安装则跳过；需要升级/修复时显式打开「覆盖 Filebeat」开关），Filebeat 把日志推到**平台自带 Kafka**（KRaft 单节点），后端按消费组 `mwops-log-ingest` 消费 topic `mwops-logs`，复用日志事件链路（错误指纹 / 通知 / AI 分析入口）。它不装 Exporter、不经过 Prometheus、不需要 `docker.sock`。详见 [docs/LOG_INTEGRATION.md](docs/LOG_INTEGRATION.md)。

**日志告警规则与 AI 代码分析**：日志事件的处理参数由**日志告警规则**（页面「日志告警 → 日志告警规则」，表 `log_alert_rules`）按「服务 + 错误指纹 + 级别」逐条配置——去重窗口、冷却期、通知渠道、AI 开关与优先级（数字小者优先）。平台**没有任何内置默认规则**：只有命中已启用规则，日志才会产生告警（未命中不入库、不通知、不分析）。另有屏蔽规则（`log_alert_exclusions`）按日志原文（子串或 `/正则/`）优先屏蔽框架噪音。

命中规则的事件由后处理 worker（[logalert_worker.go](middleware-ops/internal/service/logalert_worker.go)，定时扫描 `analysis_state=pending`，多副本用条件更新抢占）完成通知与 AI 分析：**平台本身不 clone、不缓存任何业务代码**，而是把**脱敏后的错误信息**提交给在「AI 设置」页配置的**外部 AI 分析服务**，分析完成后由服务端**回调**（`POST /api/ai/analysis/callback`，令牌鉴权、天然幂等）或平台轮询取回三点式结论（定位文件行 / 根因 / 应急处置 / 修复建议）。任务状态机为 `pending → running → awaiting → done / failed / disabled`。协议契约与平台侧验收清单见 [docs/AI_CODE_ANALYSIS_API.md](docs/AI_CODE_ANALYSIS_API.md)。

---

## 二、快速开始

### 2.1 一键部署（推荐）

```bash
cp .env.example .env
# 必改：JWT_SECRET（≥32 位随机）、ADMIN_PASSWORD、DB_PASSWORD、REDIS_PASSWORD
docker compose up -d --build
```

启动后访问 `http://<主机>:8000`，使用 `.env` 中的管理员账号登录（默认 `admin`），**登录后请立即修改密码**。

包含组件：PostgreSQL 15 + pgvector、Redis 7、Kafka（日志总线，默认启用）、Prometheus、Grafana（统一监控大盘）、后端（Go）、Nginx + 前端静态资源。

> 监控栈统一在本平台：被管项目不再需要自带 Prometheus / Grafana / Exporter——平台按集成中心配置创建只读监控账号、拉起 Exporter、自动接入对方网络并抓取；Grafana 数据源由 provisioning 自动注入，大盘按集成卡片给出的编号导入即可。

> 表结构由后端 GORM AutoMigrate 在启动时创建（`database.auto_migrate=true`）；`deploy/postgres/init/` 下的初始化脚本只创建扩展与数据库参数，不建表。请勿手工先建表（约束命名不一致会导致迁移失败）。需要手工建库的 DBA 场景请使用 [docs/SCHEMA.sql](docs/SCHEMA.sql)（约束名已按 GORM 策略显式命名），并同时把 `auto_migrate` 置为 `false`。

### 2.2 本地开发

前置：Go 1.23+、Node.js 20+（CI 使用 22）、PostgreSQL 15（Redis / Prometheus 可缺省）。

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
| Prometheus | `prometheus.base_url` 为空 → 内置确定性指标模拟器 | 填写 `prometheus.base_url` |
| 第三方 LLM | 未启用 → 规则引擎生成结构化半自动结论 | 配置 `ai_engine.third_party.*` |
| 本地 LLM | `kind: mock` → 进程内确定性引擎 | `kind: ollama` + `base_url` |
| pgvector | 未启用 → 向量以文本存储，检索走应用层余弦相似度 | 以 `-tags pgvector` 构建并按需 `CREATE EXTENSION vector` |

指标模拟器是确定性的（同一实例 + 同一指标 + 同一时间桶 → 同一数值），趋势图、规则评估与 AI 上下文始终自洽，可完整跑通告警与诊断链路。

---

## 三、文档

完整文档索引见 **[docs/README.md](docs/README.md)**。常用入口：

| 文档 | 内容 |
|---|---|
| [docs/DESIGN.md](docs/DESIGN.md) | 设计基线；第十四章为架构与实现现状映射 |
| [docs/INTEGRATION.md](docs/INTEGRATION.md) | 集成中心：端到端接入教程、落地方式、自建 Exporter 专题、排查 |
| [docs/LOG_INTEGRATION.md](docs/LOG_INTEGRATION.md) | 日志链路权威说明：Filebeat → Kafka 拓扑、幂等部署、自检、配置项 |
| [docs/AI_CODE_ANALYSIS_API.md](docs/AI_CODE_ANALYSIS_API.md) | 外部 AI 分析服务 OpenAPI v1 契约、回调 HMAC 验签（附录 A）、平台侧验收（附录 B） |
| [docs/API.md](docs/API.md) | 平台 HTTP 接口清单与错误码速查 |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | 部署、配置项速查、备份恢复、升级与值班排查 |
| [docs/POSTMORTEM.md](docs/POSTMORTEM.md) | 交付期真实故障复盘档案（历史记录保持原貌） |

---

## 四、仓库结构

```text
.
├── .github/                        # CI、Issue/PR 模板、Dependabot
├── docs/                           # 设计 / 接口 / 运维文档（索引见 docs/README.md，图示在 docs/assets/）
├── middleware-ops/                 # 后端（Go 1.23，模块化单体）
│   ├── cmd/server/                 # 主入口：配置→日志→DB→缓存→引擎→服务→路由→调度
│   ├── cmd/renderdump/             # 调试工具：导出 Ansible 渲染产物
│   ├── configs/config.yaml         # 默认配置（含注释说明）
│   └── internal/
│       ├── config/                 # viper 配置 + 环境变量覆盖 + 启动期校验
│       ├── model/ · db/            # 实体与 GORM 连接、迁移、pgvector 构建标签适配
│       ├── repository/             # 数据访问层（审计表仅追加，无 Update/Delete）
│       ├── engine/guardrail/       # 六道护栏：budget / loop_guard / timeout / permscope / quality / cost
│       ├── monitor/                # Prometheus 查询封装（不含自研采集器）+ 确定性模拟器
│       ├── logpipe/                # Kafka 消费（消费组 mwops-log-ingest）+ Filebeat 事件解析
│       ├── integration/            # 集成中心：组件模板 + 采集配置/Ansible 渲染（纯函数，可单测）
│       ├── docker/                 # Docker Engine API 最小客户端（一键拉起 Exporter）
│       ├── service/                # 业务服务：诊断、告警收敛、日志后处理、外部 AI 分析对接等
│       ├── handler/ · router/      # 接口层（权限点与操作级别在路由显式声明）
│       ├── middleware/             # Gin 中间件（追踪/恢复/限流/认证/数据权限）
│       └── pkg/cache/ · utils/     # 缓存双实现（Redis/内存）、JWT/加密/口令工具
├── middleware-ops-web/             # 前端（Vue 3 + Vite + TS + Element Plus，深浅双主题、移动端适配）
├── deploy/                         # postgres 初始化、prometheus 抓取与告警规则、grafana provisioning、ansible 工具
├── scripts/                        # onboard.sh（接入/修复）、doctor.sh（跨栈体检）、smoke-test.ps1（端到端冒烟）
├── docker-compose.yml              # 一键部署编排
├── Makefile                        # 常用开发/部署命令（make help）
├── CONTRIBUTING.md · SECURITY.md · CODE_OF_CONDUCT.md · CHANGELOG.md
└── LICENSE
```

---

## 五、核心设计

### 5.1 AI 能力六道工程护栏

| 护栏 | 实现位置 | 关键行为 |
|------|----------|----------|
| ① 上下文预算 | `engine/guardrail/budget.go` | 输入 8K / 输出 2K 可配；超预算告知被截断的维度；时序指标降采样为「均值/P95/斜率/拐点/异常片段」 |
| ② 防死循环 | `loop_guard.go` | 步数上限、同工具同参数指纹重复 2 次即终止、工具白名单、每步决策卡；工具失败不自动重试 |
| ③ 超时与降级 | `timeout.go` | 工具 10s / 任务 120s；超时放弃数据源并标注缺失维度；第三方→本地→规则引擎三级降级 + 熔断 |
| ④ 权限隔离 | `permscope.go` | AI 仅挂载只读工具集（类型层面无写方法）；数据权限在仓储查询上强制生效；AI 生成 SQL 强制只读 + 强制 LIMIT + 禁多语句 + 表白名单 |
| ⑤ 质量护栏 | `quality.go` | 强制结构化输出（根因/证据/置信度/建议/影响/待确认）；无硬证据标注「推测」；24 条典型故障评测集 |
| ⑥ 成本治理 | `cost.go` | 同实例 + 同问题签名 24h 确定性缓存；按用户/平台日预算；并发 ≤4；异常突增自动熔断 |

### 5.2 安全设计

- **RBAC**：admin / ops / dev / readonly 四内置角色 + 自定义角色；权限点在路由层显式声明，服务端强制校验。
- **数据权限**：按环境（dev/staging/prod）与分组隔离，直接落到仓储查询条件，不依赖 Prompt 约束。
- **操作分级**：L0 只读 / L1 低危 / L2 高危；L2 一律走审批（生产环境强制，30 分钟超时自动拒绝），申请人与审批人不得为同一人。
- **审计可验证**：`audit_logs` 从 API 到仓储层均无更新/删除方法；哈希链 `hash_self = SHA256(hash_prev | 规范化内容)`，每日快照落盘并校验，断链可定位。
- **凭据管理**：连接密码 AES-256-GCM 加密存储，主密钥来自环境变量或 0600 权限密钥文件（首次启动自动生成），接口永不回传密文。
- **出网合规**：默认禁止任何外发；需按服务显式加入出网白名单，堆栈信息强制脱敏（IP/手机号/请求 ID/路径/邮箱/凭据）。

### 5.3 接口与认证约定

- 统一响应：`{code, message, data}`；分页 `page` / `page_size`（默认 20，上限 100）。
- 错误码分段：400x 参数、401x 认证、403x 权限、404x 不存在、500x 系统。
- 认证：`Authorization: Bearer <JWT>`，同时写入 SameSite Cookie；每次请求从数据库重建权限，角色变更即时生效。
- 流式：`POST /api/ai/diagnose` 走 SSE，事件类型 `meta` / `data` / `done` / `error`。
- 上报 Hook：`POST /api/hooks/logs`、`POST /api/hooks/alerts`，使用 `X-Hook-Token` 服务令牌，与用户 JWT 分离。

完整接口清单见 [docs/API.md](docs/API.md)。

---

## 六、验证

```bash
# 一条命令完成提交前自检（后端格式/vet/测试 + 前端构建/冒烟）
make all-check

# 也可分项执行
make backend-lint     # gofmt -l + go vet
make backend-test     # go test ./...
make frontend-build   # 类型检查 + 构建
make frontend-smoke   # 无头浏览器加载产物，捕获白屏等运行时问题

# 端到端冒烟（需后端已在 8080 运行；PS 7 用 pwsh，Windows 自带 PS 5.1 用 powershell）
make verify
```

Windows 未安装 make 时，可在 `middleware-ops/` 下直接执行 `gofmt -l . && go vet ./... && go test ./...`，在 `middleware-ops-web/` 下执行 `npm run build` / `npm run smoke`。

单元测试重点覆盖越权用例（RBAC 权限点/级别、数据权限过滤、只读工具集强制）、告警去重指纹、六道护栏、出网脱敏、外部 AI 分析对接（协议适配/回调幂等）以及数据库 schema 不变量（唯一约束命名与 GORM 命名策略一致、列名不得命中 PostgreSQL 保留关键字）。

---

## 七、已知边界

1. **修复执行器为预演实现**：`service/dryRunExecutor` 只返回影响预览与参数校验，不对被管中间件产生副作用；接入真实执行需实现 `service.Executor` 接口并注入，L2 动作在审批通过后按预演方案人工执行并回填结果。
2. **代码分析依赖外部 AI 分析服务**：平台不 clone、不缓存业务代码，也不做 AST/代码知识图谱/向量 Rerank；它只负责按规则触发、脱敏外发、回调收敛三点式结论。深度代码索引为后续方向。
3. **因果收敛不做**：告警收敛仅实现规则级（实时）与语义聚类（离线、仅合并展示），依赖服务拓扑的因果收敛列为后续方向。
4. **通知渠道需外部配置**：飞书/企微/钉钉/邮件在未配置 webhook 时仅记录通知日志，不影响主链路。
5. **默认构建未启用 pgvector 原生类型**：向量以文本存储并在应用层做余弦检索，≤200 实例规模可接受；需要 ANN 索引时以 `-tags pgvector` 构建。
6. **前端不做手工分包**：`vite.config.ts` 刻意不使用 `manualChunks`。element-plus 与 dayjs 互相引用，强行分包会形成 chunk 循环依赖并触发 ES module TDZ（表现为白屏），详见 [docs/POSTMORTEM.md](docs/POSTMORTEM.md) INC-003。

---

## 八、参与贡献

欢迎提交 Issue 与 Pull Request。提交前请确认：

- 后端代码 `gofmt` 干净、`go vet ./...` 与 `go test ./...` 通过；提交前跑一遍 `make all-check`。
- 遵循 [Conventional Commits](https://www.conventionalcommits.org/) 风格的提交信息：`<type>(<scope>): <subject>`。
- 新增/变更行为请同步更新 `docs/` 下对应文档，并在 [docs/README.md](docs/README.md) 登记，避免死链。

更多约定见 [CONTRIBUTING.md](CONTRIBUTING.md)，参与社区请遵守 [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md)，安全漏洞请按 [SECURITY.md](SECURITY.md) 私密上报。交付过程中的真实故障复盘见 [docs/POSTMORTEM.md](docs/POSTMORTEM.md)。

## 九、许可

本项目基于 [MIT 许可证](LICENSE) 开源。指标采集依赖 Prometheus 生态与各中间件官方 Exporter（本仓库不包含采集器二进制）。
