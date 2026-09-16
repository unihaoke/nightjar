#!/bin/sh
# =============================================================================
# jd → nightjar 日志上报（零依赖，alpine + curl）
#
# 契约对齐 nightjar 的 internal/service/logalert.go:LogReport：
#   POST {NIGHTJAR_URL}/api/hooks/logs
#   Header: X-Hook-Token: <与 nightjar 的 HOOK_TOKEN 一致>
#   Body: {"service","server_name","level","message","alert_type","timestamp"}
#         service / level / message 为必填；timestamp 必须是 RFC3339
#
# 环境变量：
#   NIGHTJAR_URL           必填，例如 http://mwops-backend:8080
#   NIGHTJAR_HOOK_TOKEN    与 nightjar 的 HOOK_TOKEN 一致；为空则不带头（nightjar 未配令牌时）
#   SERVICE_NAME           默认 interview-review-backend
#   SERVER_NAME            默认 jd-host
#   LOG_TARGETS            分号分隔的 文件绝对路径:alert_type:level
#                          默认 /logs/error.log:error:ERROR
#
# 默认只采集 error.log，原因（别改回去，会踩坑）：
#   * 本脚本是「逐行一个 HTTP 请求」的简易实现，采集量必须可控；
#   * gc.log 配合后端 Dockerfile 的 -Xlog:gc*,gc+age=trace 每秒可能产生多行，
#     逐行上报会同时打爆网络与平台的日志告警列表（每行都会按指纹生成事件）；
#   * 需要 GC 诊断时按需开启，并先把 JVM 参数收敛为 -Xlog:gc（去掉 gc* 与 age trace）：
#       LOG_TARGETS="/logs/error.log:error:ERROR;/logs/gc.log:gc:INFO"
#   全量与批量采集请改用官方 Agent（带偏移量、上下文行、批量上报）：
#     cd nightjar/middleware-ops && go build -o mwops-agent ./cmd/agent
#     ./mwops-agent -config agent.yaml      # 配置模板见同目录 agent.yaml
#
# 可观测性：上报失败（401/403/404/连不上）会打印到 stderr 并计数，
#   用 `docker logs jd-log-agent` 即可看到，不再静默丢日志。
# =============================================================================

set -u

NIGHTJAR_URL="${NIGHTJAR_URL:-}"
if [ -z "$NIGHTJAR_URL" ]; then
    echo "log-shipper: 缺少 NIGHTJAR_URL，退出" >&2
    exit 1
fi

SERVICE_NAME="${SERVICE_NAME:-interview-review-backend}"
SERVER_NAME="${SERVER_NAME:-jd-host}"
LOG_TARGETS="${LOG_TARGETS:-/logs/error.log:error:ERROR}"
TOKEN="${NIGHTJAR_HOOK_TOKEN:-}"
ENDPOINT="${NIGHTJAR_URL%/}/api/hooks/logs"

FAIL_FILE=/tmp/mwops-shipper-failures
BODY_FILE=/tmp/mwops-shipper-body
ERR_FILE=/tmp/mwops-shipper-curl.err
: > "$FAIL_FILE"

# 最小 JSON 字符串转义：\ " 制表符 回车
json_escape() {
    awk '{
        gsub(/\\/, "\\\\");
        gsub(/"/, "\\\"");
        gsub(/\t/, "\\t");
        gsub(/\r/, "");
        printf "%s", $0
    }'
}

# note_failure 记录一次上报失败；前 3 次与之后每 50 次打印一条，避免刷屏。
note_failure() {
    code="$1"
    n=$(cat "$FAIL_FILE" 2>/dev/null || echo 0)
    n=$((n + 1))
    printf '%s' "$n" > "$FAIL_FILE"
    if [ "$n" -le 3 ] || [ $((n % 50)) -eq 0 ]; then
        detail=$(head -c 200 "$BODY_FILE" 2>/dev/null)
        [ -z "$detail" ] && detail=$(head -c 200 "$ERR_FILE" 2>/dev/null)
        echo "log-shipper: 上报失败 #$n HTTP=$code body=$detail" >&2
    fi
    case "$code" in
        401|403)
            echo "log-shipper: 提示 → 令牌不一致：平台 .env 的 HOOK_TOKEN 必须等于 jd 侧 NIGHTJAR_HOOK_TOKEN" >&2
            ;;
        404)
            echo "log-shipper: 提示 → 地址不对：NIGHTJAR_URL 应形如 http://mwops-backend:8080（不带路径）" >&2
            ;;
        000)
            echo "log-shipper: 提示 → 连不上平台：确认容器已在 jd-nightjar 网络内，且 mwops-backend 已启动" >&2
            ;;
    esac
}

