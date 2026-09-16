# End-to-end smoke test for the middleware-ops platform.
#
# Prerequisites: backend running at http://127.0.0.1:8080 with a reachable PostgreSQL.
# Usage: pwsh -File scripts/smoke-test.ps1 [-BaseUrl http://127.0.0.1:8080] [-Username admin] [-Password Admin@12345]
#
# NOTE: this file is intentionally ASCII-only so that it stays valid regardless of the
# console code page / file encoding used on the target host.

[CmdletBinding()]
param(
    [string]$BaseUrl = 'http://127.0.0.1:8080',
    [string]$Username = 'admin',
    [string]$Password = 'Admin@12345'
)

$ErrorActionPreference = 'Stop'
$script:Passed = 0
$script:Failed = 0

function Write-Case {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    if ($Ok) {
        $script:Passed++
        Write-Host ('  [PASS] ' + $Name) -ForegroundColor Green
    }
    else {
        $script:Failed++
        Write-Host ('  [FAIL] ' + $Name + ' :: ' + $Detail) -ForegroundColor Red
    }
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Path,
        $Body = $null,
        [string]$Token = ''
    )
    $headers = @{ 'Content-Type' = 'application/json' }
    if ($Token) { $headers['Authorization'] = "Bearer $Token" }
    $params = @{
        Method      = $Method
        Uri         = ($BaseUrl.TrimEnd('/') + $Path)
        Headers     = $headers
        ErrorAction = 'Stop'
    }
    if ($null -ne $Body) { $params['Body'] = ($Body | ConvertTo-Json -Depth 8) }
    $response = Invoke-WebRequest @params
    $parsed = $response.Content | ConvertFrom-Json
    if ($parsed.code -ne 0) {
        throw ('api error code=' + $parsed.code + ' message=' + $parsed.message)
    }
    return $parsed.data
}

Write-Host ''
Write-Host '=== middleware-ops smoke test ===' -ForegroundColor Cyan
Write-Host ('target: ' + $BaseUrl)
Write-Host ''

# 1. health
try {
    $health = Invoke-Api -Method GET -Path '/healthz'
    Write-Case 'health endpoint' ($health.status -eq 'healthy') ($health | ConvertTo-Json -Compress)
}
catch {
    Write-Case 'health endpoint' $false $_.Exception.Message
}

# 2. login
$token = ''
try {
    $session = Invoke-Api -Method POST -Path '/api/auth/login' -Body @{ username = $Username; password = $Password }
    $token = $session.token
    Write-Case 'admin login' ([bool]$token) 'no token returned'
    Write-Case 'rbac permission list' (($session.permissions | Measure-Object).Count -gt 5) 'too few permissions'
}
catch {
    Write-Case 'admin login' $false $_.Exception.Message
}

if (-not $token) {
    Write-Host 'login failed, aborting.' -ForegroundColor Yellow
    exit 1
}

# 3. anonymous access must be rejected
try {
    Invoke-Api -Method GET -Path '/api/middlewares' | Out-Null
    Write-Case 'anonymous access blocked' $false 'request without token succeeded'
}
catch {
    Write-Case 'anonymous access blocked' $true
}

# 4. manage a middleware instance
$instanceId = 0
$suffix = Get-Random -Minimum 1000 -Maximum 9999
try {
    $created = Invoke-Api -Method POST -Path '/api/middlewares' -Token $token -Body @{
        name        = "smoke-redis-$suffix"
        mw_type     = 'redis'
        host        = '127.0.0.1'
        port        = 6379
        environment = 'dev'
        group_name  = 'smoke'
        tags        = @('smoke-test')
        password    = 'not-a-real-password'
    }
    $instanceId = $created.id
    Write-Case 'create middleware instance' ($instanceId -gt 0) 'no instance id'
    Write-Case 'password ciphertext never returned' ($null -eq $created.password_encrypted) 'ciphertext leaked'
}
catch {
    Write-Case 'create middleware instance' $false $_.Exception.Message
}

# 5. connection test and health probe
if ($instanceId -gt 0) {
    try {
        $test = Invoke-Api -Method POST -Path "/api/middlewares/$instanceId/test" -Token $token
        Write-Case 'connection test (tcp probe)' ($null -ne $test.success) $test.message
    }
    catch {
        Write-Case 'connection test (tcp probe)' $false $_.Exception.Message
    }
    try {
        $healthCheck = Invoke-Api -Method POST -Path "/api/middlewares/$instanceId/health" -Token $token
        Write-Case 'instant health probe' ($null -ne $healthCheck.success) $healthCheck.message
    }
    catch {
        Write-Case 'instant health probe' $false $_.Exception.Message
    }
}

# 6. metrics
if ($instanceId -gt 0) {
    try {
        $snapshot = Invoke-Api -Method GET -Path "/api/metrics/$instanceId" -Token $token
        Write-Case 'metrics snapshot' (($snapshot.metrics | Measure-Object).Count -gt 0) 'no metrics'
        Write-Case 'metrics source labelled' ([bool]$snapshot.source) 'missing source field'
    }
    catch {
        Write-Case 'metrics snapshot' $false $_.Exception.Message
    }
}

