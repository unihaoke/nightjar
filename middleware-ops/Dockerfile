# 后端镜像（多阶段构建）。
#
# 默认构建不依赖 pgvector；如需启用原生向量类型与 HNSW 索引，请追加
#   --build-arg GO_BUILD_TAGS=pgvector

FROM golang:1.23-alpine AS builder

ARG GO_BUILD_TAGS=""

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# 先拷贝依赖清单以复用层缓存
COPY go.mod go.sum ./
RUN go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -tags "${GO_BUILD_TAGS}" \
    -ldflags "-s -w -X main.version=1.0.0" \
    -o /out/middleware-ops ./cmd/server

# 日志采集 Agent 也打进同一个镜像：
# 「集成中心 → 日志接入」会用**本镜像**创建采集容器（只覆盖 Entrypoint），
# 因此平台不需要额外的 Agent 镜像，也不需要被管项目预置任何东西。
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w" \
    -o /out/mwops-agent ./cmd/agent

# 运行阶段：仅保留二进制与必要证书
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -S mwops && adduser -S -G mwops mwops && \
    mkdir -p /app/data/logs /app/data/audit-snapshots /app/data/integrations /app/configs && \
    chown -R mwops:mwops /app

WORKDIR /app

COPY --from=builder /out/middleware-ops /usr/local/bin/middleware-ops
# 采集 Agent：平台用它创建日志采集容器（覆盖 Entrypoint 即可）
COPY --from=builder /out/mwops-agent /usr/local/bin/mwops-agent
COPY --chown=mwops:mwops configs/config.yaml /app/configs/config.yaml

USER mwops

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["middleware-ops"]
CMD ["-config", "/app/configs/config.yaml"]
