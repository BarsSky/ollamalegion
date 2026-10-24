<#
.SYNOPSIS
    Живая приёмка политики размещения и логических бэкендов (R92–R107).

.DESCRIPTION
    Проверяет на РАБОТАЮЩЕМ стенде то, что проверяется юнит-тестами только
    косвенно: что новая ручка реально отдаётся живым балансером, что сухой
    прогон ничего не меняет, что применение политики видно в состоянии и что
    стенд возвращается в исходное состояние.

    Чем этот скрипт отличается от `docker-stack-smoke.ps1`: смоук проверяет,
    что стенд СОБРАН (сервисы, пулы, OpenAI-поверхность). Здесь проверяется
    поведение одного окна — размещения: резолв стратегий, валидатор композиции,
    план мастера, применение и откат.

    ГЛАВНОЕ ПРАВИЛО: скрипт НЕ оставляет стенд изменённым. Исходные `placement`
    и `backendGroups` снимаются с живого балансера в начале и возвращаются в
    конце (тем же PUT, что и применение). Отключить — флаг -NoRevert.

    Проверки:
      A1  [R92] GET /api/v1/placement отдаёт sharded в executable + сырую политику
      A2  [R95] POST /placement/validate: заведомо сломанная композиция → ошибки
      A3  [R95] POST /placement/validate: композиция с RPC-воркером → отчёт 11 проверок
                и честная проба порта воркера (недоступный узел должен быть назван)
      A4  [R96] POST /placement/plan: план с числами по узлам (sizeGB/needGB/maxNodeGB/sumFreeGB)
      A4t [R96] трафик раскладки (только для sharded — у single/replicated его нет)
      A4b [§3] задержка раскладки масштабируется полосой канала (×5 при полосе/5)
      A4c [R96] положительная ветка: модель влезает в узел → single с числами
      A5  [R100] PUT /placement dryRun=true: ничего не применено и не сохранено
      A6  [R100] PUT /placement: политика применена и видна в GET; persisted честный
      A7  [R92] PUT /placement с backendGroups: группа видна в состоянии (groups)
      A8  [R97] modeMigration: миграция «режима» в политику объяснена оператору
      A9  [R101] GET /api/v1/backends: поле buildInfo у каждого бэкенда
      A10 откат: стенд вернулся к исходной политике (сверка с A1)
      A11 [R98/R99 -Image] image-плоскость отвечает (очередь/пакетная видимость)
      A12 [R102/R103/R104/R106] собранный WebUI отдаёт модули новых окон
      A13 [R107] в мастере нет присваивания operatingMode (уборка режимов)
      A14 [R107 остаток] переключатель режима на «Настройках» ещё подключён (INFO)

.PARAMETER ApiPort
    Порт management-API балансера. По умолчанию 18080 (LB_PORT стенда).

.PARAMETER Token
    API-токен. По умолчанию берётся из deployments/.env (CPPWORKER_API_TOKEN).

.PARAMETER RpcHost
    Адрес узла-воркера для проверки A3. По умолчанию 192.0.2.10 (машина 2).

.PARAMETER RpcPort
    RPC-порт воркера (llama.cpp rpc-server). По умолчанию 50052.

.PARAMETER NoRevert
    Не возвращать исходную политику (для отладки самого скрипта).

.PARAMETER Image
    Добавить A11 (image-плоскость).

.EXAMPLE
    powershell -File scripts\placement-live-acceptance.ps1

.EXAMPLE
    powershell -File scripts\placement-live-acceptance.ps1 -Image -RpcHost 192.0.2.10
#>
[CmdletBinding()]
param(
    [int]$ApiPort = 18080,
    [int]$WebUiPort = 18083,
    [string]$Token = '',
    [string]$Lang = 'ru',
    [string]$RpcHost = '192.0.2.10',
    [int]$RpcPort = 50052,
    # ID второго узла в балансере. Заглушка вроде «worker-machine-2» проверяла
    # ветку «узел не зарегистрирован»; когда узел реально поднят, проверять надо
    # именно его — иначе сухой прогон ругается на несуществующий id.
    [string]$Node2Id = 'CPPWORKER-34',
    [double]$BandwidthMBps = 112.0,
    [int]$PrefillTokens = 512,
    [switch]$Image,
    [switch]$NoRevert
)

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }

