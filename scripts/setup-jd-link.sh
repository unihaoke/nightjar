#!/usr/bin/env bash
# =============================================================================
# jd ⇄ nightjar 一键接入 / 一键修复（Linux / macOS，纯 bash）
#
# 目标：**你只需要维护两份 .env，其余全部由脚本推导并写回**。
# 与 PowerShell 版（setup-jd-link.ps1）行为一致，Linux 上无需安装 pwsh。
#
# 它替你做的（对应以前要手工改文件的地方）：
#   1. 生成缺失的密钥/口令（十六进制，顺带避开"口令含 @ ( ) / 导致 DSN 解析失败"）；
#   2. 对齐必须一致的项：HOOK_TOKEN == NIGHTJAR_HOOK_TOKEN、JD_NIGHTJAR_NETWORK、
#      MWOPS_PROMETHEUS_BASE_URL、INTEGRATION_EXPORTER_NETWORK；
#   3. 实例名唯一真源：JD_*_INSTANCE_NAME → 同步 prometheus-jd.yml 的 relabel
#      （jd 与 nightjar 两份副本）与 agent.yaml 的令牌/地址；
#   4. 端口错开（两个栈的 Prometheus / Web 端口不能相同）；
#   5. 用 .env 的口令重写 01-monitor-user.sql 与 my.cnf；
#   6. 创建 internal 互联网络、按正确 overlay 启动两栈、补建 MySQL 只读账号；
#   7. 清掉历史遗留的手工网络（手工 connect 造成的状态漂移）；
#   8. 体检：容器网络挂载、跨栈解析、Prometheus 里真实的 instance_name。
#
# 用法：
#   ./scripts/setup-jd-link.sh --dry-run          # 先预览（不写文件、不调 docker）
#   ./scripts/setup-jd-link.sh                    # 正式执行（幂等，可重复跑）
#   ./scripts/setup-jd-link.sh --fix-instances    # 顺带纠正平台纳管实例字段
#   ./scripts/setup-jd-link.sh --mode mwops-prometheus
#   ./scripts/setup-jd-link.sh --jd-dir ../jd --nightjar-dir .
# =============================================================================

set -uo pipefail

# 不用 set -e：这是幂等修复脚本，单条 docker 命令失败（如容器不存在）不应中断后续检查。
# 每一步都显式判断返回码并给出修复建议。

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
JD_DIR="$(cd "$NIGHTJAR_DIR/.." 2>/dev/null && pwd)/jd"
MODE="jd-prometheus"
DRY_RUN=0
SKIP_START=0
FIX_INSTANCES=0

CHANGES=()
PROBLEMS=()

# ---------------------------------------------------------------------------
# 输出
# ---------------------------------------------------------------------------
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

usage() {
  sed -n '2,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 0
}

# ---------------------------------------------------------------------------
# 参数
# ---------------------------------------------------------------------------
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run|-n)      DRY_RUN=1 ;;
    --skip-start)      SKIP_START=1 ;;
    --fix-instances)   FIX_INSTANCES=1 ;;
    --mode)            MODE="${2:-}"; shift ;;
    --jd-dir)          JD_DIR="${2:-}"; shift ;;
    --nightjar-dir)    NIGHTJAR_DIR="${2:-}"; shift ;;
    -h|--help)         usage ;;
    *) printf '未知参数：%s（用 --help 查看用法）\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

case "$MODE" in
  jd-prometheus|mwops-prometheus) ;;
  *) printf '非法 --mode：%s（可选 jd-prometheus / mwops-prometheus）\n' "$MODE" >&2; exit 2 ;;
esac

# docker compose 命令探测：优先 compose v2 插件，退回 docker-compose v1
if docker compose version >/dev/null 2>&1; then
  COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=(docker-compose)
else
  COMPOSE=(docker compose)   # 保留占位，后续会给出明确报错
fi

# ---------------------------------------------------------------------------
# .env 读写（保持注释与键顺序）
# ---------------------------------------------------------------------------
env_get() { # env_get FILE KEY
  local file="$1" key="$2"
  [ -f "$file" ] || return 0
  sed -n -E "s/^[[:space:]]*${key}[[:space:]]*=(.*)$/\1/p" "$file" | head -n1
}

# env_set 会输出变更说明（供 fix/warn 使用），返回 0 表示有改动、1 表示未改动
env_set() { # env_set FILE KEY VALUE
  local file="$1" key="$2" value="${3:-}"
  local esc old
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

# 密钥类变更一律脱敏：终端 scrollback 与 CI 日志都不该出现新口令
mask_if_secret() { # mask_if_secret KEY VALUE
  local key="$1"
  case "$key" in
    *SECRET|*PASSWORD|*PASS|*TOKEN|*_KEY) printf '******' ;;
    *) printf '%s' "$2" ;;
  esac
}

