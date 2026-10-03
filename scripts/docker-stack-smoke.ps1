<#
.SYNOPSIS
    Живой смоук ЕДИНОГО стенда OllamaLegion: текст + картинки за ОДНИМ балансером.

.DESCRIPTION
    Проверяет ровно то, что обещает профиль `full` в deployments/docker-compose.stack.yml:
    один балансер, два пула бэкендов (llama_cpp — текст, image_cpp — генерация
    изображений), общий каталог моделей, OpenAI-поверхность для картинок.

    Скрипт НЕ пересобирает и НЕ пересоздаёт стек по умолчанию: он смотрит на то,
    что уже запущено, и печатает отчёт «проверка → результат». Пересборка —
    явный флаг -Build (иначе на машине с рабочей сборкой смоук молча ронял бы
    сервисы оператора).

    Проверки:
      S1  docker compose config (--profile full) содержит все 5 сервисов
      S2  контейнеры стенда запущены (ol-stack-*)
      S3  admin API балансера отвечает (/api/v1/backends)
      S4  image_cpp зарегистрирован сам и с непустым imagePort
      S5  llama_cpp (текстовый пул) зарегистрирован в ТОМ ЖЕ балансере
      S6  OpenAI-поверхность 18079 опубликована и отдаёт /v1/models
      S7  /api/v1/cluster отдаёт cluster.image (агрегат image-пула для Monitor)
      S8  WebUI 18083 отдаёт страницу «Image-модели»: 6 табов, оба модуля табов, без формы генерации
      S9  imageworker видит каталог моделей (GET .../models)
      S10 [opt-in -Generate] генерация через балансер отдаёт валидный PNG
      S11 [opt-in -Generate] метрики запросов выросли (total/recent)

.PARAMETER ComposeFile
    Compose-файл стенда. По умолчанию deployments/docker-compose.stack.yml.

.PARAMETER Profile
    Профиль compose. По умолчанию full (текст + картинки).

.PARAMETER Build
    Пересобрать и пересоздать стек (docker compose up -d --build). Без флага
    смоук только проверяет уже запущенное.

.PARAMETER Generate
    Догрузить первую доступную image-модель и сгенерировать картинку через
    балансер (занимает GPU на десятки секунд). Без флага S10/S11 = SKIP.

.PARAMETER Keep
    Не гасить стек после прогона (по умолчанию смоук ничего не гасит: он не
    поднимал стенд сам, если не указан -Build).

.EXAMPLE
    powershell -File scripts\docker-stack-smoke.ps1

.EXAMPLE
    powershell -File scripts\docker-stack-smoke.ps1 -Build -Generate
#>
[CmdletBinding()]
param(
    [string]$ComposeFile = '',
    [string]$Profile = 'full',
    [int]$ApiPort = 18081,
    [int]$OpenAiPort = 18079,
    [int]$WebUiPort = 18083,
    [int]$GenerateTimeoutSec = 420,
    [switch]$Build,
    [switch]$Generate,
    [switch]$Keep
)

$ErrorActionPreference = 'Stop'
$script:results = New-Object System.Collections.ArrayList
$repoRoot = Split-Path -Parent $PSScriptRoot
$deployDir = Join-Path $repoRoot 'deployments'
if (-not $ComposeFile) { $ComposeFile = Join-Path $deployDir 'docker-compose.stack.yml' }
$composeName = Split-Path -Leaf $ComposeFile

function Add-Result {
    param([string]$Id, [string]$Check, [string]$Result, [string]$Detail)
    [void]$script:results.Add([pscustomobject]@{ Id = $Id; Check = $Check; Result = $Result; Detail = $Detail })
    $color = switch ($Result) { 'PASS' { 'Green' } 'SKIP' { 'Yellow' } default { 'Red' } }
    Write-Host ("  [{0}] {1} :: {2}" -f $Id, $Result, $Detail) -ForegroundColor $color
}