$script:results = New-Object System.Collections.ArrayList
$repoRoot = Split-Path -Parent $PSScriptRoot
$deployDir = Join-Path $repoRoot 'deployments'
$api = "http://127.0.0.1:$ApiPort"

function Add-Result {
    param([string]$Id, [string]$Check, [string]$Result, [string]$Detail)
    [void]$script:results.Add([pscustomobject]@{ Id = $Id; Check = $Check; Result = $Result; Detail = $Detail })
    $color = switch ($Result) { 'PASS' { 'Green' } 'SKIP' { 'Yellow' } 'INFO' { 'Cyan' } default { 'Red' } }
    Write-Host ("  [{0}] {1,-5} {2} :: {3}" -f $Id, $Result, $Check, $Detail) -ForegroundColor $color
}

# Invoke-Api — запрос без исключений на 4xx/5xx; тело шлём байтами UTF-8,
# иначе кириллица в теле уедет в CP1251.
function Invoke-Api {
    param([string]$Method = 'GET', [string]$Path, [string]$Json = '', [int]$TimeoutSec = 30)
    $out = [pscustomobject]@{ Status = 0; Body = ''; Json = $null; Error = '' }
    try {
        $params = @{
            Uri = "$api$Path"; Method = $Method; TimeoutSec = $TimeoutSec
            UseBasicParsing = $true; Headers = $script:headers
        }
        if ($Json) {
            $params['Body'] = [System.Text.Encoding]::UTF8.GetBytes($Json)
            $params['ContentType'] = 'application/json; charset=utf-8'
        }
        $resp = Invoke-WebRequest @params
        $out.Status = [int]$resp.StatusCode
        # Балансер отдаёт JSON без charset, а -UseBasicParsing декодирует тело по
        # charset ответа → кириллица превращается в мусор. Читаем сырые байты как
        # UTF-8: отчёт приёмки должен быть читаемым, иначе им нельзя поделиться.
        if ($resp.RawContentStream) {
            $out.Body = [System.Text.Encoding]::UTF8.GetString($resp.RawContentStream.ToArray())
        } else {
            $out.Body = [string]$resp.Content
        }
    } catch {
        $r = $_.Exception.Response
        if ($r) {
            try { $out.Status = [int]$r.StatusCode } catch { $out.Status = 0 }
            try {
                $sr = New-Object System.IO.StreamReader($r.GetResponseStream())
                $out.Body = $sr.ReadToEnd()
            } catch { }
        }
        $out.Error = $_.Exception.Message
    }
    if ($out.Body) { try { $out.Json = $out.Body | ConvertFrom-Json } catch { } }
    return $out
}

# Format-Issue — замечание валидатора человеку: `check: текст`.
# `errors`/`warnings`/`hints` — это объекты Issue (с check/level/target/params), а
# не строки: без рендера в консоль уехал бы @{check=...; level=...}.
function Format-Issue {
    param($Issue)
    if ($null -eq $Issue) { return '' }
    if ($Issue -is [string]) { return $Issue }
    return ("{0}: {1}" -f $Issue.check, $Issue.text)
}

# Get-IssueCheck — есть ли в списке замечание с указанным check.
function Get-IssueCheck {
    param($Issues, [string]$Check)
    foreach ($i in @($Issues)) { if ($i.check -eq $Check) { return $i } }
    return $null
}

function Get-DotEnvValue {
    param([string]$Path, [string]$Key)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match "^\s*$([regex]::Escape($Key))\s*=\s*(.+?)\s*$") { return $Matches[1].Trim('"').Trim("'") }
    }
    return ''
}

# --- Предусловия -------------------------------------------------------------
if (-not $Token) { $Token = Get-DotEnvValue -Path (Join-Path $deployDir '.env') -Key 'CPPWORKER_API_TOKEN' }
$script:headers = @{}
if ($Token) { $script:headers['X-API-Token'] = $Token }

Write-Host "=== Живая приёмка: политика размещения (R92–R107) ===" -ForegroundColor Cyan
Write-Host "api: $api"

$probe = Invoke-Api -Path '/api/v1/placement'
if ($probe.Status -ne 200) {
    Write-Host ("балансер недоступен: HTTP {0} {1}" -f $probe.Status, $probe.Error) -ForegroundColor Red
    exit 2
}

