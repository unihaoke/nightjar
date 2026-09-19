#!/usr/bin/env bash
# =============================================================================
# nightjar 侧接入被管项目（以某业务系统为例）· 一键配置
#
# 架构前提（v2 起不再需要互联网络）：
#   监控栈全部在 nightjar。集成时平台用 Docker API **自己发现**目标容器在哪张网络上，
#   并把 Exporter 接进去；被管项目不需要建网络、不需要加别名、不需要改 compose。
#   因此本脚本**只维护平台自己的 .env**，不再碰任何跨栈网络。
#
# 本脚本做七件事：
#   1. 生成缺失的密钥/口令（**数据库卷已存在时不轮换库口令**，见第 5 步）；
#   2. 打开集成能力：INTEGRATION_DOCKER_ENABLED=true，并放开 docker.sock 挂载；
#   3. 端口错开：Web/Prometheus/Grafana 若已被**别的**进程占用，自动换到下一个空闲端口；
#   4. 启动平台（普通 docker compose up -d --build）；
#   5. 对齐数据库口令：数据卷已存在而口令不一致时，用容器内 trust socket 把库内口令改成 .env 的值；
#   6. 启动失败时打印 compose 的真实输出并给出对号入座的修复建议；
#   7. 自检「平台能否看到目标容器」。
#
# 用法：
#   ./scripts/onboard.sh --dry-run              # 先预览（不写文件、不调 docker）
#   ./scripts/onboard.sh                        # 正式执行（目标容器自动发现）
#   ./scripts/onboard.sh --targets app-mysql,app-redis   # 指定要纳管的容器
#   ./scripts/onboard.sh --target app-redis     # 只检查某个目标容器
#   ./scripts/onboard.sh --skip-start           # 只改配置不起容器
#
# 说明：本脚本面向**任意被管项目**，不绑定任何具体项目；目标容器默认自动发现
# （docker ps 里排除平台自身 mwops-* 之外的容器），也可以用 --targets/--target 指定。
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# 目标容器：留空则自动发现（见 discover_targets），避免绑定任何具体项目。
TARGETS=()
DRY_RUN=0
SKIP_START=0

# discover_targets 在未显式指定目标时，列出这台机器上"看起来该被监控"的容器。
#
# 排除平台自身（mwops-*）与已由平台托管的 Exporter，最多取 20 个，避免刷屏。
discover_targets() {
  if [ "${#TARGETS[@]}" -gt 0 ] || [ "$DRY_RUN" = "1" ]; then
    return
  fi
  local found
  found=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -Ev '^mwops-' | head -n 20 || true)
  if [ -n "$found" ]; then
    # shellcheck disable=SC2207
    TARGETS=($found)
  fi
}

CHANGES=()
PROBLEMS=()

if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_CYAN=$'\033[36m'; C_GREEN=$'\033[32m'
  C_YELLOW=$'\033[33m'; C_RED=$'\033[31m'; C_GRAY=$'\033[90m'; C_BOLD=$'\033[1m'
else
  C_RESET=""; C_CYAN=""; C_GREEN=""; C_YELLOW=""; C_RED=""; C_GRAY=""; C_BOLD=""
fi

step()  { printf '\n%s=== %s ===%s\n' "$C_CYAN" "$1" "$C_RESET"; }
info()  { printf '  %s\n' "$1"; }
ok()    { printf '  %s[OK]  %s%s\n' "$C_GREEN" "$1" "$C_RESET"; }
fix()   { printf '  %s[FIX] %s%s\n' "$C_YELLOW" "$1" "$C_RESET"; CHANGES+=("$1"); }
warn()  { printf '  %s[WARN] %s%s\n' "$C_YELLOW" "$1" "$C_RESET"; }
bad()   { printf '  %s[FAIL] %s%s\n' "$C_RED" "$1" "$C_RESET"; PROBLEMS+=("$1"); }
hint()  { printf '         %s→ %s%s\n' "$C_GRAY" "$1" "$C_RESET"; }
drytag(){ printf '  %s[dry-run] %s%s\n' "$C_GRAY" "$1" "$C_RESET"; CHANGES+=("[预览] $1"); }
dryskip(){ printf '  %s[dry-run] %s%s\n' "$C_GRAY" "$1" "$C_RESET"; }

