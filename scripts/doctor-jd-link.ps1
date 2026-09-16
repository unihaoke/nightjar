# nightjar ⇄ jd 跨栈网络体检
#
# 背景：跨栈故障里最难查的一类，是「容器都在跑，但网络挂错了」——
#   * 平台没带 deploy/compose.jd-link.yml 启动 → mwops-backend 不在 jd-nightjar 上，
#     于是「查不到指标（Prometheus 解析不了 jd-prometheus）」「实例探测连接失败」
#     「jd 日志推不过来（解析不了 mwops-backend）」三件事同时发生；
#   * 容器被手工 docker network connect / disconnect 过 → 多一张或少了关键网络，
#     例如 mwops-prometheus 变成"没有任何网络"。
#   平台 UI 只会显示"没有数据"，看不出网络挂错，所以用本脚本把期望拓扑与实际拓扑对齐。
#
# 用法（在 nightjar 项目根目录）：
#   pwsh -File scripts/doctor-jd-link.ps1
#   pwsh -File scripts/doctor-jd-link.ps1 -NightjarDir ..\nightjar -JdDir ..\jd
#   pwsh -File scripts/doctor-jd-link.ps1 -Mode jd-prometheus   # 声明用哪种取数方式
#
# 退出码：0 = 全部通过；非 0 = 失败项数量（便于接 CI / 启动前自检）。

[CmdletBinding()]
param(
  # 声明指标来自哪里，决定对 Exporter 网络的期望：
  #   jd-prometheus = 复用 jd 自带的 Prometheus（方式二，默认）
  #   mwops-prometheus = 用平台自带的 Prometheus（方式一：集成中心一键集成）
  [ValidateSet('jd-prometheus', 'mwops-prometheus')]
  [string]$Mode = 'jd-prometheus',
  [string]$NightjarDir = '.',
  [string]$JdDir = '..\jd'
)

$ErrorActionPreference = 'Continue'
$script:fail = 0
$script:warn = 0

function Step($name) { Write-Host "`n=== $name ===" -ForegroundColor Cyan }
function Ok($msg) { Write-Host "  [PASS] $msg" -ForegroundColor Green }
function Bad($msg) { Write-Host "  [FAIL] $msg" -ForegroundColor Red; $script:fail++ }
function Warn($msg) { Write-Host "  [WARN] $msg" -ForegroundColor Yellow; $script:warn++ }
function Hint($msg) { Write-Host "         → $msg" -ForegroundColor DarkGray }

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
  Write-Host '未找到 docker 命令：请在 Docker 宿主机上执行本脚本' -ForegroundColor Red
  exit 2
}

# 查容器实际挂载的网络（返回字符串数组）。
function Get-ContainerNetworks($name) {
  $raw = & docker inspect -f '{{json .NetworkSettings.Networks}}' $name 2>$null
  if ($LASTEXITCODE -ne 0 -or -not $raw) { return $null }   # 容器不存在
  try {
    $obj = $raw | ConvertFrom-Json
  } catch {
    return @()
  }
  if ($null -eq $obj) { return @() }
  return @($obj.PSObject.Properties.Name)
}

# 按后缀解析实际网络名（compose 会给网络加项目前缀，如 jd_jd-data / middleware-ops_mwops）。
function Resolve-NetworkName([string]$suffix) {
  $names = @(& docker network ls --format '{{.Name}}' 2>$null)
  $hit = @($names | Where-Object { $_ -eq $suffix -or $_ -like "*_$suffix" })
  if ($hit.Count -eq 0) { return $null }
  return $hit[0]
}

function Assert-Networks($container, [string[]]$expected, [string[]]$forbidden = @()) {
  $actual = Get-ContainerNetworks $container
  if ($null -eq $actual) { Bad "$container 不存在或未运行"; return }
  if ($actual.Count -eq 0) {
    Bad "$container 没有挂任何网络（容器在跑但谁也连不上）"
    Hint "docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --force-recreate $container"
    return
  }
  $missing = @($expected | Where-Object { $_ -and ($actual -notcontains $_) })
  $extra = @($forbidden | Where-Object { $_ -and ($actual -contains $_) })
  if ($missing.Count -eq 0 -and $extra.Count -eq 0) {
    Ok "$container → $($actual -join ', ')"
    return
  }
  if ($missing.Count -gt 0) {
    Bad "$container 缺少网络：$($missing -join ', ')（实际：$($actual -join ', ')）"
  }
  if ($extra.Count -gt 0) {
    Bad "$container 多出不该有的网络：$($extra -join ', ')（实际：$($actual -join ', ')）"
  }
}

# 读取 .env 里的键值（不输出值，避免口令泄漏到日志）。
function Read-EnvValue([string]$path, [string]$key) {
  if (-not (Test-Path $path)) { return $null }
  foreach ($line in Get-Content $path) {
    $trimmed = $line.Trim()
    if ($trimmed -eq '' -or $trimmed.StartsWith('#')) { continue }
    $idx = $trimmed.IndexOf('=')
    if ($idx -le 0) { continue }
    if ($trimmed.Substring(0, $idx).Trim() -eq $key) {
      return $trimmed.Substring($idx + 1).Trim()
    }
  }
  return $null
}

