<#
.SYNOPSIS
  jd ⇄ nightjar 一键接入 / 一键修复。

.DESCRIPTION
  目标：**你只需要维护两份 .env，其余全部由脚本推导并写回**。
  本脚本把接入过程中所有「手工改文件」的动作自动化了：

    1. 生成缺失的密钥（JWT_SECRET / 各类口令 / HOOK_TOKEN），口令统一用十六进制，
       顺带避开「口令含 @ ( ) / 导致 DATA_SOURCE_NAME 解析失败」这个坑；
    2. 双向对齐必须一致的项：
         nightjar.HOOK_TOKEN == jd.NIGHTJAR_HOOK_TOKEN
         JD_NIGHTJAR_NETWORK 两边同值
         MWOPS_PROMETHEUS_BASE_URL 按取数方式指向正确的 Prometheus
         INTEGRATION_EXPORTER_NETWORK 自动拼成 "<mwops网络>,jd-nightjar"
    3. 端口自动错开（两个栈的 Prometheus / Web 端口不能相同）；
    4. 从 .env 推导并重写「以前要手工 sed」的文件：
         jd/deploy/jd-exporters/init/01-monitor-user.sql   → 监控账号口令
         jd/deploy/jd-exporters/my.cnf                     → 同上
    5. 创建 internal 互联网络、按正确的 overlay 启动两个栈、补建 MySQL 只读账号；
    6. 清掉历史遗留的手工网络（例如 jd-redis-exporter 被 connect 到平台网络）；
    7. 体检：容器 / 网络挂载 / 跨栈解析 / Prometheus 里的 instance_name 实际取值；
    8. 可选 -FixInstances：用平台接口把「prom_job 填成容器名 / 实例名与标签不一致」
       的纳管实例自动纠正为 Prometheus 里真实存在的 job 与 instance_name。

.PARAMETER Mode
  指标从哪里取：
    jd-prometheus   （默认）复用 jd 自带的 Prometheus，Exporter 由 jd 侧提供
    mwops-prometheus 用平台自带的 Prometheus，Exporter 由「集成中心」拉起

.PARAMETER DryRun
  只打印将要做的修改，不写任何文件、不调用 docker。首次使用建议先跑一次。

.EXAMPLE
  pwsh -File scripts/setup-jd-link.ps1 -DryRun
  pwsh -File scripts/setup-jd-link.ps1
  pwsh -File scripts/setup-jd-link.ps1 -Mode mwops-prometheus -FixInstances
#>
[CmdletBinding()]
param(
  [string]$NightjarDir = '.',
  [string]$JdDir = '..\jd',
  [ValidateSet('jd-prometheus', 'mwops-prometheus')]
  [string]$Mode = 'jd-prometheus',
  [switch]$DryRun,
  [switch]$SkipStart,
  [switch]$FixInstances
)

$ErrorActionPreference = 'Continue'
$script:changes = New-Object System.Collections.Generic.List[string]
$script:problems = New-Object System.Collections.Generic.List[string]

# ---------------------------------------------------------------------------
# 输出helpers
# ---------------------------------------------------------------------------
function Step($name) { Write-Host "`n=== $name ===" -ForegroundColor Cyan }
function Info($msg) { Write-Host "  $msg" }
function Ok($msg) { Write-Host "  [OK]   $msg" -ForegroundColor Green }
function Fix($msg) { Write-Host "  [FIX]  $msg" -ForegroundColor Yellow; $script:changes.Add($msg) }
function Warn($msg) { Write-Host "  [WARN] $msg" -ForegroundColor Yellow }
function Bad($msg) { Write-Host "  [FAIL] $msg" -ForegroundColor Red; $script:problems.Add($msg) }
function Hint($msg) { Write-Host "         → $msg" -ForegroundColor DarkGray }
function DryTag($msg) { Write-Host "  [dry-run] $msg" -ForegroundColor DarkGray; $script:changes.Add("[预览] $msg") }
# dry-run 的"跳过"提示不算变更，避免污染汇总
function DrySkip($msg) { Write-Host "  [dry-run] $msg" -ForegroundColor DarkGray }

# 密钥类变更一律脱敏：终端 scrollback 与 CI 日志都不该出现新口令
function Format-EnvChange([string]$key, [string]$old, [string]$new) {
  if ($key -match '(SECRET|PASSWORD|PASS|TOKEN|_KEY)$') {
    return "$key : ****** → ******（已更新，值见对应 .env）"
  }
  return "$key : $old → $new"
}

$modeText = if ($Mode -eq 'jd-prometheus') { '复用 jd 自带的 Prometheus' } else { '使用平台自带的 Prometheus（集成中心）' }
Write-Host "jd ⇄ nightjar 一键接入" -ForegroundColor White
Write-Host "  nightjar 目录 : $NightjarDir" -ForegroundColor Gray
Write-Host "  jd 目录       : $JdDir" -ForegroundColor Gray
Write-Host "  取数方式      : $Mode（$modeText）" -ForegroundColor Gray
if ($DryRun) { Write-Host "  模式          : DRY-RUN（不写文件、不调用 docker）" -ForegroundColor Yellow }

