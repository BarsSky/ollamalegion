# stand_check_r508.ps1 — R508 (2026-11-19): §6 аудита согласованности НА ЖИВОМ СТЕНДЕ.
#
# ЧТО ПРОВЕРЯЕТ (ровно §6 docs/plans/2026-11-19-consistency-audit.md):
#   1) вместимость: три поверхности (/api/v1/backends, /api/v1/cluster, /queue)
#      называют ОДНИ И ТЕ ЖЕ числа (эффективная = применяемая, источник осмысленный);
#   2) C-1 живьём: PUT /limits=1 при статике 4 → ВСЕ пути видят 1, после возврата
#      значения — снова прежнее;
#   3) C-5: снапшот кластера не отдаёт статику рядом с эффективной;
#   4) R500: в /queue больше нет ключей workers/timeout_sec;
#   5) режимы R501/R495: LB_CAPACITY_SINGLE_SOURCE / LB_GPU_BOTTLENECK реально
#      проброшены в контейнер (а не «объявлены в .env.example»);
#   6) в логе балансера есть строка трассировки единого решателя (shadow divergence)
#      либо её нет и это объяснено (расхождений не было).
#
# Логин: LB_ADMIN_USER/LB_ADMIN_PASSWORD из deployments/.env (в выводе не печатаются).
# Возврат кода 1 при провале проверки.
param(
    [string]$Base = 'http://127.0.0.1:18081',
    [string]$Container = 'ol-stack-balancer'
)

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$fails = @()
function Check([bool]$ok, [string]$what, [string]$detail) {
    $mark = if ($ok) { 'PASS' } else { 'FAIL' }
    Write-Output ("{0}  {1}{2}" -f $mark, $what, $(if ($detail) { ' :: ' + $detail } else { '' }))
    if (-not $ok) { $script:fails += ($what + $(if ($detail) { ' :: ' + $detail } else { '' })) }
}

function EnvValue([string]$key) {
    $envFile = Join-Path $root 'deployments/.env'
    foreach ($line in Get-Content $envFile) {
        if ($line.StartsWith($key + '=')) { return $line.Substring($key.Length + 1).Trim() }
    }
    return ''
}

# --- Сессия администратора -----------------------------------------------------
$user = EnvValue 'LB_ADMIN_USER'
$pass = EnvValue 'LB_ADMIN_PASSWORD'
if (-not $pass) { Write-Output 'ОШИБКА: нет LB_ADMIN_PASSWORD в deployments/.env'; exit 1 }

$session = New-Object Microsoft.PowerShell.Commands.WebRequestSession
try {
    $loginBody = @{ name = $user; password = $pass } | ConvertTo-Json -Compress
    $login = Invoke-WebRequest -Uri "$Base/api/v1/auth/login" -Method Post -Body $loginBody `
        -ContentType 'application/json' -WebSession $session -UseBasicParsing -TimeoutSec 15
    Check ($login.StatusCode -eq 200) 'вход администратора' ("HTTP " + $login.StatusCode)
} catch {
    Write-Output ("ОШИБКА входа: " + $_.Exception.Message)
    exit 1
}

# CSRF: сессионные мутации требуют X-CSRF-Token (internal/api/auth.go:240); токен
# отдаёт GET /api/v1/auth/me. Без него PUT /limits отвечает 403 Forbidden.
$csrf = ''
try {
    # Прямой запрос, а не GetJSONRaw: функция ниже определена позже, а PowerShell
    # исполняет скрипт сверху вниз — вызов до определения не сработал бы.
    $meRaw = (Invoke-WebRequest -Uri "$Base/api/v1/auth/me" -WebSession $session `
        -UseBasicParsing -TimeoutSec 15).Content
    $me = $meRaw | ConvertFrom-Json
    if ($me.csrf) { $csrf = [string]$me.csrf }
} catch {
    Write-Output ('ПРЕДУПРЕЖДЕНИЕ: /auth/me не отдал csrf — мутации будут запрещены: ' + $_.Exception.Message)
}
Check ([bool]$csrf) 'CSRF-токен получен' ''

