#!/usr/bin/env bash
# =============================================================================
# nightjar ⇄ 被管项目 跨栈体检
#
# 逐条核对"平台托管监控"所需的链路是否就绪：
#   1. 平台自身：Prometheus / Grafana / backend 是否在平台网络上
#   2. 跨栈网络：互联网络存在且为 internal；平台 backend 已接入
#   3. 被管项目：mysql/redis 在互联网络上且有稳定别名（平台据此纳管与自动接网）
#   4. 平台创建的资产：Exporter（mwops-exporter-*）网络是否齐全；日志采集容器是否存在
#   5. 抓取与标签：平台 Prometheus 上目标的 up 状态 + instance_name 实际取值
#
# 用法（在 nightjar 项目根目录）：
#   ./scripts/doctor-jd-link.sh
#   ./scripts/doctor-jd-link.sh --jd-dir ../jd
#   ./scripts/doctor-jd-link.sh --target-container interview-backend
#
# 退出码：0=全部通过；非 0=失败项数量
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
JD_DIR="$(cd "$NIGHTJAR_DIR/.." 2>/dev/null && pwd)/jd"
TARGET_CONTAINER="interview-backend"

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
    --jd-dir)            JD_DIR="${2:-}"; shift ;;
    --nightjar-dir)      NIGHTJAR_DIR="${2:-}"; shift ;;
    --target-container)  TARGET_CONTAINER="${2:-}"; shift ;;
    -h|--help)           sed -n '2,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

if ! command -v docker >/dev/null 2>&1; then
  printf '%s未找到 docker：请在 Docker 宿主机上执行本脚本%s\n' "$C_RED" "$C_RESET"
  exit 2
fi

env_get() { sed -n -E "s/^[[:space:]]*$2[[:space:]]*=(.*)$/\1/p" "$1" 2>/dev/null | head -n1; }
container_networks() { docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$1" 2>/dev/null; }
resolve_network() { docker network ls --format '{{.Name}}' 2>/dev/null | grep -E "(_|^)${1}\$" | head -n1; }

NJ_ENV="$NIGHTJAR_DIR/.env"
NET="$(env_get "$NJ_ENV" JD_NIGHTJAR_NETWORK)"; [ -z "$NET" ] && NET='jd-nightjar'
MWOPS_NET=$(resolve_network 'mwops'); [ -z "$MWOPS_NET" ] && MWOPS_NET='middleware-ops_mwops'
NJ_PROM=$(env_get "$NJ_ENV" PROMETHEUS_PORT); [ -z "$NJ_PROM" ] && NJ_PROM=9090

printf '互联网络：%s    平台网络：%s    平台 Prometheus：127.0.0.1:%s\n' "$NET" "$MWOPS_NET" "$NJ_PROM"

# ---------------------------------------------------------------------------
step '1/5 平台自身'
for pair in "mwops-backend:$MWOPS_NET" "mwops-prometheus:$MWOPS_NET" "mwops-grafana:$MWOPS_NET"; do
  name="${pair%%:*}"; want="${pair##*:}"
  actual=$(container_networks "$name")
  if [ -z "$actual" ]; then bad "$name 未运行"
  elif printf '%s' "$actual" | grep -qw "$want"; then ok "$name → $actual"
  else bad "$name 不在 $want 上（实际：$actual）"; hint "cd $NIGHTJAR_DIR && docker compose up -d $name"; fi
done

# ---------------------------------------------------------------------------
step '2/5 跨栈互联网络'
if docker network inspect "$NET" >/dev/null 2>&1; then
  internal=$(docker network inspect "$NET" --format '{{.Internal}}' 2>/dev/null)
  [ "$internal" = 'true' ] && ok "$NET 存在且为 internal" \
    || bad "$NET 不是 internal（Internal=$internal）"
else
  bad "$NET 不存在"
  hint "cd $JD_DIR && ./start.sh nightjar（被管项目负责创建）"
fi
actual=$(container_networks mwops-backend)
case " $actual " in
  *" $NET "*) ok "mwops-backend 已接入 $NET" ;;
  *) bad "mwops-backend 不在 $NET 上：平台没带 overlay 启动"
     hint "cd $NIGHTJAR_DIR && docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d" ;;
esac

