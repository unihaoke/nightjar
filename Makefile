# 中间件智能问题解决平台 · 常用开发与部署命令。
#
# 约定：所有命令均可从仓库根目录执行（Makefile 位于根目录）。
# 若本机未安装 make（Windows 常见），可直接使用 README 中的等价命令。

SHELL := /bin/bash
BACKEND_DIR := middleware-ops
FRONTEND_DIR := middleware-ops-web

.PHONY: help
help: ## 显示可用命令
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# ---------------------------------------------------------------------------
# 后端
# ---------------------------------------------------------------------------
.PHONY: backend-deps
backend-deps: ## 下载并整理后端依赖
	cd $(BACKEND_DIR) && go mod tidy

.PHONY: backend-build
backend-build: ## 编译后端
	cd $(BACKEND_DIR) && go build ./...

.PHONY: backend-run
backend-run: ## 本地运行后端（默认读取 configs/config.yaml）
	cd $(BACKEND_DIR) && go run ./cmd/server -config configs/config.yaml

.PHONY: backend-test
backend-test: ## 运行后端测试
	cd $(BACKEND_DIR) && go test ./...

.PHONY: backend-lint
backend-lint: ## 格式检查 + go vet
	cd $(BACKEND_DIR) && gofmt -l . && go vet ./...

.PHONY: backend-fmt
backend-fmt: ## 格式化后端代码
	cd $(BACKEND_DIR) && gofmt -w .

.PHONY: backend-pgvector
backend-pgvector: ## 以 pgvector 构建标签编译（启用原生向量与 HNSW 索引）
	cd $(BACKEND_DIR) && go build -tags pgvector ./...

# ---------------------------------------------------------------------------
# 前端
# ---------------------------------------------------------------------------
.PHONY: frontend-install
frontend-install: ## 安装前端依赖
	cd $(FRONTEND_DIR) && npm install

.PHONY: frontend-dev
frontend-dev: ## 启动前端开发服务器（代理到 127.0.0.1:8080）
	cd $(FRONTEND_DIR) && npm run dev

.PHONY: frontend-build
frontend-build: ## 类型检查 + 构建前端产物
	cd $(FRONTEND_DIR) && npm run build

.PHONY: frontend-typecheck
frontend-typecheck: ## 仅做前端类型检查
	cd $(FRONTEND_DIR) && npm run typecheck

# ---------------------------------------------------------------------------
# 部署
# ---------------------------------------------------------------------------
.PHONY: up
up: ## 一键部署（Docker Compose）
	docker compose up -d --build

.PHONY: down
down: ## 停止并移除容器
	docker compose down

.PHONY: logs
logs: ## 查看服务日志
	docker compose logs -f --tail=200

.PHONY: verify
verify: ## 端到端冒烟验证（需后端已在 8080 运行）
	pwsh -File scripts/smoke-test.ps1

.PHONY: all-check
all-check: backend-lint backend-test frontend-typecheck ## 提交前全量自检
	@echo "全量自检通过"