set_env_reported() { # set_env_reported FILE KEY VALUE
  local out
  if out=$(env_set "$1" "$2" "${3:-}"); then fix "$out"; else ok "$2 已是最新"; fi
}

# ---------------------------------------------------------------------------
# 工具函数
# ---------------------------------------------------------------------------
gen_secret() { # gen_secret [BYTES] → 十六进制随机串（无特殊字符）
  local bytes="${1:-24}"
  if command -v openssl >/dev/null 2>&1; then
    openssl rand -hex "$bytes"
  else
    head -c "$bytes" /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

WEAK_VALUES="please_replace_with_a_random_48_byte_string dev-secret-change-me-in-production-please-2026 admin@12345 admin root password 123456 mwo_change_me redis_change_me jd_redis_change_me exporter_change_me"

is_weak() { # is_weak VALUE
  local v
  v=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  for w in $WEAK_VALUES; do [ "$v" = "$w" ] && return 0; done
  return 1
}

port_of() { # port_of FILE KEY → 缺失/非法时返回空
  local v
  v=$(env_get "$1" "$2")
  case "$v" in ''|*[!0-9]*) printf '' ;; *) printf '%s' "$v" ;; esac
}

docker_ok() { [ "$DRY_RUN" = "1" ] && return 0; command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; }

# 从 Prometheus 的 label values 响应里取出字符串数组（优先 python3，退回 sed）
json_array() { # json_array < stdin
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import sys,json
try:
    d=json.load(sys.stdin)
    print("\n".join(d.get("data") or []))
except Exception:
    pass' 2>/dev/null
  else
    sed -n 's/.*"data":\[\([^]]*\)\].*/\1/p' | tr ',' '\n' | tr -d ' "' | sed '/^$/d'
  fi
}

http_get() { curl -fsS --max-time 5 "$1" 2>/dev/null; }

join_by() { local sep="$1"; shift; local first=1; for x in "$@"; do [ $first = 1 ] && first=0 || printf '%s' "$sep"; printf '%s' "$x"; done; }

# ---------------------------------------------------------------------------
# 0. 前置检查
# ---------------------------------------------------------------------------
printf '%sjd ⇄ nightjar 一键接入%s\n' "$C_BOLD" "$C_RESET"
printf '  nightjar 目录 : %s\n' "$NIGHTJAR_DIR"
printf '  jd 目录       : %s\n' "$JD_DIR"
if [ "$MODE" = "jd-prometheus" ]; then
  printf '  取数方式      : %s（复用 jd 自带的 Prometheus）\n' "$MODE"
else
  printf '  取数方式      : %s（平台自带 Prometheus + 集成中心）\n' "$MODE"
fi
[ "$DRY_RUN" = "1" ] && printf '  %s模式          : DRY-RUN（不写文件、不调用 docker）%s\n' "$C_YELLOW" "$C_RESET"

step '0/8 前置检查'
[ -d "$NIGHTJAR_DIR" ] || { bad "nightjar 目录不存在：$NIGHTJAR_DIR"; exit 2; }
[ -d "$JD_DIR" ] || { bad "jd 目录不存在：$JD_DIR"; hint '用 --jd-dir 指定 jd 项目路径'; exit 2; }
if [ "$DRY_RUN" = "1" ]; then
  dryskip '跳过 docker 可用性检查（dry-run 不依赖 docker）'
elif docker_ok; then
  ok "docker 可用（${COMPOSE[*]}）"
else
  bad 'docker 不可用：请确认已安装 docker 且当前用户能执行 docker（Linux 上通常需要加入 docker 组或用 sudo）'
  exit 2
fi

NJ_ENV="$NIGHTJAR_DIR/.env"
JD_ENV="$JD_DIR/.env"
NJ_SRC="$NJ_ENV"; JD_SRC="$JD_ENV"
if [ ! -f "$NJ_ENV" ]; then
  if [ "$DRY_RUN" = "1" ]; then drytag "复制 $NIGHTJAR_DIR/.env.example → $NJ_ENV"; NJ_SRC="$NIGHTJAR_DIR/.env.example"
  else cp "$NIGHTJAR_DIR/.env.example" "$NJ_ENV" && { fix "由 .env.example 生成 $NJ_ENV"; NJ_SRC="$NJ_ENV"; }; fi