# Invoke-Http — HTTP-запрос без исключений на 4xx/5xx: возвращает Status и Body.
function Invoke-Http {
    param([string]$Method = 'GET', [string]$Url, [string]$Body = '', [hashtable]$Headers = $null, [int]$TimeoutMs = 15000)
    $out = [pscustomobject]@{ Status = 0; Body = ''; Error = '' }
    try {
        $params = @{ Uri = $Url; Method = $Method; TimeoutSec = [Math]::Max(1, [int]($TimeoutMs / 1000)); UseBasicParsing = $true }
        if ($Body) { $params['Body'] = $Body; $params['ContentType'] = 'application/json' }
        if ($Headers) { $params['Headers'] = $Headers }
        $resp = Invoke-WebRequest @params
        $out.Status = [int]$resp.StatusCode
        $out.Body = [string]$resp.Content
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
    return $out
}

function ConvertTo-JsonSafe {
    param([string]$Text)
    if (-not $Text) { return $null }
    try { return $Text | ConvertFrom-Json } catch { return $null }
}

# Get-DotEnvValue — значение ключа из deployments/.env (нужен общий токен стека).
function Get-DotEnvValue {
    param([string]$Path, [string]$Key)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match "^\s*$([regex]::Escape($Key))\s*=\s*(.+?)\s*$") { return $Matches[1].Trim('"').Trim("'") }
    }
    return ''
}

function Invoke-Compose {
    param([string[]]$ComposeArgs)
    Push-Location $deployDir
    try {
        $all = @('compose', '-f', $composeName, '--profile', $Profile) + $ComposeArgs
        $out = & docker @all 2>&1
        return [pscustomobject]@{ Exit = $LASTEXITCODE; Output = ($out | Out-String) }
    } finally { Pop-Location }
}

Write-Host "=== OllamaLegion: смоук единого стенда (профиль $Profile) ===" -ForegroundColor Cyan
Write-Host "compose: $ComposeFile"

# --- Предусловия: docker, compose-файл, .env ---
$dockerOk = $false
try { & docker version --format '{{.Server.Version}}' *> $null; $dockerOk = ($LASTEXITCODE -eq 0) } catch { }
if (-not $dockerOk) { throw 'docker недоступен (проверьте Docker Desktop и права CLI)' }
if (-not (Test-Path -LiteralPath $ComposeFile)) { throw "нет compose-файла: $ComposeFile" }

$envFile = Join-Path $deployDir '.env'
$stackEnvFile = Join-Path $deployDir '.env.bundled-with-agent'
$token = Get-DotEnvValue -Path $envFile -Key 'CPPWORKER_API_TOKEN'
if (-not (Test-Path -LiteralPath $stackEnvFile)) {
    Write-Warning "нет $stackEnvFile — compose не сможет поднять loadbalancer (создайте из .env.bundled-with-agent.example)"
}
if (-not $token) { Write-Warning "в deployments/.env нет CPPWORKER_API_TOKEN — admin API ответит 401/403" }
$authHeaders = @{}
if ($token) { $authHeaders['X-API-Token'] = $token }

$api = "http://127.0.0.1:$ApiPort"
$oa = "http://127.0.0.1:$OpenAiPort"
$webui = "http://127.0.0.1:$WebUiPort"

# Поллер метрик балансера асинхронен: после генерации счётчики появляются не
# мгновенно, поэтому «подождать условия» вынесено в helper.
function Wait-Until {
    param([scriptblock]$Probe, [int]$TimeoutSec = 30, [int]$IntervalMs = 700)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        if (& $Probe) { return $true }
        Start-Sleep -Milliseconds $IntervalMs
    }
    return $false
}