# ---------------------------------------------------------------------------
# .env 读写（保持注释与键顺序，值不可解析为多行）
# ---------------------------------------------------------------------------
$script:Utf8NoBom = New-Object System.Text.UTF8Encoding($false)

function Get-EnvPath([string]$dir, [string]$name) { return (Join-Path (Resolve-Path $dir).Path $name) }

function Read-EnvLines([string]$path) {
  return [System.IO.File]::ReadAllLines($path, $script:Utf8NoBom)
}

function Get-EnvValue([string[]]$lines, [string]$key) {
  $pattern = '^\s*' + [regex]::Escape($key) + '\s*=(.*)$'
  foreach ($line in $lines) {
    if ($line -match $pattern) { return $Matches[1].Trim() }
  }
  return $null
}

function Set-EnvValue([string[]]$lines, [string]$key, [string]$value) {
  $pattern = '^\s*' + [regex]::Escape($key) + '\s*='
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match $pattern) {
      $replacement = "$key=$value"
      if ($lines[$i] -ne $replacement) {
        Fix (Format-EnvChange $key $lines[$i].Trim() $replacement)
        $lines[$i] = $replacement
      }
      return , $lines
    }
  }
  Fix (Format-EnvChange $key '(不存在)' "$key=$value")
  return , ($lines + @("$key=$value"))
}

function Save-EnvLines([string]$path, [string[]]$lines) {
  if ($DryRun) { return }
  $backup = "$path.bak-$(Get-Date -Format yyyyMMddHHmmss)"
  Copy-Item -Force $path $backup
  # .env 必须无 BOM：带 BOM 会让第一个变量名解析失败
  [System.IO.File]::WriteAllLines($path, $lines, $script:Utf8NoBom)
  Info "已写回 $path（备份：$(Split-Path $backup -Leaf)）"
}

# 生成十六进制随机串：无特殊字符，避免 SQL/DSN/URL 转义问题
function New-Secret([int]$bytes = 24) {
  $buffer = New-Object byte[] $bytes
  $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($buffer) } finally { $rng.Dispose() }
  return (($buffer | ForEach-Object { $_.ToString('x2') }) -join '')
}

# 取端口十进制值（失败返回 0）
function Get-Port([string[]]$lines, [string]$key) {
  $value = Get-EnvValue $lines $key
  $parsed = 0
  if ($value -and [int]::TryParse($value, [ref]$parsed)) { return $parsed }
  return 0
}

# ---------------------------------------------------------------------------
# docker 包装（DryRun 下只打印）
# ---------------------------------------------------------------------------
function Test-DockerAvailable {
  if ($DryRun) { return $true }
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { return $false }
  & docker info 2>$null | Out-Null
  return ($LASTEXITCODE -eq 0)
}

function Invoke-Compose([string]$dir, [string[]]$composeArgs) {
  $rendered = "docker compose $($composeArgs -join ' ')"
  if ($DryRun) { DryTag "cd $dir && $rendered"; return $true }
  Info "▶ $rendered"
  Push-Location (Resolve-Path $dir).Path
  try {
    & docker compose @composeArgs 2>&1 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    return ($LASTEXITCODE -eq 0)
  } finally { Pop-Location }
}

function Invoke-DockerCmd([string[]]$dockerArgs, [switch]$Quiet) {
  if ($DryRun) { if (-not $Quiet) { DryTag "docker $($dockerArgs -join ' ')" }; return '' }
  $output = & docker @dockerArgs 2>$null
  return ($output -join "`n")
}

# ---------------------------------------------------------------------------
# 0. 前置检查
# ---------------------------------------------------------------------------
Step '0/8 前置检查'

if (-not (Test-Path $NightjarDir)) { Bad "nightjar 目录不存在：$NightjarDir"; exit 2 }
if (-not (Test-Path $JdDir)) { Bad "jd 目录不存在：$JdDir"; Hint '用 -JdDir 指定 jd 项目路径'; exit 2 }
if (-not (Get-Command docker -ErrorAction SilentlyContinue) -and -not $DryRun) {
  Bad '未找到 docker 命令：本脚本要在 Docker 宿主机上运行'; exit 2
}
if ($DryRun) { DrySkip '跳过 docker 可用性检查（dry-run 不依赖 docker）' }
elseif (Test-DockerAvailable) { Ok 'docker 可用' }
else { Bad 'docker 守护进程不可用'; exit 2 }

$njEnvPath = Get-EnvPath $NightjarDir '.env'
$jdEnvPath = Get-EnvPath $JdDir '.env'

