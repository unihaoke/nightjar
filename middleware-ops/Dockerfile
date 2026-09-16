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

# 运行阶段：仅保留二进制与必要证书
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata curl && \
    addgroup -S mwops && adduser -S -G mwops mwops && \
    mkdir -p /app/data/logs /app/data/audit-snapshots /app/configs && \
    chown -R mwops:mwops /app

WORKDIR /app

COPY --from=builder /out/middleware-ops /usr/local/bin/middleware-ops
COPY --chown=mwops:mwops configs/config.yaml /app/configs/config.yaml

USER mwops

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD curl -fsS http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["middleware-ops"]
CMD ["-config", "/app/configs/config.yaml"]
