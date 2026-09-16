#!/usr/bin/env bash
# =============================================================================
# nightjar 侧接入被管项目（以 jd 为例）· 一键配置
#
# 架构前提（v2 起不再需要互联网络）：
#   监控栈全部在 nightjar。集成时平台用 Docker API **自己发现**目标容器在哪张网络上，
#   并把 Exporter 接进去；被管项目不需要建网络、不需要加别名、不需要改 compose。
#   因此本脚本**只维护平台自己的 .env**，不再碰任何跨栈网络。
#
# 本脚本做四件事：
#   1. 生成缺失的密钥/口令（十六进制，天然无转义问题）；
#   2. 打开集成能力：INTEGRATION_DOCKER_ENABLED=true；
#   3. 自动放开 docker-compose.yml 里 docker.sock 的挂载注释（自动发现的必要条件）；
#   4. 启动平台（普通 docker compose up -d --build）并自检「平台能否看到目标容器」。
#
# 用法：
#   ./scripts/setup-jd-link.sh --dry-run          # 先预览（不写文件、不调 docker）
#   ./scripts/setup-jd-link.sh                    # 正式执行
#   ./scripts/setup-jd-link.sh --target interview-redis   # 只检查某个目标容器
#   ./scripts/setup-jd-link.sh --skip-start       # 只改配置不起容器
#
# 说明：脚本名里的 "link" 是历史遗留（旧架构需要一条互联网络），现在它等价于"纳管关系"。
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TARGETS=(interview-mysql interview-redis interview-backend)
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

usage() { sed -n '2,27p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0; }

while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run|-n)   DRY_RUN=1 ;;
    --skip-start)   SKIP_START=1 ;;
    --target)       TARGETS=("${2:-}"); shift ;;
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

step '0/5 前置检查'
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
step '1/5 生成缺失的密钥与口令'
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
ensure_secret "$NJ" HOOK_TOKEN 24 '日志上报令牌（采集容器用）'

# ---------------------------------------------------------------------------
step '2/5 打开集成能力'
set_env_reported "$NJ" INTEGRATION_ENABLED 'true'
set_env_reported "$NJ" INTEGRATION_DOCKER_ENABLED 'true'
set_env_reported "$NJ" MWOPS_PROMETHEUS_BASE_URL 'http://prometheus:9090'
enable_docker_sock

# 旧版本写过跨栈网络名：留着会让后来者以为还要建互联网络，这里主动清掉。
if grep -qE '^[[:space:]]*JD_NIGHTJAR_NETWORK[[:space:]]*=' "$NJ" 2>/dev/null; then
  if [ "$DRY_RUN" = "1" ]; then drytag '删除 JD_NIGHTJAR_NETWORK（新架构不再需要互联网络）'
  else
    sed -i -E '/^[[:space:]]*JD_NIGHTJAR_NETWORK[[:space:]]*=/d' "$NJ"
    fix '已删除 JD_NIGHTJAR_NETWORK（新架构不再需要互联网络）'
  fi
fi
if grep -qE '^[[:space:]]*INTEGRATION_EXPORTER_NETWORK[[:space:]]*=.*jd-nightjar' "$NJ" 2>/dev/null; then
  if [ "$DRY_RUN" = "1" ]; then drytag '把 INTEGRATION_EXPORTER_NETWORK 收敛为平台网络（目标网络自动发现）'
  else
    set_env_reported "$NJ" INTEGRATION_EXPORTER_NETWORK 'middleware-ops_mwops' >/dev/null
    fix 'INTEGRATION_EXPORTER_NETWORK 已收敛为平台网络（目标网络由平台自动发现）'
  fi
fi

# ---------------------------------------------------------------------------
step '3/5 写回 .env'
if [ "$DRY_RUN" = "1" ]; then
  dryskip 'dry-run：不写回 .env（上面 [FIX] 即正式运行的改动）'
else
  ts=$(date +%Y%m%d%H%M%S)
  cp "$NJ" "$NJ.bak-$ts" && info "已写回 $NJ（备份：.env.bak-$ts）"
fi

# ---------------------------------------------------------------------------
step '4/5 启动平台'
if [ "$SKIP_START" = "1" ]; then
  info '按 --skip-start 跳过启动'
elif [ "$DRY_RUN" = "1" ]; then
  drytag "cd $NIGHTJAR_DIR && ${COMPOSE[*]} up -d --build"
else
  info "▶ ${COMPOSE[*]} up -d --build"
  ( cd "$NIGHTJAR_DIR" && "${COMPOSE[@]}" up -d --build ) \
    || bad '平台启动失败：查看上面的 docker compose 输出（常见原因：.env 缺少必改项）'
fi

# ---------------------------------------------------------------------------
step '5/5 自检（平台视角）'
if [ "$DRY_RUN" = "1" ]; then
  dryskip '跳过容器检查（dry-run）'
else
  # 1) 平台容器
  for name in mwops-backend mwops-prometheus mwops-grafana; do
    state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null)
    [ "$state" = 'running' ] && ok "$name 运行中" || bad "$name 未运行（状态：${state:-不存在}）"
  done

  # 2) 平台能否看到 docker（自动发现的前提）
  if docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' mwops-backend 2>/dev/null | grep -q '/var/run/docker.sock'; then
    ok 'mwops-backend 已挂载 docker.sock（平台可自动发现并接入目标网络）'
  else
    bad 'mwops-backend 未挂载 docker.sock：平台无法自动发现目标网络'
    hint '取消 docker-compose.yml 中 backend.volumes 的 docker.sock 注释后重新 up -d'
  fi

  # 3) 目标容器与它们真正所在的网络（这就是平台集成时会自动接入的网络）
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

printf '\n%s接下来（全部在平台上点，被管项目无需任何改动）%s\n' "$C_CYAN" "$C_RESET"
printf '  1. 平台入口   http://127.0.0.1:%s（管理员 %s）\n' "$(env_get "$NJ" WEB_PORT)" "$(env_get "$NJ" ADMIN_USER)"
printf '  2. 集成 MySQL  名称 jd-mysql，地址 interview-mysql:3306，勾选「由平台创建只读监控账号」+「一键拉起 Exporter」\n'
printf '  3. 集成 Redis  名称 jd-redis，地址 interview-redis:6379，口令填被管项目的 REDIS_PASSWORD\n'
printf '  4. 日志接入    目标容器名 interview-backend\n'
printf '  5. 看大盘      http://127.0.0.1:%s（数据源已自动配置）\n' "$(env_get "$NJ" GRAFANA_PORT)"
printf '  6. 逐项体检    %s/scripts/doctor-jd-link.sh\n' "$NIGHTJAR_DIR"

[ "${#PROBLEMS[@]}" -gt 0 ] && exit 1
exit 0