# 缺少 .env 时从 .env.example 复制（DryRun 只提示）
$njExample = Get-EnvPath $NightjarDir '.env.example'
$jdExample = Get-EnvPath $JdDir '.env.example'
$njSource = $njEnvPath
$jdSource = $jdEnvPath
if (-not (Test-Path $njEnvPath)) {
  if ($DryRun) { DryTag "复制 $njExample → $njEnvPath"; $njSource = $njExample }
  else { Copy-Item $njExample $njEnvPath; Fix "由 .env.example 生成 $njEnvPath" }
}
if (-not (Test-Path $jdEnvPath)) {
  if ($DryRun) { DryTag "复制 $jdExample → $jdEnvPath"; $jdSource = $jdExample }
  else { Copy-Item $jdExample $jdEnvPath; Fix "由 .env.example 生成 $jdEnvPath" }
}
if (-not (Test-Path $njSource) -or -not (Test-Path $jdSource)) {
  Bad '缺少 .env / .env.example，无法继续'; exit 2
}
Ok "读取 nightjar 配置：$(Split-Path $njSource -Leaf)"
Ok "读取 jd 配置：$(Split-Path $jdSource -Leaf)"

$nj = Read-EnvLines $njSource
$jd = Read-EnvLines $jdSource

# ---------------------------------------------------------------------------
# 1. 生成缺失的密钥与口令（已有值一律保留）
# ---------------------------------------------------------------------------
Step '1/8 补齐密钥与口令（已有值一律保留）'

# 示例/弱口令清单：密钥类可以安全轮换（只影响会话），口令类只警告不改动。
$script:weakValues = @(
  'please_replace_with_a_random_48_byte_string', 'dev-secret-change-me-in-production-please-2026',
  'admin@12345', 'admin', 'root', 'password', '123456',
  'mwo_change_me', 'redis_change_me', 'jd_redis_change_me', 'exporter_change_me'
)

# Ensure-Secret：JWT / HOOK 这类"改了只影响会话"的密钥——为空或仍是示例值时自动生成。
function Ensure-Secret([string[]]$lines, [string]$key, [int]$bytes, [string]$desc) {
  $current = Get-EnvValue $lines $key
  if ([string]::IsNullOrWhiteSpace($current) -or ($script:weakValues -contains $current.ToLower())) {
    return , (Set-EnvValue $lines $key (New-Secret $bytes))
  }
  Ok "$key 已设置（$desc）"
  return , $lines
}

# Ensure-Password：数据库/缓存口令——只在**为空**时生成。
#
# 为什么不自动替换弱口令：MySQL 的 MYSQL_ROOT_PASSWORD / PostgreSQL 的 POSTGRES_PASSWORD
# 只在数据卷首次初始化时生效，事后改 .env 不会改库里的口令，改了反而连不上。
# 因此这里只警告，并把"改法与影响"说清楚。
function Ensure-Password([string[]]$lines, [string]$key, [int]$bytes, [string]$desc, [string]$impact) {
  $current = Get-EnvValue $lines $key
  if ([string]::IsNullOrWhiteSpace($current)) {
    return , (Set-EnvValue $lines $key (New-Secret $bytes))
  }
  if ($script:weakValues -contains $current.ToLower()) {
    Warn "$key 仍是示例/弱口令（$desc）：$impact"
  } else {
    Ok "$key 已设置（$desc）"
  }
  return , $lines
}

$nj = Ensure-Secret $nj 'JWT_SECRET' 32 '平台 JWT 密钥'
$nj = Ensure-Password $nj 'DB_PASSWORD' 16 '平台 PostgreSQL 口令' '已有数据卷时不要直接改，需先 ALTER USER 改库内口令再同步 .env'
$nj = Ensure-Password $nj 'REDIS_PASSWORD' 16 '平台自身 Redis 口令' 'Redis 口令每次启动都会生效，直接改是安全的（会重建容器）'
$nj = Ensure-Secret $nj 'ADMIN_PASSWORD' 12 '平台管理员口令'
$nj = Ensure-Secret $nj 'HOOK_TOKEN' 24 '日志上报令牌'

$jd = Ensure-Secret $jd 'JWT_SECRET' 32 'jd 应用 JWT 密钥'
$jd = Ensure-Password $jd 'DB_PASS' 16 'jd MySQL root 口令' 'MySQL 只在空数据卷首启时应用该口令，已有数据卷请用 ALTER USER 改库内口令后同步 .env'
$jd = Ensure-Password $jd 'REDIS_PASSWORD' 16 'jd Redis 口令' 'Redis 口令每次启动都会生效，直接改是安全的（会重建容器）'
$jd = Ensure-Password $jd 'MYSQL_EXPORTER_PASSWORD' 16 'MySQL 只读监控账号口令' '脚本会把新口令同步写入 01-monitor-user.sql 与 my.cnf，重跑 initdb 即生效'

# ---------------------------------------------------------------------------
# 2. 双向对齐必须一致的项
# ---------------------------------------------------------------------------
Step '2/8 对齐跨项目必须一致的值'

# 2.1 日志令牌：nightjar.HOOK_TOKEN == jd.NIGHTJAR_HOOK_TOKEN
$hook = Get-EnvValue $nj 'HOOK_TOKEN'
$jdHook = Get-EnvValue $jd 'NIGHTJAR_HOOK_TOKEN'
if ($hook -ne $jdHook) {
  $jd = Set-EnvValue $jd 'NIGHTJAR_HOOK_TOKEN' $hook
} else {
  Ok 'HOOK_TOKEN 与 NIGHTJAR_HOOK_TOKEN 一致'
}

