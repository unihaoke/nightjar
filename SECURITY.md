# 安全政策（Security Policy）

## 支持范围

我们只对默认分支（`master`）上的最新代码提供安全修复，不保证对历史版本的长期支持。

## 报告漏洞

请**不要**通过公开 Issue、Discussion 或 PR 披露安全漏洞。

推荐使用 GitHub 平台的私密渠道：

1. 打开仓库页面 → **Security** → **Report a vulnerability**（GitHub Security Advisories）；
2. 在报告中尽量提供：
   - 影响范围与严重程度评估；
   - 最小复现步骤或 PoC（请脱敏）；
   - 受影响的版本 / 提交；
   - 如已知，附上修复建议。

我们会在收到报告后尽快确认，并在修复发布后再公开致谢（如需匿名请注明）。

## 部署与配置注意事项

本项目默认以「内网可信环境」为部署前提，上线前请务必：

- 替换 `.env` 中全部默认口令与密钥（`JWT_SECRET`、`ADMIN_PASSWORD`、`DB_PASSWORD`、`REDIS_PASSWORD`、`GRAFANA_PASSWORD` 等）；
- 生产环境**关闭匿名模式**（`allowAnonymous`），由上游网关统一鉴权；
- 谨慎配置出网白名单 `MWOPS_SECURITY_OUTBOUND_WHITELIST`（默认空 = 全禁）；
- 不要将 `.env`、`deploy/exporters/my.cnf` 等含凭据的文件提交到版本库；
- 通过 TLS / 反向代理对外暴露服务，不要将数据库、Redis、Kafka 直接监听公网。

## 已知安全边界

关于权限隔离、只读工具集、SQL 规则校验、出网合规等设计，参见 [`docs/DESIGN.md`](docs/DESIGN.md) 与 [`docs/OPERATIONS.md`](docs/OPERATIONS.md)。
