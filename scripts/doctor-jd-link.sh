#!/usr/bin/env bash
# =============================================================================
# nightjar ⇄ jd 跨栈网络体检（Linux / macOS，纯 bash）
#
# 背景：跨栈故障里最难查的一类是「容器都在跑，但网络挂错了」——
#   * 平台没带 deploy/compose.jd-link.yml 启动 → mwops-backend 不在 jd-nightjar 上，
#     于是「查不到指标」「实例探测连接失败」「日志推不过来」同时发生；
#   * 容器被手工 docker network connect/disconnect 过 → 多一张或少了关键网络
#     （例如 mwops-prometheus 变成"没有任何网络"）。
#   平台 UI 只会显示"没有数据"，看不出网络挂错，所以用本脚本对齐期望拓扑。
#
# 用法（在 nightjar 项目根目录执行）：
#   ./scripts/doctor-jd-link.sh
#   ./scripts/doctor-jd-link.sh --mode mwops-prometheus        # 声明用集成中心取数
#   ./scripts/doctor-jd-link.sh --jd-dir ../jd
#
# 退出码：0=全部通过；非 0=失败项数量（便于接 CI / 启动前自检）
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
JD_DIR="$(cd "$NIGHTJAR_DIR/.." 2>/dev/null && pwd)/jd"
MODE='jd-prometheus'

if [ -t 1 ]; then
  C_RESET=$'\033[0m'; C_GREEN=$'\033[32m'; C_RED=$'\033[31m'; C_YELLOW=$'\033[33m'; C_CYAN=$'\033[36m'; C_GRAY=$'\033[90m'
else
  C_RESET=""; C_GREEN=""; C_RED=""; C_YELLOW=""; C_CYAN=""; C_GRAY=""
fi

fail=0; warns=0
step() { printf '\n%s=== %s ===%s\n' "$C_CYAN" "$1" "$C_RESET"; }
ok()   { printf '  %s[PASS]%s %s\n' "$C_GREEN" "$C_RESET" "$1"; }
bad()  { printf '  %s[FAIL]%s %s\n' "$C_RED" "$C_RESET" "$1"; fail=$((fail + 1)); }
warn() { printf '  %s[WARN]%s %s\n' "$C_YELLOW" "$C_RESET" "$1"; warns=$((warns + 1)); }
hint() { printf '         %s→ %s%s\n' "$C_GRAY" "$1" "$C_RESET"; }
info() { printf '  %s\n' "$1"; }

while [ $# -gt 0 ]; do
  case "$1" in
    --mode)       MODE="${2:-}"; shift ;;
    --jd-dir)     JD_DIR="${2:-}"; shift ;;
    --nightjar-dir) NIGHTJAR_DIR="${2:-}"; shift ;;
    -h|--help)    sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

if ! command -v docker >/dev/null 2>&1; then
  printf '%s未找到 docker：请在 Docker 宿主机上执行本脚本%s\n' "$C_RED" "$C_RESET"
  exit 2
fi

env_get() {
  local file="$1" key="$2"
  [ -f "$file" ] || return 0
  sed -n -E "s/^[[:space:]]*${key}[[:space:]]*=(.*)$/\1/p" "$file" | head -n1
}

# 按后缀解析实际网络名（compose 会给网络加项目前缀）
resolve_network() { # resolve_network SUFFIX
  docker network ls --format '{{.Name}}' 2>/dev/null | grep -E "(_|^)${1}\$" | head -n1
}

container_networks() { docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$1" 2>/dev/null; }

assert_networks() { # assert_networks NAME [FORBIDDEN] -- EXPECTED...
  local name="$1"; shift
  local forbidden="$1"; shift
  local actual; actual=$(container_networks "$name")
  if [ -z "$actual" ]; then
    bad "$name 未运行，或没有任何网络（容器在跑但谁也连不上）"
    hint "docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --force-recreate $name"
    return
  fi
  local problems=""
  for want in "$@"; do
    [ -z "$want" ] && continue
    case " $actual " in *" $want "*) ;; *) problems="$problems 缺少:$want" ;; esac
  done
  if [ -n "$forbidden" ]; then
    case " $actual " in *" $forbidden "*) problems="$problems 多出:$forbidden" ;; esac
  fi
  if [ -n "$problems" ]; then bad "$name$problems（实际：$actual）"
  else ok "$name → $actual"; fi
}

