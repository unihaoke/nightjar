#!/usr/bin/env bash
# =============================================================================
# nightjar 侧接入 jd（或任意被管项目）· 一键配置
#
# 架构前提：监控栈统一在 nightjar —— Exporter / 抓取 / 大盘 / 日志采集都由平台承担，
# 被管项目只需暴露"网络别名 + 日志卷"（jd 侧的 ./start.sh nightjar 负责）。
#
# 本脚本负责平台这一侧（**只维护 .env，其余自动**）：
#   1. 生成缺失的密钥/口令（十六进制，天然无转义问题）；
#   2. 端口错开：平台 Prometheus 9090 / Grafana 3000 不与宿主机既有占用冲突；
#   3. 写入接入相关配置：MWOPS_PROMETHEUS_BASE_URL、INTEGRATION_EXPORTER_NETWORK（含被管项目网络）；
#   4. 校验 docker.sock 是否就绪（一键拉起 Exporter / 代建账号 / 日志接入都依赖它）；
#   5. 启动平台（docker compose + jd-link overlay），并自检跨栈连通性；
#   6. 打印"接下来在集成中心点什么"。
#
# 用法：
#   ./scripts/setup-jd-link.sh --dry-run          # 先预览（不写文件、不调 docker）
#   ./scripts/setup-jd-link.sh                    # 正式执行
#   ./scripts/setup-jd-link.sh --jd-dir ../jd     # 指定被管项目目录
#   ./scripts/setup-jd-link.sh --skip-start       # 只改配置不起容器
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
JD_DIR="$(cd "$NIGHTJAR_DIR/.." 2>/dev/null && pwd)/jd"
DRY_RUN=0
SKIP_START=0

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

usage() { sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run|-n)   DRY_RUN=1 ;;
    --skip-start)   SKIP_START=1 ;;
    --jd-dir)       JD_DIR="${2:-}"; shift ;;
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

# ---------------------------------------------------------------------------
printf '%snightjar 侧一键接入%s\n' "$C_BOLD" "$C_RESET"
printf '  nightjar 目录 : %s\n' "$NIGHTJAR_DIR"
printf '  被管项目目录  : %s\n' "$JD_DIR"
[ "$DRY_RUN" = "1" ] && printf '  %s模式          : DRY-RUN（不写文件、不调 docker）%s\n' "$C_YELLOW" "$C_RESET"

step '0/6 前置检查'
[ -d "$NIGHTJAR_DIR" ] || { bad "nightjar 目录不存在：$NIGHTJAR_DIR"; exit 2; }
JD_PRESENT=0
[ -d "$JD_DIR" ] && JD_PRESENT=1
if [ "$JD_PRESENT" = "1" ]; then
  ok "被管项目存在：$JD_DIR"
else
  warn "未找到被管项目目录（$JD_DIR）：将只配置平台自身，跨栈网络名需手工确认"
  hint '用 --jd-dir 指定被管项目路径'
fi
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

JD_ENV="$JD_DIR/.env"
[ "$JD_PRESENT" = "1" ] && [ ! -f "$JD_ENV" ] && {
  if [ "$DRY_RUN" = "1" ]; then drytag "复制 $JD_DIR/.env.example → $JD_ENV"
  else cp "$JD_DIR/.env.example" "$JD_ENV" && fix "由 .env.example 生成 $JD_ENV"; fi
}

if [ "$DRY_RUN" = "1" ]; then
  TMP_WORK=$(mktemp -d); trap 'rm -rf "$TMP_WORK"' EXIT
  cp "$NJ_SRC" "$TMP_WORK/nightjar.env"; NJ_SRC="$TMP_WORK/nightjar.env"
  [ "$JD_PRESENT" = "1" ] && [ -f "$JD_ENV" ] && { cp "$JD_ENV" "$TMP_WORK/jd.env"; JD_ENV="$TMP_WORK/jd.env"; }
fi
NJ="$NJ_SRC"

# ---------------------------------------------------------------------------
step '1/6 生成缺失的密钥与口令'
ensure_secret() {
  local file="$1" key="$2" bytes="$3" desc="$4" cur
  cur=$(env_get "$file" "$key")
  if [ -z "$cur" ] || is_weak "$cur"; then set_env_reported "$file" "$key" "$(gen_secret "$bytes")"
  else ok "$key 已设置（$desc）"; fi
}
ensure_secret "$NJ" JWT_SECRET 32 '平台 JWT 密钥'
ensure_secret "$NJ" ADMIN_PASSWORD 12 '平台管理员口令'
ensure_secret "$NJ" DB_PASSWORD 16 '平台 PostgreSQL 口令'
ensure_secret "$NJ" REDIS_PASSWORD 16 '平台自身 Redis 口令'
ensure_secret "$NJ" GRAFANA_PASSWORD 12 'Grafana 管理员口令'
ensure_secret "$NJ" HOOK_TOKEN 24 '日志上报令牌（平台内部使用）'

# ---------------------------------------------------------------------------
step '2/6 写入接入配置'
NET="jd-nightjar"
if [ "$JD_PRESENT" = "1" ] && [ -f "$JD_ENV" ]; then
  jd_net=$(env_get "$JD_ENV" JD_NIGHTJAR_NETWORK)
  [ -n "$jd_net" ] && NET="$jd_net"
fi
set_env_reported "$NJ" JD_NIGHTJAR_NETWORK "$NET"