# Исходное состояние — снимаем ДО любых изменений: это и эталон для A10.
$before = $probe.Json
$beforePlacementJson = $before.placement | ConvertTo-Json -Depth 10 -Compress
$beforeGroupsJson = @($before.backendGroups) | ConvertTo-Json -Depth 10 -Compress
Write-Host ("исходно: enabled={0} rules={1} classes={2} groups={3}" -f `
    $before.placement.enabled, @($before.placement.models).Count, @($before.placement.classes).Count, @($before.backendGroups).Count)

# --- A1: ручка новая, а не старая сборка ------------------------------------
$exe = @($before.executable)
if ($exe -contains 'sharded' -and $null -ne $before.placement) {
    Add-Result 'A1' 'R92: executable + сырая политика' 'PASS' ("executable=" + ($exe -join ','))
} else {
    Add-Result 'A1' 'R92: executable + сырая политика' 'FAIL' ("executable=" + ($exe -join ',') + " (нет sharded — образ старый?)")
}

# --- A2: валидатор ловит сломанную композицию -------------------------------
$badBody = @{
    group = @{
        id = 'accept-bad'; model = 'qwen3-14b'; strategy = 'layer'
        members = @(
            @{ backendId = 'cppworker-gpu-bundled-agent'; role = 'worker' }
        )
    }
    model = @{ name = 'qwen3-14b'; layers = 40; hiddenSize = 5120; nCtx = 32768; kvLayers = 40; kvHeads = 8; kvHeadDim = 128; kvBytesPerElem = 2 }
    lang = $Lang
    skipProbes = $true
} | ConvertTo-Json -Depth 10
$a2 = Invoke-Api -Method POST -Path '/api/v1/placement/validate' -Json $badBody
# Структурные болячки конфига (нет мастера, воркер без rpcEndpoint) приходят
# списком configWarnings, а не в errors: errors — про СОБИРАЕМОСТЬ раскладки,
# configWarnings — про сам конфиг группы. Проверяем оба поля по назначению.
$cw = @($a2.Json.configWarnings)
if ($a2.Status -eq 200 -and $cw.Count -gt 0) {
    Add-Result 'A2' 'R95: сломанный конфиг группы назван' 'PASS' ("configWarnings=" + $cw.Count + " checks=" + @($a2.Json.checks).Count)
    foreach ($w in $cw) { Write-Host ("        configWarning: " + $w) -ForegroundColor DarkGray }
} else {
    Add-Result 'A2' 'R95: сломанный конфиг группы назван' 'FAIL' ("HTTP {0} configWarnings={1}" -f $a2.Status, $cw.Count)
}

# --- A3: валидатор на реальной композиции с RPC-воркером ---------------------
# Проба порта НЕ отключена (skipProbes=false): проверяем, что недоступный узел
# называется вслух, а не проходит молча.
$goodBody = @{
    group = @{
        id = 'accept-two-machine'; model = 'qwen3-14b'; strategy = 'layer'
        members = @(
            @{ backendId = 'cppworker-gpu-bundled-agent'; role = 'master' },
            @{ backendId = $Node2Id; role = 'worker'; rpcEndpoint = "$RpcHost`:$RpcPort" }
        )
    }
    model = @{
        name = 'qwen3-14b'; layers = 40; hiddenSize = 5120; nCtx = 32768
        kvLayers = 40; kvHeads = 8; kvHeadDim = 128; kvBytesPerElem = 2; bytesPerElem = 2
    }
    lang = $Lang
    bandwidthMBps = $BandwidthMBps
    prefillTokens = $PrefillTokens
} | ConvertTo-Json -Depth 10
$a3 = Invoke-Api -Method POST -Path '/api/v1/placement/validate' -Json $goodBody -TimeoutSec 60
if ($a3.Status -eq 200) {
    $errs = @($a3.Json.errors); $warns = @($a3.Json.warnings); $hints = @($a3.Json.hints)
    $detail = "checks={0} errors={1} warnings={2} hints={3}" -f @($a3.Json.checks).Count, $errs.Count, $warns.Count, $hints.Count
    Add-Result 'A3' 'R95: композиция с RPC-воркером' 'PASS' $detail
    # Главное здесь — не «ошибок нет», а что узел, которого нет в балансере,
    # назван по имени: молчаливое «всё хорошо» на несуществующем узле было бы
    # худшим исходом сухого прогона.
    if ($null -ne (Get-IssueCheck $a3.Json.issues 'node_unreachable')) {
        Write-Host ("        узел {0} назван НЕзарегистрированным — поднимите его или укажите -Node2Id" -f $Node2Id) -ForegroundColor DarkGray
    } else {
        Write-Host ("        узел {0} найден в балансере: композиция проверена по живому узлу" -f $Node2Id) -ForegroundColor DarkGray
    }
    foreach ($e in $errs) { Write-Host ("        error:   " + (Format-Issue $e)) -ForegroundColor DarkGray }
    foreach ($w in $warns) { Write-Host ("        warning: " + (Format-Issue $w)) -ForegroundColor DarkGray }
    foreach ($h in $hints) { Write-Host ("        hint:    " + (Format-Issue $h)) -ForegroundColor DarkGray }
} else {
    Add-Result 'A3' 'R95: композиция с RPC-воркером' 'FAIL' ("HTTP {0} {1}" -f $a3.Status, $a3.Error)
}

