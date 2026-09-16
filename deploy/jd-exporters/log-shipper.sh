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
#                          默认 /logs/error.log:error:ERROR;/logs/gc.log:gc:INFO
#
# 这是「够用就好」的实现：逐行上报，不做多行堆栈合并、不做本地断点续传。
# 生产环境建议换成 nightjar 官方的 mwops-agent（带偏移量、上下文行、批量上报）：
#   cd nightjar/middleware-ops && go build -o mwops-agent ./cmd/agent
#   ./mwops-agent -config agent.yaml      # 配置模板见同目录 agent.yaml
# =============================================================================

set -u

NIGHTJAR_URL="${NIGHTJAR_URL:-}"
if [ -z "$NIGHTJAR_URL" ]; then
    echo "log-shipper: 缺少 NIGHTJAR_URL，退出" >&2
    exit 1
fi

SERVICE_NAME="${SERVICE_NAME:-interview-review-backend}"
SERVER_NAME="${SERVER_NAME:-jd-host}"
LOG_TARGETS="${LOG_TARGETS:-/logs/error.log:error:ERROR;/logs/gc.log:gc:INFO}"
TOKEN="${NIGHTJAR_HOOK_TOKEN:-}"
ENDPOINT="${NIGHTJAR_URL%/}/api/hooks/logs"

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

post_line() {
    line="$1"
    level="$2"
    alert_type="$3"

    msg=$(printf '%s' "$line" | json_escape)
    [ -z "$msg" ] && return 0

    ts=$(date -u +%Y-%m-%dT%H:%M:%SZ)
    # 限制单条长度，避免一次异常把整份堆栈塞进平台（平台侧本就有 200 行上限）
    payload=$(printf '{"service":"%s","server_name":"%s","level":"%s","alert_type":"%s","message":"%s","timestamp":"%s"}' \
        "$SERVICE_NAME" "$SERVER_NAME" "$level" "$alert_type" "$msg" "$ts")

    if [ -n "$TOKEN" ]; then
        curl -sS -o /dev/null --max-time 10 \
            -X POST "$ENDPOINT" \
            -H 'Content-Type: application/json' \
            -H "X-Hook-Token: $TOKEN" \
            --data-binary "$payload" 2>/dev/null
    else
        curl -sS -o /dev/null --max-time 10 \
            -X POST "$ENDPOINT" \
            -H 'Content-Type: application/json' \
            --data-binary "$payload" 2>/dev/null
    fi
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

    echo "log-shipper: 开始 tail $file (alert_type=$alert_type, level=$level)"
    # -n 0 只上报启动之后的新增行；-F 跟随轮转（按文件名重试）
    tail -n 0 -F "$file" | while IFS= read -r line; do
        post_line "$line" "$level" "$alert_type"
    done
}

echo "log-shipper: endpoint=$ENDPOINT service=$SERVICE_NAME server=$SERVER_NAME"

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