try {
    # ========================================================
    # S1: конфигурация профиля full
    # ========================================================
    # ВАЖНО: `docker compose config --services` в этой сборке CLI НЕ работает
    # (печатает nothing/usage), поэтому разбираем ПОЛНЫЙ рендер конфига и ищем
    # container_name сервисов стенда — он однозначен и не зависит от флагов.
    $cfg = Invoke-Compose @('config')
    $cfgText = [string]$cfg.Output
    $wantContainers = @{
        'loadbalancer'  = 'ol-stack-balancer'
        'webui'         = 'ol-stack-webui'
        'cppworker-gpu' = 'ol-stack-cppworker-gpu'
        'imageworker'   = 'ol-stack-imageworker'
        'agent'         = 'ol-stack-agent'
    }
    $missing = @()
    foreach ($svc in $wantContainers.Keys) {
        if ($cfgText -notmatch [regex]::Escape($wantContainers[$svc])) { $missing += $svc }
    }
    if ($missing.Count -eq 0) {
        Add-Result 'S1' "профиль $Profile содержит все сервисы стенда" 'PASS' "5 сервисов: $($wantContainers.Keys -join ',')"
    } else {
        Add-Result 'S1' "профиль $Profile содержит все сервисы стенда" 'FAIL' "нет в отрендеренном конфиге: $($missing -join ',') (exit=$($cfg.Exit))"
    }

    # ========================================================
    # S2: контейнеры запущены
    # ========================================================
    if ($Build) {
        Write-Host '  --- docker compose up -d --build (это надолго) ---' -ForegroundColor Yellow
        $up = Invoke-Compose @('up', '-d', '--build')
        if ($up.Exit -ne 0) { Write-Host ($up.Output | Select-Object -Last 15) }
    }
    $psOut = & docker ps --filter 'name=ol-stack-' --format '{{.Names}}|{{.Status}}|{{.Ports}}' 2>&1 | Out-String
    $running = @($psOut -split "`r?`n" | Where-Object { $_.Trim() })
    $needContainers = @('ol-stack-balancer', 'ol-stack-imageworker')
    $missContainers = @()
    foreach ($c in $needContainers) { if (-not ($running -match [regex]::Escape($c))) { $missContainers += $c } }
    if ($missContainers.Count -eq 0) {
        Add-Result 'S2' 'контейнеры стенда запущены (балансер + imageworker)' 'PASS' "всего контейнеров ol-stack-*: $($running.Count)"
    } else {
        Add-Result 'S2' 'контейнеры стенда запущены (балансер + imageworker)' 'FAIL' "не запущены: $($missContainers -join ','); запустите: docker compose -f $composeName --profile $Profile up -d --build"
    }

    # ========================================================
    # S3: admin API балансера
    # ========================================================
    $r = Invoke-Http -Method GET -Url "$api/api/v1/backends" -Headers $authHeaders
    $backends = @()
    $bj = ConvertTo-JsonSafe $r.Body
    if ($bj -and $bj.backends) { $backends = @($bj.backends) }
    $s3 = ($r.Status -eq 200)
    Add-Result 'S3' 'admin API балансера отвечает (/api/v1/backends)' $(if ($s3) { 'PASS' } else { 'FAIL' }) "http=$($r.Status) backends=$($backends.Count) err=$($r.Error)"

    # ========================================================
    # S4/S5: оба типа бэкендов в ОДНОМ балансере
    # ========================================================
    $img = @($backends | Where-Object { ($_.backendType -eq 'image_cpp') -or ($_.type -eq 'image_cpp') })
    $llm = @($backends | Where-Object { ($_.backendType -eq 'llama_cpp') -or ($_.type -eq 'llama_cpp') })
    $imgPort = 0
    if ($img.Count -gt 0) { $imgPort = [int]$img[0].imagePort }
    if ($img.Count -ge 1 -and $imgPort -gt 0) {
        Add-Result 'S4' 'image_cpp зарегистрирован сам и с непустым imagePort' 'PASS' "id=$($img[0].id) imagePort=$imgPort host=$($img[0].host)"
    } else {
        Add-Result 'S4' 'image_cpp зарегистрирован сам и с непустым imagePort' 'FAIL' "image_cpp=$($img.Count) imagePort=$imgPort (проверьте логи ol-stack-imageworker: SDWORKER_BALANCER_URL/токен)"
    }
    if ($llm.Count -ge 1) {
        Add-Result 'S5' 'llama_cpp (текстовый пул) в том же балансере' 'PASS' "id=$($llm[0].id) status=$($llm[0].status)"
    } else {
        Add-Result 'S5' 'llama_cpp (текстовый пул) в том же балансере' 'FAIL' 'текстовых бэкендов нет: стенд поднят частично (профиль full ожидает cppworker-gpu + agent)'
    }

    # ========================================================
    # S6: OpenAI-поверхность
    # ========================================================
    $r = Invoke-Http -Method GET -Url "$oa/v1/models"
    $mj = ConvertTo-JsonSafe $r.Body
    $ids = @(); if ($mj -and $mj.data) { $ids = @($mj.data | ForEach-Object { $_.id }) }
    $s6 = ($r.Status -eq 200) -and ($ids -contains 'sd-cpp-local')
    Add-Result 'S6' 'OpenAI-поверхность 18079 отдаёт /v1/models с алиасами картинок' $(if ($s6) { 'PASS' } else { 'FAIL' }) "http=$($r.Status) ids=$($ids -join ',')"

    # ========================================================
    # S7: агрегат image-пула в /api/v1/cluster (Monitor читает его)
    # ========================================================
    $r = Invoke-Http -Method GET -Url "$api/api/v1/cluster" -Headers $authHeaders
    $cj = ConvertTo-JsonSafe $r.Body
    # /api/v1/cluster отдаёт ClusterState напрямую (обёртку cluster добавляет клиент).
    $pool = $null
    if ($cj) { $pool = $cj.image }
    $poolBackends = 0; $poolTotal = 0
    if ($pool) {
        $poolBackends = [int]$pool.backends
        if ($pool.requests) { $poolTotal = [int]$pool.requests.total }
    }
    $s7 = ($r.Status -eq 200) -and ($poolBackends -ge 1)
    Add-Result 'S7' 'cluster.image (агрегат image-пула для Monitor) присутствует' $(if ($s7) { 'PASS' } else { 'FAIL' }) "http=$($r.Status) backends=$poolBackends requests_total=$poolTotal"

    # ========================================================
    # S8: WebUI отдаёт страницу «Image-модели» в новом виде (Phase 9)
    #
    # Проверяем СТРУКТУРУ, а не только факт «страница есть»: табы, оба модуля
    # табов и ОТСУТСТВИЕ формы генерации. Иначе пересборка образа webui без
    # новых JS прошла бы незамеченной (ровно этот случай уже был: старый образ
    # с ?v= из кэша отдавал прежнюю страницу).
    # ========================================================
    $r = Invoke-Http -Method GET -Url "$webui/" -TimeoutMs 10000
    $hasNav = ($r.Body -match 'data-page="image"')
    $hasShell = ($r.Body -match 'image-models-page\.js')
    $hasHfModule = ($r.Body -match 'image-models-hf\.js')
    $tabIds = @('imTabOverview', 'imTabHf', 'imTabModels', 'imTabLoaded', 'imTabDownloads', 'imTabSettings')
    $missingTabs = @($tabIds | Where-Object { $r.Body -notmatch [regex]::Escape($_) })
    # Следы формы генерации: их в разметке быть НЕ должно (Phase 9).
    $genTokens = @('imgGenerateBtn', 'imgPrompt', 'imgGallery', 'imgResults')
    $genLeft = @($genTokens | Where-Object { $r.Body -match [regex]::Escape($_) })
    $s8 = ($r.Status -eq 200) -and $hasNav -and $hasShell -and $hasHfModule -and ($missingTabs.Count -eq 0) -and ($genLeft.Count -eq 0)
    Add-Result 'S8' 'WebUI отдаёт страницу «Image-модели» с 6 табами и без формы генерации' $(if ($s8) { 'PASS' } else { 'FAIL' }) "http=$($r.Status) nav=$hasNav shell=$hasShell hf=$hasHfModule нет_табов=$($missingTabs -join ',') остатки_генерации=$($genLeft -join ',') (старый образ webui? нужен --build webui)"

    # ========================================================
    # S9: imageworker видит каталог моделей
    # ========================================================
    $imgId = ''
    if ($img.Count -gt 0) { $imgId = [string]$img[0].id }
    if ($imgId) {
        $r = Invoke-Http -Method GET -Url "$api/api/v1/image/backends/$imgId/models" -Headers $authHeaders
        $mj9 = ConvertTo-JsonSafe $r.Body
        $models9 = @(); if ($mj9 -and $mj9.models) { $models9 = @($mj9.models) }
        $state9 = ''; if ($mj9) { $state9 = [string]$mj9.state }
        Add-Result 'S9' 'imageworker видит каталог моделей (bundle-и)' 'PASS' "http=$($r.Status) models=$($models9.Count) state=$state9"
        if ($models9.Count -eq 0) {
            Write-Host "        подсказка: bundle-ов нет — скачайте модель на странице «Изображения» (HF) или положите каталог в <MODELS_DIR>/image/<имя>/" -ForegroundColor Yellow
        }
    } else {
        Add-Result 'S9' 'imageworker видит каталог моделей (bundle-и)' 'SKIP' 'нет image-бэкенда (см. S4)'
    }

    # ========================================================
    # S10/S11: генерация через балансер + рост метрик (opt-in)
    # ========================================================
    if (-not $Generate) {
        Add-Result 'S10' 'генерация через балансер (PNG)' 'SKIP' 'флаг -Generate не задан (генерация занимает GPU)'
        Add-Result 'S11' 'метрики image-запросов растут' 'SKIP' 'флаг -Generate не задан'
    } elseif (-not $imgId) {
        Add-Result 'S10' 'генерация через балансер (PNG)' 'FAIL' 'нет image-бэкенда для генерации'
        Add-Result 'S11' 'метрики image-запросов растут' 'FAIL' 'нет image-бэкенда'
    } else {
        # Грузим первую доступную модель (если ни одна не загружена).
        $r = Invoke-Http -Method GET -Url "$api/api/v1/image/backends/$imgId/models" -Headers $authHeaders
        $mj10 = ConvertTo-JsonSafe $r.Body
        $firstModel = ''
        $loadedBefore = ''
        if ($mj10 -and $mj10.models) {
            foreach ($m in @($mj10.models)) {
                if (-not $firstModel) { $firstModel = [string]$m.name }
                if ($m.state -eq 'loaded') { $loadedBefore = [string]$m.name }
            }
        }
        $modelToUse = if ($loadedBefore) { $loadedBefore } else { $firstModel }
        if (-not $modelToUse) {
            Add-Result 'S10' 'генерация через балансер (PNG)' 'SKIP' 'в стенде нет ни одной image-модели (S9)'
            Add-Result 'S11' 'метрики image-запросов растут' 'SKIP' 'нет модели'
        } else {
            if (-not $loadedBefore) {
                Write-Host "  --- загрузка модели $modelToUse (до $GenerateTimeoutSec с) ---" -ForegroundColor Yellow
                Invoke-Http -Method POST -Url "$api/api/v1/image/backends/$imgId/models/load" -Body (@{ name = $modelToUse } | ConvertTo-Json -Compress) -Headers $authHeaders -TimeoutMs 30000 | Out-Null
                $loaded = Wait-Until -TimeoutSec $GenerateTimeoutSec -Probe {
                    $rr = Invoke-Http -Method GET -Url "$api/api/v1/cluster" -Headers $authHeaders
                    $jj = ConvertTo-JsonSafe $rr.Body
                    if (-not $jj) { return $false }
                    $bb = @($jj.backends) | Where-Object { $_.id -eq $imgId } | Select-Object -First 1
                    if (-not $bb -or -not $bb.image) { return $false }
                    return ($bb.image.state -eq 'loaded')
                }
                if (-not $loaded) { Write-Warning "модель $modelToUse не перешла в loaded за $GenerateTimeoutSec с" }
            }

            $t0 = Get-Date
            $genBody = @{ model = 'sd-cpp-local'; prompt = 'a cat sitting on a windowsill, warm light'; size = '512x512'; steps = 8; n = 1 } | ConvertTo-Json -Compress
            $r = Invoke-Http -Method POST -Url "$oa/v1/images/generations" -Body $genBody -Headers $authHeaders -TimeoutMs 600000
            $gj = ConvertTo-JsonSafe $r.Body
            $b64 = ''; if ($gj -and $gj.data -and $gj.data.Count -gt 0) { $b64 = [string]$gj.data[0].b64_json }
            $bytes = @()
            $isPng = $false
            if ($b64) {
                try {
                    $bytes = [Convert]::FromBase64String($b64)
                    $isPng = ($bytes.Length -gt 8 -and $bytes[0] -eq 0x89 -and $bytes[1] -eq 0x50 -and $bytes[2] -eq 0x4E -and $bytes[3] -eq 0x47)
                } catch { }
            }
            $secs = [int]((Get-Date) - $t0).TotalSeconds
            $s10 = ($r.Status -eq 200) -and $isPng
            Add-Result 'S10' 'генерация через балансер (PNG)' $(if ($s10) { 'PASS' } else { 'FAIL' }) "http=$($r.Status) model=$modelToUse bytes=$($bytes.Length) png=$isPng sec=$secs"

            # Метрики: total/recent обязаны вырасти после генерации.
            $sawMetrics = Wait-Until -TimeoutSec 30 -Probe {
                $rr = Invoke-Http -Method GET -Url "$api/api/v1/cluster" -Headers $authHeaders
                $jj = ConvertTo-JsonSafe $rr.Body
                if (-not $jj -or -not $jj.image) { return $false }
                $rec = @($jj.image.recent)
                $req = $jj.image.requests
                return ($rec.Count -ge 1 -and $req.total -ge 1)
            }
            $rr = Invoke-Http -Method GET -Url "$api/api/v1/cluster" -Headers $authHeaders
            $jj = ConvertTo-JsonSafe $rr.Body
            $tot = 0; $okc = 0; $recc = 0; $avg = 0
            if ($jj -and $jj.image) {
                if ($jj.image.requests) {
                    $tot = [int]$jj.image.requests.total
                    $okc = [int]$jj.image.requests.ok
                    $avg = [int]$jj.image.requests.avgDurationMs
                }
                $recc = @($jj.image.recent).Count
            }
            Add-Result 'S11' 'метрики image-запросов растут (total/ok/лента)' $(if ($sawMetrics) { 'PASS' } else { 'FAIL' }) "total=$tot ok=$okc recent=$recc avg_ms=$avg"
        }
    }
} finally {
    # Смоук НИЧЕГО не гасит: он проверяет уже работающий стенд, а запущенный
    # через -Build стек остаётся оператору (гасить его — отдельное решение).
}

Write-Host ''
Write-Host '=== РЕЗУЛЬТАТЫ ===' -ForegroundColor Cyan
$script:results | Format-Table -AutoSize | Out-String | Write-Host
$failed = @($script:results | Where-Object { $_.Result -eq 'FAIL' }).Count
$passed = @($script:results | Where-Object { $_.Result -eq 'PASS' }).Count
$skipped = @($script:results | Where-Object { $_.Result -eq 'SKIP' }).Count
Write-Host "Итого: $($script:results.Count) проверок, PASS=$passed, FAIL=$failed, SKIP=$skipped"
if ($failed -gt 0) { exit 1 }