# --- A4: план мастера с числами ---------------------------------------------
# Размер задаём явно: без sizeBytes балансер честно отказывается предлагать
# раскладку («не гадаем»), и проверить числа было бы не на чем.
$planBody = @{
    models = @(@{
        name = 'acceptance-14b'; sizeBytes = [int64]9000000000; hiddenSize = 5120
        layers = 40; nCtx = 32768; kvLayers = 40; kvHeads = 8; kvHeadDim = 128
    })
    lang = $Lang
    bandwidthMBps = $BandwidthMBps
    prefillTokens = $PrefillTokens
    tokensPerSecond = 100
} | ConvertTo-Json -Depth 10
$a4 = Invoke-Api -Method POST -Path '/api/v1/placement/plan' -Json $planBody -TimeoutSec 60
$items = @($a4.Json.plans)
if ($a4.Status -eq 200 -and $items.Count -gt 0) {
    $it = $items | Where-Object { $_.model -eq 'acceptance-14b' } | Select-Object -First 1
    if ($null -eq $it) { $it = $items[0] }
    if ($it.knownSize -eq $true -and $it.reason) {
        Add-Result 'A4' 'R96: план с числами по каждому узлу' 'PASS' ("model={0} strategy={1} sizeGB={2:N2} needGB={3:N2} maxNodeGB={4:N2} sumFreeGB={5:N2} degraded={6}" -f `
            $it.model, $it.strategy, $it.sizeGB, $it.needGB, $it.maxNodeGB, $it.sumFreeGB, $it.degraded)
        Write-Host ("        reason: " + $it.reason) -ForegroundColor DarkGray
        foreach ($n in @($it.notes)) { Write-Host ("        note:   " + $n) -ForegroundColor DarkGray }
        if ($null -ne $it.traffic) {
            Add-Result 'A4t' 'R96: трафик раскладки (sharded) посчитан' 'PASS' ("layer={0:N3} ms/токен, row={1:N3} ms/токен, prefill512={2:N1} ms" -f `
                $it.traffic.layer.TokenOverheadMs, $it.traffic.row.TokenOverheadMs, $it.traffic.layer.PrefillMs)
        } else {
            Add-Result 'A4t' 'R96: трафик раскладки (sharded) посчитан' 'INFO' ("не считается для strategy={0}: трафик есть только у sharded, а раскладка требует ≥2 узлов с известным VRAM (сейчас узлов-кандидатов: maxNodeGB={1:N2} = sumFreeGB={2:N2})" -f `
                $it.strategy, $it.maxNodeGB, $it.sumFreeGB)
        }
    } else {
        Add-Result 'A4' 'R96: план с числами по каждому узлу' 'FAIL' ("knownSize={0} reason='{1}'" -f $it.knownSize, $it.reason)
    }
} else {
    Add-Result 'A4' 'R96: план с числами по каждому узлу' 'FAIL' ("HTTP {0} plans={1} {2}" -f $a4.Status, $items.Count, $a4.Error)
}

# --- A4b: математика §3 живая, а не зашитая ---------------------------------
# Одна и та же композиция (row split) при полосе 112 МБ/с и при 22.4 МБ/с
# (1/5): задержка на токен обязана вырасти ровно в 5 раз. Это проверяет, что
# числа приходят из запроса и формул §3, а не из константы в ответе.
function Invoke-TrafficProbe {
    param([double]$Bandwidth)
    $body = @{
        group = @{
            id = 'accept-traffic'; model = 'acceptance-traffic'; strategy = 'row'
            members = @(
                @{ backendId = 'cppworker-gpu-bundled-agent'; role = 'master' },
                @{ backendId = $Node2Id; role = 'worker'; rpcEndpoint = "$RpcHost`:$RpcPort" }
            )
        }
        model = @{
            name = 'acceptance-traffic'; layers = 48; hiddenSize = 5120; nCtx = 32768
            kvLayers = 48; kvHeads = 8; kvHeadDim = 128; kvBytesPerElem = 2; bytesPerElem = 2
        }
        lang = $Lang
        bandwidthMBps = $Bandwidth
        prefillTokens = $PrefillTokens
        tokensPerSecond = 100
        skipProbes = $true
    } | ConvertTo-Json -Depth 10
    return Invoke-Api -Method POST -Path '/api/v1/placement/validate' -Json $body -TimeoutSec 60
}
$fast = Invoke-TrafficProbe -Bandwidth $BandwidthMBps
$slow = Invoke-TrafficProbe -Bandwidth ($BandwidthMBps / 5)
$issueFast = Get-IssueCheck $fast.Json.issues 'traffic_row_high'
$issueSlow = Get-IssueCheck $slow.Json.issues 'traffic_row_high'
if ($null -ne $issueFast -and $null -ne $issueSlow) {
    $msFast = [double]$issueFast.params.overheadMs
    $msSlow = [double]$issueSlow.params.overheadMs
    $ratio = if ($msFast -gt 0) { $msSlow / $msFast } else { 0 }
    $bytesPerToken = [int64]$issueFast.params.bytesPerToken
    $detail = ("{0} Б/токен: {1:N2} мс при {2:N0} МБ/с → {3:N2} мс при {4:N0} МБ/с (×{5:N2}), prefill{6}={7:N1} мс" -f `
        $bytesPerToken, $msFast, $BandwidthMBps, $msSlow, ($BandwidthMBps / 5), $ratio, $PrefillTokens, $issueFast.params.prefillMs)
    if ($ratio -ge 4.5 -and $ratio -le 5.5) {
        Add-Result 'A4b' '§3: задержка раскладки масштабируется полосой' 'PASS' $detail
    } else {
        Add-Result 'A4b' '§3: задержка раскладки масштабируется полосой' 'FAIL' ($detail + ' — ожидалось ×5')
    }
} else {
    Add-Result 'A4b' '§3: задержка раскладки масштабируется полосой' 'FAIL' ("traffic_row_high не выдан: fast={0} slow={1}" -f $null -ne $issueFast, $null -ne $issueSlow)
}

# --- A4c: положительная ветка плана (модель влезает) -------------------------
# Без неё приёмка проверяла бы только честный отказ: на одноузловом стенде
# ветка «влезает в ≥2 узла → replicated» недостижима, а «влезает в один» —
# достижима и должна быть с числами.
$smallBody = @{
    models = @(@{
        name = 'acceptance-small'; sizeBytes = [int64]2000000000; hiddenSize = 3072
        layers = 28; nCtx = 8192; kvLayers = 28; kvHeads = 8; kvHeadDim = 128
    })
    lang = $Lang
    bandwidthMBps = $BandwidthMBps
    prefillTokens = $PrefillTokens
} | ConvertTo-Json -Depth 10
$a4c = Invoke-Api -Method POST -Path '/api/v1/placement/plan' -Json $smallBody -TimeoutSec 60
$small = @($a4c.Json.plans) | Where-Object { $_.model -eq 'acceptance-small' } | Select-Object -First 1
if ($null -ne $small -and $small.knownSize -eq $true -and ($small.strategy -in @('single', 'replicated')) -and $small.degraded -eq $false) {
    # Обе стратегии корректны, и выбор зависит от числа узлов: на одноузловом
    # стенде это single, на двухузловом — replicated («копии дают кратную
    # пропускную способность», §8). Поэтому проверяем не конкретную стратегию, а
    # то, что предложение обосновано числами и не помечено degraded.
    Add-Result 'A4c' 'R96: влезающая модель → single/replicated с числами' 'PASS' ("strategy={0} sizeGB={1:N2} needGB={2:N2} maxNodeGB={3:N2} → {4}" -f `
        $small.strategy, $small.sizeGB, $small.needGB, $small.maxNodeGB, $small.reason)
} elseif ($null -ne $small) {
    Add-Result 'A4c' 'R96: влезающая модель → single/replicated с числами' 'FAIL' ("strategy={0} degraded={1} reason='{2}'" -f $small.strategy, $small.degraded, $small.reason)
} else {
    Add-Result 'A4c' 'R96: влезающая модель → single/replicated с числами' 'FAIL' ("HTTP {0}: нет предложения по acceptance-small" -f $a4c.Status)
}