# 2.2 跨栈网络名
$network = Get-EnvValue $nj 'JD_NIGHTJAR_NETWORK'
if ([string]::IsNullOrWhiteSpace($network)) { $network = 'jd-nightjar' }
$nj = Set-EnvValue $nj 'JD_NIGHTJAR_NETWORK' $network
$jd = Set-EnvValue $jd 'JD_NIGHTJAR_NETWORK' $network

# 2.3 指标来源与集成中心网络
$wantBaseUrl = if ($Mode -eq 'jd-prometheus') { 'http://jd-prometheus:9090' } else { 'http://prometheus:9090' }
$nj = Set-EnvValue $nj 'MWOPS_PROMETHEUS_BASE_URL' $wantBaseUrl

# compose 项目名 middleware-ops（见 docker-compose.yml 的 name:）→ 网络 middleware-ops_mwops
$mwopsNetwork = 'middleware-ops_mwops'
$jd = Set-EnvValue $jd 'NIGHTJAR_URL' 'http://mwops-backend:8080'
$nj = Set-EnvValue $nj 'INTEGRATION_EXPORTER_NETWORK' "$mwopsNetwork,$network"

# 2.4 实例名（唯一真源）：平台「实例名称」== Prometheus 的 instance_name 标签值。
# 第 5 步会用它同步 prometheus-jd.yml 的 relabel replacement 与 agent.yaml。
$redisName = Get-EnvValue $jd 'JD_REDIS_INSTANCE_NAME'
$mysqlName = Get-EnvValue $jd 'JD_MYSQL_INSTANCE_NAME'
if ([string]::IsNullOrWhiteSpace($redisName)) { $redisName = 'jd-redis' }
if ([string]::IsNullOrWhiteSpace($mysqlName)) { $mysqlName = 'jd-mysql' }
$jd = Set-EnvValue $jd 'JD_REDIS_INSTANCE_NAME' $redisName
$jd = Set-EnvValue $jd 'JD_MYSQL_INSTANCE_NAME' $mysqlName
Ok "实例名（Prometheus instance_name）：redis=$redisName，mysql=$mysqlName"

# ---------------------------------------------------------------------------
# 3. 端口错开
# ---------------------------------------------------------------------------
Step '3/8 端口错开（两个栈不能占用同一个宿主端口）'

$njProm = Get-Port $nj 'PROMETHEUS_PORT'
$jdProm = Get-Port $jd 'PROMETHEUS_PORT'
if ($njProm -eq 0 -and $jdProm -eq 0) { $njProm = 9090; $jdProm = 9091 }
if ($njProm -eq 0) { $njProm = if ($jdProm -eq 9090) { 9091 } else { 9090 } }
if ($jdProm -eq 0) { $jdProm = if ($njProm -eq 9090) { 9091 } else { 9090 } }
if ($njProm -eq $jdProm) {
  # 约定：nightjar 保持 9090，把 jd 挪到 9091（与 jd/.env.example 的注释一致）
  if ($njProm -eq 9090) { $jdProm = 9091 } else { $njProm = 9090 }
}
$nj = Set-EnvValue $nj 'PROMETHEUS_PORT' "$njProm"
$jd = Set-EnvValue $jd 'PROMETHEUS_PORT' "$jdProm"
Ok "Prometheus 宿主端口：nightjar=$njProm，jd=$jdProm"

$njWeb = Get-Port $nj 'WEB_PORT'
$jdWeb = Get-Port $jd 'WEB_PORT'
if ($njWeb -eq 0) { $njWeb = 8000 }
if ($jdWeb -eq 0) { $jdWeb = 8542 }
if ($njWeb -eq $jdWeb) {
  $njWeb = if ($jdWeb -eq 8000) { 8001 } else { 8000 }
}
$nj = Set-EnvValue $nj 'WEB_PORT' "$njWeb"
$jd = Set-EnvValue $jd 'WEB_PORT' "$jdWeb"
Ok "Web 宿主端口：nightjar=$njWeb，jd=$jdWeb"

# ---------------------------------------------------------------------------
# 4. 写回 .env
# ---------------------------------------------------------------------------
Step '4/8 写回 .env'
Save-EnvLines $njSource $nj
Save-EnvLines $jdSource $jd
# 写回后以新值重新读取，供后续步骤（派生文件、校验）使用
if (-not $DryRun) {
  if (Test-Path $njEnvPath) { $nj = Read-EnvLines $njEnvPath }
  if (Test-Path $jdEnvPath) { $jd = Read-EnvLines $jdEnvPath }
}

# ---------------------------------------------------------------------------
# 5. 从 .env 推导「以前要手工改」的文件
# ---------------------------------------------------------------------------
Step '5/8 由 .env 推导派生的配置文件'

