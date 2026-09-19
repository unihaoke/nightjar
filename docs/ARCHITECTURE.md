# 架构与实现映射说明

本文档说明「设计文档条目 → 代码落点」的对应关系，便于评审与后续演进时快速定位。

## 一、模块化单体分层

```text
handler（HTTP 语义：参数绑定、权限点声明、统一响应）
   │
router（路由表：权限点 + 操作级别的显式声明）
   │
middleware（追踪/恢复/限流/认证/数据权限）
   │
service（业务编排：resource / ai / control 三域）
   ├── resource：纳管、监控、告警、日志告警（规则 + 后处理编排）、服务器与仓库
   ├── ai      ：诊断编排、知识库、代码分析、上下文与 Prompt
   └── control ：认证授权、修复执行、审批、审计、通知
   │
repo（服务代码仓库的本地缓存：首次 clone、之后更新到远端；供 AI 代码分析定位代码行）
   │
engine（AI 引擎抽象 + 六道护栏；与业务解耦，可整体替换）
   │
repository（数据访问；审计表仅追加）
   │
model / db（实体与迁移，pgvector 适配）
```

依赖方向单向向下，`engine` 与 `service` 之间只通过接口交互，便于替换引擎与独立测试护栏。
`internal/repo`（代码仓库本地缓存）是 `service` 之下的能力包：服务层只依赖 `RepoFetcher` 接口并每次描述
「要哪个服务的哪条分支」，缓存根目录、git 路径与超时等**进程级**配置在容器装配时一次性注入
（见 `service/repo_adapter.go`），换实现（本地镜像解包、对象存储下载）不需要动服务层。

## 一之一、日志集成的数据流（Filebeat → 平台 Kafka）

日志已不再由平台侧采集（旧的「反查 docker 卷 + 平台起采集容器」实现已整体删除，
见 [`LOG_INTEGRATION.md`](LOG_INTEGRATION.md)）。现在的通路是：

```text
目标服务器（被管侧）                       平台（nightjar）
─────────────────────                     ─────────────────────────────────────────
应用日志 /var/log/app/*.log
      │
      ▼
filebeat（平台用 Ansible 幂等部署：已装则复用、配置内容变化才重启）
      │  output.kafka（JSON 编码）
      ▼
                                  ┌──────────────────────────────────────┐
       Kafka EXTERNAL :9092 ─────▶│ kafka 容器（KRaft 单节点）            │
      （KAFKA_ADVERTISED_HOST）   │  INTERNAL :29092 ← 平台内部消费       │
                                  │  topic: mwops-logs                   │
                                  └───────────────┬──────────────────────┘
                                                  │ consumer group: mwops-log-ingest
                                                  ▼
                                  internal/logpipe（解析 Filebeat 事件）
                                                  ▼
                                  service/logpipeline.go（消费编排 + 链路状态/探测）
                                                  ▼
                                  service/logalert.go Ingest（错误指纹 /
                                  匹配 log_alert_rules：窗口去重 + 冷却抑制）
                                                  ▼
                                  log_alert_events（analysis_state=pending）
                                                  ▼
                                  定时任务（log_alert.worker_interval_seconds）
                                                  ▼
                                  service/logalert_worker.go 后处理：
                                  ① 外发通知渠道（写 notified_at）
                                  ② internal/repo 拉代码（首次 clone，之后更新）
                                  ③ service/codeanalysis.go AI 三点式结论
```

要点：

- 平台只提供 **Kafka 端口 + 消费链路**，采集在被管侧自洽运行，平台重启不影响采集（位点在 Filebeat 注册表）；
- 消费位点**只在 Ingest 成功后提交**；解析失败的消息计入 `dropped` 并照常提交，避免一条脏消息堵住分区；
- 平台自检 `POST /api/integrations/:id/selfcheck` 对日志类型返回三段环节：
  平台 → Kafka / 被管机接入地址（Kafka EXTERNAL）/ 日志是否真的进来了；
- 回退通路只有一条：`POST /api/hooks/logs`（应用直推，零侵入兜底），字段与 Filebeat 路径统一映射。

## 一之二、日志事件的数据流（规则 → 窗口/冷却 → 后处理）

落库之后的那一段值得单独记住：**"这条告警为什么没通知我"全靠这条链上的字段解释**。

