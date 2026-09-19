# syntax=docker/dockerfile:1
# 后端镜像（多阶段构建）。
#
# 构建选项：
#   --build-arg GO_BUILD_TAGS=pgvector     启用 pgvector 原生向量类型与 HNSW 索引
#   --build-arg WITH_ANSIBLE=false         不装 ansible（只做本机 Docker 部署时给镜像瘦身 ~200MB）
#   --build-arg ALPINE_MIRROR=mirrors.aliyun.com
#                                          换 Alpine 源（国内构建 apk 下载慢时用）
#
# 为什么重复构建也应该很快（本文件的两条约定）：
#   1. `go mod download` 与两次 `go build` 都用了 BuildKit 缓存挂载
#      （/go/pkg/mod 与 /root/.cache/go-build）——即使 `COPY . .` 因源码改动失效，
#      **Go 编译缓存也不会丢**，第二次起只编译你改过的包；
#   2. ansible 的安装在**独立且靠前的层**里，只由 WITH_ANSIBLE/ALPINE_MIRROR 决定，
#      改业务代码不会触发重新下载那 200MB。
#
# 若宿主关闭了 BuildKit（DOCKER_BUILDKIT=0），缓存挂载语法会报错：
# 请改回 `docker build`（现代 Docker 默认开启 BuildKit），或临时删掉 --mount 参数。

FROM golang:1.23-alpine AS builder

ARG GO_BUILD_TAGS=""
ARG ALPINE_MIRROR=""
ARG ALPINE_VERSION="3.20"

# 显式写入 main + community 两个源：
#   - community 是 ansible 所在仓库，Alpine 默认 /etc/apk/repositories 里**没有**它；
#   - ALPINE_MIRROR 支持宽松写法（带不带 https://、带不带 /alpine 都行），
#     国内构建务必设置，否则 apk 拉几个小包都可能要几分钟。
RUN M="${ALPINE_MIRROR#http://}"; M="${M#https://}"; M="${M%/}"; M="${M%/alpine}"; \
    if [ -n "$M" ]; then BASE="https://${M}/alpine"; \
    else BASE="https://dl-cdn.alpinelinux.org/alpine"; fi && \
    printf '%s\n' "${BASE}/v${ALPINE_VERSION}/main" "${BASE}/v${ALPINE_VERSION}/community" > /etc/apk/repositories && \
    apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# 先拷贝依赖清单以复用层缓存；依赖落到缓存卷，源码变更不会重下模块
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go env -w GOPROXY=https://goproxy.cn,direct && go mod download

COPY . .

# 编译缓存挂载：重复构建时只重编改动的包（否则每次改一行代码都要全量重编）
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
    -tags "${GO_BUILD_TAGS}" \
    -ldflags "-s -w -X main.version=1.0.0" \
    -o /out/middleware-ops ./cmd/server

# 运行阶段：仅保留二进制与必要证书
FROM alpine:3.20

# 远程安装 Exporter（Ansible）**默认安装**：远程部署是集成中心的默认路径，
# 缺 ansible-playbook 会让"一键远程安装"开箱即不可用（代价是镜像约 +200MB）。
# 只做本机 Docker 部署、想瘦身时用 --build-arg WITH_ANSIBLE=false（见 deploy/ansible/README.md）。
ARG WITH_ANSIBLE=true
ARG ALPINE_MIRROR=""
ARG ALPINE_VERSION="3.20"

# 注意层次顺序：**先装 ansible（最贵、最稳定的一层），再创建用户与目录**。
# 这样改业务代码、改用户/目录都不会导致重新下载 ansible 包。
RUN M="${ALPINE_MIRROR#http://}"; M="${M#https://}"; M="${M%/}"; M="${M%/alpine}"; \
    if [ -n "$M" ]; then BASE="https://${M}/alpine"; \
    else BASE="https://dl-cdn.alpinelinux.org/alpine"; fi && \
    printf '%s\n' "${BASE}/v${ALPINE_VERSION}/main" "${BASE}/v${ALPINE_VERSION}/community" > /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata curl git openssh-client && \
    if [ "$WITH_ANSIBLE" = "true" ]; then \
      apk add --no-cache ansible openssh-client sshpass && \
      ansible-playbook --version >/dev/null && \
      command -v ssh >/dev/null && command -v sshpass >/dev/null; \
    fi

RUN addgroup -S mwops && adduser -S -G mwops mwops && \
    # repos：AI 代码分析的代码仓库缓存目录（config code_repo.cache_dir 指向 /app/data/repos）。
    # 预建并赋权，避免首次 clone 时因卷的权限回落导致 mwops 无法写入。
    mkdir -p /app/data/logs /app/data/audit-snapshots /app/data/integrations /app/data/ansible /app/data/repos /app/configs && \
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
