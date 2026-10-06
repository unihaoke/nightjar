# 贡献指南（Contributing）

感谢你愿意为 **中间件智能问题解决平台（nightjar）** 贡献力量！本文说明开发环境、代码规范与协作流程。

## 1. 环境要求

| 组件 | 版本 |
|------|------|
| Go | 1.23+（CI 使用 1.23） |
| Node.js | 20+（前端，CI 使用 22） |
| Docker / Docker Compose | 可选，用于一键部署与联调 |
| GNU Make | 可选（Windows 可直接用 README 中的等价命令） |

## 2. 仓库结构

```
├── middleware-ops/       # 后端（Go，模块化单体）
├── middleware-ops-web/   # 前端（Vue 3 + Vite + TS）
├── deploy/               # Postgres / Prometheus / Grafana 初始化与抓取配置
├── docs/                 # 设计、接口、运维文档；图片放 docs/assets/
├── scripts/              # 接入 / 体检 / 冒烟脚本
├── docker-compose.yml    # 一键部署编排
└── Makefile              # 常用开发与部署命令
```

> 仓库结构以 [README](README.md#四仓库结构) 为准，新增顶层目录时请同步更新该章节。

## 3. 本地开发

```bash
# 后端
cd middleware-ops && go mod download
go run ./cmd/server -config configs/config.yaml

# 前端（代理 /api 到 127.0.0.1:8080）
cd middleware-ops-web && npm install
npm run dev
```

## 4. 提交前自检

```bash
make all-check        # 后端 lint/test + 前端构建/冒烟
```

至少确保：

- 后端：`gofmt -l .` 无输出、`go vet ./...` 通过、`go test ./...` 通过；
- 前端：`npm run typecheck` 与 `npm run build` 通过。

## 5. 代码规范

- **Go**：遵循 `gofmt`；导出标识符需有注释；错误须显式处理，禁止静默吞错；新增逻辑尽量配套单元测试（`*_test.go`）。
- **TypeScript / Vue**：保持 strict 类型；组件按 `src/views`、`src/components` 既有分层放置。
- **提交信息**：`<type>(<scope>): <subject>`，`type` 取 `feat|fix|docs|refactor|perf|test|build|ci|chore`，例如 `fix(logalert): 修正冷却期内重复触发 AI 的问题`。
- **禁止提交**：密钥、口令、生产地址、`.env`、证书私钥；本地运行数据与构建产物已在 `.gitignore` 中忽略，请勿强制加入。

## 6. 分支与 Pull Request

1. 从 `master` 切出特性分支，命名如 `feat/xxx`、`fix/xxx`；
2. 提交 PR 前请先 rebase 到最新 `master`，并确保 CI 通过；
3. PR 描述请使用仓库模板，说明动机、变更点与自检结果；
4. 涉及接口或配置变更时，**必须**同步更新 `README.md` 与 `docs/`。

## 7. 文档

- 面向用户的接口/设计文档放在 `docs/`（英文文件名，正文中文），图片统一放 `docs/assets/`；
- 文档清单与阅读路径维护在 [docs/README.md](docs/README.md)：新增/重命名文档后**必须**同步更新该索引、根 README（中文 `README.md` 与英文 `README.en.md` 双语一致）及相关文档中的链接，避免死链；
- 设计与代码不一致时以代码为准，在 `docs/DESIGN.md` 第十四章追加「实现现状注记」，不回改历史结论；`docs/POSTMORTEM.md` 只追加不改写。

## 8. 安全问题

请勿通过公开 Issue 报告安全漏洞，流程见 [SECURITY.md](SECURITY.md)。

---

参与本项目即表示你同意遵守 [行为准则](CODE_OF_CONDUCT.md)。