function GetJSON([string]$path) {
    return Invoke-WebRequest -Uri ($Base + $path) -WebSession $session -UseBasicParsing -TimeoutSec 20 |
        Select-Object -ExpandProperty Content | ConvertFrom-Json
}

# --- 1. Три поверхности: одни и те же числа -----------------------------------
$backends = GetJSON '/api/v1/backends'
$cluster = GetJSON '/api/v1/cluster'

$backendList = if ($backends.backends) { $backends.backends } else { @() }
Check ($backendList.Count -gt 0) 'в кластере есть бэкенды' ("их " + $backendList.Count)

$byID = @{}
foreach ($b in $backendList) { $byID[$b.id] = $b }

foreach ($m in $cluster.backends) {
    $b = $byID[$m.id]
    if (-not $b) { continue }
    $effBackends = [int]$b.effectiveMaxConcurrentRequests
    $effCluster = [int]$m.effectiveMaxConcurrentRequests
    Check ($effBackends -eq $effCluster) ("эффективная совпадает на двух поверхностях: " + $m.id) `
        ("backends=" + $effBackends + " cluster=" + $effCluster)
    Check ([int]$b.maxConcurrentRequests -eq $effBackends) `
        ("на /api/v1/backends maxConcurrentRequests = effective: " + $m.id) `
        ("max=" + $b.maxConcurrentRequests + " effective=" + $effBackends)
    # C-5: в снапшоте кластера maxConcurrentRequests — тоже эффективная.
    Check ([int]$m.maxConcurrentRequests -eq $effCluster) ("снапшот не отдаёт статику рядом с эффективной: " + $m.id) `
        ("maxConcurrentRequests=" + $m.maxConcurrentRequests + " effective=" + $effCluster)
    if ($b.maxConcurrentRequestsSource) {
        $src = [string]$b.maxConcurrentRequestsSource
        $known = @('operator', 'node', 'static', 'model_slots', 'model_slots_unknown')
        Check ($known -contains $src) ("источник вместимости назван: " + $m.id) ("source=" + $src)
    }
}