post_line() {
    line="$1"
    level="$2"
    alert_type="$3"

    msg=$(printf '%s' "$line" | json_escape)
    [ -z "$msg" ] && return 0

    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    payload=$(printf '{"service":"%s","server_name":"%s","level":"%s","alert_type":"%s","message":"%s","timestamp":"%s"}' \
        "$SERVICE_NAME" "$SERVER_NAME" "$level" "$alert_type" "$msg" "$ts")

    # -w '%{http_code}' 让失败可见：早期版本用 -o /dev/null 且吞掉 stderr，
    # 401/403/连不上都表现为"平台没有日志"，无从排查。
    if [ -n "$TOKEN" ]; then
        status=$(curl -sS -o "$BODY_FILE" -w '%{http_code}' --max-time 10 \
            -X POST "$ENDPOINT" \
            -H 'Content-Type: application/json' \
            -H "X-Hook-Token: $TOKEN" \
            --data-binary "$payload" 2>"$ERR_FILE")
    else
        status=$(curl -sS -o "$BODY_FILE" -w '%{http_code}' --max-time 10 \
            -X POST "$ENDPOINT" \
            -H 'Content-Type: application/json' \
            --data-binary "$payload" 2>"$ERR_FILE")
    fi
    case "$status" in
        2*) : ;;
        *) note_failure "${status:-000}" ;;
    esac
    return 0
}

follow_file() {
    file="$1"
    alert_type="$2"
    level="$3"

    # 后端首次启动前日志文件还不存在，先等一会儿
    i=0
    while [ ! -f "$file" ]; do
        i=$((i + 1))
        if [ "$i" -gt 120 ]; then
            echo "log-shipper: 等待 $file 超时（10 分钟），放弃该目标" >&2
            return 0
        fi
        sleep 5
    done
    if [ ! -r "$file" ]; then
        echo "log-shipper: $file 不可读（检查卷挂载与文件权限），放弃该目标" >&2
        return 0
    fi

    echo "log-shipper: 开始 tail $file (alert_type=$alert_type, level=$level)"
    # -n 0 只上报启动之后的新增行；-F 跟随轮转（按文件名重试）
    tail -n 0 -F "$file" | while IFS= read -r line; do
        post_line "$line" "$level" "$alert_type"
    done
}

echo "log-shipper: endpoint=$ENDPOINT service=$SERVICE_NAME server=$SERVER_NAME"
echo "log-shipper: targets=$LOG_TARGETS"
if [ -z "$TOKEN" ]; then
    echo "log-shipper: 警告 → 未设置 NIGHTJAR_HOOK_TOKEN，将不带令牌上报（仅在平台 HOOK_TOKEN 为空时可用）" >&2
fi

# 注意：这里刻意不用 `echo | while read` 的管道写法——管道会让 while 跑在子 shell 里，
# 后台任务就成了子 shell 的子进程，外层 wait 等不到，容器会立刻退出。
OLD_IFS="$IFS"
IFS=';'
for target in $LOG_TARGETS; do
    [ -z "$target" ] && continue
    file="${target%%:*}"
    rest="${target#*:}"
    alert_type="${rest%%:*}"
    level="${rest##*:}"
    [ "$file" = "$target" ] && continue
    follow_file "$file" "$alert_type" "$level" &
done
IFS="$OLD_IFS"

# 保持前台运行：等待所有 tail 任务（任一退出都不应让容器退出）
wait
