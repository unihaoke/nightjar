#!/usr/bin/env pwsh
<#
.SYNOPSIS
    日志上报接入示例（应用侧 HTTP Hook）。

.DESCRIPTION
    演示如何把应用日志推送到平台 /api/hooks/logs，覆盖三种常见形态：
      1. 单条 ERROR + 堆栈（Java 风格）
      2. 批量上报（同一错误出现 N 次，用 count 让平台累加）
      3. 从日志文件尾部扫描并上报（简易 Agent 形态，无 Go Agent 时的替代方案）

.PARAMETER BaseUrl
    平台地址，默认 http://127.0.0.1:8000（Docker 部署）/ http://127.0.0.1:8080（本地开发）

.PARAMETER HookToken
    上报令牌，需与后端环境变量 MWOPS_HOOK_TOKEN 一致；未开启校验时留空。

.PARAMETER LogFile
    指定后用「扫描日志文件」模式：按关键词抓取 ERROR 行并附带上下文上报。

.EXAMPLE
    pwsh -File scripts/hook-log-report.ps1 -BaseUrl http://127.0.0.1:8080
.EXAMPLE
    pwsh -File scripts/hook-log-report.ps1 -LogFile /var/log/order-service/error.log -TailLines 400
#>
[CmdletBinding()]
param(
    [string]$BaseUrl = 'http://127.0.0.1:8080',
    [string]$HookToken = '',
    [string]$Service = 'order-service',
    [string]$LogFile = '',
    [int]$TailLines = 400,
    [int]$ContextLines = 20
)

$ErrorActionPreference = 'Stop'
$endpoint = ($BaseUrl.TrimEnd('/')) + '/api/hooks/logs'

function Send-LogReport {
    param(
        [string]$Level,
        [string]$Message,
        [string]$Stacktrace = '',
        [string]$Context = '',
        [int]$Count = 1,
        [string]$AlertType = 'error'
    )
    $body = @{
        service       = $Service
        level         = $Level
        message       = $Message
        alert_type    = $AlertType
        count         = $Count
        timestamp     = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
    if ($Stacktrace) { $body.stacktrace = $Stacktrace }
    if ($Context) { $body.context_lines = $Context }

    $headers = @{ 'Content-Type' = 'application/json' }
    if ($HookToken) { $headers['X-Hook-Token'] = $HookToken }

    try {
        $response = Invoke-WebRequest -Method POST -Uri $endpoint -Headers $headers `
            -Body ($body | ConvertTo-Json -Depth 6) -TimeoutSec 15
        $parsed = ($response.Content | ConvertFrom-Json)
        $data = $parsed.data
        Write-Host ("上报成功  event_id={0}  signature={1}  merged={2}  count={3}" -f `
            $data.event_id, $data.signature, $data.merged, $data.count) -ForegroundColor Green
    }
    catch {
        Write-Host ("上报失败: " + $_.Exception.Message) -ForegroundColor Red
    }
}

if ($LogFile) {
    # ---- 模式 3：扫描日志文件，抓取 ERROR 行并附带上下文 ----
    if (-not (Test-Path $LogFile)) {
        Write-Host ("日志文件不存在: " + $LogFile) -ForegroundColor Red
        exit 1
    }
    $lines = Get-Content -LiteralPath $LogFile -Tail $TailLines
    Write-Host ("扫描 " + $LogFile + " 最近 " + $lines.Count + " 行…")

    $reported = 0
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -notmatch 'ERROR|FATAL|Exception|OutOfMemory|Full GC') { continue }

        # 向后聚合堆栈行（以 at / \tat 开头）
        $stack = $lines[$i]
        $j = $i + 1
        while ($j -lt $lines.Count -and $lines[$j] -match '^\s+at\s') {
            $stack += [Environment]::NewLine + $lines[$j]
            $j++
        }
        # 错误前后上下文
        $start = [Math]::Max(0, $i - [int]($ContextLines / 2))
        $end = [Math]::Min($lines.Count - 1, $j + [int]($ContextLines / 2))
        $context = ($lines[$start..$end] -join [Environment]::NewLine)

        $alertType = if ($stack -match '^\s+at\s') { 'stack' } else { 'error' }
        Send-LogReport -Level 'ERROR' -Message $lines[$i] -Stacktrace $stack -Context $context -AlertType $alertType
        $reported++
        $i = $j - 1
        if ($reported -ge 20) {
            Write-Host '已达单次上报上限 20 条，停止扫描。' -ForegroundColor Yellow
            break
        }
    }
    Write-Host ("扫描完成，共上报 " + $reported + " 条。")
    exit 0
}

# ---- 模式 1：单条 ERROR + 堆栈 ----
$stack = @(
    'java.lang.NullPointerException: Cannot invoke "Order.getAmount()" because "order" is null',
    '    at com.demo.order.OrderService.process(OrderService.java:42)',
    '    at com.demo.order.OrderController.create(OrderController.java:88)'
) -join [Environment]::NewLine

Send-LogReport -Level 'ERROR' -Message 'Order 10086 处理失败' -Stacktrace $stack -AlertType 'stack'

# ---- 模式 2：批量上报（同一错误在窗口内出现 37 次，用 count 让平台直接累加）----
Send-LogReport -Level 'ERROR' -Message 'Redis connection reset by peer' -Count 37

Write-Host ''
Write-Host '说明：' -ForegroundColor Cyan
Write-Host '  1) 相同「错误模板 + 首个业务栈帧」会被平台归并为同一错误指纹，5 分钟窗口内合并计数；'
Write-Host '  2) 冷却期内不会重复通知，避免告警风暴；'
Write-Host '  3) 到「日志告警」页可查看事件、触发 AI 代码分析；'
Write-Host '  4) 生产环境建议用轻量 Agent（cmd/agent）常驻采集，本脚本适合联调与应急批量补报。'