function Update-FileByRegex([string]$path, [string]$pattern, [string]$replacement, [string]$desc, [string]$masked) {
  if (-not (Test-Path $path)) { Info "跳过不存在的文件：$path"; return }
  $raw = [System.IO.File]::ReadAllText($path, $script:Utf8NoBom)
  $updated = [regex]::Replace($raw, $pattern, $replacement)
  if ($updated -eq $raw) { Ok "$desc 已是最新"; return }
  if ($DryRun) { DryTag "$desc → $masked（$path）"; return }
  [System.IO.File]::WriteAllText($path, $updated, $script:Utf8NoBom)
  Fix "$desc → $masked（$path）"
}

# Sync-PrometheusRelabel 把两个中间件 job 的 instance_name（relabel replacement）
# 对齐到 .env 里的实例名。
#
# 逐行处理并跟踪当前 job_name：replacement 在文件里出现多次，只有跟在
# middleware-exporter-redis / -mysql 之后的那个才该改；顺带避免误改注释里的示例文本。
function Sync-PrometheusRelabel([string]$path, [string]$redisName, [string]$mysqlName) {
  if (-not (Test-Path $path)) { Info "跳过不存在的文件：$path"; return }
  $lines = [System.IO.File]::ReadAllLines($path, $script:Utf8NoBom)
  $currentJob = ''
  $changes = New-Object System.Collections.Generic.List[string]
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match "job_name:\s*'?([^'\s]+)'?") { $currentJob = $Matches[1] }
    if ($lines[$i] -match '^(\s*)replacement:\s*(\S+)\s*$') {
      $indent = $Matches[1]
      $current = $Matches[2]
      $want = switch ($currentJob) {
        'middleware-exporter-redis' { $redisName }
        'middleware-exporter-mysql' { $mysqlName }
        default { $null }
      }
      if ($want -and $current -ne $want) {
        $lines[$i] = "${indent}replacement: $want"
        $changes.Add("$currentJob : $current → $want")
      }
    }
  }
  if ($changes.Count -eq 0) { Ok "Prometheus relabel 实例名已是最新（$path）"; return }
  if ($DryRun) { foreach ($c in $changes) { DryTag "prometheus relabel $c（$path）" }; return }
  [System.IO.File]::WriteAllLines($path, $lines, $script:Utf8NoBom)
  foreach ($c in $changes) { Fix "prometheus relabel $c（$path）" }
}

$monitorPassword = Get-EnvValue $jd 'MYSQL_EXPORTER_PASSWORD'
if (-not [string]::IsNullOrWhiteSpace($monitorPassword)) {
  # 只替换 SQL 语句里的 BY '<口令>'，不动注释里的占位说明（例如 "本文件的 BY '<口令>'"）。
  $sqlPath = Join-Path (Resolve-Path $JdDir).Path 'deploy\jd-exporters\init\01-monitor-user.sql'
  if (Test-Path $sqlPath) {
    $sqlLines = [System.IO.File]::ReadAllLines($sqlPath, $script:Utf8NoBom)
    $touched = $false
    for ($i = 0; $i -lt $sqlLines.Count; $i++) {
      if ($sqlLines[$i] -match '^\s*(CREATE|ALTER)\s+USER\b') {
        $replaced = [regex]::Replace($sqlLines[$i], "BY\s+'[^']*'", "BY '$monitorPassword'")
        if ($replaced -ne $sqlLines[$i]) { $sqlLines[$i] = $replaced; $touched = $true }
      }
    }
    if (-not $touched) {
      Ok 'MySQL 监控账号口令（01-monitor-user.sql）已是最新'
    } elseif ($DryRun) {
      DryTag "MySQL 监控账号口令（01-monitor-user.sql）→ BY ******"
    } else {
      [System.IO.File]::WriteAllLines($sqlPath, $sqlLines, $script:Utf8NoBom)
      Fix "MySQL 监控账号口令（01-monitor-user.sql）→ BY ******"
    }
  } else {
    Info "跳过不存在的文件：$sqlPath"
  }
  $cnfPath = Join-Path (Resolve-Path $JdDir).Path 'deploy\jd-exporters\my.cnf'
  Update-FileByRegex $cnfPath '(?m)^\s*password\s*=.*$' "password = $monitorPassword" 'my.cnf 的 exporter 口令' 'password = ******'
}

# 5.2 实例名：把 .env 的 JD_*_INSTANCE_NAME 同步进 Prometheus relabel 与 agent.yaml。
#
# 为什么要同步：平台按 instance_name 标签定位实例，而这个标签值写在 prometheus-jd.yml 的
# relabel replacement 里，是**文件里的硬编码**。以前改名要同时改 .env、relabel、平台实例名
# 三处，漏一处就出现"up=1 但 matched=0"。现在以 .env 为唯一真源，脚本负责铺开
# （这两个值在第 2 步已写回 .env）。
$relabelTargets = @(
  (Join-Path (Resolve-Path $JdDir).Path 'deploy\jd-exporters\prometheus-jd.yml'),
  # 平台仓库里保留的同名副本也一起同步，避免两份配置漂移
  (Join-Path (Resolve-Path $NightjarDir).Path 'deploy\jd-exporters\prometheus-jd.yml')
)
$redisName = Get-EnvValue $jd 'JD_REDIS_INSTANCE_NAME'
$mysqlName = Get-EnvValue $jd 'JD_MYSQL_INSTANCE_NAME'
foreach ($relabelPath in $relabelTargets) {
  Sync-PrometheusRelabel $relabelPath $redisName $mysqlName
}