# --- A5: dryRun ничего не меняет --------------------------------------------
$probePolicy = @{
    placement = @{
        enabled = $true
        fallback = 'error'
        classes = @(@{ match = @{ model = '*' }; strategy = 'auto'; reason = 'acceptance dryRun' })
    }
    backendGroups = @()
    dryRun = $true
} | ConvertTo-Json -Depth 10
$a5 = Invoke-Api -Method PUT -Path '/api/v1/placement' -Json $probePolicy
$after5 = Invoke-Api -Path '/api/v1/placement'
$unchanged = ((@($after5.Json.placement.classes) | ConvertTo-Json -Depth 10 -Compress) -eq (@($before.placement.classes) | ConvertTo-Json -Depth 10 -Compress))
if ($a5.Status -eq 200 -and $a5.Json.applied -eq $false -and $a5.Json.persisted -eq $false -and $unchanged) {
    Add-Result 'A5' 'R100: dryRun не применяет и не сохраняет' 'PASS' ("valid={0} applied=false persisted=false; политика не изменилась" -f $a5.Json.valid)
} else {
    Add-Result 'A5' 'R100: dryRun не применяет и не сохраняет' 'FAIL' ("HTTP {0} applied={1} persisted={2} unchanged={3}" -f $a5.Status, $a5.Json.applied, $a5.Json.persisted, $unchanged)
}