usage() { sed -n '2,25p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run|-n)   DRY_RUN=1 ;;
    --skip-start)   SKIP_START=1 ;;
    --target)       TARGETS=("${2:-}"); shift ;;
    --targets)      IFS=',' read -r -a TARGETS <<< "${2:-}"; shift ;;
    --nightjar-dir) NIGHTJAR_DIR="${2:-}"; shift ;;
    -h|--help)      usage ;;
    *) printf '未知参数：%s（用 --help 查看用法）\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

if docker compose version >/dev/null 2>&1; then COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then COMPOSE=(docker-compose)
else COMPOSE=(docker compose); fi

# ---------------------------------------------------------------------------
# .env 读写（保持注释与键顺序）
# ---------------------------------------------------------------------------
env_get() { sed -n -E "s/^[[:space:]]*$2[[:space:]]*=(.*)$/\1/p" "$1" 2>/dev/null | head -n1; }

mask_if_secret() {
  case "$1" in *SECRET|*PASSWORD|*PASS|*TOKEN|*_KEY) printf '******' ;; *) printf '%s' "$2" ;; esac
}

# env_set FILE KEY VALUE → 有改动时打印说明并返回 0，否则返回 1
env_set() {
  local file="$1" key="$2" value="${3:-}" esc old
  esc=$(printf '%s' "$value" | sed -e 's/[\\&|]/\\&/g')
  if grep -qE "^[[:space:]]*${key}[[:space:]]*=" "$file"; then
    old=$(env_get "$file" "$key")
    [ "$old" = "$value" ] && return 1
    sed -i -E "s|^[[:space:]]*${key}[[:space:]]*=.*$|${key}=${esc}|" "$file"
    printf '%s : %s → %s\n' "$key" "$(mask_if_secret "$key" "$old")" "$(mask_if_secret "$key" "$value")"
  else
    printf '\n%s=%s\n' "$key" "$value" >> "$file"
    printf '%s : (新增) %s\n' "$key" "$(mask_if_secret "$key" "$value")"
  fi
  return 0
}

set_env_reported() {
  local out
  if out=$(env_set "$1" "$2" "${3:-}"); then fix "$out"; else ok "$2 已是最新"; fi
}

gen_secret() {
  local bytes="${1:-24}"
  if command -v openssl >/dev/null 2>&1; then openssl rand -hex "$bytes"
  else head -c "$bytes" /dev/urandom | od -An -tx1 | tr -d ' \n'; fi
}

WEAK_VALUES="please_replace_with_a_random_48_byte_string admin@12345 admin root password 123456 mwo_change_me redis_change_me grafana_change_me"
is_weak() {
  local v; v=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  for w in $WEAK_VALUES; do [ "$v" = "$w" ] && return 0; done
  return 1
}

port_of() { local v; v=$(env_get "$1" "$2"); case "$v" in ''|*[!0-9]*) printf '' ;; *) printf '%s' "$v" ;; esac; }

# 端口是否已被**别的**进程占用（平台自己的容器占用不算冲突，否则重复执行会一直换端口）
port_taken() {
  local port="$1" holders
  [ -z "$port" ] && return 1
  # 平台自己的容器：幂等重跑时不该换端口
  if docker ps --filter 'name=mwops-' --format '{{.Ports}}' 2>/dev/null | grep -qE "[:.]${port}->"; then
    return 1
  fi
  if command -v ss >/dev/null 2>&1; then
    holders=$(ss -ltn 2>/dev/null | awk '{print $4}')
  elif command -v netstat >/dev/null 2>&1; then
    holders=$(netstat -ltn 2>/dev/null | awk '{print $4}')
  else
    holders=$(docker ps --format '{{.Ports}}' 2>/dev/null)
  fi
  printf '%s\n' "$holders" | grep -qE "[:.]${port}\$|[:.]${port}->"
}

next_free_port() {
  local port="$1" i
  for i in $(seq 0 30); do
    if ! port_taken "$((port + i))"; then printf '%s' "$((port + i))"; return 0; fi
  done
  printf '%s' "$port"
}

