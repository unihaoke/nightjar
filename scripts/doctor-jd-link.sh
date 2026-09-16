#!/usr/bin/env bash
# =============================================================================
# nightjar ⇄ 被管项目 · 接入体检
#
# 逐条核对"平台托管监控"所需的链路是否就绪。**不再检查任何跨栈互联网络/别名**：
# 新架构下目标容器所在的网络由平台在集成时自动发现并接入，被管项目零改动。
#
#   1. 平台自身：backend / Prometheus / Grafana 是否运行在平台网络上
#   2. 自动发现能力：平台是否挂载了 docker.sock（没有它就无法自动接网）
#   3. 被管项目：目标容器是否在运行，并列出它们**真实所在**的网络
#   4. 平台创建的资产，以及 Prometheus 上目标的 up 状态 + instance_name 实际取值
#
# 用法（在 nightjar 项目根目录）：
#   ./scripts/doctor-jd-link.sh
#   ./scripts/doctor-jd-link.sh --targets interview-mysql,interview-redis
#   ./scripts/doctor-jd-link.sh --target-container interview-backend
#
# 退出码：0=全部通过；非 0=失败项数量
# =============================================================================

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NIGHTJAR_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
TARGETS=(interview-mysql interview-redis)
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
    --targets)           IFS=',' read -r -a TARGETS <<< "${2:-}"; shift ;;
    --target-container)  TARGET_CONTAINER="${2:-}"; shift ;;
    --nightjar-dir)      NIGHTJAR_DIR="${2:-}"; shift ;;
    -h|--help)           sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf '未知参数：%s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

if ! command -v docker >/dev/null 2>&1; then
  printf '%s未找到 docker：请在 Docker 宿主机上执行本脚本%s\n' "$C_RED" "$C_RESET"
  exit 2
fi

env_get() { sed -n -E "s/^[[:space:]]*$2[[:space:]]*=(.*)$/\1/p" "$1" 2>/dev/null | head -n1; }
container_networks() { docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$1" 2>/dev/null | tr -s ' ' | sed 's/^ //;s/ $//'; }
resolve_network() { docker network ls --format '{{.Name}}' 2>/dev/null | grep -E "(_|^)${1}\$" | head -n1; }
has_net() { printf ' %s ' "$1" | grep -qw "$2"; }

NJ_ENV="$NIGHTJAR_DIR/.env"
MWOPS_NET=$(resolve_network 'mwops'); [ -z "$MWOPS_NET" ] && MWOPS_NET='middleware-ops_mwops'
NJ_PROM=$(env_get "$NJ_ENV" PROMETHEUS_PORT); [ -z "$NJ_PROM" ] && NJ_PROM=9090

printf '平台网络：%s    平台 Prometheus：127.0.0.1:%s    目标容器：%s\n' "$MWOPS_NET" "$NJ_PROM" "${TARGETS[*]}"

# ---------------------------------------------------------------------------
step '1/4 平台自身'
for pair in "mwops-backend:$MWOPS_NET" "mwops-prometheus:$MWOPS_NET" "mwops-grafana:$MWOPS_NET"; do
  name="${pair%%:*}"; want="${pair##*:}"
  actual=$(container_networks "$name")
  if [ -z "$actual" ]; then bad "$name 未运行"
  elif has_net "$actual" "$want"; then ok "$name → $actual"
  else bad "$name 不在 $want 上（实际：$actual）"; hint "cd $NIGHTJAR_DIR && docker compose up -d $name"; fi
done

# ---------------------------------------------------------------------------
step '2/4 自动发现能力（平台侧唯一前置条件）'
sock_src=$(docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' mwops-backend 2>/dev/null | tr ' ' '\n' | grep -x '/var/run/docker.sock' || true)
if [ -n "$sock_src" ]; then
  ok 'mwops-backend 已挂载 docker.sock：集成时可自动发现并接入目标网络'
  docker_flag=$(env_get "$NJ_ENV" INTEGRATION_DOCKER_ENABLED)
  case "$docker_flag" in
    true|1|yes) ok 'INTEGRATION_DOCKER_ENABLED 已开启' ;;
    *) bad "INTEGRATION_DOCKER_ENABLED=${docker_flag:-未设置}：一键拉起 Exporter/代建账号/日志接入都会不可用"
       hint "在 $NJ_ENV 里设为 true（或重跑 ./scripts/setup-jd-link.sh）" ;;
  esac