# --- A6: применение политики -------------------------------------------------
$applyBody = @{
    placement = @{
        enabled = $true
        fallback = 'error'
        classes = @(@{ match = @{ model = 'acceptance-*' }; strategy = 'auto'; reason = 'живая приёмка R107' })
    }
    backendGroups = @()
    dryRun = $false
} | ConvertTo-Json -Depth 10
$a6 = Invoke-Api -Method PUT -Path '/api/v1/placement' -Json $applyBody
$after6 = Invoke-Api -Path '/api/v1/placement'
$ruleSeen = $false
foreach ($c in @($after6.Json.placement.classes)) { if ($c.match.model -eq 'acceptance-*') { $ruleSeen = $true } }
if ($a6.Status -eq 200 -and $after6.Json.enabled -eq $true -and $ruleSeen) {
    Add-Result 'A6' 'R100: политика применена и видна' 'PASS' ("applied={0} persisted={1} persistError='{2}'" -f $a6.Json.applied, $a6.Json.persisted, $a6.Json.persistError)
} else {
    Add-Result 'A6' 'R100: политика применена и видна' 'FAIL' ("HTTP {0} enabled={1} ruleSeen={2} {3}" -f $a6.Status, $after6.Json.enabled, $ruleSeen, $a6.Error)
}

# --- A7: логический бэкенд (группа) -----------------------------------------
$groupBody = @{
    placement = @{
        enabled = $true
        fallback = 'error'
        classes = @()
        models = @(@{ model = 'acceptance-model'; strategy = 'sharded'; reason = 'живая приёмка R107: раскладка по слоям' })
    }
    backendGroups = @(@{
        id = 'acceptance-group'; model = 'acceptance-model'; strategy = 'layer'; reason = 'живая приёмка R107'
        members = @(
            @{ backendId = 'cppworker-gpu-bundled-agent'; role = 'master' },
            @{ backendId = $Node2Id; role = 'worker'; rpcEndpoint = "$RpcHost`:$RpcPort" }
        )
    })
    dryRun = $false
    allowWarnings = $true
} | ConvertTo-Json -Depth 10
$a7 = Invoke-Api -Method PUT -Path '/api/v1/placement' -Json $groupBody
$after7 = Invoke-Api -Path '/api/v1/placement'
$groupSeen = $false
foreach ($g in @($after7.Json.groups)) { if ($g.id -eq 'acceptance-group') { $groupSeen = $true } }
if ($a7.Status -eq 200 -and $groupSeen) {
    $g0 = @($after7.Json.groups) | Where-Object { $_.id -eq 'acceptance-group' } | Select-Object -First 1
    Add-Result 'A7' 'R92: группа бэкендов видна в состоянии' 'PASS' ("members={0} healthy={1} degraded={2} vramKnown={3}" -f @($g0.members).Count, $g0.healthy, $g0.degraded, $g0.vramKnownMembers)
    foreach ($w in @($after7.Json.backendGroupWarnings)) { Write-Host ("        groupWarning: " + $w) -ForegroundColor DarkGray }
} else {
    Add-Result 'A7' 'R92: группа бэкендов видна в состоянии' 'FAIL' ("HTTP {0} groupSeen={1} {2}" -f $a7.Status, $groupSeen, $a7.Error)
    foreach ($w in @($a7.Json.warnings)) { Write-Host ("        rejected: " + $w) -ForegroundColor DarkGray }
}