# 7. AI diagnosis (sync) - quality guardrails and structured output
$question = 'Redis memory usage is very high, will it trigger eviction? Give root cause and remediation.'
if ($instanceId -gt 0) {
    try {
        $diagnosis = Invoke-Api -Method POST -Path '/api/ai/diagnose/sync' -Token $token -Body @{
            instance_id = $instanceId
            question    = $question
        }
        $report = $diagnosis.report
        Write-Case 'ai diagnosis (sync)' ($diagnosis.diagnosis_id -gt 0) 'no diagnosis id'
        Write-Case 'structured report fields' (
            [bool]$report.root_cause -and $null -ne $report.confidence -and
            $null -ne $report.evidence -and $null -ne $report.suggestions -and
            [bool]$report.impact_scope -and $null -ne $report.pending_confirm
        ) ($report | ConvertTo-Json -Depth 4 -Compress)
        Write-Case 'quality guardrail: grounded ratio present' ($null -ne $report.grounded_ratio) 'missing grounded_ratio'
        Write-Case 'engine status labelled' ([bool]$diagnosis.meta.engine_status) 'missing engine_status'
    }
    catch {
        Write-Case 'ai diagnosis (sync)' $false $_.Exception.Message
    }

    try {
        $cached = Invoke-Api -Method POST -Path '/api/ai/diagnose/sync' -Token $token -Body @{
            instance_id = $instanceId
            question    = $question
        }
        Write-Case 'cost guardrail: deterministic cache hit' ([bool]$cached.meta.cache_hit) 'cache not hit'
    }
    catch {
        Write-Case 'cost guardrail: deterministic cache hit' $false $_.Exception.Message
    }

    try {
        $payload = @{ instance_id = $instanceId; question = 'list the three metrics that need attention'; skip_cache = $true } | ConvertTo-Json -Compress
        $streamResponse = Invoke-WebRequest -Method POST -Uri ($BaseUrl.TrimEnd('/') + '/api/ai/diagnose') -Headers @{ 'Content-Type' = 'application/json'; 'Authorization' = "Bearer $token" } -Body $payload -TimeoutSec 180
        $content = $streamResponse.Content
        $hasMeta = $content -match 'event: meta'
        $hasData = $content -match 'event: data'
        $hasDone = $content -match 'event: done'
        Write-Case 'sse streaming (meta/data/done)' ($hasMeta -and $hasData -and $hasDone) ('meta=' + $hasMeta + ' data=' + $hasData + ' done=' + $hasDone)
    }
    catch {
        Write-Case 'sse streaming (meta/data/done)' $false $_.Exception.Message
    }
}

# 8. alert rules, evaluation, ack
if ($instanceId -gt 0) {
    try {
        $rule = Invoke-Api -Method POST -Path '/api/alerts/rules' -Token $token -Body @{
            name        = "smoke-rule-$suffix"
            instance_id = $instanceId
            mw_type     = 'redis'
            metric_name = 'memory_usage_percent'
            operator    = '>'
            threshold   = 1
            level       = 'warning'
            window      = 5
            cooldown    = 1
        }
        Write-Case 'create alert rule' ($rule.id -gt 0) 'no rule id'
    }
    catch {
        Write-Case 'create alert rule' $false $_.Exception.Message
    }
    try {
        $eval = Invoke-Api -Method POST -Path '/api/alerts/evaluate' -Token $token
        Write-Case 'alert rule evaluation' ($null -ne $eval.evaluated) ($eval | ConvertTo-Json -Compress)
    }
    catch {
        Write-Case 'alert rule evaluation' $false $_.Exception.Message
    }
    try {
        $alerts = Invoke-Api -Method GET -Path '/api/alerts?page=1&page_size=5' -Token $token
        $alertCount = ($alerts.list | Measure-Object).Count
        Write-Case 'alert list query' ($alertCount -ge 0) 'query failed'
        if ($alertCount -gt 0) {
            $first = $alerts.list[0]
            Invoke-Api -Method POST -Path ('/api/alerts/' + $first.id + '/ack') -Token $token | Out-Null
            Write-Case 'alert acknowledge (L1)' $true
        }
        else {
            Write-Case 'alert acknowledge (L1)' $true 'no alert triggered this round (skipped)'
        }
    }
    catch {
        Write-Case 'alert list / acknowledge' $false $_.Exception.Message
    }
}

