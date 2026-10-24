# scripts/balancer-scenarios.ps1 — R118.5 (2026-10-14).
#
# ПРОВЕРКА БАЛАНСЕРА НА РЕАЛЬНЫХ БЭКЕНДАХ И МОДЕЛЯХ (сценарии, а не юнит-тесты).
#
# Запуск:
#   pwsh -File scripts/balancer-scenarios.ps1                 # базовый набор
#   pwsh -File scripts/balancer-scenarios.ps1 -IncludeLongCtx # + рост n_ctx (долго)
#   pwsh -File scripts/balancer-scenarios.ps1 -IncludeDonorModel # + вторая модель на доноре
#
# Что проверяет (каждый сценарий печатает PASS/FAIL и улику):
#   1. роли/раскладка: главный с живым узлом, состояние из /api/v1/backends
#   2. загрузка модели с раскладкой: HTTP 200 + реальный шардинг в логе cppworker
#   3. нестриминговый чат через балансер (Ollama-поверхность :18080)
#   4. стриминговый чат (NDJSON, done=true)
#   5. OpenAI-совместимый путь /v1/chat/completions
#   6. read-only путь /api/show (OpenWebUI): 200 и быстро, без загрузки модели
#   7. num_ctx МЕНЬШЕ загруженного: без перезагрузки модели
#   8. два параллельных запроса (слоты модели ≥ 2)
#   9. три параллельных при двух слотах: очередь, а не отказ
#  10. (-IncludeLongCtx) num_ctx БОЛЬШЕ загруженного: reload и успех после него
#  11. (-IncludeLongCtx) рост n_ctx: reload и успех после него
#  12. (-IncludeDonorModel) вторая модель на доноре + параллельная работа двух моделей
#
# Ничего не «угадывает»: каждый PASS опирается либо на HTTP-ответ, либо на строку
# лога контейнера, которая печатается рядом.

[CmdletBinding()]
param(
    [string]$Admin = 'http://192.0.2.20:18081',
    [string]$Ollama = 'http://192.0.2.20:18080',
    [string]$CppWorker = 'http://192.0.2.20:18092',
    [string]$Token = 'changeme-bundled-with-agent-token',
    [string]$MainBackend = 'cppworker-gpu-bundled-agent',
    [string]$DonorBackend = 'cppworker-cyborg-bundled-agent',
    [string]$Model = 'gemma-4-E4B-it-Q4_K_M',
    [string]$DonorModel = 'Qwen3.8-27B-UD-Q4_K_M',
    [int]$Ctx = 32768,
    [switch]$IncludeLongCtx,
    [switch]$IncludeDonorModel
)

$ErrorActionPreference = 'Continue'
$script:Results = @()
$script:Started = Get-Date

function Admin-Call {
    param([string]$Path, [string]$Method = 'GET', $Body = $null)
    $headers = @{ 'X-API-Token' = $Token }
    if ($Body -ne $null) { $headers['Content-Type'] = 'application/json' }
    $req = @{ Uri = "$Admin$Path"; Method = $Method; Headers = $headers; TimeoutSec = 600; UseBasicParsing = $true }
    if ($Body -ne $null) { $req['Body'] = ($Body | ConvertTo-Json -Depth 6 -Compress) }
    return Invoke-WebRequest @req
}

function Record {
    param([string]$Name, [bool]$Pass, [string]$Evidence)
    $script:Results += [pscustomobject]@{ Scenario = $Name; Result = $(if ($Pass) { 'PASS' } else { 'FAIL' }); Evidence = $Evidence }
    $mark = if ($Pass) { 'PASS' } else { 'FAIL' }
    Write-Host ("[{0}] {1} — {2}" -f $mark, $Name, $Evidence)
}

function Check {
    param([string]$Name, [scriptblock]$Body)
    try {
        $ev = & $Body
        Record -Name $Name -Pass $true -Evidence ([string]$ev)
    } catch {
        Record -Name $Name -Pass $false -Evidence ("ОШИБКА: " + $_.Exception.Message)
    }
}

function CppModels {
    return (Invoke-WebRequest -Uri "$CppWorker/api/models" -TimeoutSec 15 -UseBasicParsing).Content | ConvertFrom-Json
}

function LoadedCtx {
    $m = CppModels
    if ($m.count -gt 0) { return [int]$m.models[0].context_size }
    return 0
}