Write-Host "取数方式：$Mode" -ForegroundColor Gray
Write-Host "nightjar 目录：$NightjarDir" -ForegroundColor Gray
Write-Host "jd 目录：$JdDir" -ForegroundColor Gray

# ---------------------------------------------------------------------------
Step '1/5 网络存在性与隔离属性'
# ---------------------------------------------------------------------------
$linkNet = Resolve-NetworkName 'jd-nightjar'
$dataNet = Resolve-NetworkName 'jd-data'
$mwopsNet = Resolve-NetworkName 'mwops'

if (-not $linkNet) {
  Bad 'jd-nightjar 网络不存在'
  Hint 'cd <jd> && ./start.sh link     # 或 ./start.sh nightjar（脚本会以 --internal 创建）'
} else {
  $internal = (& docker network inspect $linkNet --format '{{.Internal}}' 2>$null)
  if ("$internal".Trim() -eq 'true') { Ok "$linkNet 存在且为 internal（数据面隔离生效）" }
  else {
    Bad "$linkNet 不是 internal 网络（Internal=$internal）：jd 的 MySQL/Redis 会重新获得出网路由"
    Hint "docker network rm $linkNet  后重新执行 cd <jd> && ./start.sh link"
  }
}
if ($dataNet) { Ok "$dataNet 存在" } else { Bad 'jd 的数据面网络（*-jd-data）不存在：jd 侧还没启动？' }
if ($mwopsNet) { Ok "$mwopsNet 存在" } else { Bad '平台网络（*-mwops）不存在：nightjar 侧还没启动？' }

# ---------------------------------------------------------------------------
Step '2/5 平台侧容器网络（关键：backend 必须在 jd-nightjar 上）'
# ---------------------------------------------------------------------------
Assert-Networks 'mwops-backend' @($mwopsNet, $linkNet)
if ((Get-ContainerNetworks 'mwops-backend') -notcontains $linkNet) {
  Hint '平台没有带跨栈 overlay 启动。正确命令（在 nightjar 目录）：'
  Hint 'docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d --build'
}
# 平台 Prometheus 只需要平台网络；集成中心拉起的 Exporter 同时挂在两张网上。
Assert-Networks 'mwops-prometheus' @($mwopsNet)
Assert-Networks 'mwops-frontend' @($mwopsNet)
Assert-Networks 'mwops-postgres' @($mwopsNet)
Assert-Networks 'mwops-redis' @($mwopsNet)

# ---------------------------------------------------------------------------
Step '3/5 jd 侧容器网络'
# ---------------------------------------------------------------------------
Assert-Networks 'interview-mysql' @($dataNet, $linkNet)
Assert-Networks 'interview-redis' @($dataNet, $linkNet)
Assert-Networks 'interview-prometheus' @($dataNet, $linkNet)
Assert-Networks 'jd-log-agent' @($linkNet)

if ($Mode -eq 'jd-prometheus') {
  # 方式二：jd 的 Prometheus 抓 jd 侧的 Exporter，两者同在 jd-data 即可。
  Assert-Networks 'jd-redis-exporter' @($dataNet) @($mwopsNet)
  Assert-Networks 'jd-mysqld-exporter' @($dataNet) @($mwopsNet)
  Write-Host '  [INFO] 方式二下平台只查 http://jd-prometheus:9090，不直接抓 Exporter' -ForegroundColor DarkGray
} else {
  # 方式一：平台 Prometheus 抓「集成中心」拉起的 mwops-exporter-*（同时挂 mwops + jd-nightjar）。
  $managed = @(& docker ps --format '{{.Names}}' 2>$null | Where-Object { $_ -like 'mwops-exporter-*' })
  if ($managed.Count -eq 0) {
    Warn '未发现集成中心拉起的 Exporter（mwops-exporter-*）：请在「集成中心」新建集成并勾选一键拉起容器'
  } else {
    foreach ($name in $managed) { Assert-Networks $name @($mwopsNet, $linkNet) }
  }
}

# ---------------------------------------------------------------------------
Step '4/5 跨栈连通性实测（从平台容器内解析 jd 别名）'
# ---------------------------------------------------------------------------
function Test-Alias($alias) {
  $out = & docker exec mwops-backend sh -c "getent hosts $alias" 2>$null
  if ($LASTEXITCODE -eq 0 -and $out) { Ok "mwops-backend 能解析 $alias（$($out -join '; ')）" }
  else {
    Bad "mwops-backend 解析不了 $alias"
    Hint "说明 mwops-backend 不在 $linkNet 上，或 jd 侧别名未生效"
  }
}
if ((Get-ContainerNetworks 'mwops-backend') -contains $linkNet) {
  Test-Alias 'jd-prometheus'
  Test-Alias 'jd-mysql'
  Test-Alias 'jd-redis'
} else {
  Bad 'mwops-backend 不在跨栈网络上，跳过别名解析（先修第 2 步）'
}