# 列出某容器所在网络（平台集成时会自动接入这些网络）
container_networks() {
  docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$1" 2>/dev/null | tr -s ' '
}

# 打开 docker-compose.yml 里 backend 的 docker.sock 挂载（自动发现的前提）
enable_docker_sock() {
  local file="$NIGHTJAR_DIR/docker-compose.yml"
  [ -f "$file" ] || { warn "找不到 $file"; return; }
  if ! grep -qE '^[[:space:]]*#[[:space:]]*-[[:space:]]*/var/run/docker\.sock' "$file"; then
    ok 'docker.sock 挂载已放开'
    return
  fi
  if [ "$DRY_RUN" = "1" ]; then
    drytag '取消 docker-compose.yml 中 docker.sock 挂载的注释'
    return
  fi
  cp "$file" "$file.bak-$(date +%Y%m%d%H%M%S)"
  sed -i -E 's|^([[:space:]]*)#[[:space:]]*(-[[:space:]]*/var/run/docker\.sock:/var/run/docker\.sock.*)$|\1\2|' "$file"
  fix '已放开 docker-compose.yml 的 docker.sock 挂载（备份已保留）'
}

# ---------------------------------------------------------------------------
printf '%snightjar 侧一键接入（平台自动发现目标网络，被管项目零改动）%s\n' "$C_BOLD" "$C_RESET"
printf '  平台目录 : %s\n' "$NIGHTJAR_DIR"
printf '  目标容器 : %s\n' "${TARGETS[*]}"
[ "$DRY_RUN" = "1" ] && printf '  %s模式     : DRY-RUN（不写文件、不调 docker）%s\n' "$C_YELLOW" "$C_RESET"

step '0/7 前置检查'
[ -d "$NIGHTJAR_DIR" ] || { bad "nightjar 目录不存在：$NIGHTJAR_DIR"; exit 2; }
if [ "$DRY_RUN" = "1" ]; then dryskip '跳过 docker 可用性检查（dry-run 不依赖 docker）'
elif command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then ok "docker 可用（${COMPOSE[*]}）"
else bad 'docker 不可用（Linux 上通常需要加入 docker 组或用 sudo）'; exit 2; fi

NJ_ENV="$NIGHTJAR_DIR/.env"
NJ_SRC="$NJ_ENV"
if [ ! -f "$NJ_ENV" ]; then
  if [ "$DRY_RUN" = "1" ]; then drytag "复制 $NIGHTJAR_DIR/.env.example → $NJ_ENV"; NJ_SRC="$NIGHTJAR_DIR/.env.example"
  else cp "$NIGHTJAR_DIR/.env.example" "$NJ_ENV" && { fix "由 .env.example 生成 $NJ_ENV"; NJ_SRC="$NJ_ENV"; }; fi
fi
[ -f "$NJ_SRC" ] || { bad '缺少 .env / .env.example，无法继续'; exit 2; }

if [ "$DRY_RUN" = "1" ]; then
  TMP_WORK=$(mktemp -d); trap 'rm -rf "$TMP_WORK"' EXIT
  cp "$NJ_SRC" "$TMP_WORK/nightjar.env"; NJ_SRC="$TMP_WORK/nightjar.env"
fi
NJ="$NJ_SRC"

# ---------------------------------------------------------------------------
# 数据库卷是否已存在：PostgreSQL 的 POSTGRES_PASSWORD **只在数据卷为空时**生效，
# 卷已存在说明库里的口令早就定死了——此时改写 .env 只会让后端连不上（SQLSTATE 28P01）。
POSTGRES_VOLUME="middleware-ops_postgres-data"
POSTGRES_VOLUME_FOUND=0
if [ "$DRY_RUN" != "1" ] && docker volume inspect "$POSTGRES_VOLUME" >/dev/null 2>&1; then
  POSTGRES_VOLUME_FOUND=1
fi