fi
if [ ! -f "$JD_ENV" ]; then
  if [ "$DRY_RUN" = "1" ]; then drytag "复制 $JD_DIR/.env.example → $JD_ENV"; JD_SRC="$JD_DIR/.env.example"
  else cp "$JD_DIR/.env.example" "$JD_ENV" && { fix "由 .env.example 生成 $JD_ENV"; JD_SRC="$JD_ENV"; }; fi
fi
[ -f "$NJ_SRC" ] && [ -f "$JD_SRC" ] || { bad '缺少 .env / .env.example，无法继续'; exit 2; }
ok "读取 nightjar 配置：$(basename "$NJ_SRC")"
ok "读取 jd 配置：$(basename "$JD_SRC")"

# dry-run 时直接在 .env.example 上做内存推演会写坏模板，因此复制到临时目录
if [ "$DRY_RUN" = "1" ]; then
  TMP_WORK=$(mktemp -d)
  trap 'rm -rf "$TMP_WORK"' EXIT
  cp "$NJ_SRC" "$TMP_WORK/nightjar.env"; NJ_SRC="$TMP_WORK/nightjar.env"
  cp "$JD_SRC" "$TMP_WORK/jd.env";      JD_SRC="$TMP_WORK/jd.env"
fi

# 从这一步开始，环境变量改动都作用在 $NJ_SRC / $JD_SRC 上（dry-run 时是临时副本）
NJ="$NJ_SRC"; JD="$JD_SRC"

# ---------------------------------------------------------------------------
# 1. 生成缺失的密钥与口令
# ---------------------------------------------------------------------------
step '1/8 补齐密钥与口令（已有值一律保留）'

# 密钥类：为空或仍是示例值时自动生成（改了只影响会话，安全）
ensure_secret() { # ensure_secret FILE KEY BYTES DESC
  local file="$1" key="$2" bytes="$3" desc="$4" cur
  cur=$(env_get "$file" "$key")
  if [ -z "$cur" ] || is_weak "$cur"; then
    set_env_reported "$file" "$key" "$(gen_secret "$bytes")"
  else
    ok "$key 已设置（$desc）"
  fi
}

# 口令类：只在为空时生成。
# 为什么不自动替换弱口令：MySQL 的 MYSQL_ROOT_PASSWORD / PostgreSQL 的 POSTGRES_PASSWORD
# 只在数据卷首次初始化时生效，事后改 .env 不会改库里的口令，改了反而连不上。
ensure_password() { # ensure_password FILE KEY BYTES DESC IMPACT
  local file="$1" key="$2" bytes="$3" desc="$4" impact="$5" cur
  cur=$(env_get "$file" "$key")
  if [ -z "$cur" ]; then
    set_env_reported "$file" "$key" "$(gen_secret "$bytes")"
  elif is_weak "$cur"; then
    warn "$key 仍是示例/弱口令（$desc）：$impact"
  else
    ok "$key 已设置（$desc）"
  fi
}

ensure_secret   "$NJ" JWT_SECRET 32 '平台 JWT 密钥'
ensure_password "$NJ" DB_PASSWORD 16 '平台 PostgreSQL 口令' '已有数据卷时不要直接改，需先 ALTER USER 改库内口令再同步 .env'
ensure_password "$NJ" REDIS_PASSWORD 16 '平台自身 Redis 口令' 'Redis 口令每次启动都会生效，直接改是安全的（会重建容器）'
ensure_secret   "$NJ" ADMIN_PASSWORD 12 '平台管理员口令'
ensure_secret   "$NJ" HOOK_TOKEN 24 '日志上报令牌'

ensure_secret   "$JD" JWT_SECRET 32 'jd 应用 JWT 密钥'
ensure_password "$JD" DB_PASS 16 'jd MySQL root 口令' 'MySQL 只在空数据卷首启时应用该口令，已有数据卷请用 ALTER USER 改库内口令后同步 .env'
ensure_password "$JD" REDIS_PASSWORD 16 'jd Redis 口令' 'Redis 口令每次启动都会生效，直接改是安全的（会重建容器）'
ensure_password "$JD" MYSQL_EXPORTER_PASSWORD 16 'MySQL 只读监控账号口令' '脚本会把新口令同步写入 01-monitor-user.sql 与 my.cnf，重跑 initdb 即生效'

# ---------------------------------------------------------------------------
# 2. 双向对齐必须一致的项
# ---------------------------------------------------------------------------
step '2/8 对齐跨项目必须一致的值'