# 反向：jd 的日志 Agent 能否解析平台后端。
$agentNets = Get-ContainerNetworks 'jd-log-agent'
if ($null -ne $agentNets) {
  $out = & docker exec jd-log-agent sh -c "getent hosts mwops-backend" 2>$null
  if ($LASTEXITCODE -eq 0 -and $out) { Ok "jd-log-agent 能解析 mwops-backend（日志链路可用）" }
  else { Bad 'jd-log-agent 解析不了 mwops-backend：日志推不过来'; Hint '先修第 2 步（平台 backend 接入 jd-nightjar）' }
}

# ---------------------------------------------------------------------------
Step '5/5 平台 .env 关键项与 jd 侧口令一致性'
# ---------------------------------------------------------------------------
$njEnv = Join-Path $NightjarDir '.env'
$jdEnv = Join-Path $JdDir '.env'
if (-not (Test-Path $njEnv)) { Warn "$njEnv 不存在（未复制 .env.example？）" }
else {
  $baseUrl = Read-EnvValue $njEnv 'MWOPS_PROMETHEUS_BASE_URL'
  if (-not $baseUrl) {
    Warn 'MWOPS_PROMETHEUS_BASE_URL 未设置（将回退到平台自带 Prometheus 或内置模拟器）'
    Hint "方式二请设置：MWOPS_PROMETHEUS_BASE_URL=http://jd-prometheus:9090"
  } elseif ($Mode -eq 'jd-prometheus' -and $baseUrl -notlike '*jd-prometheus*') {
    Warn "MWOPS_PROMETHEUS_BASE_URL=$baseUrl，但当前声明用 jd 的 Prometheus"
    Hint '方式二应为 http://jd-prometheus:9090；方式一应为 http://prometheus:9090'
  } else {
    Ok "MWOPS_PROMETHEUS_BASE_URL=$baseUrl"
  }
  $port = Read-EnvValue $njEnv 'PROMETHEUS_PORT'
  if ($port -and $port -eq '9090') {
    Warn 'PROMETHEUS_PORT=9090 与 jd 的 Prometheus 冲突（同机部署）'
    Hint '改成 PROMETHEUS_PORT=9091 后 docker compose up -d prometheus'
  }
}
if ((Test-Path $njEnv) -and (Test-Path $jdEnv)) {
  $njToken = Read-EnvValue $njEnv 'HOOK_TOKEN'
  $jdToken = Read-EnvValue $jdEnv 'NIGHTJAR_HOOK_TOKEN'
  if ([string]::IsNullOrWhiteSpace($njToken)) {
    Warn 'nightjar HOOK_TOKEN 为空：/api/hooks/* 不鉴权（仅测试可用）'
  } elseif ($njToken -eq $jdToken) {
    Ok 'HOOK_TOKEN 与 jd 的 NIGHTJAR_HOOK_TOKEN 一致'
  } else {
    Bad 'HOOK_TOKEN 与 jd 的 NIGHTJAR_HOOK_TOKEN 不一致：日志上报会被 401 拒绝'
    Hint '两边改成同一串后，docker compose up -d backend（平台）与 ./start.sh nightjar-logs（jd）'
  }
  $nn = Read-EnvValue $njEnv 'JD_NIGHTJAR_NETWORK'
  $jn = Read-EnvValue $jdEnv 'JD_NIGHTJAR_NETWORK'
  if ($nn -and $jn -and $nn -ne $jn) {
    Bad "两侧 JD_NIGHTJAR_NETWORK 不一致：平台=$nn，jd=$jn"
  }
} else {
  Warn '缺少 .env（平台或 jd）：跳过口令一致性检查'
}

# ---------------------------------------------------------------------------
Write-Host "`n================ 结果 ================" -ForegroundColor Cyan
if ($script:fail -eq 0) {
  Write-Host "拓扑检查通过（$script:warn 条提示）" -ForegroundColor Green
  Write-Host '下一步：平台实例详情 →「接入自检」，确认 matched>0 且来源为 Prometheus' -ForegroundColor Gray
} else {
  Write-Host "失败 $script:fail 项、提示 $script:warn 条，按上面的 [FAIL]/[HINT] 逐条处理" -ForegroundColor Red
  Write-Host '提示：网络改动后统一用同一条命令重建，避免手工 docker network connect 造成状态漂移：' -ForegroundColor Gray
  Write-Host '  cd <nightjar> && docker compose -f docker-compose.yml -f deploy/compose.jd-link.yml up -d' -ForegroundColor Gray
  Write-Host '  cd <jd>       && ./start.sh nightjar' -ForegroundColor Gray
}
exit $script:fail