```text
日志事件 ──▶ ① 规则匹配（service/logalertrule.go）
                 按「服务 + 错误指纹 + 级别」匹配 log_alert_rules：
                 priority 数字小者优先、同优先级按 id 升序，取第一条 enabled 的规则；
                 没命中 → 用 log_alert.default_* 构造的虚拟规则（零配置也能跑通）
             ──▶ ② 窗口去重（命中规则的 dedup_window，分钟）
                 窗口内同指纹【合并计数】、不新增事件（error_count 累加、last_seen_at 刷新）
             ──▶ ③ 冷却抑制（命中规则的 cooldown，分钟）
                 冷却内不重复通知、不重复触发 AI，但【事件照常记录】
                 （suppressed=true + cooldown_until；冷却过后再次合并才重新通知，且不重跑 AI）
             ──▶ ④ 后处理（service/logalert_worker.go）
                 定时任务扫 analysis_state=pending：
                 通知渠道（写 notified_at）→ 拉代码（internal/repo）→ AI 三点式结论
                 （定位文件行 / 根因 / 应急处置 / 修复建议），
                 失败或跳过原因写 analysis_error（disabled=规则关了 AI 或没配仓库映射）
```

三个结构性保证：

- **状态在数据库**（`analysis_state` / `notified_at` / `cooldown_until`）：进程重启不丢，页面能解释每条事件的下场；
- **多副本安全**：`ClaimForAnalysis` 用带条件的 UPDATE（`pending → running`）抢占，一条事件只被处理一次；
- **缓存不是工作区**：`internal/repo` 只保证"本地有一份与远端一致的代码"（首次 clone、之后更新），
  强制重置到远端，避免脏缓存给出错误行号（详见 [`COLLECTOR.md`](COLLECTOR.md) 6.6）。

## 二、设计文档条目映射

| 设计文档条目 | 代码落点 | 关键实现说明 |
|--------------|----------|--------------|
| 2.1 技术栈（Gin/GORM/go-redis/cron/viper/zap/JWT） | `internal/router`、`internal/repository`、`internal/pkg/cache`、`internal/service/scheduler.go`、`internal/config`、`internal/logger`、`internal/utils/jwt.go` | 全部按选型落地；Redis 缺省降级为内存实现 |
| 2.2 前端（Vue3/Vite/Element Plus/Pinia/ECharts/markdown-it/SSE） | `middleware-ops-web/src` | SSE 用 fetch + ReadableStream（需携带认证头）；ECharts 按需引入 |
| 3.2 模块化单体 | `cmd/server/main.go` | 单进程装配：DB → 缓存 → 引擎 → 服务 → 路由 → 调度 |
| 3.4 关键机制（引擎插拔/任务队列/并发≤4） | `engine/factory.go`、`engine/hybrid.go`、`service/container.go`、`engine/guardrail/timeout.go` | `ai_engine.strategy` 三态；并发额度由护栏信号量控制 |
| 4.1 纳管 + 能力矩阵 | `service/middleware.go`、`internal/monitor/profile.go`、`handler/system.go: capabilityMatrix` | 能力矩阵由代码声明并在 `/api/system/info` 透出，声明与实现一致 |
| 4.2 统一监控（PromQL，不建自有指标表） | `internal/monitor/prometheus.go`、`service/metrics.go` | 无 `metric_snapshots` 表；指标目录由 `profile.go` 定义 |
| 4.3 AI 诊断中心 | `service/diagnose.go`、`service/ai/{bundle,prompt}.go` | 固定管线：解析目标 → 权限 → 缓存 → 预采集 → 一次调用 → 结构化 → 落库 |
| 4.4 告警治理（规则级/语义聚类/因果收敛） | `service/alert.go` | 指纹 + 窗口去重 + 冷却；聚类仅合并展示；因果收敛未实现（二期） |
| 4.5 知识库质量闭环 | `service/knowledge.go`、`service/diagnose.go: sinkKnowledge` | 诊断沉淀 `status=draft`；采纳率参与检索加权 |
| 4.6 操作分级与执行 | `service/fix.go`、`service/approval.go` | L0/L1/L2 判定；L2 转工单；结果回填复核 |
| 4.7 审计（只追加 + 哈希链） | `repository/audit.go`、`service/audit.go` | 仓储无 Update/Delete；`hash_self = SHA256(hash_prev\|内容)` |
| 4.8 日志告警与代码分析 | `service/logalert.go`、`service/logalertrule.go`、`service/logalert_worker.go`、`internal/repo`、`service/codeanalysis.go`、`internal/logpipe`、`service/logpipeline.go` | 错误指纹归并；日志告警规则（`log_alert_rules`：去重窗口/冷却期/优先级/通知渠道/AI 开关）由 `logalertrule.go` 匹配与读写；落库后的通知 + 拉代码 + AI 三点式分析由 `logalert_worker.go` 定时编排；`internal/repo` 维护服务代码的本地缓存（首次 clone、之后更新到远端）；三点式模板；出网白名单 + 脱敏；日志集成由 `logpipe` 消费 Kafka 并把 Filebeat 事件映射成日志事件 |
| 5.1 不做自主循环 Agent | `service/diagnose.go` | 单轮采集 + 一次 LLM 调用；护栏②为二期 Agent 预留 |
| 5.2 上下文预算 | `engine/guardrail/budget.go` | 截断维度透出；`SummarizeSeries` 做降采样摘要 |
| 5.3 防死循环 | `engine/guardrail/loop_guard.go` | 步数/指纹/白名单/决策卡；工具失败不重试 |
| 5.4 超时与降级链 | `engine/guardrail/timeout.go`、`engine/hybrid.go` | 工具/任务超时；熔断 + 三级降级 |
| 5.5 权限隔离 | `engine/guardrail/permscope.go`、`service/middleware.go: GuardScope` | 只读工具集类型约束；数据权限落库过滤；SQL 规则校验 |
| 5.6 质量护栏 | `engine/guardrail/quality.go`、`web/src/components/DiagnosisReport.vue` | 结构化 schema、证据引用、推测标注、24 条评测集 |
| 5.7 成本治理 | `engine/guardrail/cost.go` | 确定性缓存键、日预算、并发、突增熔断 |
| 6.1 RBAC 与数据权限 | `service/auth.go`、`handler/auth.go` | 四内置角色 + 权限点目录；环境/分组隔离 |
| 6.2 操作分级与 IM 边界 | `service/fix.go`、`service/notifier.go` | 卡片不含执行动作，仅「查看详情 / 确认 / 驳回」 |
| 6.3 凭据与密钥管理 | `utils/crypto.go` | AES-256-GCM；主密钥文件 0600；接口不回传密文 |
| 6.4 审计可验证 | `repository/audit.go: CreateSnapshot/VerifyChain` | 每日快照落盘 + 周期校验；断链可定位 |
| 6.5 出网合规 | `service/redact.go`、`service/codeanalysis.go` | 默认关闭；正则脱敏；片段行数上限 |
| 6.6 传输与存储安全 | `middleware/middleware.go`、`utils/password.go`、`router/router.go` | CSP/X-Frame-Options/SameSite；bcrypt；GORM 参数化 |
| 7.1 表结构 | `internal/model/model.go` | 含 v0.2 新增字段（feedback/engine_status/hash_prev/hash_self/status/adopt_count） |
| 7.2 索引 | `db/vector_plain.go`、`db/vector_pgvector.go`、`deploy/postgres/init/02-schema.sql` | 按方言分别建立；HNSW 仅在 pgvector 构建下创建 |
| 八、接口设计 | `router/router.go`、`docs/API.md` | 统一响应/错误码分段/SSE 事件/权限点标注 |
| 十、非功能需求 | `configs/config.yaml`、`service/scheduler.go` | 采集与评估周期、保留策略、限流与并发上限可配 |