step '1/7 生成缺失的密钥与口令'
ensure_secret() {
  local file="$1" key="$2" bytes="$3" desc="$4" cur
  cur=$(env_get "$file" "$key")
  if [ -z "$cur" ] || is_weak "$cur"; then set_env_reported "$file" "$key" "$(gen_secret "$bytes")"
  else ok "$key 已设置（$desc）"; fi
}
# 这两个是 compose 里用 ${VAR:?} 声明的**必填项**，缺了平台根本起不来。
ensure_secret "$NJ" JWT_SECRET 32 '平台 JWT 密钥（必填）'
ensure_secret "$NJ" ADMIN_PASSWORD 12 '平台管理员口令（必填）'
if [ "$DRY_RUN" = "1" ]; then
  dryskip "跳过数据库卷检测：正式运行时会先判断 $POSTGRES_VOLUME 是否存在，存在则**不**轮换 DB_PASSWORD"
fi
if [ "$POSTGRES_VOLUME_FOUND" = "1" ]; then
  # 已有数据卷：绝不轮换库口令，否则必然 28P01。
  cur_db_pw=$(env_get "$NJ" DB_PASSWORD)
  if [ -z "$cur_db_pw" ]; then
    set_env_reported "$NJ" DB_PASSWORD 'mwo_change_me'
    warn "数据库卷 $POSTGRES_VOLUME 已存在，但 .env 没有 DB_PASSWORD：已回填默认值 mwo_change_me"
    hint '若第 5 步对齐失败，说明原口令不是它——请在库里改口令或重建数据卷'
  else
    ok "DB_PASSWORD 保持不变（数据库卷已存在，PostgreSQL 不会重新应用口令）"
  fi
else
  ensure_secret "$NJ" DB_PASSWORD 16 '平台 PostgreSQL 口令（首次初始化）'
fi
ensure_secret "$NJ" REDIS_PASSWORD 16 '平台自身 Redis 口令'
ensure_secret "$NJ" GRAFANA_PASSWORD 12 'Grafana 管理员口令'
ensure_secret "$NJ" HOOK_TOKEN 24 '日志上报令牌（采集容器用）'

# ---------------------------------------------------------------------------
step '2/7 打开集成能力'
set_env_reported "$NJ" INTEGRATION_ENABLED 'true'
set_env_reported "$NJ" INTEGRATION_DOCKER_ENABLED 'true'
set_env_reported "$NJ" MWOPS_PROMETHEUS_BASE_URL 'http://prometheus:9090'
enable_docker_sock

# docker.sock 的权限：平台容器以非 root 用户运行，必须把宿主 docker 组的 GID
# 作为附加组加进容器，否则会报 "connect: permission denied"。
detect_docker_gid() {
  local gid=""
  if [ -S /var/run/docker.sock ]; then
    gid=$(stat -c '%g' /var/run/docker.sock 2>/dev/null || true)
  fi
  if [ -z "$gid" ] && command -v getent >/dev/null 2>&1; then
    gid=$(getent group docker 2>/dev/null | cut -d: -f3 || true)
  fi
  printf '%s' "$gid"
}
if [ "$DRY_RUN" = "1" ]; then
  dryskip "跳过 docker.sock 权限探测（正式运行会写入 DOCKER_GID）"
else
  DOCKER_SOCK_GID=$(detect_docker_gid)
  if [ -n "$DOCKER_SOCK_GID" ]; then
    set_env_reported "$NJ" DOCKER_GID "$DOCKER_SOCK_GID"
    if [ -S /var/run/docker.sock ] && [ "$(stat -c '%a' /var/run/docker.sock 2>/dev/null)" = "666" ]; then
      ok 'socket 权限为 666（任何用户可读写），DOCKER_GID 实际不影响使用'
    else
      ok "已按宿主 socket 写入 DOCKER_GID=$DOCKER_SOCK_GID（平台容器以非 root 用户运行，靠它才有权限）"
    fi
  else
    warn '未能探测 docker 组 GID（宿主上找不到 /var/run/docker.sock 或 docker 组）'
    hint 'Docker Desktop（macOS/Windows）通常无需设置；Linux 上手工填：stat -c %g /var/run/docker.sock'
  fi
fi