# ---------------------------------------------------------------------------
step '3/5 被管项目的别名（平台纳管与自动接网的依据）'
for pair in 'interview-mysql:jd-mysql:3306' 'interview-redis:jd-redis:6379'; do
  c="${pair%%:*}"; rest="${pair#*:}"; alias_name="${rest%%:*}"
  actual=$(container_networks "$c")
  if [ -z "$actual" ]; then warn "$c 未运行（未接入该被管项目时属正常）"; continue; fi
  if printf '%s' "$actual" | grep -qw "$NET"; then ok "$c 在 $NET 上（别名 $alias_name）"
  else bad "$c 不在 $NET 上"; hint "cd $JD_DIR && ./start.sh nightjar"; fi
done
if docker exec mwops-backend getent hosts jd-mysql >/dev/null 2>&1; then ok 'mwops-backend 能解析 jd-mysql'
else warn 'mwops-backend 解析不了 jd-mysql（纳管探测会显示连接失败）'; fi

# ---------------------------------------------------------------------------
step '4/5 平台创建的资产'
managed=$(docker ps --format '{{.Names}}' 2>/dev/null | grep '^mwops-exporter-' || true)
if [ -z "$managed" ]; then
  warn '未发现平台创建的 Exporter（mwops-exporter-*）：到「集成中心」集成中间件并勾选一键拉起'
else
  for name in $managed; do
    actual=$(container_networks "$name")
    missing=""
    for want in "$MWOPS_NET" "$NET"; do
      printf '%s' "$actual" | grep -qw "$want" || missing="$missing $want"
    done
    [ -z "$missing" ] && ok "$name → $actual" \
      || bad "$name 缺少网络：$missing（实际：$actual）"
  done
fi
collectors=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mwops-exporter-.*-logs$|^mwops-logcollect' || true)
if [ -n "$collectors" ]; then
  for name in $collectors; do ok "日志采集容器 $name 运行中"; done
else
  info '尚无日志采集容器（在「集成中心 → 日志接入」创建）'
fi

# ---------------------------------------------------------------------------
step '5/5 抓取与标签'
if curl -fsS --max-time 5 "http://127.0.0.1:$NJ_PROM/-/healthy" >/dev/null 2>&1; then
  ok "平台 Prometheus 可达（127.0.0.1:$NJ_PROM）"
  body=$(curl -fsS --max-time 5 "http://127.0.0.1:$NJ_PROM/api/v1/targets?state=active" 2>/dev/null)
  if [ -n "$body" ]; then
    if command -v python3 >/dev/null 2>&1 && python3 -c 'pass' >/dev/null 2>&1; then
      summary=$(printf '%s' "$body" | python3 -c '
import sys, json
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
targets = (doc.get("data") or {}).get("activeTargets") or []
if not targets:
    print("  [INFO] 平台 Prometheus 暂无抓取目标（还没集成任何中间件）")
for t in targets:
    labels = t.get("labels") or {}
    if t.get("health") != "up":
        err = (t.get("lastError") or "").strip() or "(无 lastError：容器可能未启动)"
        name = labels.get("instance_name") or labels.get("instance") or "?"
        print("  [FAIL] target %s 未就绪：%s" % (name, err))
up = [t for t in targets if t.get("health") == "up"]
if up:
    print("  [PASS] %d 个抓取目标处于 up" % len(up))
' 2>/dev/null)
      if [ -n "$summary" ]; then
        printf '%s\n' "$summary"
        bad_lines=$(printf '%s\n' "$summary" | grep -c '\[FAIL\]' || true)
        fail=$((fail + bad_lines))
      fi
    else
      info '（未安装可用的 python3，跳过目标明细；可直接看 Prometheus /targets 页面）'
    fi
  fi
else
  bad "平台 Prometheus 不可达（127.0.0.1:$NJ_PROM）"
  hint "确认 mwops-prometheus 运行中，且 .env 的 PROMETHEUS_PORT=$NJ_PROM"
fi

# ---------------------------------------------------------------------------
printf '\n%s================ 结果 ================%s\n' "$C_CYAN" "$C_RESET"
if [ "$fail" -eq 0 ]; then
  printf '%s跨栈链路就绪（%d 条提示）%s\n' "$C_GREEN" "$warns" "$C_RESET"
  printf '%s下一步：平台「集成中心」集成中间件/日志；实例详情「接入自检」应显示 matched>0%s\n' "$C_GRAY" "$C_RESET"
else
  printf '%s失败 %d 项、提示 %d 条，按上面的 [FAIL]/[HINT] 处理%s\n' "$C_RED" "$fail" "$warns" "$C_RESET"
fi
exit "$fail"