printf '取数方式：%s\n' "$MODE"
printf 'nightjar 目录：%s\n' "$NIGHTJAR_DIR"
printf 'jd 目录：%s\n' "$JD_DIR"

# ---------------------------------------------------------------------------
step '1/5 网络存在性与隔离属性'
link_net=$(resolve_network 'jd-nightjar')
data_net=$(resolve_network 'jd-data')
mwops_net=$(resolve_network 'mwops')

if [ -z "$link_net" ]; then
  bad 'jd-nightjar 网络不存在'
  hint "cd $JD_DIR && ./start.sh link（或 ./start.sh nightjar，脚本会以 --internal 创建）"
else
  internal=$(docker network inspect "$link_net" --format '{{.Internal}}' 2>/dev/null)
  if [ "$internal" = 'true' ]; then ok "$link_net 存在且为 internal（数据面隔离生效）"
  else
    bad "$link_net 不是 internal 网络（Internal=$internal）：jd 的 MySQL/Redis 会重新获得出网路由"
    hint "docker network rm $link_net 后重新执行 cd $JD_DIR && ./start.sh link"
  fi
fi
[ -n "$data_net" ] && ok "$data_net 存在" || bad 'jd 的数据面网络（*-jd-data）不存在：jd 侧还没启动？'
[ -n "$mwops_net" ] && ok "$mwops_net 存在" || bad '平台网络（*-mwops）不存在：nightjar 侧还没启动？'

# ---------------------------------------------------------------------------
step '2/5 平台侧容器网络（关键：backend 必须在 jd-nightjar 上）'
assert_networks mwops-backend '' "$mwops_net" "$link_net"
if [ -n "$link_net" ]; then
  case " $(container_networks mwops-backend) " in
    *" $link_net "*) ;;
    *) hint "平台没带跨栈 overlay 启动。正确命令：cd $NIGHTJAR_DIR && docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build" ;;
  esac
fi
assert_networks mwops-prometheus '' "$mwops_net"
assert_networks mwops-frontend '' "$mwops_net"
assert_networks mwops-postgres '' "$mwops_net"
assert_networks mwops-redis '' "$mwops_net"

# ---------------------------------------------------------------------------
step '3/5 jd 侧容器网络'
assert_networks interview-mysql '' "$data_net" "$link_net"
assert_networks interview-redis '' "$data_net" "$link_net"
assert_networks interview-prometheus '' "$data_net" "$link_net"
assert_networks jd-log-agent '' "$link_net"

if [ "$MODE" = 'jd-prometheus' ]; then
  # 方式二：jd 的 Prometheus 抓 jd 侧 Exporter，两者同在 jd-data 即可；
  # 出现在平台网络上属于手工 connect 的遗留，会导致"平台查到自己 Prometheus 的示例 job"这类假象。
  assert_networks jd-redis-exporter "$mwops_net" "$data_net"
  assert_networks jd-mysqld-exporter "$mwops_net" "$data_net"
  info '方式二下平台只查 http://jd-prometheus:9090，不直接抓 Exporter'
else
  managed=$(docker ps --format '{{.Names}}' 2>/dev/null | grep '^mwops-exporter-' || true)
  if [ -z "$managed" ]; then
    warn '未发现集成中心拉起的 Exporter（mwops-exporter-*）：请在「集成中心」新建集成并勾选一键拉起容器'
  else
    for name in $managed; do assert_networks "$name" '' "$mwops_net" "$link_net"; done
  fi
fi

# ---------------------------------------------------------------------------
step '4/5 跨栈连通性实测'
if [ -n "$link_net" ] && case " $(container_networks mwops-backend) " in *" $link_net "*) true ;; *) false ;; esac; then
  for alias in jd-prometheus jd-mysql jd-redis; do
    if docker exec mwops-backend getent hosts "$alias" >/dev/null 2>&1; then ok "mwops-backend 能解析 $alias"
    else bad "mwops-backend 解析不了 $alias"; hint "说明 mwops-backend 不在 $link_net 上，或 jd 侧别名未生效"; fi
  done
else
  bad 'mwops-backend 不在跨栈网络上，跳过别名解析（先修第 2 步）'
fi
if docker inspect jd-log-agent >/dev/null 2>&1; then
  if docker exec jd-log-agent getent hosts mwops-backend >/dev/null 2>&1; then ok 'jd-log-agent 能解析 mwops-backend（日志链路可用）'
  else bad 'jd-log-agent 解析不了 mwops-backend：日志推不过来'; fi