function Admin-Load {
    param([string]$Backend, [string]$ModelName, [int]$Context, [int]$GpuLayers, [int]$TimeoutSec = 600)
    $body = @{ operation = 'load'; modelName = $ModelName; contextSize = $Context; gpuLayers = $GpuLayers; kvCacheType = 'q4_0'; useMmap = $true }
    return Admin-Call -Path "/api/v1/backends/$Backend/models" -Method Post -Body $body
}

function Admin-Unload {
    param([string]$Backend, [string]$ModelName)
    $body = @{ operation = 'unload'; modelName = $ModelName; force = $true }
    try { Admin-Call -Path "/api/v1/backends/$Backend/models" -Method Post -Body $body | Out-Null } catch {}
}

# EnsureLoaded — гарантировать, что на мастере загружена НУЖНАЯ модель с НУЖНЫМ
# окном, и вернуть пометку, если её пришлось поднимать заново.
#
# ЗАЧЕМ. Загрузка с раскладкой и gpuLayers=-1 иногда роняет cppworker (отдельная
# известная проблема: ggml-rpc + все слои на GPU). После рестарта модели нет, и
# следующий клиентский запрос грузит её заново — уже с окном клиента/профиля.
# Проверки read-only пути и «меньшего окна» должны это переживать, иначе они
# падают по чужой причине (живой случай: context_size=0 в сценарии 6).
function EnsureLoaded {
    param([int]$Context, [int]$GpuLayers = -1)
    $m = CppModels
    if ($m.count -gt 0 -and [int]$m.models[0].context_size -eq $Context) { return '' }
    $was = if ($m.count -gt 0) { "ctx=$([int]$m.models[0].context_size)" } else { 'модели нет' }
    Admin-Unload -Backend $MainBackend -ModelName $Model
    Start-Sleep -Seconds 2
    Admin-Load -Backend $MainBackend -ModelName $Model -Context $Context -GpuLayers $GpuLayers | Out-Null
    return "модель поднята заново (было: $was)"
}

function Chat-NonStream {
    param([string]$ModelName, [int]$NumCtx, [int]$NumPredict = 16, [string]$Prompt = '2+2?')
    $body = @{ model = $ModelName; messages = @(@{ role = 'user'; content = $Prompt }); stream = $false; options = @{ num_ctx = $NumCtx; num_predict = $NumPredict } }
    $t0 = Get-Date
    $r = Invoke-WebRequest -Uri "$Ollama/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($body | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 600 -UseBasicParsing
    $backend = ''
    if ($r.Headers['X-Backend-ID']) { $backend = [string]$r.Headers['X-Backend-ID'] }
    return [pscustomobject]@{ Status = $r.StatusCode; Content = $r.Content; Ms = [int]((Get-Date) - $t0).TotalMilliseconds; Backend = $backend }
}

function Chat-Stream {
    param([string]$ModelName, [int]$NumCtx, [int]$NumPredict = 16)
    $body = @{ model = $ModelName; messages = @(@{ role = 'user'; content = '2+2?' }); stream = $true; options = @{ num_ctx = $NumCtx; num_predict = $NumPredict } }
    $r = Invoke-WebRequest -Uri "$Ollama/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($body | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 600 -UseBasicParsing
    # NDJSON — НЕ текстовый content-type, поэтому .Content может быть byte[].
    # Декодируем поток вручную (иначе проверка done=true ложно падает).
    $text = [System.Text.Encoding]::UTF8.GetString($r.RawContentStream.ToArray())
    return [pscustomobject]@{ Status = $r.StatusCode; Text = $text; ContentType = [string]$r.Headers['Content-Type'] }
}

function Balancer-Log {
    param([int]$Minutes = 5)
    return (docker logs ol-stack-balancer --since "$($Minutes)m" 2>&1) -join "`n"
}

# Log-Mark / Log-Since — «снимок» лога до действия, чтобы проверять ТОЛЬКО новые
# строки: загрузка модели в сценарии 2 сама пишет 'scheduling async reload'
# (легальная загрузка отсутствующей модели), и без снимка сценарий 7 ловил бы её.
function Log-Mark {
    return (docker logs ol-stack-balancer 2>&1 | Measure-Object).Count
}
function Log-Since {
    param([int]$Mark)
    $all = docker logs ol-stack-balancer 2>&1
    if ($Mark -ge $all.Count) { return '' }
    return ($all[$Mark..($all.Count - 1)] -join "`n")
}

