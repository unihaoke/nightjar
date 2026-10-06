# 文档索引

本目录是 nightjar（中间件智能问题解决平台）的文档根。文档正文使用中文，文件名使用英文；图片统一放在 `assets/`。

## 文档地图

| 文档 | 内容 | 读者 |
|---|---|---|
| [DESIGN.md](DESIGN.md) | 设计基线：功能模块、AI 护栏、安全设计、库表、非功能需求；第十四章为架构与实现现状映射 | 全体开发、评审 |
| [INTEGRATION.md](INTEGRATION.md) | 集成中心使用与设计：端到端接入教程、三种落地方式、组件模板矩阵、自备 Prometheus/自建 Exporter 专题、日志集成摘要、排查 | 实施、SRE、被管项目方 |
| [LOG_INTEGRATION.md](LOG_INTEGRATION.md) | 日志链路权威说明：Filebeat → 平台 Kafka 拓扑、幂等部署、消费与后处理编排、自检、配置项速查 | 实施、SRE、后端 |
| [AI_CODE_ANALYSIS_API.md](AI_CODE_ANALYSIS_API.md) | 外部 AI 分析服务的 OpenAPI v1 契约（平台是调用方）：鉴权、同步/异步接口、回调 HMAC 验签（附录 A）、平台侧接入验收清单（附录 B） | AI 服务对接方、后端 |
| [API.md](API.md) | 平台 HTTP 接口清单，按功能域组织，含错误码速查，与 `middleware-ops/internal/router/router.go` 对齐 | 前端、集成方 |
| [OPERATIONS.md](OPERATIONS.md) | 部署拓扑、首次部署清单、配置项速查、备份恢复、升级、审计校验、日常故障排查 | SRE、值班 |
| [SCHEMA.sql](SCHEMA.sql) | 数据库结构参考（以 Go 迁移为权威，此文件供查阅） | DBA、后端 |
| [POSTMORTEM.md](POSTMORTEM.md) | 交付期真实故障复盘档案（INC-001 起）。历史记录保持原貌，其中提及的旧文档名与旧实现不代表现状 | 全体开发 |

## 按角色推荐阅读路径

- **第一次接触项目**：根目录 [README.md](../README.md) → INTEGRATION.md 第 1、3、4 章
- **接入一个新中间件**：INTEGRATION.md 第 4–6 章；自备采集器时看第 12 章；要接日志看 LOG_INTEGRATION.md
- **对接外部 AI 分析服务**：AI_CODE_ANALYSIS_API.md 全文（重点第 2 章鉴权、第 5–6 章接口、附录 A/B）
- **部署与值班**：OPERATIONS.md；排障查 INTEGRATION.md 第 9 章；历史同类故障查 POSTMORTEM.md
- **改后端代码**：DESIGN.md（先读第十四章实现现状）→ API.md → 对应包源码
- **改前端 / 调接口**：API.md 对应章节

## 文档约定

- 新增文档必须在本索引和根 README 中登记，并在相关文档中补链接，禁止死链。
- 设计与实现不一致时，以代码为准，并在 DESIGN.md 第十四章以「实现现状注记」方式补充，不回改历史结论。
- POSTMORTEM.md 是历史档案，只追加、不回改事实。