# --- A8: миграция режима объяснена ------------------------------------------
$notes = @($after7.Json.modeMigration)
if ($notes.Count -gt 0) {
    $first = [string]$notes[0]
    if ($first.Length -gt 110) { $first = $first.Substring(0, 110) + '…' }
    Add-Result 'A8' 'R97: modeMigration объясняет происхождение' 'PASS' $first
} else {
    Add-Result 'A8' 'R97: modeMigration объясняет происхождение' 'INFO' 'заметок нет: конфиг уже выражен политикой (миграция не потребовалась)'
}

# --- A9: buildInfo бэкендов --------------------------------------------------
$a9 = Invoke-Api -Path '/api/v1/backends'
if ($a9.Status -eq 200) {
    $withBuild = @($a9.Json.backends | Where-Object { $_.buildInfo -and "$($_.buildInfo)".Trim() -ne '' })
    $detail = ("бэкендов={0} с buildInfo={1}" -f @($a9.Json.backends).Count, $withBuild.Count)
    if ($withBuild.Count -gt 0) {
        Add-Result 'A9' 'R101: buildInfo бэкендов' 'PASS' ($detail + " (" + $withBuild[0].buildInfo + ")")
    } else {
        # Не «ждём новую сборку»: образ cppworker УЖЕ новый (version=r107), а
        # buildInfo всё равно пусто — потому что в бандл-стенде бэкенд
        # регистрирует ВСТРОЕННЫЙ агент (CPPWORKER_REGISTER_DISABLE=true), а
        # buildInfo шлёт только Go-сторона cppworker
        # (cmd/cppworker/balancer_register.go). Это находка приёмки, а не статус.
        Add-Result 'A9' 'R101: buildInfo бэкендов' 'INFO' ($detail + ' — путь R101 в бандл-стенде не задействован: регистрирует встроенный агент, а buildInfo шлёт только Go-сторона cppworker (её регистрация выключена)')
    }
} else {
    Add-Result 'A9' 'R101: buildInfo бэкендов' 'FAIL' ("HTTP {0}" -f $a9.Status)
}

# --- A10: откат --------------------------------------------------------------
if ($NoRevert) {
    Add-Result 'A10' 'откат к исходной политике' 'SKIP' 'указан -NoRevert: стенд остаётся изменённым'
} else {
    $revertBody = @{ placement = $before.placement; backendGroups = @($before.backendGroups); dryRun = $false; allowWarnings = $true } | ConvertTo-Json -Depth 10
    $a10 = Invoke-Api -Method PUT -Path '/api/v1/placement' -Json $revertBody
    $after10 = Invoke-Api -Path '/api/v1/placement'
    $pOk = ((@($after10.Json.placement.classes) | ConvertTo-Json -Depth 10 -Compress) -eq (@($before.placement.classes) | ConvertTo-Json -Depth 10 -Compress))
    $gOk = ((@($after10.Json.backendGroups) | ConvertTo-Json -Depth 10 -Compress) -eq (@($before.backendGroups) | ConvertTo-Json -Depth 10 -Compress))
    if ($a10.Status -eq 200 -and $pOk -and $gOk) {
        Add-Result 'A10' 'откат к исходной политике' 'PASS' ("enabled={0} classes={1} groups={2}" -f $after10.Json.placement.enabled, @($after10.Json.placement.classes).Count, @($after10.Json.backendGroups).Count)
    } else {
        Add-Result 'A10' 'откат к исходной политике' 'FAIL' ("HTTP {0} policyOk={1} groupsOk={2}" -f $a10.Status, $pOk, $gOk)
    }
}