# Kafka 对外地址（KAFKA_ADVERTISED_HOST）：**被管服务器上的 Filebeat 用它连平台 Kafka**，
# 因此必须是"被管机可达"的地址，不能是 127.0.0.1（除非平台与被管机就是同一台）。
# 填错的现场极有迷惑性：Filebeat 能连上 9092 握手成功，随后被 Kafka 的 broker 元数据
# 引导去连它**自己**的 127.0.0.1，于是报 connection refused（见 docs/LOG_INTEGRATION.md）。
# 只在"空值或默认回环"时自动探测：用户显式填了域名/IP 就不覆盖。
detect_host_ip() {
  local ip=""
  ip=$(ip route get 1 2>/dev/null | awk '{for (i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')
  [ -z "$ip" ] && ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  printf '%s' "$ip"
}
CURRENT_KAFKA_HOST=$(env_get "$NJ" KAFKA_ADVERTISED_HOST)
if [ -z "$CURRENT_KAFKA_HOST" ] || [ "$CURRENT_KAFKA_HOST" = "127.0.0.1" ]; then
  if [ "$DRY_RUN" = "1" ]; then
    dryskip "跳过 Kafka 对外地址探测（正式运行会写入 KAFKA_ADVERTISED_HOST）"
  else
    HOST_IP=$(detect_host_ip)
    if [ -n "$HOST_IP" ]; then
      set_env_reported "$NJ" KAFKA_ADVERTISED_HOST "$HOST_IP"
      ok "已写入 KAFKA_ADVERTISED_HOST=$HOST_IP（跨机采集日志时被管机的 Filebeat 用它连平台）"
    else
      warn '未能探测宿主 IP：跨机采集时请手工把 KAFKA_ADVERTISED_HOST 改成被管机可达的平台地址'
      hint '取法：ip route get 1 | awk '\''{print $7; exit}'\''   或   hostname -I | awk '\''{print $1}'\'''
    fi
  fi
else
  ok "KAFKA_ADVERTISED_HOST 已是 $CURRENT_KAFKA_HOST（沿用已有配置）"
fi

# ---------------------------------------------------------------------------
step '3/7 端口错开（被占用时自动换端口）'
# 说明：宿主上已经有 Prometheus/Grafana/其他 Web 服务时，默认端口会冲突，
# 表现为 docker compose 报 "port is already allocated" —— 修的就是这里。
declare -a PORT_KEYS=(WEB_PORT PROMETHEUS_PORT GRAFANA_PORT KAFKA_PORT)
declare -a PORT_DEFAULTS=(8000 9090 3000 9092)
for idx in "${!PORT_KEYS[@]}"; do
  key="${PORT_KEYS[$idx]}"; want="${PORT_DEFAULTS[$idx]}"
  cur=$(port_of "$NJ" "$key"); [ -z "$cur" ] && cur="$want"
  if [ "$DRY_RUN" = "1" ]; then
    dryskip "检查 $key（当前 $cur）"
    continue
  fi
  if port_taken "$cur"; then
    new=$(next_free_port "$cur")
    if [ "$new" = "$cur" ]; then
      bad "$key=$cur 已被占用，且连续 30 个端口都不可用"
      hint "手工在 $NJ 里把 $key 改成空闲端口后重跑"
    else
      set_env_reported "$NJ" "$key" "$new"
      warn "$key=$cur 已被别的进程占用 → 已改用 $new"
    fi
  else
    ok "$key=$cur 可用"
  fi
done

# ---------------------------------------------------------------------------
step '4/7 写回 .env'
if [ "$DRY_RUN" = "1" ]; then
  dryskip 'dry-run：不写回 .env（上面 [FIX] 即正式运行的改动）'
else
  ts=$(date +%Y%m%d%H%M%S)
  cp "$NJ" "$NJ.bak-$ts" && info "已写回 $NJ（备份：.env.bak-$ts）"
fi