network=$(env_get "$NJ" JD_NIGHTJAR_NETWORK); [ -z "$network" ] && network='jd-nightjar'
hook=$(env_get "$NJ" HOOK_TOKEN)
jd_hook=$(env_get "$JD" NIGHTJAR_HOOK_TOKEN)
if [ "$hook" != "$jd_hook" ]; then set_env_reported "$JD" NIGHTJAR_HOOK_TOKEN "$hook"
else ok 'HOOK_TOKEN 与 NIGHTJAR_HOOK_TOKEN 一致'; fi

set_env_reported "$NJ" JD_NIGHTJAR_NETWORK "$network"
set_env_reported "$JD" JD_NIGHTJAR_NETWORK "$network"

if [ "$MODE" = 'jd-prometheus' ]; then want_base='http://jd-prometheus:9090'; else want_base='http://prometheus:9090'; fi
set_env_reported "$NJ" MWOPS_PROMETHEUS_BASE_URL "$want_base"
set_env_reported "$JD" NIGHTJAR_URL 'http://mwops-backend:8080'

# 平台 compose 项目名来自 docker-compose.yml 的 name:（默认 middleware-ops）
# 网络名 = <项目名>_mwops；这里优先探测实际存在的网络，探测不到再用默认值。
mwops_network=$(docker network ls --format '{{.Name}}' 2>/dev/null | grep -E '_mwops$' | head -n1)
[ -z "$mwops_network" ] && mwops_network='middleware-ops_mwops'
set_env_reported "$NJ" INTEGRATION_EXPORTER_NETWORK "$mwops_network,$network"

redis_name=$(env_get "$JD" JD_REDIS_INSTANCE_NAME); [ -z "$redis_name" ] && redis_name='jd-redis'
mysql_name=$(env_get "$JD" JD_MYSQL_INSTANCE_NAME); [ -z "$mysql_name" ] && mysql_name='jd-mysql'
set_env_reported "$JD" JD_REDIS_INSTANCE_NAME "$redis_name"
set_env_reported "$JD" JD_MYSQL_INSTANCE_NAME "$mysql_name"
ok "实例名（Prometheus instance_name）：redis=$redis_name，mysql=$mysql_name"

# ---------------------------------------------------------------------------
# 3. 端口错开
# ---------------------------------------------------------------------------
step '3/8 端口错开（两个栈不能占用同一个宿主端口）'

nj_prom=$(port_of "$NJ" PROMETHEUS_PORT)
jd_prom=$(port_of "$JD" PROMETHEUS_PORT)
if [ -z "$nj_prom" ] && [ -z "$jd_prom" ]; then nj_prom=9090; jd_prom=9091; fi
if [ -z "$nj_prom" ]; then if [ "$jd_prom" = "9090" ]; then nj_prom=9091; else nj_prom=9090; fi; fi
if [ -z "$jd_prom" ]; then if [ "$nj_prom" = "9090" ]; then jd_prom=9091; else jd_prom=9090; fi; fi
if [ "$nj_prom" = "$jd_prom" ]; then
  # 约定：nightjar 保持 9090，把 jd 挪到 9091（与 jd/.env.example 注释一致）
  if [ "$nj_prom" = "9090" ]; then jd_prom=9091; else nj_prom=9090; fi
fi
set_env_reported "$NJ" PROMETHEUS_PORT "$nj_prom"
set_env_reported "$JD" PROMETHEUS_PORT "$jd_prom"
ok "Prometheus 宿主端口：nightjar=$nj_prom，jd=$jd_prom"

nj_web=$(port_of "$NJ" WEB_PORT); [ -z "$nj_web" ] && nj_web=8000
jd_web=$(port_of "$JD" WEB_PORT); [ -z "$jd_web" ] && jd_web=8542
if [ "$nj_web" = "$jd_web" ]; then
  if [ "$jd_web" = "8000" ]; then nj_web=8001; else nj_web=8000; fi
fi
set_env_reported "$NJ" WEB_PORT "$nj_web"
set_env_reported "$JD" WEB_PORT "$jd_web"
ok "Web 宿主端口：nightjar=$nj_web，jd=$jd_web"

# ---------------------------------------------------------------------------
# 4. 写回 .env（带备份）
# ---------------------------------------------------------------------------
step '4/8 写回 .env'
if [ "$DRY_RUN" = "1" ]; then
  dryskip 'dry-run：不写回 .env（上面 [FIX]/[预览] 即正式运行时将发生的改动）'