# --- A11: image-плоскость (opt-in) ------------------------------------------
if ($Image) {
    $a11 = Invoke-Api -Path '/api/v1/image/backends'
    if ($a11.Status -eq 200) {
        Add-Result 'A11' 'R98/R99: image-плоскость отвечает' 'PASS' ("backends=" + @($a11.Json.backends).Count)
    } else {
        Add-Result 'A11' 'R98/R99: image-плоскость отвечает' 'FAIL' ("HTTP {0} {1}" -f $a11.Status, $a11.Error)
    }
}

# --- A12..A14: собранный WebUI (не только API) ------------------------------
# Проверяем ОБРАЗ, а не рабочее дерево: модули должны отдаваться той сборкой,
# что раскатана, — иначе «в репозитории есть, в браузере нет» (так и было до
# пересборки: стенд крутил прошлый выпуск).
$webuiBase = "http://127.0.0.1:$WebUiPort"

function Get-WebUiText {
    param([string]$Path)
    try {
        $r = Invoke-WebRequest -Uri "$webuiBase$Path" -UseBasicParsing -TimeoutSec 15
        if ([int]$r.StatusCode -ne 200) { return $null }
        if ($r.RawContentStream) { return [System.Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray()) }
        return [string]$r.Content
    } catch { return $null }
}

$wantedModules = @(
    'js/modules/setup-plan.js',            # R103: план раскладки в мастере
    'js/modules/setup-wizard-presets.js',  # R106: пресеты раскладки
    'js/modules/backend-merge.js',         # R102: окно объединения
    'js/modules/monitor-groups.js'         # R104: карточка групп на мониторе
)
$missing = @()
$served = @{}
foreach ($m in $wantedModules) {
    $text = Get-WebUiText "/$m"
    if ($null -eq $text) { $missing += $m } else { $served[$m] = $text }
}
if ($missing.Count -eq 0) {
    Add-Result 'A12' 'R102/R103/R104/R106: WebUI отдаёт модули' 'PASS' ("{0} модулей из образа на :{1}" -f $wantedModules.Count, $WebUiPort)
} else {
    Add-Result 'A12' 'R102/R103/R104/R106: WebUI отдаёт модули' 'FAIL' ("не отдаются: " + ($missing -join ', ') + " — образ webui старый?")
}

# A13: уборка режимов именно в МАСТЕРЕ. Ищем присваивание в коде, а не слово:
# в комментариях `operatingMode` упоминается намеренно (объясняет, что снято).
$wizardMain = Get-WebUiText '/js/modules/setup-wizard.js'
if ($null -ne $wizardMain) {
    if ($wizardMain -notmatch 'operatingMode\s*:') {
        Add-Result 'A13' 'R107: в мастере нет operatingMode' 'PASS' 'присваивания нет (комментарии не считаем)'
    } else {
        Add-Result 'A13' 'R107: в мастере нет operatingMode' 'FAIL' 'мастер всё ещё отправляет operatingMode'
    }
} else {
    Add-Result 'A13' 'R107: в мастере нет operatingMode' 'FAIL' 'js/modules/setup-wizard.js не отдаётся'
}

# A14: честный остаток R107. Переключатель режима на странице «Настройки» ещё
# жив: он описан в плане как отдельная работа (нужен живой браузер). Молчать об
# этом нельзя — иначе «уборка завершена» звучало бы как «режимов больше нет».
$indexHtml = Get-WebUiText '/index.html'
if ($null -ne $indexHtml -and $indexHtml -match 'mode-wizard\.js') {
    Add-Result 'A14' 'R107 остаток: переключатель режима на «Настройках»' 'INFO' 'mode-wizard.js подключён в index.html — страница «Настройки» всё ещё предлагает режим работы (описано в плане)'
} elseif ($null -ne $indexHtml) {
    Add-Result 'A14' 'R107 остаток: переключатель режима на «Настройках»' 'PASS' 'переключатель больше не подключён'
}

# --- Итог --------------------------------------------------------------------
$pass = @($script:results | Where-Object { $_.Result -eq 'PASS' }).Count
$fail = @($script:results | Where-Object { $_.Result -eq 'FAIL' }).Count
$skip = @($script:results | Where-Object { $_.Result -eq 'SKIP' }).Count
$info = @($script:results | Where-Object { $_.Result -eq 'INFO' }).Count
Write-Host ("--- итог: PASS={0} FAIL={1} SKIP={2} INFO={3} ---" -f $pass, $fail, $skip, $info) -ForegroundColor $(if ($fail -gt 0) { 'Red' } else { 'Green' })
exit $(if ($fail -gt 0) { 1 } else { 0 })