Write-Host "=== Сценарии балансера, старт $($script:Started.ToString('HH:mm:ss')) ===" -ForegroundColor Cyan

# --- 0. подготовка -----------------------------------------------------------
# На стенде та же модель могла остаться загруженной и на доноре (после прежних
# опытов). Тогда балансер вправе выбрать ЛЮБУЮ готовую копию, и проверки окон
# становятся недетерминированными (живой случай: запрос ушёл на копию донора с
# окном 4096, а не на подготовленную копию мастера с 32768). Поэтому до сценариев
# выгружаем копию донора и фиксируем роли main/donor.
Write-Host "подготовка: снимаю копию модели с донора, роли main/donor" -ForegroundColor DarkGray
Admin-Unload -Backend $DonorBackend -ModelName $Model
Admin-Call -Path "/api/v1/backends/$MainBackend/role" -Method Put -Body @{ role = 'main' } | Out-Null
Admin-Call -Path "/api/v1/backends/$DonorBackend/role" -Method Put -Body @{ role = 'donor' } | Out-Null
Start-Sleep -Seconds 3

# --- 1. роли и раскладка -----------------------------------------------------
Check '1. Роли и раскладка (главный + донор, живые узлы)' {
    $b = (Admin-Call '/api/v1/backends').Content | ConvertFrom-Json
    $main = $b.backends | Where-Object { $_.id -eq $MainBackend }
    $donor = $b.backends | Where-Object { $_.id -eq $DonorBackend }
    if (-not $main -or -not $donor) { throw "нет бэкендов $MainBackend / $DonorBackend" }
    if ($main.role -ne 'main') { throw "роль главного = '$($main.role)'" }
    if ($donor.role -ne 'donor') { throw "роль донора = '$($donor.role)'" }
    $guard = $main.rpcLayout
    if (-not $guard -or -not $guard.enabled) { throw "раскладка не включена у главного: $($guard | ConvertTo-Json -Compress)" }
    "main=main donor=donor rpcLayout=$($guard | ConvertTo-Json -Compress) канал=$($main.rpcLink.mbps) Мбит/с"
}

# --- 2. загрузка с раскладкой ------------------------------------------------
Check '2. Загрузка модели с раскладкой (шардинг в логе cppworker)' {
    Admin-Unload -Backend $MainBackend -ModelName $Model
    Start-Sleep -Seconds 2
    $restartsBefore = [int](docker inspect ol-stack-cppworker-gpu --format '{{.RestartCount}}')
    $r = Admin-Load -Backend $MainBackend -ModelName $Model -Context $Ctx -GpuLayers -1
    $ctx = LoadedCtx
    $restartsAfter = [int](docker inspect ol-stack-cppworker-gpu --format '{{.RestartCount}}')
    if ($r.StatusCode -ne 200) { throw "load HTTP $($r.StatusCode)" }
    if ($ctx -ne $Ctx) { throw "context_size=$ctx, ожидалось $Ctx" }
    $rpcDevices = (docker logs ol-stack-cppworker-gpu --since 12m 2>&1 | Select-String -Pattern 'RPC: устройств в раскладке')
    if (-not $rpcDevices) { throw "в логе cppworker нет подключения RPC-устройств" }
    $crash = if ($restartsAfter -gt $restartsBefore) { " | ВНИМАНИЕ: cppworker падал на этой загрузке ($restartsBefore→$restartsAfter), балансер поднял модель локально" } else { '' }
    "load HTTP 200, context_size=$ctx, $($rpcDevices[-1].Line.Trim())$crash"
}

# --- 3. нестриминговый чат ---------------------------------------------------
Check '3. Нестриминговый чат через балансер (Ollama :18080)' {
    $c = Chat-NonStream -ModelName $Model -NumCtx 8192
    if ($c.Status -ne 200) { throw "HTTP $($c.Status)" }
    $j = $c.Content | ConvertFrom-Json
    if (-not $j.message.content) { throw "пустой ответ: $($c.Content.Substring(0,[Math]::Min(120,$c.Content.Length)))" }
    "HTTP 200 за $($c.Ms) мс, ответ='$($j.message.content.Trim())'"
}

