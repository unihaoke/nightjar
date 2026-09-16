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
   ├── resource：纳管、监控、告警、日志告警、服务器与仓库
   ├── ai      ：诊断编排、知识库、代码分析、上下文与 Prompt
   └── control ：认证授权、修复执行、审批、审计、通知
   │
engine（AI 引擎抽象 + 六道护栏；与业务解耦，可整体替换）
   │
repository（数据访问；审计表仅追加）
   │
model / db（实体与迁移，pgvector 适配）
```

依赖方向单向向下，`engine` 与 `service` 之间只通过接口交互，便于替换引擎与独立测试护栏。

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
| 4.8 日志告警与代码分析 | `service/logalert.go`、`service/codeanalysis.go`、`cmd/agent` | 错误指纹归并；三点式模板；出网白名单 + 脱敏 |
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
| Kafka / VictoriaMetrics / MinIO（2.3 可选组件） | `pkg/cache` 的接口抽象方式 | 对应的队列与存储适配器 |