fi

# ---------------------------------------------------------------------------
step '5/5 关键 .env 项与口令一致性'
NJ_ENV="$NIGHTJAR_DIR/.env"; JD_ENV="$JD_DIR/.env"
if [ ! -f "$NJ_ENV" ]; then
  warn "$NJ_ENV 不存在（未复制 .env.example？）"
else
  base_url=$(env_get "$NJ_ENV" MWOPS_PROMETHEUS_BASE_URL)
  if [ -z "$base_url" ]; then
    warn 'MWOPS_PROMETHEUS_BASE_URL 未设置（会回退平台自带 Prometheus 或内置模拟器）'
    hint '方式二请设置：MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090'
  elif [ "$MODE" = 'jd-prometheus' ] && [ "${base_url#*jd-prometheus}" = "$base_url" ]; then
    warn "MWOPS_PROMETHEUS_BASE_URL=$base_url，但当前声明用 jd 的 Prometheus"
    hint '方式二应为 http://jd-prometheus:9090；方式一应为 http://prometheus:9090'
  else
    ok "MWOPS_PROMETHEUS_BASE_URL=$base_url"
  fi
  nj_prom=$(env_get "$NJ_ENV" PROMETHEUS_PORT)
  jd_prom=$(env_get "$JD_ENV" PROMETHEUS_PORT)
  if [ -n "$nj_prom" ] && [ "$nj_prom" = "$jd_prom" ]; then
    warn "两边 PROMETHEUS_PORT 都是 $nj_prom（同机部署会端口冲突）"
    hint '约定 jd=9091 / nightjar=9090；或直接跑 scripts/setup-jd-link.sh 自动错开'
  else
    ok "Prometheus 宿主端口：nightjar=${nj_prom:-默认9090}，jd=${jd_prom:-默认9090}"
  fi
fi
if [ -f "$NJ_ENV" ] && [ -f "$JD_ENV" ]; then
  nj_token=$(env_get "$NJ_ENV" HOOK_TOKEN)
  jd_token=$(env_get "$JD_ENV" NIGHTJAR_HOOK_TOKEN)
  if [ -z "$nj_token" ]; then warn 'nightjar HOOK_TOKEN 为空：/api/hooks/* 不鉴权（仅测试可用）'
  elif [ "$nj_token" = "$jd_token" ]; then ok 'HOOK_TOKEN 与 jd 的 NIGHTJAR_HOOK_TOKEN 一致'
  else
    bad 'HOOK_TOKEN 与 jd 的 NIGHTJAR_HOOK_TOKEN 不一致：日志上报会被 401 拒绝'
    hint '跑 scripts/setup-jd-link.sh 自动对齐'
  fi
  nn=$(env_get "$NJ_ENV" JD_NIGHTJAR_NETWORK); jn=$(env_get "$JD_ENV" JD_NIGHTJAR_NETWORK)
  if [ -n "$nn" ] && [ -n "$jn" ] && [ "$nn" != "$jn" ]; then bad "两侧 JD_NIGHTJAR_NETWORK 不一致：平台=$nn，jd=$jn"
  else ok "JD_NIGHTJAR_NETWORK 一致（${nn:-默认 jd-nightjar}）"; fi
else
  warn '缺少 .env（平台或 jd）：跳过口令一致性检查'
fi

# ---------------------------------------------------------------------------
printf '\n%s================ 结果 ================%s\n' "$C_CYAN" "$C_RESET"
if [ "$fail" -eq 0 ]; then
  printf '%s拓扑检查通过（%d 条提示）%s\n' "$C_GREEN" "$warns" "$C_RESET"
  printf '%s下一步：平台实例详情 →「接入自检」，确认 matched>0 且来源为 Prometheus%s\n' "$C_GRAY" "$C_RESET"
else
  printf '%s失败 %d 项、提示 %d 条，按上面的 [FAIL]/[HINT] 逐条处理%s\n' "$C_RED" "$fail" "$warns" "$C_RESET"
  printf '%s统一重建（避免手工 network connect 造成状态漂移）：%s\n' "$C_GRAY" "$C_RESET"
  printf '  cd %s && docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d\n' "$NIGHTJAR_DIR"
  printf '  cd %s && ./start.sh nightjar\n' "$JD_DIR"
fi
exit "$fail"
