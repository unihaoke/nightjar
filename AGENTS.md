# AGENTS.md

本文件为在本仓库工作的 AI 编码助手提供约定。路由清单、文档注册与校验命令的机器可读清单见 [`.harness/harness.yaml`](.harness/harness.yaml)（Repository Harness 0.2）。

## 项目概览

nightjar 是中间件智能问题解决平台，monorepo 结构：

- `middleware-ops/`：Go 1.23 后端（Gin + GORM 模块化单体，module `github.com/unihaoke/nightjar/middleware-ops`）
- `middleware-ops-web/`：Vue 3 + Vite + TypeScript + Element Plus 前端
- `deploy/`：Postgres 初始化、Prometheus/Grafana provisioning、Filebeat Ansible 工具
- `docs/`：中文正文文档（英文文件名），索引见 [docs/README.md](docs/README.md)

设计文档是评审基线；**设计与代码冲突时以代码为准**，在 [docs/DESIGN.md](docs/DESIGN.md) 第十四章以「实现现状注记」补充，不回改历史结论。[docs/POSTMORTEM.md](docs/POSTMORTEM.md) 是历史故障档案，只追加、不改写事实。

## 必备命令

```bash
make all-check        # 提交前全量自检（等价于下面四步）
# 分项（无 make 的 Windows 环境可直接执行）
(cd middleware-ops && gofmt -l . && go vet ./... && go test ./... -count=1)
(cd middleware-ops-web && npm run build && npm run smoke)
make backend-fmt      # 用 gofmt -w 就地修复格式
```

## 编码约定

### Go

- 代码必须 `gofmt` 干净、`go vet ./...` 与 `go test ./...` 通过；测试就近放在包内（`*_test.go`）。
- 分层：`handler/router`（权限点与操作级别显式声明在路由）→ `service`（业务编排）→ `repository`（数据访问）；审计表仅追加，禁止增加 Update/Delete 方法。
- 改实体后依赖 GORM AutoMigrate；唯一约束名必须与 GORM 命名策略一致，列名不得使用 PostgreSQL 保留字（有 schema 不变量测试）。不要手工建表。
- pgvector 相关代码受构建标签约束：默认产物走纯文本向量，改这部分时同时检查 `vector_pgvector.go` 与 `vector_plain.go`。
- 配置走 viper（`configs/config.yaml` + `MWOPS_*` 环境变量覆盖），新增配置必须同步更新 config.yaml 注释、[docs/OPERATIONS.md](docs/OPERATIONS.md) 配置表与启动期校验。

### 前端

- 改完跑 `npm run build`（含类型检查）；涉及页面渲染再跑 `npm run smoke`（无头浏览器，防白屏/TDZ）。
- 表单控件按响应式规则绑定 key（`npm run check:reactivity` 专查此项）。
- 不要在 `vite.config.ts` 加 `manualChunks`（chunk 循环依赖会触发 TDZ 白屏，见 POSTMORTEM INC-003）。

### 文档

- 文件名用英文，正文用中文；图片统一放 `docs/assets/`。
- 新增/重命名文档必须同步更新 [docs/README.md](docs/README.md)、根 README 和相关文档里的链接，**禁止死链**。
- 合并文档时先把内容并入目标文档再删源文件，并全局搜索旧文件名修正引用（包括 `.env.example`、代码中的用户可见提示）。

### 提交与换行

- 提交信息：`<type>(<scope>): <subject>`（Conventional Commits）。
- `.gitattributes` 强制 sh/sql/yml 用 LF、ps1/cmd 用 CRLF，不要改乱。
- **不要主动执行 git commit / push**，除非用户明确要求。

## 关键领域事实（避免按旧设计臆测）

- **平台不 clone、不缓存任何业务代码仓库**：没有 `internal/repo` 包、没有 `code_repos` 表。AI 代码分析是把脱敏错误信息提交给外部 AI 分析服务，回调 `POST /api/ai/analysis/callback`（幂等）+ 轮询兜底，状态机 `pending → running → awaiting → done/failed/disabled`。对接配置在「AI 设置」页（AES-256-GCM 落库），协议契约见 [docs/AI_CODE_ANALYSIS_API.md](docs/AI_CODE_ANALYSIS_API.md)；平台自身不暴露 `/api/v1/openapi/*`。
- 日志告警**没有内置默认规则**：未命中启用规则的日志不入库、不通知、不分析；屏蔽规则优先于所有规则。
- 集成抓取目标走 Prometheus **http_sd**（`GET /api/sd/integrations`，30s），不是 file_sd。
- 日志采集是 Ansible 在目标机幂等部署 Filebeat → 平台自带 Kafka，平台侧不跑采集容器。
- 默认禁止任何外发请求；新增外发目标必须接入出网白名单与脱敏逻辑。