# agent.yaml 只在改用官方 Agent 时才生效，同样从 .env 派生，省一次手填
$agentPath = Join-Path (Resolve-Path $JdDir).Path 'deploy\jd-exporters\agent.yaml'
$hookToken = Get-EnvValue $jd 'NIGHTJAR_HOOK_TOKEN'
$nightjarUrl = Get-EnvValue $jd 'NIGHTJAR_URL'
if (Test-Path $agentPath) {
  Update-FileByRegex $agentPath '(?m)^\s*hook_token:.*$' "hook_token: $hookToken" 'agent.yaml 的上报令牌' 'hook_token: ******'
  Update-FileByRegex $agentPath '(?m)^\s*platform_url:.*$' "platform_url: $nightjarUrl" 'agent.yaml 的平台地址' "platform_url: $nightjarUrl"
} else {
  Info '跳过不存在的文件：agent.yaml（未使用官方 Agent 时属正常）'
}

# 需要 docker.sock 才能一键拉起 Exporter，这里只做提示
if ((Get-EnvValue $nj 'INTEGRATION_DOCKER_ENABLED') -eq 'true') {
  $composePath = Join-Path (Resolve-Path $NightjarDir).Path 'docker-compose.yml'
  $composeRaw = [System.IO.File]::ReadAllText($composePath, $script:Utf8NoBom)
  if ($composeRaw -match '(?m)^\s*#\s*-\s*/var/run/docker\.sock') {
    Bad 'INTEGRATION_DOCKER_ENABLED=true，但 docker-compose.yml 里的 docker.sock 挂载仍是注释状态'
    Hint '取消 docker-compose.yml 中 backend.volumes 的 "/var/run/docker.sock:/var/run/docker.sock" 注释'
  } else {
    Ok 'docker.sock 已挂载（一键部署可用）'
  }
}

# ---------------------------------------------------------------------------
# 6. 网络与启动
# ---------------------------------------------------------------------------
Step '6/8 互联网络与启动'

if ($DryRun) {
  DryTag "docker network create --driver bridge --internal $network（不存在时）"
} else {
  $internal = Invoke-DockerCmd @('network', 'inspect', $network, '--format', '{{.Internal}}') -Quiet
  if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($internal)) {
    Info "创建跨栈网络 $network（internal）"
    Invoke-DockerCmd @('network', 'create', '--driver', 'bridge', '--internal', $network) | Out-Null
    Fix "创建 internal 网络 $network"
  } elseif ($internal.Trim() -ne 'true') {
    Bad "$network 已存在但不是 internal 网络（Internal=$internal）"
    Hint "docker network rm $network 后重跑本脚本"
  } else {
    Ok "$network 存在且为 internal"
  }
}

if ($SkipStart) {
  Info '按 -SkipStart 跳过启动'
} else {
  # jd 侧：方式二带 Exporter overlay；方式一只需网络 overlay。
  # 若已配置日志令牌，顺带带上 logs profile，日志 Agent 一起起来。
  $jdOverlay = if ($Mode -eq 'jd-prometheus') { 'deploy/jd-exporters/docker-compose.jd.yml' } else { 'deploy/jd-exporters/docker-compose.jd-link.yml' }
  $jdArgs = @('-f', 'docker-compose.yml', '-f', $jdOverlay)
  if (-not [string]::IsNullOrWhiteSpace((Get-EnvValue $jd 'NIGHTJAR_HOOK_TOKEN'))) { $jdArgs += @('--profile', 'logs') }
  $jdArgs += @('up', '-d', '--build')
  Invoke-Compose $JdDir $jdArgs | Out-Null

  if ($Mode -eq 'jd-prometheus') {
    Info '等待 MySQL 就绪并补建只读监控账号…'
    if (-not $DryRun) {
      for ($i = 0; $i -lt 30; $i++) {
        $health = Invoke-DockerCmd @('inspect', '-f', '{{.State.Health.Status}}', 'interview-mysql') -Quiet
        if ($health -and $health.Trim() -eq 'healthy') { break }
        Start-Sleep -Seconds 5
      }
    }
    Invoke-Compose $JdDir (@('-f', 'docker-compose.yml', '-f', $jdOverlay, '--profile', 'initdb', 'run', '--rm', 'mysql-monitor-user')) | Out-Null
  }

  Invoke-Compose $NightjarDir @('-f', 'docker-compose.yml', '-f', 'deploy/compose.jd-link.yml', 'up', '-d', '--build') | Out-Null
}