else
  ts=$(date +%Y%m%d%H%M%S)
  cp "$NJ" "$NJ.bak-$ts" && info "已写回 $NJ（备份：$(basename "$NJ").bak-$ts）"
  cp "$JD" "$JD.bak-$ts" && info "已写回 $JD（备份：$(basename "$JD").bak-$ts）"
fi

# ---------------------------------------------------------------------------
# 5. 从 .env 推导「以前要手工改」的文件
# ---------------------------------------------------------------------------
step '5/8 由 .env 推导派生的配置文件'

# replace_lines：用 awk 程序重写文件，内容不变则不落盘（避免无意义 mtime 变化）
apply_rewrite() { # apply_rewrite FILE DESC MASKED AWK_PROGRAM [AWK_VARS...]
  local file="$1" desc="$2" masked="$3" prog="$4"; shift 4
  if [ ! -f "$file" ]; then info "跳过不存在的文件：$file"; return 0; fi
  local tmp; tmp=$(mktemp)
  awk "$@" "$prog" "$file" > "$tmp" 2>/dev/null
  if cmp -s "$file" "$tmp"; then
    rm -f "$tmp"; ok "$desc 已是最新"
  elif [ "$DRY_RUN" = "1" ]; then
    rm -f "$tmp"; drytag "$desc → $masked（$file）"
  else
    mv "$tmp" "$file"; fix "$desc → $masked（$file）"
  fi
}

monitor_password=$(env_get "$JD" MYSQL_EXPORTER_PASSWORD)
if [ -n "$monitor_password" ]; then
  # 只替换 SQL 语句里的 BY '<口令>'，不动注释里的占位说明
  apply_rewrite "$JD_DIR/deploy/jd-exporters/init/01-monitor-user.sql" \
    'MySQL 监控账号口令（01-monitor-user.sql）' 'BY ******' \
    '/^[[:space:]]*(CREATE|ALTER)[[:space:]]+USER/ { sub(/BY[[:space:]]+'"'"'[^'"'"']*'"'"'/, "BY '"'"'" pw "'"'"'") } { print }' \
    -v pw="$monitor_password"

  apply_rewrite "$JD_DIR/deploy/jd-exporters/my.cnf" 'my.cnf 的 exporter 口令' 'password = ******' \
    '/^[[:space:]]*password[[:space:]]*=/ { sub(/=.*/, "= " pw) } { print }' \
    -v pw="$monitor_password"
fi

# 实例名：JD_*_INSTANCE_NAME 是唯一真源，这里铺开到 relabel 与 agent.yaml。
# 平台按 instance_name 定位实例，而该标签值写在 prometheus-jd.yml 的 relabel replacement 里，
# 以前改名要同时改 .env、relabel、平台实例名三处，漏一处就出现"up=1 但 matched=0"。
for relabel in "$JD_DIR/deploy/jd-exporters/prometheus-jd.yml" \
               "$NIGHTJAR_DIR/deploy/jd-exporters/prometheus-jd.yml"; do
  apply_rewrite "$relabel" 'Prometheus relabel 实例名' "redis=$redis_name mysql=$mysql_name" \
    '/job_name:/ { job=$0; sub(/.*job_name:[[:space:]]*/, "", job); gsub(/['"'"'"]/, "", job) }
     /^[[:space:]]*replacement:/ {
       if (job == "middleware-exporter-redis" && $2 != r) { sub(/replacement:.*/, "replacement: " r) }
       else if (job == "middleware-exporter-mysql" && $2 != m) { sub(/replacement:.*/, "replacement: " m) }
     }
     { print }' \
    -v r="$redis_name" -v m="$mysql_name"
done

agent_yaml="$JD_DIR/deploy/jd-exporters/agent.yaml"
if [ -f "$agent_yaml" ]; then
  apply_rewrite "$agent_yaml" 'agent.yaml 的上报令牌' 'hook_token: ******' \
    '/^[[:space:]]*hook_token:/ { sub(/:.*/, ": " t) } { print }' -v t="$hook"
  apply_rewrite "$agent_yaml" 'agent.yaml 的平台地址' "platform_url: $(env_get "$JD" NIGHTJAR_URL)" \
    '/^[[:space:]]*platform_url:/ { sub(/:.*/, ": " u) } { print }' -v u="$(env_get "$JD" NIGHTJAR_URL)"
else
  info '跳过不存在的文件：agent.yaml（未使用官方 Agent 时属正常）'
fi