# ---------------------------------------------------------------------------
step '5/7 数据库口令对齐'
# 背景（真实故障）：PostgreSQL 的 POSTGRES_PASSWORD 只在数据卷为空时生效。
# 平台之前启动过、数据卷里已经存了旧口令，此时只改 .env 会让后端报
#   FATAL: password authentication failed for user "mwo" (SQLSTATE 28P01)
# 容器内的本地 socket 是 trust 认证，因此平台可以**用新口令直接改库内口令**，
# 数据一行不丢；这比"重建数据卷"温和得多。
reconcile_db_password() {
  local pw user db i
  pw=$(env_get "$NJ" DB_PASSWORD); user=$(env_get "$NJ" DB_USER); db=$(env_get "$NJ" DB_NAME)
  [ -z "$user" ] && user='mwo'
  [ -z "$db" ] && db='middleware_ops'
  if [ "$POSTGRES_VOLUME_FOUND" != "1" ]; then
    info "数据库卷 $POSTGRES_VOLUME 尚未创建：首次初始化会直接使用 .env 里的口令，无需对齐"
    return
  fi
  if [ -z "$pw" ]; then
    bad 'DB_PASSWORD 为空：数据库卷已存在，必须先确定原口令（或在库内改口令）'
    return
  fi
  # 只起 postgres，起完再对齐，避免后端先崩一堆日志
  ( cd "$NIGHTJAR_DIR" && "${COMPOSE[@]}" up -d postgres >/dev/null 2>&1 ) \
    || { warn 'postgres 未能启动，跳过口令对齐（稍后可重跑本脚本）'; return; }
  for i in $(seq 1 30); do
    docker exec mwops-postgres pg_isready -U "$user" -d "$db" >/dev/null 2>&1 && break
    sleep 2
  done
  if ! docker exec mwops-postgres pg_isready -U "$user" -d "$db" >/dev/null 2>&1; then
    warn 'postgres 未在 60s 内就绪，跳过错位对齐'
    hint "docker logs mwops-postgres 查看失败原因"
    return
  fi
  # 走 TCP（scram 认证）验证 .env 里的口令是否真的能用
  if docker exec -e PGPASSWORD="$pw" mwops-postgres psql -h 127.0.0.1 -U "$user" -d "$db" -tAc 'select 1' >/dev/null 2>&1; then
    ok "库内口令与 .env 一致（用户 $user）"
    return
  fi
  warn "库内口令与 .env 不一致（就是 28P01）：正在把库内口令对齐为 .env 中的值"
  # 本地 socket 为 trust，无需原口令；口令是十六进制，SQL 里不需要转义
  if docker exec mwops-postgres psql -U "$user" -d "$db" -tAc "ALTER USER \"$user\" WITH PASSWORD '$pw';" >/dev/null 2>&1; then
    if docker exec -e PGPASSWORD="$pw" mwops-postgres psql -h 127.0.0.1 -U "$user" -d "$db" -tAc 'select 1' >/dev/null 2>&1; then
      fix "已把 PostgreSQL 口令对齐为 $NJ 中的 DB_PASSWORD（用户 $user，数据未受影响）"
    else
      bad '口令已改但仍无法连接：请检查 DB_USER / DB_NAME 是否与卷内初始化时一致'
    fi
  else
    bad '口令对齐失败（可能在库内该用户不存在，例如 DB_USER 被改过）'
    hint "手工执行：docker exec mwops-postgres psql -U $user -c \"ALTER USER $user WITH PASSWORD '<新口令>';\""
    hint "或彻底重建平台数据卷（会清空集成/告警/审计数据）：cd $NIGHTJAR_DIR && ${COMPOSE[*]} down -v"
  fi
}
if [ "$DRY_RUN" = "1" ]; then
  dryskip 'dry-run：跳过数据库口令对齐'
elif [ "$SKIP_START" = "1" ]; then
  info '按 --skip-start 跳过（口令对齐需要 postgres 在运行）'
else
  reconcile_db_password
fi

# ---------------------------------------------------------------------------
step '6/7 启动平台'
if [ "$SKIP_START" = "1" ]; then
  info '按 --skip-start 跳过启动'
elif [ "$DRY_RUN" = "1" ]; then
  drytag "cd $NIGHTJAR_DIR && ${COMPOSE[*]} up -d --build"