else
  bad 'mwops-backend 未挂载 docker.sock：平台无法自动发现目标网络（也不会自动接网）'
  hint '取消 docker-compose.yml 中 backend.volumes 的 docker.sock 注释后 docker compose up -d backend'
  hint '或直接重跑 ./scripts/setup-jd-link.sh（它会自动放开注释）'
fi

# ---------------------------------------------------------------------------
step '3/4 被管项目（平台会接入它们所在网络）'
target_nets=""
for name in "${TARGETS[@]}" "$TARGET_CONTAINER"; do
  [ -z "$name" ] && continue
  state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null)
  if [ -z "$state" ]; then
    warn "未找到容器 $name（未部署该容器时属正常）"
    continue
  fi
  nets=$(container_networks "$name")
  # 去掉平台网络本身，剩下的就是"平台需要自动接入"的目标网络
  for n in $nets; do
    [ "$n" = "$MWOPS_NET" ] && continue
    target_nets="$target_nets $n"
  done
  if [ "$state" = 'running' ]; then
    ok "$name 运行中，所在网络：${nets:-（无）}"
  else
    warn "$name 状态为 $state（不是 running）"
  fi
done
if [ -n "$(printf '%s' "$target_nets" | tr -d ' ')" ]; then
  info "平台集成时会自动接入的目标网络：$(printf '%s' "$target_nets" | tr ' ' '\n' | sort -u | tr '\n' ' ')"
else
  warn '未发现任何目标容器网络：被管项目还没起来，或目标容器名与默认值不同'
  hint '用 --targets 指定容器名，例如 --targets my-mysql,my-redis'
fi

# ---------------------------------------------------------------------------
step '4/4 平台创建的资产与抓取状态'
managed=$(docker ps --format '{{.Names}}' 2>/dev/null | grep '^mwops-exporter-' || true)
if [ -z "$managed" ]; then
  info '未发现平台创建的 Exporter（mwops-exporter-*）：到「集成中心」集成中间件并勾选一键拉起'
else
  for name in $managed; do
    actual=$(container_networks "$name")
    missing=""
    # 监控面必须有（Prometheus 要抓它）；目标网络也必须有（它要连被管实例）。
    has_net "$actual" "$MWOPS_NET" || missing="$missing $MWOPS_NET"
    for n in $(printf '%s' "$target_nets" | tr ' ' '\n' | sort -u); do
      [ -z "$n" ] && continue
      has_net "$actual" "$n" || missing="$missing $n"
    done
    if [ -z "$missing" ]; then
      ok "$name → $actual"
    else
      bad "$name 缺少网络：$missing（实际：$actual）"
      hint "在集成中心点该集的「重新应用」——平台会重新发现并接入目标网络"
    fi
  done
fi

collectors=$(docker ps --format '{{.Names}}' 2>/dev/null | grep -E '^mwops-exporter-.*-logs$|^mwops-logcollect|^mwops-.*-logs$' || true)
if [ -n "$collectors" ]; then
  for name in $collectors; do ok "日志采集容器 $name 运行中"; done
else
  info '尚无日志采集容器（在「集成中心 → 日志接入」创建）'
fi

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
        err = (t.get("lastError") or "").strip() or "(无 lastError：Exporter 容器可能未启动)"
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
  printf '%s接入链路就绪（%d 条提示）%s\n' "$C_GREEN" "$warns" "$C_RESET"
  printf '%s下一步：平台「集成中心」集成中间件/日志；实例详情「接入自检」应显示 matched>0%s\n' "$C_GRAY" "$C_RESET"
else
  printf '%s失败 %d 项、提示 %d 条，按上面的 [FAIL]/[HINT] 处理%s\n' "$C_RED" "$fail" "$warns" "$C_RESET"
fi
exit "$fail"