# 平台自带的 Prometheus 抓自己创建的 Exporter（监控面 = 平台网络）
mwops_net=$(docker network ls --format '{{.Name}}' 2>/dev/null | grep -E '_mwops$' | head -n1)
[ -z "$mwops_net" ] && mwops_net='middleware-ops_mwops'
set_env_reported "$NJ" MWOPS_PROMETHEUS_BASE_URL 'http://prometheus:9090'
set_env_reported "$NJ" INTEGRATION_ENABLED 'true'
set_env_reported "$NJ" INTEGRATION_DOCKER_ENABLED 'true'
set_env_reported "$NJ" INTEGRATION_EXPORTER_NETWORK "$mwops_net,$NET"
ok "Exporter/采集容器将接入：$mwops_net（监控面）+ $NET（数据面，按别名自动发现）"

# ---------------------------------------------------------------------------
step '3/6 端口与 docker.sock'
nj_prom=$(port_of "$NJ" PROMETHEUS_PORT); [ -z "$nj_prom" ] && nj_prom=9090
nj_graf=$(port_of "$NJ" GRAFANA_PORT); [ -z "$nj_graf" ] && nj_graf=3000
set_env_reported "$NJ" PROMETHEUS_PORT "$nj_prom"
set_env_reported "$NJ" GRAFANA_PORT "$nj_graf"
set_env_reported "$NJ" GRAFANA_USER "$(env_get "$NJ" GRAFANA_USER)"
ok "平台端口：Prometheus $nj_prom / Grafana $nj_graf / Web $(env_get "$NJ" WEB_PORT)"

if [ "$(env_get "$NJ" INTEGRATION_DOCKER_ENABLED)" = 'true' ]; then
  if grep -qE '^[[:space:]]*#[[:space:]]*-[[:space:]]*/var/run/docker\.sock' "$NIGHTJAR_DIR/docker-compose.yml" 2>/dev/null; then
    bad 'INTEGRATION_DOCKER_ENABLED=true，但 docker-compose.yml 里的 docker.sock 挂载仍是注释状态'
    hint '取消 docker-compose.yml 中 backend.volumes 的 "/var/run/docker.sock:/var/run/docker.sock" 注释后重跑'
  else
    ok 'docker.sock 已挂载（一键拉起 Exporter / 代建账号 / 日志接入均可用）'
  fi
fi

# ---------------------------------------------------------------------------
step '4/6 写回 .env'
if [ "$DRY_RUN" = "1" ]; then
  dryskip 'dry-run：不写回 .env（上面 [FIX] 即正式运行的改动）'
else
  ts=$(date +%Y%m%d%H%M%S)
  cp "$NJ" "$NJ.bak-$ts" && info "已写回 $NJ（备份：.env.bak-$ts）"
fi

# ---------------------------------------------------------------------------
step '5/6 启动平台'
if [ "$SKIP_START" = "1" ]; then
  info '按 --skip-start 跳过启动'
else
  if [ "$DRY_RUN" = "1" ]; then
    drytag "cd $NIGHTJAR_DIR && ${COMPOSE[*]} -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build"
  else
    info "▶ ${COMPOSE[*]} -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build"
    ( cd "$NIGHTJAR_DIR" && "${COMPOSE[@]}" -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build ) \
      || bad '平台启动失败（多数是 jd-nightjar 网络不存在：先在 jd 侧执行 ./start.sh nightjar）'
  fi
fi

# ---------------------------------------------------------------------------
step '6/6 自检'
if [ "$DRY_RUN" = "1" ]; then
  dryskip '跳过容器检查（dry-run）'
else
  nets=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' mwops-backend 2>/dev/null)
  if [ -z "$nets" ]; then bad 'mwops-backend 未运行'
  else
    case " $nets " in
      *" $mwops_net "*) ok "mwops-backend → $nets" ;;
      *) bad "mwops-backend 缺少 $mwops_net（实际：$nets）" ;;
    esac
    case " $nets " in
      *" $NET "*) ok "mwops-backend 已接入跨栈网络 $NET" ;;
      *) bad "mwops-backend 不在 $NET 上：确认平台带 deploy/compose.jd-link.yml 启动"
         hint "cd $NIGHTJAR_DIR && ${COMPOSE[*]} -f docker-compose.yml -f deploy/compose.jd-link.yml up -d" ;;
    esac
  fi
  if docker exec mwops-backend getent hosts jd-mysql >/dev/null 2>&1; then
    ok 'mwops-backend 能解析 jd-mysql（纳管探测可用）'
  else
    warn 'mwops-backend 解析不了 jd-mysql：若 jd 还没执行 ./start.sh nightjar 属正常'
  fi
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

printf '\n%s接下来（全部在平台上点，被管项目无需再改）%s\n' "$C_CYAN" "$C_RESET"
printf '  1. 平台入口   http://127.0.0.1:%s（管理员 %s）\n' "$(env_get "$NJ" WEB_PORT)" "$(env_get "$NJ" ADMIN_USER)"
printf '  2. 集成 MySQL  名称 jd-mysql，地址 jd-mysql:3306，勾选「由平台创建只读监控账号」+「一键拉起 Exporter」\n'
printf '  3. 集成 Redis  名称 jd-redis，地址 jd-redis:6379\n'
printf '  4. 日志接入    目标容器名 interview-backend\n'
printf '  5. 看大盘      http://127.0.0.1:%s（数据源已自动配置）\n' "$nj_graf"
printf '  6. 逐项体检    %s/scripts/doctor-jd-link.sh\n' "$NIGHTJAR_DIR"

[ "${#PROBLEMS[@]}" -gt 0 ] && exit 1
exit 0