else
  # 先做配置预检：${VAR:?} 缺失、YAML 语法错误在这一步就能拿到**准确**信息
  if ! cfg_err=$(cd "$NIGHTJAR_DIR" && "${COMPOSE[@]}" config -q 2>&1); then
    bad 'docker compose 配置预检未通过（还没开始构建）'
    printf '%s\n' "$cfg_err" | sed 's/^/         /'
    hint "按上面的提示改 $NJ（必填项：JWT_SECRET、ADMIN_PASSWORD）后重跑本脚本"
  else
    ok 'docker compose 配置预检通过（必填项齐全、YAML 合法）'
    log=$(mktemp)
    info "▶ ${COMPOSE[*]} up -d --build"
    if ( cd "$NIGHTJAR_DIR" && "${COMPOSE[@]}" up -d --build ) 2>&1 | tee "$log"; then
      ok '启动命令执行完成'
    else
      code=$?
      bad "平台启动失败（退出码 $code）"
      printf '\n%s--- docker compose 输出（末尾 30 行）---%s\n' "$C_GRAY" "$C_RESET"
      tail -n 30 "$log" | sed 's/^/  /'
      printf '%s--------------------------------------%s\n' "$C_GRAY" "$C_RESET"
      printf '\n%s最常见的原因与处理：%s\n' "$C_YELLOW" "$C_RESET"
      if grep -qiE '28P01|password authentication failed' "$log"; then
        printf '  · 数据库口令与已有数据卷不一致（SQLSTATE 28P01）：\n'
        printf '    PostgreSQL 只在数据卷为空时应用 POSTGRES_PASSWORD，卷存在时改 .env 不生效。\n'
        printf '    ① 首选：重跑本脚本（第 5 步会用容器内 trust socket 把库内口令对齐到 .env）\n'
        printf '    ② 或手工：docker exec mwops-postgres psql -U %s -c "ALTER USER %s WITH PASSWORD '\''<新口令>'\'';"\n' \
          "$(env_get "$NJ" DB_USER)" "$(env_get "$NJ" DB_USER)"
        printf '    ③ 或重建平台数据卷（会清空集成/告警/审计数据）：%s down -v\n' "${COMPOSE[*]}"
      fi
      if grep -qiE 'role .* does not exist' "$log"; then
        printf '  · 数据库用户不存在：%s 里的 DB_USER 与数据卷初始化时用的用户名不一致；\n' "$NJ"
        printf '    改回原用户名，或重建数据卷（%s down -v）\n' "${COMPOSE[*]}"
      fi
      if grep -qiE 'port is already allocated|address already in use' "$log"; then
        printf '  · 端口被占用：本应已被第 3 步自动错开。若仍冲突，说明占用者是运行中的容器，\n'
        printf '    用 docker ps 找到它并停掉，或在 %s 里手工把 WEB_PORT/PROMETHEUS_PORT/GRAFANA_PORT 改开\n' "$NJ"
      fi
      if grep -qiE 'no such file or directory|error while creating mount source path' "$log"; then
        printf '  · 挂载路径不存在：docker.sock 或某个 volume 路径在宿主上缺失；Windows/macOS 上请确认\n'
        printf '    Docker Desktop 已开启「file sharing」，Linux 上确认 /var/run/docker.sock 存在\n'
      fi
      if grep -qiE 'pull access denied|manifest unknown|failed to resolve|i/o timeout|TLS handshake' "$log"; then
        printf '  · 拉取镜像失败：基础镜像（postgres/redis/prometheus/grafana/nginx）没拉到；\n'
        printf '    检查网络或给 Docker 配置镜像加速器后重试\n'
      fi
      if grep -qiE 'Cannot connect to the Docker daemon|permission denied' "$log"; then
        printf '  · 无法访问 docker：确认当前用户在 docker 组、或改用 sudo 执行本脚本\n'
      fi
      if grep -qiE 'invalid compose project|error while interpolating|required variable|is required' "$log"; then
        printf '  · .env 有必填项缺失或格式错误：见上面的原始信息，改 %s 后重跑\n' "$NJ"
      fi
      printf '  · 完整日志：cd %s && %s up -d --build（前台重跑可看到全部输出）\n' "$NIGHTJAR_DIR" "${COMPOSE[*]}"
      printf '  · 或看容器状态：%s ps -a\n' "${COMPOSE[*]}"
      rm -f "$log"
    fi
    rm -f "$log" 2>/dev/null
  fi