# 一键部署需要 docker.sock：只提示，不擅自改 compose
if [ "$(env_get "$NJ" INTEGRATION_DOCKER_ENABLED)" = 'true' ]; then
  if grep -qE '^[[:space:]]*#[[:space:]]*-[[:space:]]*/var/run/docker\.sock' "$NIGHTJAR_DIR/docker-compose.yml" 2>/dev/null; then
    bad 'INTEGRATION_DOCKER_ENABLED=true，但 docker-compose.yml 里的 docker.sock 挂载仍是注释状态'
    hint '取消 docker-compose.yml 中 backend.volumes 的 "/var/run/docker.sock:/var/run/docker.sock" 注释'
  else
    ok 'docker.sock 已挂载（一键部署可用）'
  fi
fi

# ---------------------------------------------------------------------------
# 6. 网络与启动
# ---------------------------------------------------------------------------
step '6/8 互联网络与启动'

if [ "$DRY_RUN" = "1" ]; then
  drytag "docker network create --driver bridge --internal $network（不存在时）"
else
  internal=$(docker network inspect "$network" --format '{{.Internal}}' 2>/dev/null)
  if [ -z "$internal" ]; then
    info "创建跨栈网络 $network（internal）"
    docker network create --driver bridge --internal "$network" >/dev/null 2>&1 \
      && fix "创建 internal 网络 $network" || bad "创建网络 $network 失败"
  elif [ "$internal" != 'true' ]; then
    bad "$network 已存在但不是 internal 网络（Internal=$internal）"
    hint "docker network rm $network 后重跑本脚本（先确认没有容器在用）"
  else
    ok "$network 存在且为 internal"
  fi
fi

compose_run() { # compose_run DIR ARGS...
  local dir="$1"; shift
  if [ "$DRY_RUN" = "1" ]; then drytag "cd $dir && ${COMPOSE[*]} $*"; return 0; fi
  info "▶ ${COMPOSE[*]} $*"
  ( cd "$dir" && "${COMPOSE[@]}" "$@" ) || { bad "${COMPOSE[*]} $* 执行失败"; return 1; }
}

if [ "$SKIP_START" = "1" ]; then
  info '按 --skip-start 跳过启动'
else
  if [ "$MODE" = 'jd-prometheus' ]; then
    jd_overlay='deploy/jd-exporters/docker-compose.jd.yml'
  else
    jd_overlay='deploy/jd-exporters/docker-compose.jd-link.yml'
  fi
  jd_args=(-f docker-compose.yml -f "$jd_overlay")
  # 已配置日志令牌就顺带带上 logs profile，日志 Agent 一起起来
  [ -n "$(env_get "$JD" NIGHTJAR_HOOK_TOKEN)" ] && jd_args+=(--profile logs)
  jd_args+=(up -d --build)
  compose_run "$JD_DIR" "${jd_args[@]}"

  if [ "$MODE" = 'jd-prometheus' ]; then
    info '等待 MySQL 就绪并补建只读监控账号…'
    if [ "$DRY_RUN" != "1" ]; then
      i=0
      while [ "$i" -lt 30 ]; do
        st=$(docker inspect -f '{{.State.Health.Status}}' interview-mysql 2>/dev/null)
        [ "$st" = 'healthy' ] && break
        i=$((i + 1)); sleep 5
      done
    fi
    compose_run "$JD_DIR" -f docker-compose.yml -f "$jd_overlay" --profile initdb run --rm mysql-monitor-user
  fi

  compose_run "$NIGHTJAR_DIR" -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build
fi

# ---------------------------------------------------------------------------
# 7. 清理历史遗留的手工网络
# ---------------------------------------------------------------------------
step '7/8 清理历史遗留的手工网络'
for name in jd-redis-exporter jd-mysqld-exporter; do
  if [ "$DRY_RUN" = "1" ]; then drytag "docker network disconnect $mwops_network $name（若存在）"; continue; fi
  nets=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$name" 2>/dev/null)
  if [ -z "$nets" ]; then info "$name 不存在，跳过"; continue; fi
  case " $nets " in
    *" $mwops_network "*) docker network disconnect "$mwops_network" "$name" >/dev/null 2>&1 \
        && fix "$name 从 $mwops_network 摘除（该网络本不该有它）" ;;
    *) ok "$name 网络正常（$nets）" ;;
  esac
done

# ---------------------------------------------------------------------------
# 8. 体检
# ---------------------------------------------------------------------------
step '8/8 体检'