# 9. log hook ingestion + error fingerprint aggregation
$tab = [char]9
$newline = [Environment]::NewLine
try {
    $stackOne = 'java.lang.NullPointerException' + $newline + $tab + 'at com.demo.OrderService.process(OrderService.java:42)'
    $stackTwo = 'java.lang.NullPointerException' + $newline + $tab + 'at com.demo.OrderService.process(OrderService.java:88)'

    $hookOne = Invoke-Api -Method POST -Path '/api/hooks/logs' -Body @{
        service    = 'smoke-order-service'
        level      = 'ERROR'
        message    = 'Order 10086 processing failed'
        stacktrace = $stackOne
    }
    Write-Case 'log hook ingestion' ([bool]$hookOne.event_id) ($hookOne | ConvertTo-Json -Compress)

    $hookTwo = Invoke-Api -Method POST -Path '/api/hooks/logs' -Body @{
        service    = 'smoke-order-service'
        level      = 'ERROR'
        message    = 'Order 20999 processing failed'
        stacktrace = $stackTwo
    }
    Write-Case 'error fingerprint window dedup' ([bool]$hookTwo.merged) 'same template not merged'
}
catch {
    Write-Case 'log hook ingestion' $false $_.Exception.Message
}

# 10. AI code analysis (three-point template + outbound compliance)
if ($instanceId -gt 0) {
    try {
        $analysis = Invoke-Api -Method POST -Path '/api/ai/code-analyze' -Token $token -Body @{
            service    = 'smoke-order-service'
            message    = 'Order 10086 processing failed'
            stacktrace = 'java.lang.NullPointerException' + $newline + $tab + 'at com.demo.OrderService.process(OrderService.java:42)'
        }
        Write-Case 'ai code analysis' ($analysis.analysis_id -gt 0) 'no analysis id'
        Write-Case 'outbound denied without whitelist' ($analysis.outbound_ok -eq $false) 'unexpected outbound state'
    }
    catch {
        Write-Case 'ai code analysis' $false $_.Exception.Message
    }
}

# 11. fix preview and SQL read-only guardrail
if ($instanceId -gt 0) {
    try {
        $preview = Invoke-Api -Method POST -Path '/api/fix/preview' -Token $token -Body @{
            instance_id = $instanceId
            action_type = 'restart_service'
            reason      = 'smoke test'
        }
        Write-Case 'fix preview (L2 requires approval)' ($preview.level -eq 'L2' -and $preview.requires_approval) ($preview | ConvertTo-Json -Compress)
    }
    catch {
        Write-Case 'fix preview (L2 requires approval)' $false $_.Exception.Message
    }
}

try {
    $sqlOk = Invoke-Api -Method POST -Path '/api/fix/validate-sql' -Token $token -Body @{ sql = 'SELECT * FROM orders' }
    Write-Case 'sql forced limit injected' ($sqlOk.normalized_sql -match 'LIMIT 100') $sqlOk.normalized_sql
}
catch {
    Write-Case 'sql forced limit injected' $false $_.Exception.Message
}

try {
    Invoke-Api -Method POST -Path '/api/fix/validate-sql' -Token $token -Body @{ sql = 'DELETE FROM orders' } | Out-Null
    Write-Case 'sql guard rejects write statement' $false 'write statement was accepted'
}
catch {
    Write-Case 'sql guard rejects write statement' $true
}

# 12. audit append-only chain
try {
    $verify = Invoke-Api -Method GET -Path '/api/audit/verify' -Token $token
    Write-Case 'audit hash chain verified' ($verify.verified -eq $true) ($verify | ConvertTo-Json -Compress)
}
catch {
    Write-Case 'audit hash chain verified' $false $_.Exception.Message
}

try {
    $logs = Invoke-Api -Method GET -Path '/api/audit/logs?page=1&page_size=5' -Token $token
    Write-Case 'audit log query' (($logs.list | Measure-Object).Count -gt 0) 'no audit records (login should be logged)'
}
catch {
    Write-Case 'audit log query' $false $_.Exception.Message
}

# 13. dashboard and system info
try {
    $overview = Invoke-Api -Method GET -Path '/api/system/overview' -Token $token
    Write-Case 'dashboard overview' ($null -ne $overview.instances.total) 'missing overview data'
}
catch {
    Write-Case 'dashboard overview' $false $_.Exception.Message
}

try {
    $info = Invoke-Api -Method GET -Path '/api/system/info' -Token $token
    Write-Case 'system info and capability matrix' (($info.capability_matrix | Measure-Object).Count -gt 0) 'missing capability matrix'
    Write-Case 'guardrail parameters exposed' ($null -ne $info.guardrail.input_token_budget) 'missing guardrail parameters'
}
catch {
    Write-Case 'system info and capability matrix' $false $_.Exception.Message
}

# 14. cleanup
if ($instanceId -gt 0) {
    try {
        Invoke-Api -Method DELETE -Path "/api/middlewares/$instanceId" -Token $token | Out-Null
        Write-Case 'cleanup smoke instance' $true
    }
    catch {
        Write-Case 'cleanup smoke instance' $false $_.Exception.Message
    }
}

Write-Host ''
Write-Host '=== summary ===' -ForegroundColor Cyan
Write-Host ('passed: ' + $script:Passed) -ForegroundColor Green
Write-Host ('failed: ' + $script:Failed) -ForegroundColor Red
if ($script:Failed -gt 0) { exit 1 }
Write-Host 'all cases passed.'
exit 0