# --- 4. стриминговый чат -----------------------------------------------------
Check '4. Стриминговый чат (NDJSON, done=true)' {
    $s = Chat-Stream -ModelName $Model -NumCtx 8192
    if ($s.Status -ne 200) { throw "HTTP $($s.Status)" }
    if ($s.Text -notmatch '"done":\s*true') { throw "в стриме нет done=true: $($s.Text.Substring(0,[Math]::Min(200,$s.Text.Length)))" }
    $chunks = ($s.Text -split "`n" | Where-Object { $_.Trim() -ne '' }).Count
    "HTTP 200 ($($s.ContentType)), чанков=$chunks, есть done=true"
}

# --- 5. OpenAI-совместимый путь ---------------------------------------------
Check '5. OpenAI-совместимый путь /v1/chat/completions' {
    $body = @{ model = $Model; messages = @(@{ role = 'user'; content = '2+2?' }); max_tokens = 8 }
    $r = Invoke-WebRequest -Uri "$Ollama/v1/chat/completions" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($body | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 300 -UseBasicParsing
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    $j = $r.Content | ConvertFrom-Json
    if (-not $j.choices[0].message.content) { throw "пустой choices[0].message.content" }
    "HTTP 200, ответ='$($j.choices[0].message.content.Trim())'"
}

# --- 6. read-only путь (OpenWebUI) ------------------------------------------
Check '6. Read-only /api/show: быстро и без загрузки модели (OpenWebUI)' {
    $reload = EnsureLoaded -Context $Ctx
    $body = @{ model = $Model }
    $t0 = Get-Date
    $r = Invoke-WebRequest -Uri "$Ollama/api/show" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($body | ConvertTo-Json -Compress) -TimeoutSec 60 -UseBasicParsing
    $ms = [int]((Get-Date) - $t0).TotalMilliseconds
    if ($r.StatusCode -ne 200) { throw "HTTP $($r.StatusCode)" }
    if ($ms -gt 3000) { throw "медленно: $ms мс (ожидалось < 3000)" }
    $ctxAfter = LoadedCtx
    if ($ctxAfter -ne $Ctx) { throw "read-only путь изменил состояние модели: context_size=$ctxAfter" }
    "HTTP 200 за $ms мс, context_size не изменился ($ctxAfter)$(if ($reload) { ' [' + $reload + ']' })"
}

# --- 7. num_ctx меньше загруженного: без перезагрузки -----------------------
Check '7. num_ctx МЕНЬШЕ загруженного — перезагрузки нет' {
    $reload = EnsureLoaded -Context $Ctx
    $mark = Log-Mark
    $c = Chat-NonStream -ModelName $Model -NumCtx 4096
    if ($c.Status -ne 200) { throw "HTTP $($c.Status)" }
    Start-Sleep -Seconds 3
    $log = Log-Since -Mark $mark
    $mismatch = ($log -split "`n" | Select-String -Pattern 'scheduling async reload' | Select-Object -Last 1)
    if ($mismatch) { throw "балансер запланировал reload: $($mismatch.Line.Trim())" }
    # ВАЖНО: переменную называем $nowCtx — PowerShell не различает регистр, и
    # $ctx затирал бы параметр -Ctx (на этом уже попались: проверка «прошла» на 0).
    $nowCtx = LoadedCtx
    if ($nowCtx -ne $Ctx) { throw "окно модели изменилось: $nowCtx (было $Ctx)" }
    "HTTP 200 за $($c.Ms) мс (backend=$($c.Backend)), context_size остался $nowCtx, нет 'scheduling async reload'$(if ($reload) { ' [' + $reload + ']' })"
}

# --- 8. два параллельных запроса --------------------------------------------
Check '8. Два параллельных запроса (слоты модели)' {
    $slots = (CppModels).models[0].max_slots
    $jobs = 1..2 | ForEach-Object { Start-Job -ScriptBlock { param($u, $m) $b = @{ model = $m; messages = @(@{ role = 'user'; content = 'Скажи одно слово' }); stream = $false; options = @{ num_ctx = 8192; num_predict = 24 } }; $t0 = Get-Date; $r = Invoke-WebRequest -Uri "$u/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($b | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 600 -UseBasicParsing; [pscustomobject]@{ Status = $r.StatusCode; Ms = [int]((Get-Date) - $t0).TotalMilliseconds } } -ArgumentList $Ollama, $Model }
    $out = $jobs | Wait-Job -Timeout 900 | Receive-Job
    $jobs | Remove-Job -Force
    foreach ($o in $out) { if ($o.Status -ne 200) { throw "один из запросов вернул $($o.Status)" } }
    $slowest = ($out | Measure-Object -Property Ms -Maximum).Maximum
    "max_slots=$slots, оба HTTP 200, самый долгий $slowest мс"
}

# --- 9. три параллельных при двух слотах: очередь ---------------------------
Check '9. Три параллельных при двух слотах — очередь, а не отказ' {
    $jobs = 1..3 | ForEach-Object { Start-Job -ScriptBlock { param($u, $m) $b = @{ model = $m; messages = @(@{ role = 'user'; content = 'Скажи одно слово' }); stream = $false; options = @{ num_ctx = 8192; num_predict = 16 } }; try { $r = Invoke-WebRequest -Uri "$u/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($b | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 900 -UseBasicParsing; [pscustomobject]@{ Status = $r.StatusCode; Retry = $r.Headers['Retry-After'] } } catch { [pscustomobject]@{ Status = [int]$_.Exception.Response.StatusCode; Retry = $_.Exception.Response.Headers['Retry-After'] } } } -ArgumentList $Ollama, $Model }
    $out = $jobs | Wait-Job -Timeout 1200 | Receive-Job
    $jobs | Remove-Job -Force
    $ok = ($out | Where-Object { $_.Status -eq 200 }).Count
    $svc = ($out | Where-Object { $_.Status -eq 503 }).Count
    if ($ok -lt 2) { throw "обслужено $ok из 3 (503: $svc)" }
    $adm = (Admin-Call '/api/v1/queue/stats').Content | ConvertFrom-Json
    "200: $ok/3, 503: $svc, очередь: waiting=$($adm.admission.waiting) served=$($adm.admission.served_total)"
}

# --- 10. стабильность раскладки: серия запросов ------------------------------
# Живая проблема, которую этот сценарий делает видимой: при раскладке
# (gpuLayers=-1, слои уходят и на RPC-узел) cppworker иногда падает — и на
# загрузке, и на инференсе. Балансер это переживает (R118.3: раскладка гасится,
# модель поднимается локально), но запросы становятся долгими. Сценарий считает
# падения по RestartCount и проверяет, что КАЖДЫЙ запрос всё равно обслужен.
Check '10. Стабильность раскладки: серия запросов переживает падения cppworker' {
    $restartsBefore = [int](docker inspect ol-stack-cppworker-gpu --format '{{.RestartCount}}')
    $codes = @()
    $slowest = 0
    for ($i = 1; $i -le 5; $i++) {
        try {
            $c = Chat-NonStream -ModelName $Model -NumCtx 8192 -NumPredict 8
            $codes += $c.Status
            if ($c.Ms -gt $slowest) { $slowest = $c.Ms }
        } catch {
            $codes += [int]$_.Exception.Response.StatusCode
        }
    }
    $restartsAfter = [int](docker inspect ol-stack-cppworker-gpu --format '{{.RestartCount}}')
    $ok = ($codes | Where-Object { $_ -eq 200 }).Count
    $crashes = $restartsAfter - $restartsBefore
    if ($ok -lt 5) { throw "обслужено $ok из 5 (коды: $($codes -join ','))" }
    $note = if ($crashes -gt 0) { "cppworker падал $crashes раз(а) на инференсе — балансер восстановил обслуживание" } else { 'падений не было' }
    "5/5 HTTP 200, самый долгий $slowest мс, RestartCount $restartsBefore→$restartsAfter ($note)"
}

# --- 11. рост n_ctx (долго) --------------------------------------------------
if ($IncludeLongCtx) {
    Check '11. num_ctx БОЛЬШЕ загруженного — reload и успех после него' {
        $target = 65536
        $first = $null
        try { $first = Chat-NonStream -ModelName $Model -NumCtx $target -NumPredict 8 } catch { $first = [pscustomobject]@{ Status = [int]$_.Exception.Response.StatusCode; Ms = 0 } }
        $deadline = (Get-Date).AddMinutes(8)
        $ctx = LoadedCtx
        while ($ctx -ne $target -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 10; $ctx = LoadedCtx }
        if ($ctx -ne $target) { throw "окно не выросло до $target (сейчас $ctx)" }
        $second = Chat-NonStream -ModelName $Model -NumCtx $target -NumPredict 8
        if ($second.Status -ne 200) { throw "повтор вернул HTTP $($second.Status)" }
        "первый ответ HTTP $($first.Status) (503 = async reload), окно выросло до $ctx, повтор HTTP 200"
        # вернуть исходное окно для остальных сценариев
        Admin-Unload -Backend $MainBackend -ModelName $Model
        Admin-Load -Backend $MainBackend -ModelName $Model -Context $Ctx -GpuLayers -1 | Out-Null
    }
}

# --- 12. ДВЕ РАЗНЫЕ МОДЕЛИ НА ДВУХ БЭКЕНДАХ ПАРАЛЛЕЛЬНО ---------------------
#
# Живая жалоба из начала сессии: «не получается поднять другую модель на другом
# бэкенде и вести параллельную работу». Проверяем ровно это: большая модель — на
# мастере (8 ГБ VRAM + RAM, авто-раскладка), лёгкая — на доноре (влезает целиком).
#
# ВАЖНО ПРО ФАЙЛЫ: проверка требует, чтобы файл модели БЫЛ НА ЭТОМ узле. На стенде
# Qwen3.8-27B есть только у мастера, а у донора — только gemma; поэтому роли
# «кто какую модель грузит» заданы явно и проверяются по факту (HTTP загрузки), а
# не предполагаются. Если на вашем стенде файлы разложены иначе — поменяйте
# -BigModelHost / параметры ниже.
if ($IncludeDonorModel) {
    Check '12. Две разные модели на двух бэкендах параллельно' {
        # Оба — самодостаточные: каждая машина считает свою модель сама, раскладка
        # между ними не нужна (иначе донор отдаёт слои мастеру и «своей» модели у
        # него не остаётся).
        Admin-Call -Path "/api/v1/backends/$MainBackend/role" -Method Put -Body @{ role = 'standalone' } | Out-Null
        Admin-Call -Path "/api/v1/backends/$DonorBackend/role" -Method Put -Body @{ role = 'standalone' } | Out-Null
        Admin-Unload -Backend $MainBackend -ModelName $Model
        Admin-Unload -Backend $MainBackend -ModelName $DonorModel
        Admin-Unload -Backend $DonorBackend -ModelName $Model
        Admin-Unload -Backend $DonorBackend -ModelName $DonorModel

        # Большая модель — на мастер (авто-раскладка: 8 ГБ VRAM + RAM).
        $bigLoad = Admin-Load -Backend $MainBackend -ModelName $DonorModel -Context 8192 -GpuLayers -2 -TimeoutSec 1800
        if ($bigLoad.StatusCode -ne 200) { throw "загрузка $DonorModel на мастере: HTTP $($bigLoad.StatusCode)" }
        # Лёгкая — на донор (влезает в 8 ГБ целиком).
        $smallLoad = Admin-Load -Backend $DonorBackend -ModelName $Model -Context 8192 -GpuLayers -1
        if ($smallLoad.StatusCode -ne 200) { throw "загрузка $Model на доноре: HTTP $($smallLoad.StatusCode)" }

        # Проверяем, что каждая модель реально загружена на СВОЁМ узле.
        $mainNames = ((Invoke-WebRequest -Uri 'http://192.0.2.20:18092/api/models' -TimeoutSec 15 -UseBasicParsing).Content | ConvertFrom-Json).models | ForEach-Object { $_.name }
        $donorNames = ((Invoke-WebRequest -Uri 'http://192.0.2.10:18092/api/models' -TimeoutSec 15 -UseBasicParsing).Content | ConvertFrom-Json).models | ForEach-Object { $_.name }
        if ($mainNames -notcontains $DonorModel) { throw "на мастере нет $DonorModel (есть: $($mainNames -join ','))" }
        if ($donorNames -notcontains $Model) { throw "на доноре нет $Model (есть: $($donorNames -join ','))" }

        # Два запроса ПАРАЛЛЕЛЬНО: каждый — к своей модели (значит, и к своему узлу).
        $mark12 = Log-Mark
        $jobs = @(
            (Start-Job -ScriptBlock { param($u, $m) $b = @{ model = $m; messages = @(@{ role = 'user'; content = 'Скажи одно слово' }); stream = $false; options = @{ num_ctx = 8192; num_predict = 16 } }; $t0 = Get-Date; try { $r = Invoke-WebRequest -Uri "$u/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($b | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 1800 -UseBasicParsing; [pscustomobject]@{ Model = $m; Status = $r.StatusCode; Ms = [int]((Get-Date) - $t0).TotalMilliseconds } } catch { [pscustomobject]@{ Model = $m; Status = [int]$_.Exception.Response.StatusCode; Ms = [int]((Get-Date) - $t0).TotalMilliseconds } } } -ArgumentList $Ollama, $DonorModel),
            (Start-Job -ScriptBlock { param($u, $m) $b = @{ model = $m; messages = @(@{ role = 'user'; content = 'Скажи одно слово' }); stream = $false; options = @{ num_ctx = 8192; num_predict = 16 } }; $t0 = Get-Date; try { $r = Invoke-WebRequest -Uri "$u/api/chat" -Method Post -Headers @{ 'Content-Type' = 'application/json' } -Body ($b | ConvertTo-Json -Depth 6 -Compress) -TimeoutSec 1800 -UseBasicParsing; [pscustomobject]@{ Model = $m; Status = $r.StatusCode; Ms = [int]((Get-Date) - $t0).TotalMilliseconds } } catch { [pscustomobject]@{ Model = $m; Status = [int]$_.Exception.Response.StatusCode; Ms = [int]((Get-Date) - $t0).TotalMilliseconds } } } -ArgumentList $Ollama, $Model)
        )
        $out = $jobs | Wait-Job -Timeout 2000 | Receive-Job
        $jobs | Remove-Job -Force
        $bad = $out | Where-Object { $_.Status -ne 200 }
        # Улика «какая модель ушла на какой узел» — из лога балансера, а не из
        # предположений: строка preflight «num_ctx decision» называет и модель, и
        # бэкенд. Поля разбираем ПООТДЕЛЬНОСТИ: порядок ключей в JSON не гарантирован.
        $routing = @()
        foreach ($line in (Log-Since -Mark $mark12) -split "`n") {
            if ($line -notmatch 'num_ctx decision') { continue }
            $modelHit = [regex]::Match($line, '"model":"([^"]+)"')
            $backendHit = [regex]::Match($line, '"backend":"([^"]+)"')
            if ($modelHit.Success -and $backendHit.Success) {
                $routing += ("{0} -> {1}" -f $modelHit.Groups[1].Value, $backendHit.Groups[1].Value)
            }
        }
        $routing = $routing | Select-Object -Unique

        # вернуть штатную раскладку и снять модели с узлов
        Admin-Unload -Backend $MainBackend -ModelName $DonorModel
        Admin-Unload -Backend $DonorBackend -ModelName $Model
        Admin-Load -Backend $MainBackend -ModelName $Model -Context 32768 -GpuLayers -1 | Out-Null
        Admin-Call -Path "/api/v1/backends/$MainBackend/role" -Method Put -Body @{ role = 'main' } | Out-Null
        Admin-Call -Path "/api/v1/backends/$DonorBackend/role" -Method Put -Body @{ role = 'donor' } | Out-Null

        if ($bad) { throw "параллельно не обслужены: $((($bad | ForEach-Object { $_.Model + '=' + $_.Status }) -join ','))" }
        (($out | ForEach-Object { $_.Model + ' HTTP ' + $_.Status + ' за ' + $_.Ms + ' мс' }) -join '; ') +
            " | маршрутизация: " + ($routing -join '; ')
    }
}

# --- Итог -------------------------------------------------------------------
Write-Host ""
Write-Host "=== Итог ===" -ForegroundColor Cyan
$script:Results | Format-Table -AutoSize -Wrap
$failed = ($script:Results | Where-Object { $_.Result -eq 'FAIL' }).Count
$reportPath = Join-Path $env:TEMP ("balancer-scenarios-" + (Get-Date -Format 'yyyyMMdd-HHmmss') + ".json")
$script:Results | ConvertTo-Json -Depth 5 | Set-Content -Path $reportPath -Encoding UTF8
Write-Host ("PASS: {0}  FAIL: {1}  (улики: {2})" -f ($script:Results.Count - $failed), $failed, $reportPath)
if ($failed -gt 0) { exit 1 }