container_networks() { docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$1" 2>/dev/null; }

assert_networks() { # assert_networks NAME EXPECTED...
  local name="$1"; shift
  local actual; actual=$(container_networks "$name")
  if [ -z "$actual" ]; then bad "$name 未运行（或没有任何网络）"; return; fi
  local missing=""
  for want in "$@"; do
    [ -z "$want" ] && continue
    case " $actual " in *" $want "*) ;; *) missing="$missing $want" ;; esac
  done
  if [ -n "$missing" ]; then bad "$name 缺少网络：$missing（实际：$actual）"
  else ok "$name → $actual"; fi
}

if [ "$DRY_RUN" = "1" ]; then
  dryskip '跳过容器网络与连通性检查（dry-run）'
else
  assert_networks mwops-backend "$mwops_network" "$network"
  assert_networks mwops-prometheus "$mwops_network"
  assert_networks interview-redis "$network"
  assert_networks interview-mysql "$network"
  assert_networks interview-prometheus "$network"

  for alias in jd-prometheus jd-mysql jd-redis; do
    if docker exec mwops-backend getent hosts "$alias" >/dev/null 2>&1; then
      ok "mwops-backend 解析 $alias 正常"
    else
      bad "mwops-backend 解析不了 $alias"
      hint '确认平台是带 deploy/compose.jd-link.yml 启动的（mwops-backend 需在跨栈网络上）'
    fi
  done
  if docker exec jd-log-agent getent hosts mwops-backend >/dev/null 2>&1; then
    ok "jd-log-agent 能解析 mwops-backend（日志链路可用）"
  else
    bad "jd-log-agent 解析不了 mwops-backend：日志推不过来"
  fi

  # 指标标签核验：直接问 Prometheus 该 job 下真实的 instance_name
  if [ "$MODE" = 'jd-prometheus' ]; then prom_port="$jd_prom"; else prom_port="$nj_prom"; fi
  job='middleware-exporter-redis'
  matcher=$(printf 'up{job="%s"}' "$job" | sed 's/"/%22/g; s/{/%7B/g; s/}/%7D/g; s/=/\%3D/g')
  resp=$(http_get "http://127.0.0.1:$prom_port/api/v1/label/instance_name/values?match[]=$matcher")
  if [ -z "$resp" ]; then
    bad "无法访问 http://127.0.0.1:$prom_port"
    hint '端口可能不是这个：jd 的看 <jd>/.env 的 PROMETHEUS_PORT，平台的看 <nightjar>/.env 的 PROMETHEUS_PORT'
  else
    names=$(printf '%s' "$resp" | json_array | tr '\n' ',' | sed 's/,$//')
    if [ -z "$names" ]; then
      bad "Prometheus(:$prom_port) 上 job=$job 没有 instance_name 标签"
      hint '抓取配置缺 relabel，或 Prometheus 未用带 overlay 的配置重建：'
      hint "cd $JD_DIR && ${COMPOSE[*]} -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml up -d --force-recreate prometheus"
    else
      ok "Prometheus(:$prom_port) 上 job=$job 的 instance_name = $names"
      case ",$names," in
        *",$redis_name,"*) ok "与 .env 的 JD_REDIS_INSTANCE_NAME（$redis_name）一致，平台实例名应填 $redis_name" ;;
        *) warn "与 .env 的 JD_REDIS_INSTANCE_NAME（$redis_name）不一致：relabel 改动尚未生效"
           hint '重建 prometheus 容器后再生效：--force-recreate prometheus' ;;
      esac
    fi
  fi
fi

