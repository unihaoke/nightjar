# 更新日志（Changelog）

本项目所有值得记录的变更都会写入本文件。
格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added

- 开源配套：`LICENSE`（MIT）、`CONTRIBUTING.md`、`CODE_OF_CONDUCT.md`、`SECURITY.md`、`.editorconfig`、GitHub Actions CI 与 Issue/PR 模板。
- `README.en.md`：README 中英文双语，顶部增加 CI / Go 版本 / License / PR 徽章。
- `docs/README.md`：文档索引与按角色的阅读路径。
- `AGENTS.md` 与 `.harness/harness.yaml`：接入 Repository Harness 0.2（文档注册、关注点路由与校验命令）。
- `middleware-ops/.dockerignore`、`middleware-ops-web/.dockerignore`：收窄 Docker 构建上下文，避免本地文件与密钥进入镜像。

### Changed

- 文档清理与合并（英文文件名、正文中文，交叉引用全部修复）：
  - `ARCHITECTURE.md` 并入 `docs/DESIGN.md` 第十四章「架构与实现映射」后删除；
  - `CALLBACK_INTEGRATION.md` 与 `AI_ANALYSIS_OPENAPI.md` 并入 `docs/AI_CODE_ANALYSIS_API.md` 附录 A（回调 HMAC 验签）/ 附录 B（平台侧接入验收清单）后删除；
  - `COLLECTOR.md` 与 `GUIDE-ONBOARD.md` 并入 `docs/INTEGRATION.md`（端到端教程第 4 章、自备 Prometheus/Exporter 专题第 12 章）后删除。
- `README.md` 重写并修正文档与实现的漂移：抓取目标为 Prometheus http_sd（`GET /api/sd/integrations`）；平台不 clone/缓存代码，AI 代码分析对接外部 AI 分析服务（回调 + 轮询）。
- `docs/DESIGN.md`、`docs/LOG_INTEGRATION.md`、`docs/OPERATIONS.md` 同步实现现状：无默认告警规则、任务状态机含 `awaiting`、删除 `code_repo.*` 过时配置描述。
- `.env.example` 与后端自检提示文案中对已删除文档的引用改指 `docs/INTEGRATION.md`。

## [1.0.0] - 2026-09-16

首个可执行版本，对应《中间件智能问题解决平台 · 设计文档 v1.0》。

### Added

- **M1 基础闭环**：中间件纳管 + 监控 + 告警规则/通知 + RBAC + 审计。
- **M2 AI 诊断**：AI 诊断中心 + 知识库 + AI 能力六道工程护栏 + 成本治理。
- **M3 代码分析**：日志集成（Filebeat → 平台 Kafka）+ 日志告警规则 + AI 代码分析（第三方开放接口 + 本地兜底）+ 高危执行审批闭环。
- 集成中心：组件模板、Exporter 暴露、Prometheus `file_sd` 抓取、推荐告警规则。

<!--
维护约定：
- 发布新版本时，将 [Unreleased] 下的条目移动到新的版本标题下，并补上日期（YYYY-MM-DD）。
- 分类使用：Added / Changed / Deprecated / Removed / Fixed / Security。
-->