## 三、关键取舍与理由

1. **不做自研采集器**：指标来自 Prometheus + 官方 Exporter，平台只做 PromQL 查询与语义化封装（阈值、状态判定、指标目录），避免重复造轮子并降低运维面。
2. **引擎层承载护栏**：六道护栏全部放在 `engine/guardrail`，业务代码只调接口。这样切换模型、调整预算或更换评测集都不需要改动诊断编排。
3. **规则引擎作为降级链末端**：规则引擎输出与 LLM 完全同构的结构化报告，使上层解析、展示、落库逻辑无需分支处理；同时保证「AI 不可用时诊断链路不中断」。
4. **审计只追加**：从 handler 到 repository 都不提供修改/删除接口，配合哈希链与每日快照，把「可验证」做成结构性保证而非流程约定。
5. **执行默认预演**：平台不假装能执行未接入的操作。`dryRunExecutor` 明确拒绝 L2 动作，避免「显示成功但实际未执行」的误导；接入真实客户端只需实现 `Executor` 接口。
6. **移动端优先的响应式**：`AppShell` 在 <768px 切换为抽屉导航，列表页切换为卡片形态，表格统一置于横向滚动容器中；输入控件在窄屏占满整行，触控目标 ≥38px。
7. **主题令牌化**：深浅主题只切换 CSS 变量（含 Element Plus 变量映射），组件样式不做分支，避免两套样式漂移。

## 四、二期演进建议

| 方向 | 可复用基础 | 需要新增 |
|------|------------|----------|
| 自主排障 Agent（设计文档 5.1 探索项） | 护栏②的 `LoopGuard`、护栏④的只读工具注册表 `ToolRegistry` | Agent 编排器、决策卡展示、人工确认中断点 |
| 因果收敛（4.4 二期） | 告警指纹与聚类结果 | 服务拓扑数据模型与依赖推断 |
| 自研三层代码索引（4.8.3 二期） | `CodeAnalysisService.locateCode` | AST 解析、知识图谱、向量检索 + Rerank |
| 多租户与微服务拆分（3.3） | 服务容器与仓储接口 | 租户维度注入、按域拆进程与独立部署 |
| VictoriaMetrics / MinIO（2.3 可选组件） | `pkg/cache` 的接口抽象方式 | 对应的队列与存储适配器 |