# ---------------------------------------------------------------------------
# 9. 可选：纠正平台的纳管实例
# ---------------------------------------------------------------------------
if [ "$FIX_INSTANCES" = "1" ]; then
  step '附加：纠正平台纳管实例（prom_job / 实例名）'
  if [ "$DRY_RUN" = "1" ]; then
    drytag '登录平台并把 prom_job 非 job 名的实例纠正为 middleware-exporter-<类型>'
  else
    admin=$(env_get "$NJ" ADMIN_USER); [ -z "$admin" ] && admin='admin'
    admin_pass=$(env_get "$NJ" ADMIN_PASSWORD)
    base="http://127.0.0.1:$nj_web"
    login=$(curl -fsS --max-time 10 -X POST "$base/api/auth/login" \
      -H 'Content-Type: application/json' \
      -d "{\"username\":\"$admin\",\"password\":\"$admin_pass\"}" 2>/dev/null)
    token=$(printf '%s' "$login" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
    if [ -z "$token" ]; then
      bad '平台登录失败：请核对 .env 的 ADMIN_USER / ADMIN_PASSWORD，并确认平台已启动'
    else
      if [ "$MODE" = 'jd-prometheus' ]; then prom_port="$jd_prom"; else prom_port="$nj_prom"; fi
      jobs=$(http_get "http://127.0.0.1:$prom_port/api/v1/label/job/values" | json_array)
      list=$(curl -fsS --max-time 10 -H "Authorization: Bearer $token" \
        "$base/api/middlewares?page=1&page_size=100" 2>/dev/null)
      # 用 sed 粗提实例片段：每条形如 {"id":1,"name":"...","mw_type":"...",...}
      # 注意用进程替换而不是管道：管道会让 while 跑在子 shell 里，CHANGES/bad 的累计会丢失。
      while IFS= read -r frag; do
        id=$(printf '%s' "$frag" | sed -n 's/.*"id":\([0-9]*\).*/\1/p' | head -n1)
        name=$(printf '%s' "$frag" | sed -n 's/.*"name":"\([^"]*\)".*/\1/p' | head -n1)
        mw=$(printf '%s' "$frag" | sed -n 's/.*"mw_type":"\([^"]*\)".*/\1/p' | head -n1)
        prom_job=$(printf '%s' "$frag" | sed -n 's/.*"prom_job":"\([^"]*\)".*/\1/p' | head -n1)
        [ -z "$id" ] && continue
        expected_job="middleware-exporter-$mw"
        if [ "$mw" = 'redis' ]; then expected_name="$redis_name"; else expected_name="$mysql_name"; fi

        new_job="$prom_job"; need=0
        if [ -z "$prom_job" ] || { [ -n "$jobs" ] && ! printf '%s\n' "$jobs" | grep -qx "$prom_job"; }; then
          new_job="$expected_job"; need=1
        fi
        [ "$name" != "$expected_name" ] && need=1
        if [ "$need" = "0" ]; then ok "$name 已正确（job=$new_job，名称=$expected_name）"; continue; fi

        body=$(printf '{"name":"%s","mw_type":"%s","host":"%s","port":%s,"username":"%s","environment":"%s","group_name":"%s","prom_job":"%s","prom_instance":""}' \
          "$expected_name" "$mw" \
          "$(printf '%s' "$frag" | sed -n 's/.*"host":"\([^"]*\)".*/\1/p' | head -n1)" \
          "$(printf '%s' "$frag" | sed -n 's/.*"port":\([0-9]*\).*/\1/p' | head -n1)" \
          "$(printf '%s' "$frag" | sed -n 's/.*"username":"\([^"]*\)".*/\1/p' | head -n1)" \
          "$(printf '%s' "$frag" | sed -n 's/.*"environment":"\([^"]*\)".*/\1/p' | head -n1)" \
          "$(printf '%s' "$frag" | sed -n 's/.*"group_name":"\([^"]*\)".*/\1/p' | head -n1)" \
          "$new_job")
        if curl -fsS --max-time 10 -X PUT "$base/api/middlewares/$id" \
             -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
             -d "$body" >/dev/null 2>&1; then
          fix "实例 #$id：$name/$prom_job → $expected_name/$new_job"
        else
          bad "纠正实例 #$id 失败"
        fi
      done < <(printf '%s' "$list" | tr '{' '\n' | grep -E '"mw_type":"(redis|mysql)"')
    fi
  fi
fi

# ---------------------------------------------------------------------------
# 汇总
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

printf '\n%s下一步：%s\n' "$C_CYAN" "$C_RESET"
printf '  平台入口   http://127.0.0.1:%s   （账号 %s，口令见 <nightjar>/.env 的 ADMIN_PASSWORD）\n' \
  "$nj_web" "$(env_get "$NJ" ADMIN_USER)"
if [ "$MODE" = 'jd-prometheus' ]; then
  printf '  jd 控制台  http://127.0.0.1:%s   Grafana http://127.0.0.1:%s\n' "$jd_web" "$(env_get "$JD" GRAFANA_PORT)"
  printf '  jd 指标源  http://127.0.0.1:%s（jd 的 Prometheus，prometheus.base_url=http://jd-prometheus:9090）\n' "$jd_prom"
else
  printf '  集成中心   平台「资源 → 集成中心」新建集成，即可由平台拉起 Exporter\n'
fi
printf '  逐项体检   %s/scripts/doctor-jd-link.sh\n' "$NIGHTJAR_DIR"
printf '  指标自检   bash %s/scripts/verify-nightjar.sh（在 jd 目录执行）\n' "$JD_DIR"
printf '  实例自检   平台「实例详情 → 接入自检」，应看到 matched > 0\n'

[ "${#PROBLEMS[@]}" -gt 0 ] && exit 1
exit 0