fi

# ---------------------------------------------------------------------------
step '7/7 自检（平台视角）'
if [ "$DRY_RUN" = "1" ]; then
  dryskip '跳过容器检查（dry-run）'
else
  for name in mwops-backend mwops-prometheus mwops-grafana; do
    state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null)
    [ "$state" = 'running' ] && ok "$name 运行中" || bad "$name 未运行（状态：${state:-不存在}）"
  done

  if docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' mwops-backend 2>/dev/null | grep -q '/var/run/docker.sock'; then
    ok 'mwops-backend 已挂载 docker.sock（平台可自动发现并接入目标网络）'
  else
    bad 'mwops-backend 未挂载 docker.sock：平台无法自动发现目标网络'
    hint '取消 docker-compose.yml 中 backend.volumes 的 docker.sock 注释后重新 up -d'
  fi

  discover_targets
  if [ "${#TARGETS[@]}" -eq 0 ]; then
    warn '没有发现可纳管的容器（docker ps 里除平台自身外为空）'
    hint '启动被管项目后重跑，或用 --targets app-mysql,app-redis 指定'
  fi
  for name in "${TARGETS[@]}"; do
    [ -z "$name" ] && continue
    state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null)
    if [ -z "$state" ]; then
      warn "未找到容器 $name（若尚未启动被管项目属正常）"
      continue
    fi
    nets=$(container_networks "$name")
    if [ "$state" = 'running' ]; then
      ok "$name 运行中，所在网络：${nets:-（无）}"
      hint "集成中心地址直接填 $name:端口 即可（平台会自动接入上述网络）"
    else
      warn "$name 状态为 $state（不是 running）"
    fi
  done
fi

# ---------------------------------------------------------------------------
step '结果汇总'
if [ "${#CHANGES[@]}" -eq 0 ]; then
  printf '  %s没有任何需要修改的内容（幂等：重复执行是安全的）%s\n' "$C_GREEN" "$C_RESET"
else
  printf '  %s本次共 %d 项变更：%s\n' "$C_YELLOW" "${#CHANGES[@]}" "$C_RESET"
  for c in "${CHANGES[@]}"; do printf '    - %s\n' "$c"; done
fi
if [ "${#PROBLEMS[@]}" -gt 0 ]; then
  printf '  %s%d 项需要处理：%s\n' "$C_RED" "${#PROBLEMS[@]}" "$C_RESET"
  for p in "${PROBLEMS[@]}"; do printf '    ! %s\n' "$p"; done
fi

WEB_PORT_OUT=$(env_get "$NJ" WEB_PORT); [ -z "$WEB_PORT_OUT" ] && WEB_PORT_OUT=8000
GRAF_PORT_OUT=$(env_get "$NJ" GRAFANA_PORT); [ -z "$GRAF_PORT_OUT" ] && GRAF_PORT_OUT=3000
printf '\n%s接下来（全部在平台上点，被管项目无需任何改动）%s\n' "$C_CYAN" "$C_RESET"
printf '  1. 平台入口   http://127.0.0.1:%s（管理员 %s）\n' "$WEB_PORT_OUT" "$(env_get "$NJ" ADMIN_USER)"
printf '  2. 集成中间件  在「集成中心」选组件 → 填地址（用 docker ps 里的容器名或 IP:端口）\n'
printf '                 MySQL/PostgreSQL 的只读账号默认由平台自动创建，只需填一次管理员凭据\n'
printf '  3. 部署位置    本机（平台用 Docker 创建 Exporter）或远程服务器（Ansible 一键安装）\n'
printf '  4. 日志接入    目标容器名（脚本上面的自检已列出可选项）\n'
printf '  5. 账号管理    集成中心 → 监控账号（查看来源 / 轮换口令 / 删除账号）\n'
printf '  6. 看大盘      http://127.0.0.1:%s（数据源已自动配置）\n' "$GRAF_PORT_OUT"
printf '  7. 逐项体检    %s/scripts/doctor.sh\n' "$NIGHTJAR_DIR"

[ "${#PROBLEMS[@]}" -gt 0 ] && exit 1
exit 0