# 清掉历史遗留的手工网络（不清理会让"平台 Prometheus 抓到 jd 的 Exporter"这类假象继续存在）
Step '7/8 清理历史遗留的手工网络'
foreach ($name in @('jd-redis-exporter', 'jd-mysqld-exporter')) {
  if ($DryRun) { DryTag "docker network disconnect $mwopsNetwork $name（若存在）"; continue }
  $nets = Invoke-DockerCmd @('inspect', '-f', '{{json .NetworkSettings.Networks}}', $name) -Quiet
  if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($nets)) { Info "$name 不存在，跳过"; continue }
  if ($nets -match [regex]::Escape($mwopsNetwork)) {
    Invoke-DockerCmd @('network', 'disconnect', $mwopsNetwork, $name) | Out-Null
    Fix "$name 从 $mwopsNetwork 摘除（该网络本不该有它）"
  } else {
    Ok "$name 网络正常"
  }
}

# ---------------------------------------------------------------------------
# 8. 体检
# ---------------------------------------------------------------------------
Step '8/8 体检'

function Get-ContainerNetworks([string]$name) {
  if ($DryRun) { return @() }
  $raw = Invoke-DockerCmd @('inspect', '-f', '{{json .NetworkSettings.Networks}}', $name) -Quiet
  if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace($raw)) { return $null }
  try { $obj = $raw | ConvertFrom-Json } catch { return @() }
  if ($null -eq $obj) { return @() }
  return @($obj.PSObject.Properties.Name)
}

function Assert-Networks([string]$name, [string[]]$expected) {
  $actual = Get-ContainerNetworks $name
  if ($null -eq $actual) { Bad "$name 未运行"; return }
  if ($actual.Count -eq 0) { Bad "$name 没有任何网络"; return }
  $missing = @($expected | Where-Object { $_ -and ($actual -notcontains $_) })
  if ($missing.Count -gt 0) {
    Bad "$name 缺少网络：$($missing -join ', ')（实际：$($actual -join ', ')）"
  } else {
    Ok "$name → $($actual -join ', ')"
  }
}