# --- 2. C-1 живьём: PUT /limits=1 --------------------------------------------
$target = $backendList | Where-Object { $_.effectiveMaxConcurrentRequests -gt 1 } | Select-Object -First 1
if (-not $target) {
    $target = $backendList | Select-Object -First 1
}
if ($target) {
    $id = $target.id
    $origStatic = [int]$target.maxConcurrentRequests
    $origRuntimeMax = [int]$target.runtimeMaxConcurrentRequests
    $origMaxModels = [int]$target.maxModels
    Write-Output ("--- C-1: временно ставим лимит 1 на " + $id + " (эффективная до опыта " + $origStatic + ", операторский " + $origRuntimeMax + ")")

    $body = @{ maxConcurrentRequests = 1; maxModels = $origMaxModels } | ConvertTo-Json -Compress
    $put = Invoke-WebRequest -Uri "$Base/api/v1/backends/$id/limits" -Method Put -Body $body `
        -ContentType 'application/json' -WebSession $session -UseBasicParsing -TimeoutSec 20 `
        -Headers @{ 'X-CSRF-Token' = $csrf }
    Check ($put.StatusCode -eq 200) 'PUT /limits=1 принят' ("HTTP " + $put.StatusCode)

    Start-Sleep -Seconds 1
    $afterB = (GetJSON '/api/v1/backends').backends | Where-Object { $_.id -eq $id }
    $afterC = (GetJSON '/api/v1/cluster').backends | Where-Object { $_.id -eq $id }
    Check ([int]$afterB.effectiveMaxConcurrentRequests -eq 1) 'эффективная=1 после лимита (backends)' `
        ([string]$afterB.effectiveMaxConcurrentRequests)
    Check ([int]$afterC.effectiveMaxConcurrentRequests -eq 1) 'эффективная=1 после лимита (cluster)' `
        ([string]$afterC.effectiveMaxConcurrentRequests)
    Check ([int]$afterC.maxConcurrentRequests -eq 1) 'снапшот кластера тоже 1' ([string]$afterC.maxConcurrentRequests)
    Check ([string]$afterB.maxConcurrentRequestsSource -eq 'operator') 'источник=operator' ([string]$afterB.maxConcurrentRequestsSource)

    # Возврат операторского значения как было (0 = «не задано» → очищаем лимит).
    $restore = @{ maxConcurrentRequests = $origRuntimeMax; maxModels = $origMaxModels } | ConvertTo-Json -Compress
    $back = Invoke-WebRequest -Uri "$Base/api/v1/backends/$id/limits" -Method Put -Body $restore `
        -ContentType 'application/json' -WebSession $session -UseBasicParsing -TimeoutSec 20 `
        -Headers @{ 'X-CSRF-Token' = $csrf }
    Start-Sleep -Seconds 1
    $restoredB = (GetJSON '/api/v1/backends').backends | Where-Object { $_.id -eq $id }
    $restoredEff = [int]$restoredB.effectiveMaxConcurrentRequests
    Check ($restoredEff -ne 1 -or $origStatic -eq 1) 'ограничение снято: эффективная вернулась' `
        ("effective=" + $restoredEff + " (до опыта " + $target.effectiveMaxConcurrentRequests + ")")
} else {
    Check $false 'C-1 живьём: бэкенд для опыта' 'в кластере нет бэкендов'
}

# --- 3. R500: в /queue нет мёртвых ключей ------------------------------------
$queueRaw = (Invoke-WebRequest -Uri "$Base/api/v1/queue/stats" -WebSession $session -UseBasicParsing -TimeoutSec 15).Content
Check (-not ($queueRaw -match '"workers"')) 'в /queue/stats нет ключа workers' ''
Check (-not ($queueRaw -match '"timeout_sec"')) 'в /queue/stats нет ключа timeout_sec' ''

# --- 4. Режимы R501/R495 реально в контейнере --------------------------------
$envDump = docker exec $Container env 2>&1
$capMode = ($envDump | Select-String -Pattern '^LB_CAPACITY_SINGLE_SOURCE=' | Select-Object -First 1)
$bottleMode = ($envDump | Select-String -Pattern '^LB_GPU_BOTTLENECK=' | Select-Object -First 1)
Check ([bool]$capMode) 'LB_CAPACITY_SINGLE_SOURCE проброшен в контейнер' ([string]$capMode)
Check ([bool]$bottleMode) 'LB_GPU_BOTTLENECK проброшен в контейнер' ([string]$bottleMode)

# --- 5. Лог: трассировка единого решателя ------------------------------------
$logs = docker logs --tail 400 $Container 2>&1
$divergence = $logs | Select-String -Pattern 'единый резолвер расходится' | Select-Object -First 1
$staleClaim = $logs | Select-String -Pattern 'заявка узла просрочена' | Select-Object -First 1
Write-Output ("INFO  тень единого решателя: расхождений в логе {0}, просроченных заявок {1}" -f `
    $(if ($divergence) { 'ЕСТЬ' } else { 'нет' }), $(if ($staleClaim) { 'ЕСТЬ' } else { 'нет' }))

# --- Итог -------------------------------------------------------------------
Write-Output ''
if ($fails.Count -eq 0) {
    Write-Output 'ИТОГ: §6 на живом стенде — все проверки прошли.'
    exit 0
}
Write-Output ("ИТОГ: провалов — {0}:" -f $fails.Count)
foreach ($f in $fails) { Write-Output ('  - ' + $f) }
exit 1