if ($DryRun) {
  DrySkip '跳过容器网络与连通性检查（dry-run）'
} else {
  Assert-Networks 'mwops-backend' @($mwopsNetwork, $network)
  Assert-Networks 'mwops-prometheus' @($mwopsNetwork)
  Assert-Networks 'interview-redis' @(($network))
  Assert-Networks 'interview-mysql' @(($network))
  Assert-Networks 'interview-prometheus' @($network)

  foreach ($alias in @('jd-prometheus', 'jd-mysql', 'jd-redis')) {
    $resolved = Invoke-DockerCmd @('exec', 'mwops-backend', 'getent', 'hosts', $alias) -Quiet
    if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($resolved)) { Ok "mwops-backend 解析 $alias 正常" }
    else { Bad "mwops-backend 解析不了 $alias"; Hint '确认平台是带 deploy/compose.jd-link.yml 启动的' }
  }

  # 指标标签核验：直接问 Prometheus 该 job 下真实的 instance_name
  $promPort = if ($Mode -eq 'jd-prometheus') { $jdProm } else { $njProm }
  $job = 'middleware-exporter-redis'
  try {
    $uri = "http://127.0.0.1:$promPort/api/v1/label/instance_name/values?match[]=" + [uri]::EscapeDataString("up{job=`"$job`"}")
    $resp = Invoke-RestMethod -Uri $uri -TimeoutSec 5
    if ($resp.data.Count -gt 0) {
      Ok "Prometheus(:$promPort) 上 job=$job 的 instance_name = $($resp.data -join ', ')"
      if ($resp.data -contains $redisName) {
        Ok "与 .env 的 JD_REDIS_INSTANCE_NAME（$redisName）一致，平台实例名应填 $redisName"
      } else {
        Warn "与 .env 的 JD_REDIS_INSTANCE_NAME（$redisName）不一致：relabel 改动尚未生效"
        Hint 'docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml up -d --force-recreate prometheus'
      }
    } else {
      Bad "Prometheus(:$promPort) 上 job=$job 没有 instance_name 标签"
      Hint '抓取配置缺 relabel，或 Prometheus 未用带 overlay 的配置重建（docker compose ... up -d --force-recreate prometheus）'
    }
  } catch {
    Bad "无法访问 http://127.0.0.1:$promPort（$($_.Exception.Message)）"
    Hint '端口可能不是这个：jd 的看 <jd>/.env 的 PROMETHEUS_PORT，平台的看 <nightjar>/.env 的 PROMETHEUS_PORT'
  }
}

# ---------------------------------------------------------------------------
# 9. 可选：纠正平台的纳管实例
# ---------------------------------------------------------------------------
if ($FixInstances) {
  Step '附加：纠正平台纳管实例（prom_job / 实例名）'
  if ($DryRun) {
    DryTag '登录平台并把 prom_job 非 job 名的实例纠正为 middleware-exporter-<类型>'
  } else {
    $admin = Get-EnvValue $nj 'ADMIN_USER'; if (-not $admin) { $admin = 'admin' }
    $adminPass = Get-EnvValue $nj 'ADMIN_PASSWORD'
    $base = "http://127.0.0.1:$njWeb"
    try {
      $login = Invoke-RestMethod -Method Post -Uri "$base/api/auth/login" -ContentType 'application/json' `
        -Body (@{ username = $admin; password = $adminPass } | ConvertTo-Json)
      $token = $login.data.token
      $headers = @{ Authorization = "Bearer $token" }
      $list = Invoke-RestMethod -Uri "$base/api/middlewares?page=1&page_size=100" -Headers $headers
      $promPort = if ($Mode -eq 'jd-prometheus') { $jdProm } else { $njProm }
      $jobs = @()
      try { $jobs = (Invoke-RestMethod -Uri "http://127.0.0.1:$promPort/api/v1/label/job/values" -TimeoutSec 5).data } catch { }
      foreach ($item in $list.data.items) {
        if ($item.mw_type -notin @('redis', 'mysql')) { continue }
        $expectedJob = "middleware-exporter-$($item.mw_type)"
        $needFix = $false
        $newJob = $item.prom_job
        if ([string]::IsNullOrWhiteSpace($newJob) -or ($jobs.Count -gt 0 -and $jobs -notcontains $newJob)) {
          $newJob = $expectedJob; $needFix = $true
        }
        # 实例名以 .env 的 JD_*_INSTANCE_NAME 为准（relabel 已按它同步），
        # Prometheus 里的实际标签值只用于提示"是否需要重建 Prometheus 让配置生效"。
        $expectedName = if ($item.mw_type -eq 'redis') { $redisName } else { $mysqlName }
        if ([string]::IsNullOrWhiteSpace($expectedName)) { $expectedName = $item.name }
        try {
          $matcher = [uri]::EscapeDataString("up{job=`"$newJob`"}")
          $names = (Invoke-RestMethod -Uri "http://127.0.0.1:$promPort/api/v1/label/instance_name/values?match[]=$matcher" -TimeoutSec 5).data
          if ($names.Count -gt 0 -and $names -notcontains $expectedName) {
            Warn "Prometheus 上 job=$newJob 的实际标签是 $($names -join ', ')，与 .env 的 $expectedName 不同"
            Hint '说明 relabel 改动还没生效：docker compose -f docker-compose.yml -f deploy/jd-exporters/docker-compose.jd.yml up -d --force-recreate prometheus'
          }
        } catch { }
        if (-not $needFix) { Ok "$($item.name) 已正确（job=$newJob，名称=$expectedName）"; continue }
        $body = @{
          name = $expectedName; mw_type = $item.mw_type; host = $item.host; port = $item.port
          username = $item.username; environment = $item.environment; group_name = $item.group_name
          tags = $item.tags; config = $item.config
          prom_job = $newJob; prom_instance = ''
        } | ConvertTo-Json -Depth 6
        Invoke-RestMethod -Method Put -Uri "$base/api/middlewares/$($item.id)" -Headers $headers `
          -ContentType 'application/json' -Body $body | Out-Null
        Fix "实例 #$($item.id)：$($item.name)/$($item.prom_job) → $expectedName/$newJob"
      }
    } catch {
      Bad "纠正实例失败：$($_.Exception.Message)"
    }
  }
}

# ---------------------------------------------------------------------------
# 汇总
# ---------------------------------------------------------------------------
Step '结果汇总'
if ($script:changes.Count -eq 0) {
  Write-Host '  没有任何需要修改的内容（幂等：重复执行是安全的）' -ForegroundColor Green
} else {
  Write-Host "  本次共 $($script:changes.Count) 项变更：" -ForegroundColor Yellow
  $script:changes | ForEach-Object { Write-Host "    - $_" }
}
if ($script:problems.Count -gt 0) {
  Write-Host "  $($script:problems.Count) 项需要处理：" -ForegroundColor Red
  $script:problems | ForEach-Object { Write-Host "    ! $_" }
}

Write-Host ''
Write-Host '下一步：' -ForegroundColor Cyan
Write-Host "  平台入口   http://127.0.0.1:$njWeb   （账号 $(Get-EnvValue $nj 'ADMIN_USER')，口令见 <nightjar>/.env 的 ADMIN_PASSWORD）"
if ($Mode -eq 'jd-prometheus') {
  Write-Host "  jd 控制台  http://127.0.0.1:$(Get-EnvValue $jd 'WEB_PORT')   Grafana http://127.0.0.1:$(Get-EnvValue $jd 'GRAFANA_PORT')"
  Write-Host "  jd 指标源  http://127.0.0.1:$jdProm（jd 的 Prometheus，prometheus.base_url=http://jd-prometheus:9090）"
} else {
  Write-Host '  集成中心   平台「资源 → 集成中心」新建集成，即可由平台拉起 Exporter'
}
Write-Host '  逐项体检   pwsh -File scripts/doctor-jd-link.ps1'
Write-Host '  实例自检   平台「实例详情 → 接入自检」，应看到 matched > 0'

if ($script:problems.Count -gt 0) { exit 1 }
exit 0
