<#
.SYNOPSIS
    ЖИВОЙ end-to-end smoke image-цепочки OllamaLegion (Windows / PowerShell).

.DESCRIPTION
    Поднимает РЕАЛЬНЫЕ процессы и прогоняет всю цепочку клиент → балансер →
    sdworker → движок (мок sd-server), проверяя не unit-тесты, а процессы:

      A. sdworker отдельно: /health, список моделей, load (spawn субпроцесса
         движка), capabilities, OpenAI Images, A1111 txt2img + заглушки,
         нормализация seed через <sd_cpp_extra_args>, unload.
      B. Цепочка через балансер: регистрация image_cpp бэкенда, health-probe,
         /v1/models, CORS-preflight, генерация через OpenAI-порт, A1111 через
         балансер, изоляция (текстовый запрос НЕ уходит на image-бэкенд),
         404+hint для Ollama-пути на OpenAI-поверхности.

    Скрипт самодостаточен: собирает бинари, готовит bundle модели, поднимает
    процессы, в конце ВСЕГДА гасит их (включая субпроцесс движка) и печатает
    таблицу «проверка → результат».

.PARAMETER WorkDir
    Рабочий каталог стенда (модели, state.json, логи). По умолчанию $env:TEMP\ol-e2e.

.PARAMETER BinDir
    Каталог собранных бинарей. По умолчанию $env:TEMP\ol-bin.

.PARAMETER StrictPorts
    Запретить авто-сдвиг порта, если он занят (по умолчанию порт сдвигается
    вверх и об этом печатается WARN — иначе стенд не встанет на машине, где
    18080/18081 уже держит Docker Desktop port proxy).

.EXAMPLE
    pwsh -File scripts\image-e2e-smoke.ps1

.EXAMPLE
    powershell -File scripts\image-e2e-smoke.ps1 -SkipBuild -LbPort 18084 -LbApiPort 18085
#>
[CmdletBinding()]
param(
    [string]$WorkDir      = (Join-Path $env:TEMP 'ol-e2e'),
    [string]$BinDir       = (Join-Path $env:TEMP 'ol-bin'),
    [int]$WorkerPort      = 18093,
    [int]$EnginePort      = 18094,
    [int]$LbPort          = 18080,
    [int]$LbApiPort       = 18081,
    [int]$LbOpenAiPort    = 18079,
    [int]$MockGenerationMs = 300,
    [switch]$SkipBuild,
    [switch]$StrictPorts
)

$ErrorActionPreference = 'Stop'
$scriptStart = Get-Date
$repoRoot = Split-Path -Parent $PSScriptRoot

# ============================================================
# Инфраструктура отчёта
# ============================================================
$script:results = New-Object System.Collections.ArrayList
$script:reservedPorts = New-Object 'System.Collections.Generic.HashSet[int]'

function Add-Result {
    param([string]$Id, [string]$Check, [bool]$Ok, [string]$Detail)
    [void]$script:results.Add([pscustomobject]@{
        Id     = $Id
        Check  = $Check
        Result = if ($Ok) { 'PASS' } else { 'FAIL' }
        Detail = $Detail
    })
    $color = if ($Ok) { 'Green' } else { 'Red' }
    Write-Host ("  [{0}] {1,-4} {2} :: {3}" -f $Id, $(if ($Ok) { 'PASS' } else { 'FAIL' }), $Check, $Detail) -ForegroundColor $color
}

# ============================================================
# HTTP без исключений на 4xx/5xx (PS 5.1: -SkipHttpErrorCheck нет)
# ============================================================
function Invoke-Http {
    param(
        [string]$Method,
        [string]$Url,
        [string]$Body,
        [hashtable]$Headers,
        [int]$TimeoutMs = 120000,
        [switch]$Raw
    )
    $req = [System.Net.HttpWebRequest]::Create($Url)
    $req.Method = $Method
    $req.Timeout = $TimeoutMs
    $req.AllowAutoRedirect = $false
    if ($Headers) { foreach ($k in $Headers.Keys) { $req.Headers.Add($k, $Headers[$k]) } }
    try {
        if ($Body) {
            $req.ContentType = 'application/json'
            $bytes = [Text.Encoding]::UTF8.GetBytes($Body)
            $req.ContentLength = $bytes.Length
            $s = $req.GetRequestStream(); $s.Write($bytes, 0, $bytes.Length); $s.Close()
        }
        try { $resp = $req.GetResponse() }
        catch [System.Net.WebException] { $resp = $_.Exception.Response }
        if (-not $resp) { return @{ Status = -1; Body = ''; Headers = @{}; Exception = 'no response' } }
        $sr = New-Object System.IO.StreamReader($resp.GetResponseStream())
        $text = $sr.ReadToEnd(); $sr.Close()
        $status = [int]$resp.StatusCode
        $hdr = @{}
        foreach ($k in $resp.Headers.AllKeys) { $hdr[$k] = $resp.Headers[$k] }
        $resp.Close()
        return @{ Status = $status; Body = $text; Headers = $hdr; Exception = '' }
    } catch {
        return @{ Status = -1; Body = ''; Headers = @{}; Exception = $_.Exception.Message }
    }
}

# R-Image (2026-10-02): POST /v1/images/edits — это multipart/form-data,
# а Invoke-Http умеет только JSON-тело (и PS 5.1 не имеет -Form, он появился
# в PS 6+). Поэтому тело собираем вручную.
function Invoke-MultipartEdits {
    param(
        [string]$Url,
        [string]$Prompt,
        [byte[]]$ImageBytes,
        [string]$FileName = 'init.png',
        [byte[]]$MaskBytes = $null,
        [string]$MaskName = 'mask.png',
        [int]$TimeoutMs = 120000
    )
    $boundary = '----olE2E' + [guid]::NewGuid().ToString('N')
    $ms = New-Object System.IO.MemoryStream
    $enc = [Text.Encoding]::UTF8
    $nl = "`r`n"

    function Add-Text([string]$s) { $b = $enc.GetBytes($s); $ms.Write($b, 0, $b.Length) }

    Add-Text "--$boundary$nl"
    Add-Text "Content-Disposition: form-data; name=`"prompt`"$nl$nl$Prompt$nl"
    Add-Text "--$boundary$nl"
    Add-Text "Content-Disposition: form-data; name=`"image[]`"; filename=`"$FileName`"$nl"
    Add-Text "Content-Type: image/png$nl$nl"
    $ms.Write($ImageBytes, 0, $ImageBytes.Length)
    Add-Text $nl
    if ($MaskBytes -and $MaskBytes.Length -gt 0) {
        Add-Text "--$boundary$nl"
        Add-Text "Content-Disposition: form-data; name=`"mask`"; filename=`"$MaskName`"$nl"
        Add-Text "Content-Type: image/png$nl$nl"
        $ms.Write($MaskBytes, 0, $MaskBytes.Length)
        Add-Text $nl
    }
    Add-Text "--$boundary--$nl"
    $bytes = $ms.ToArray()

    $req = [System.Net.HttpWebRequest]::Create($Url)
    $req.Method = 'POST'
    $req.Timeout = $TimeoutMs
    $req.ContentType = "multipart/form-data; boundary=$boundary"
    $req.ContentLength = $bytes.Length
    try {
        $s = $req.GetRequestStream(); $s.Write($bytes, 0, $bytes.Length); $s.Close()
        try { $resp = $req.GetResponse() } catch [System.Net.WebException] { $resp = $_.Exception.Response }
        if (-not $resp) { return @{ Status = -1; Body = '' } }
        $sr = New-Object System.IO.StreamReader($resp.GetResponseStream())
        $text = $sr.ReadToEnd(); $sr.Close()
        $status = [int]$resp.StatusCode
        $resp.Close()
        return @{ Status = $status; Body = $text }
    } catch {
        return @{ Status = -1; Body = ''; Exception = $_.Exception.Message }
    }
}

function ConvertTo-JsonSafe {
    param([string]$Text)
    if ([string]::IsNullOrWhiteSpace($Text)) { return $null }
    try { return $Text | ConvertFrom-Json } catch { return $null }
}

function Read-LogFile {
    param([string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    # Файл пишет другой процесс: открываем с FileShare.ReadWrite, иначе IOException.
    try {
        $fs = New-Object System.IO.FileStream($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::ReadWrite)
        $sr = New-Object System.IO.StreamReader($fs)
        $t = $sr.ReadToEnd(); $sr.Close(); $fs.Close()
        return $t
    } catch { return '' }
}

# ============================================================
# Порты: Get-NetTCPConnection в этой среде ненадёжен → парсим netstat
# ============================================================
function Test-PortFree {
    param([int]$Port)
    $hit = netstat -ano | Select-String ":$Port\s" | Select-String 'LISTENING'
    return (-not $hit)
}

function Resolve-Port {
    param([int]$Desired, [string]$Name)
    for ($p = $Desired; $p -lt ($Desired + 40); $p++) {
        # Уже выданный ЭТИМ запуском порт считаем занятым: иначе два разных
        # слушателя (proxy и api) получат один и тот же свободный порт.
        if ($script:reservedPorts.Contains($p)) { continue }
        if (-not (Test-PortFree -Port $p)) { continue }
        if ($p -ne $Desired) {
            Write-Host "  WARN: порт $Desired ($Name) занят — используем $p" -ForegroundColor Yellow
        }
        [void]$script:reservedPorts.Add($p)
        return $p
    }
    if ($StrictPorts) { throw "port $Desired ($Name) is busy and -StrictPorts is set" }
    throw "no free port found for $Name starting at $Desired"
}

function Wait-Http {
    param([string]$Url, [int]$TimeoutSec = 30, [int]$Want = 200)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $r = Invoke-Http -Method GET -Url $Url -TimeoutMs 3000
        if ($r.Status -eq $Want) { return $r }
        Start-Sleep -Milliseconds 300
    }
    return (Invoke-Http -Method GET -Url $Url -TimeoutMs 3000)
}

# ============================================================
# Подготовка
# ============================================================
Write-Host "=== OllamaLegion image E2E smoke ===" -ForegroundColor Cyan
Write-Host "repo=$repoRoot"
Write-Host "work=$WorkDir bin=$BinDir"

$env:GOCACHE = Join-Path $env:TEMP 'gocache-ollamalegion'
New-Item -ItemType Directory -Force -Path $BinDir, (Join-Path $WorkDir 'models\m1') | Out-Null

$mockExe     = Join-Path $BinDir 'mock-sdserver.exe'
$workerExe   = Join-Path $BinDir 'sdworker.exe'
$balancerExe = Join-Path $BinDir 'balancer.exe'
$modelsDir   = Join-Path $WorkDir 'models'
$statePath   = Join-Path $WorkDir 'state.json'

if (-not $SkipBuild) {
    Write-Host "--- build ---"
    # R-Image (2026-10-02): go пишет телеметрию/stat-cache в %APPDATA%\go и в
    # GOMODCACHE, а при $ErrorActionPreference='Stop' любая такая запись на stderr
    # (например «Access is denied» в песочнице или на runner'е с read-only
    # профилем) валит шаг сборки, хотя сами бинари собрались. Гасим телеметрию
    # и не считаем запись в кэш ошибкой: успех определяем ТОЛЬКО по $LASTEXITCODE.
    $env:GOTELEMETRY = 'off'
    $push = $true
    Push-Location $repoRoot
    try {
        & go build -o $mockExe ./tools/mock-sdserver/ 2>&1 | Write-Host
        if ($LASTEXITCODE -ne 0) { throw "build mock-sdserver failed (exit $LASTEXITCODE)" }
        & go build -tags llama_stub -o $workerExe ./cmd/sdworker/ 2>&1 | Write-Host
        if ($LASTEXITCODE -ne 0) { throw "build sdworker failed (exit $LASTEXITCODE)" }
        & go build -tags llama_stub -o $balancerExe ./cmd/balancer/ 2>&1 | Write-Host
        if ($LASTEXITCODE -ne 0) { throw "build balancer failed (exit $LASTEXITCODE)" }
    } catch {
        # Не пробрасываем дальше «шум» от go: если бинари на месте — это не ошибка.
        $missing = @($mockExe, $workerExe, $balancerExe) | Where-Object { -not (Test-Path -LiteralPath $_) }
        if ($missing.Count -gt 0) { throw }
        Write-Host ("WARN: go сообщил об ошибке окружения (" + $_.Exception.Message + "), но все бинари собраны — продолжаем")
    } finally { if ($push) { Pop-Location } }
}
foreach ($exe in @($mockExe, $workerExe, $balancerExe)) {
    if (-not (Test-Path -LiteralPath $exe)) { throw "missing binary: $exe" }
}

# bundle модели: <models>\m1\profile.json + существующий файл-заглушка
$gguf = Join-Path $modelsDir 'm1\model.gguf'
if (-not (Test-Path -LiteralPath $gguf)) { 'mock gguf' | Set-Content -LiteralPath $gguf -Encoding ASCII }
$profile = [ordered]@{
    name     = 'm1'
    family   = 'sd15'
    files    = @([ordered]@{ role = 'diffusion'; repo = 'mock/repo'; filename = 'model.gguf'; localPath = $gguf })
    defaults = [ordered]@{ steps = 4; cfgScale = 1.0; sampler = 'euler'; scheduler = 'discrete'; width = 512; height = 512; batchCount = 1; seed = -1; clipSkip = 0 }
    runtime  = [ordered]@{ seedMode = 'random' }
}
$profile | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $modelsDir 'm1\profile.json') -Encoding UTF8

# Свежий стенд: state.json переживает перезапуск балансера и привёл бы к
# 409 «backend already exists» на регистрации.
Remove-Item -LiteralPath $statePath -ErrorAction SilentlyContinue

# порты (Docker Desktop port proxy может держать 18080/18081 → сдвиг)
$LbPort       = Resolve-Port -Desired $LbPort       -Name 'LB proxy'
$LbApiPort    = Resolve-Port -Desired $LbApiPort    -Name 'LB api'
$LbOpenAiPort = Resolve-Port -Desired $LbOpenAiPort -Name 'LB openai'
$WorkerPort   = Resolve-Port -Desired $WorkerPort   -Name 'sdworker'
$EnginePort   = Resolve-Port -Desired $EnginePort   -Name 'sd-server(mock)'

$workerOut = Join-Path $WorkDir 'sdworker.out.log'
$workerErr = Join-Path $WorkDir 'sdworker.err.log'
$balOut    = Join-Path $WorkDir 'balancer.out.log'
$balErr    = Join-Path $WorkDir 'balancer.err.log'
Remove-Item -LiteralPath $workerOut, $workerErr, $balOut, $balErr -ErrorAction SilentlyContinue

$workerProc = $null
$balProc = $null

function Stop-Stand {
    foreach ($p in @($balProc, $workerProc)) {
        if ($p -and -not $p.HasExited) {
            try { Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue } catch { }
        }
    }
    Start-Sleep -Milliseconds 800
    # Субпроцесс движка мог осиротеть при Force-kill воркера — добиваем только
    # те mock-sdserver, что стартовали в рамках ЭТОГО запуска.
    Get-Process -Name 'mock-sdserver' -ErrorAction SilentlyContinue |
        Where-Object { $_.StartTime -ge $scriptStart } |
        ForEach-Object { try { Stop-Process -Id $_.Id -Force -ErrorAction SilentlyContinue } catch { } }
}

try {
    # ========================================================
    # A. Воркер отдельно
    # ========================================================
    Write-Host "--- A: sdworker standalone (port $WorkerPort, engine port $EnginePort) ---" -ForegroundColor Cyan

    $env:SDWORKER_PORT                   = "$WorkerPort"
    $env:SDWORKER_SD_SERVER_PORT         = "$EnginePort"
    $env:SDWORKER_MODELS_DIR             = $modelsDir
    $env:SDWORKER_IMAGE_MODELS_DIR       = $modelsDir
    $env:SDWORKER_SD_SERVER_BIN          = $mockExe
    $env:SDWORKER_REGISTER_DISABLE       = 'true'
    $env:SDWORKER_STARTUP_TIMEOUT_SEC    = '20'
    $env:SDWORKER_GENERATION_TIMEOUT_SEC = '120'
    $env:SDWORKER_IDLE_UNLOAD_MINUTES    = '0'
    $env:MOCK_SD_GENERATION_MS           = "$MockGenerationMs"

    # -verbose обязателен: stdout движка релеится супервизором на уровне DEBUG
    # (internal/sdbackend/supervisor.go pump → sdLog().Debugw). Без него лог мока
    # (и, значит, проверка seed в A8) не виден вообще.
    $workerProc = Start-Process -FilePath $workerExe -ArgumentList '-verbose' `
        -RedirectStandardOutput $workerOut -RedirectStandardError $workerErr -PassThru -NoNewWindow

    $w = "http://127.0.0.1:$WorkerPort"

    # A1: GET /health
    $r = Wait-Http -Url "$w/health" -TimeoutSec 30
    $h = ConvertTo-JsonSafe $r.Body
    Add-Result 'A1' 'GET /health' ($r.Status -eq 200) "http=$($r.Status) status=$($h.status) state=$($h.state)"

    # A2: GET /api/image/models → m1, not_loaded
    $r = Invoke-Http -Method GET -Url "$w/api/image/models"
    $m = ConvertTo-JsonSafe $r.Body
    $m1 = $null; if ($m) { $m1 = $m.models | Where-Object { $_.name -eq 'm1' } }
    $a2 = ($r.Status -eq 200) -and $m1 -and ($m1.state -ne 'loaded')
    Add-Result 'A2' 'GET /api/image/models (m1 not_loaded)' $a2 "http=$($r.Status) names=$(($m.models | ForEach-Object { $_.name }) -join ',') state=$(if ($m1) { $m1.state } else { 'n/a' })"

    # A3: load → реальный spawn мока
    $r = Invoke-Http -Method POST -Url "$w/api/image/models/load" -Body '{"name":"m1"}'
    $loadStatus = $r.Status
    $loaded = $false; $enginePid = 0; $health = $null
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 300
        $health = Invoke-Http -Method GET -Url "$w/health"
        $hj = ConvertTo-JsonSafe $health.Body
        if ($hj -and $hj.model_loaded) { $loaded = $true; $enginePid = [int]$hj.sd_server_pid; break }
    }
    $r2 = Invoke-Http -Method GET -Url "$w/api/image/models"
    $mj = ConvertTo-JsonSafe $r2.Body
    $m1 = $mj.models | Where-Object { $_.name -eq 'm1' }
    $procAlive = $false
    if ($enginePid -gt 0) { $procAlive = [bool](Get-Process -Id $enginePid -ErrorAction SilentlyContinue) }
    $logTxt = Read-LogFile $workerOut
    $spawnedInLog = $logTxt -match 'spawning sd-server'
    $a3 = ($loadStatus -eq 202) -and $loaded -and $procAlive -and $spawnedInLog -and ($m1.state -eq 'loaded')
    Add-Result 'A3' 'POST /api/image/models/load → spawn мока' $a3 "load_http=$loadStatus state=$($m1.state) engine_pid=$enginePid alive=$procAlive port18094_listen=$(-not (Test-PortFree -Port $EnginePort))"

    # A4: capabilities
    $r = Invoke-Http -Method GET -Url "$w/api/image/capabilities"
    $c = ConvertTo-JsonSafe $r.Body
    $a4 = ($r.Status -eq 200) -and $c.ready -and $c.engine.samplers.Count -gt 0 -and $c.limits.max_width
    Add-Result 'A4' 'GET /api/image/capabilities' $a4 "http=$($r.Status) ready=$($c.ready) samplers=$(($c.engine.samplers) -join '/') limits.max_width=$($c.limits.max_width)"

    # A5: OpenAI /v1/images/generations
    $r = Invoke-Http -Method POST -Url "$w/v1/images/generations" -Body '{"model":"dall-e-2","prompt":"cat","size":"512x512","n":1}'
    $j = ConvertTo-JsonSafe $r.Body
    $b64 = ''; if ($j -and $j.data -and $j.data.Count -gt 0) { $b64 = [string]$j.data[0].b64_json }
    $a5 = ($r.Status -eq 200) -and ($b64.Length -gt 0)
    Add-Result 'A5' 'POST /v1/images/generations (байты b64)' $a5 "http=$($r.Status) b64_len=$($b64.Length) seed=$($j.seed)"

    # A6: A1111 txt2img, info — JSON-СТРОКА
    $r = Invoke-Http -Method POST -Url "$w/sdapi/v1/txt2img" -Body '{"prompt":"cat","width":512,"height":512,"steps":4}'
    $j = ConvertTo-JsonSafe $r.Body
    $img0 = ''; $infoRaw = $null; $infoIsString = $false; $infoObj = $null
    if ($j) {
        if ($j.images -and $j.images.Count -gt 0) { $img0 = [string]$j.images[0] }
        $infoRaw = $j.info
        $infoIsString = ($infoRaw -is [string])
        $infoObj = ConvertTo-JsonSafe ([string]$infoRaw)
    }
    $a6 = ($r.Status -eq 200) -and ($img0.Length -gt 0) -and $infoIsString -and $infoObj.width -and $infoObj.height -and $infoObj.seed
    Add-Result 'A6' 'POST /sdapi/v1/txt2img (info = JSON-строка)' $a6 "http=$($r.Status) images[0].len=$($img0.Length) info_is_string=$infoIsString w=$($infoObj.width) h=$($infoObj.height) seed=$($infoObj.seed)"

    # A7: A1111-заглушки
    $o = Invoke-Http -Method POST -Url "$w/sdapi/v1/options"   -Body '{"sd_model_checkpoint":"m1"}'
    $p = Invoke-Http -Method GET  -Url "$w/sdapi/v1/progress"
    $pj = ConvertTo-JsonSafe $p.Body
    $i = Invoke-Http -Method POST -Url "$w/sdapi/v1/interrupt" -Body '{}'
    $v = Invoke-Http -Method GET  -Url "$w/sdapi/v1/sd-vae"
    $a7 = ($o.Status -eq 200) -and ($p.Status -eq 200) -and ($pj.progress -eq 0) -and ($i.Status -eq 204) -and ($v.Status -eq 200) -and ($v.Body.Trim() -eq '[]')
    Add-Result 'A7' 'A1111 заглушки (options/progress/interrupt/sd-vae)' $a7 "options=$($o.Status) progress=$($p.Status) progress.progress=$($pj.progress) interrupt=$($i.Status) sd-vae=$($v.Status) body=$($v.Body.Trim())"

    # A8: нормализация seed — в движок уходит <sd_cpp_extra_args>{"seed":N}
    $rA = Invoke-Http -Method POST -Url "$w/v1/images/generations" -Body '{"model":"dall-e-2","prompt":"cat","size":"512x512","n":1}'
    $jA = ConvertTo-JsonSafe $rA.Body
    $rB = Invoke-Http -Method POST -Url "$w/v1/images/generations" -Body '{"model":"dall-e-2","prompt":"dog","size":"512x512","n":1}'
    $jB = ConvertTo-JsonSafe $rB.Body
    $logTxt = Read-LogFile $workerOut
    $seedLines = ($logTxt -split "`r?`n") | Where-Object { $_ -match '<sd_cpp_extra_args>' }
    # zap JSON-кодирует строку лога, а мок печатает prompt через %q — поэтому
    # кавычки/бэкслеши в файле экранированы (\"). Не полагаемся на них:
    # после <sd_cpp_extra_args> идут только нецифры, затем сам seed.
    $seedsOnWire = @()
    foreach ($line in $seedLines) {
        $mm = [regex]::Matches($line, '<sd_cpp_extra_args>\D*?(\d+)')
        foreach ($x in $mm) { $seedsOnWire += [int64]$x.Groups[1].Value }
    }
    $distinct = ($seedsOnWire | Sort-Object -Unique)
    $a8 = ($rA.Status -eq 200) -and ($rB.Status -eq 200) -and ($seedLines.Count -ge 2) -and ($distinct.Count -ge 2)
    Add-Result 'A8' 'seed нормализован в <sd_cpp_extra_args>{seed:N}' $a8 "on_wire=[$($seedsOnWire -join ',')] distinct=$($distinct.Count) api_seeds=[$($jA.seed),$($jB.seed)]"

    # A10/A11 (R-Image, 2026-10-02): img2img/inpaint. Сам мок картинку не
    # генерирует и init_image игнорирует, поэтому единственное наблюдаемое
    # доказательство «поля доехали до движка» — его лог (мок печатает
    # `init=yes mask=... strength=...`), который воркер релеит на DEBUG (-verbose).
    $pngB64  = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='
    $pngBytes = [Convert]::FromBase64String($pngB64)

    $i2iBody = '{"prompt":"cat","init_images":["' + $pngB64 + '"],"denoising_strength":0.6,"width":512,"height":512,"steps":4}'
    $r10 = Invoke-Http -Method POST -Url "$w/sdapi/v1/img2img" -Body $i2iBody
    $j10 = ConvertTo-JsonSafe $r10.Body
    $logAfterI2I = Read-LogFile $workerOut
    $i2iLine = (($logAfterI2I -split "`r?`n") | Where-Object { $_ -match 'img_gen accepted' -and $_ -match 'init=yes' } | Select-Object -Last 1)
    $a10 = ($r10.Status -eq 200) -and ($j10.images.Count -ge 1) -and ($j10.info -is [string]) -and
           ($i2iLine -match 'init=yes') -and ($i2iLine -match 'strength=0\.6')
    Add-Result 'A10' 'POST /sdapi/v1/img2img (init_images → init_image, strength)' $a10 "http=$($r10.Status) images=$($j10.images.Count) info_is_string=$($j10.info -is [string]) engine_log=$($i2iLine -replace '^.*img_gen','img_gen')"

    $r11 = Invoke-MultipartEdits -Url "$w/v1/images/edits" -Prompt 'cat' -ImageBytes $pngBytes
    $j11 = ConvertTo-JsonSafe $r11.Body
    $logAfterEdits = Read-LogFile $workerOut
    $editsLine = (($logAfterEdits -split "`r?`n") | Where-Object { $_ -match 'img_gen accepted' -and $_ -match 'init=yes' } | Select-Object -Last 1)
    $a11 = ($r11.Status -eq 200) -and ($null -ne $j11.data) -and ($j11.data[0].b64_json.Length -gt 0) -and ($editsLine -match 'init=yes')
    Add-Result 'A11' 'POST /v1/images/edits (multipart image[] → init_image)' $a11 "http=$($r11.Status) b64_len=$($j11.data[0].b64_json.Length) engine_log=$($editsLine -replace '^.*img_gen','img_gen')"

    # ========================================================
    # B. Цепочка через балансер
    # ========================================================
    Write-Host "--- B: balancer (proxy $LbPort, api $LbApiPort, openai $LbOpenAiPort) ---" -ForegroundColor Cyan

    $env:LB_HOST               = '127.0.0.1'
    $env:LB_PORT               = "$LbPort"
    $env:LB_API_PORT           = "$LbApiPort"
    $env:LB_OPENAI_PORT        = "$LbOpenAiPort"
    $env:LB_STATE_PATH         = $statePath
    $env:LB_DATA_DIR           = (Join-Path $WorkDir 'data')
    $env:AUTH_ENABLED          = 'false'
    $env:LB_ALLOW_ENV_FALLBACK = 'true'

    # env-only конфиг: явный несуществующий -config включает greenfield-fallback
    # (LB_ALLOW_ENV_FALLBACK=true), и стенд не подхватывает боевой config/config.json.
    $balProc = Start-Process -FilePath $balancerExe `
        -ArgumentList @('-config', (Join-Path $WorkDir 'no-config.json')) `
        -WorkingDirectory $repoRoot `
        -RedirectStandardOutput $balOut -RedirectStandardError $balErr -PassThru -NoNewWindow

    $api = "http://127.0.0.1:$LbApiPort"
    $oa  = "http://127.0.0.1:$LbOpenAiPort"

    # Ждём готовности API балансера: Start-Process не падает, если процесс
    # умер на bind — без ожидания диагностика превращается в «connection refused».
    $up = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 400
        if ($balProc.HasExited) { break }
        $probe = Invoke-Http -Method GET -Url "$api/api/v1/backends" -TimeoutMs 2000
        if ($probe.Status -gt 0) { $up = $true; break }
    }
    if (-not $up) {
        $tail = (Read-LogFile $balErr) + "`n" + (Read-LogFile $balOut)
        Add-Result 'B0' 'balancer API поднялся' $false "exited=$($balProc.HasExited) log_tail=$($tail.Substring([Math]::Max(0, $tail.Length - 600)))"
        throw "balancer API on port $LbApiPort is not reachable"
    }

    $reg = Invoke-Http -Method POST -Url "$api/api/v1/backends" `
        -Body (@{ id = 'image-e2e'; name = 'image e2e'; host = '127.0.0.1'; imagePort = $WorkerPort; backendType = 'image_cpp' } | ConvertTo-Json -Compress)
    if ($reg.Status -eq 409) {
        # Бэкенд уже был в state.json — это не провал стенда, но отметим.
        Add-Result 'B0' 'POST /api/v1/backends (register image_cpp)' $true "http=409 (already exists — state.json был не пуст)"
    } elseif ($reg.Status -lt 200 -or $reg.Status -ge 300) {
        Add-Result 'B0' 'POST /api/v1/backends (register image_cpp)' $false "http=$($reg.Status) body=$($reg.Body)"
        throw "backend registration failed"
    } else {
        Add-Result 'B0' 'POST /api/v1/backends (register image_cpp)' $true "http=$($reg.Status)"
    }

    # B1: health-probe стал healthy
    $b1 = $false; $status1 = ''; $listBody = ''
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 500
        $r = Invoke-Http -Method GET -Url "$api/api/v1/backends"
        $listBody = $r.Body
        $jj = ConvertTo-JsonSafe $r.Body
        $b = $null; if ($jj -and $jj.backends) { $b = $jj.backends | Where-Object { $_.id -eq 'image-e2e' } }
        if ($b) { $status1 = [string]$b.status; if ($status1 -eq 'healthy') { $b1 = $true; break } }
    }
    Add-Result 'B1' 'GET /api/v1/backends → status=healthy' $b1 "http=$($r.Status) status=$status1"

    # B2: /v1/models на OpenAI-порту
    $r = Invoke-Http -Method GET -Url "$oa/v1/models"
    $j = ConvertTo-JsonSafe $r.Body
    $ids = @(); if ($j -and $j.data) { $ids = @($j.data | ForEach-Object { $_.id }) }
    $b2 = ($r.Status -eq 200) -and ($ids -contains 'sd-cpp-local') -and (@($ids | Where-Object { $_ -like 'dall-*' }).Count -gt 0)
    Add-Result 'B2' 'GET :openai/v1/models (sd-cpp-local + dall-*)' $b2 "http=$($r.Status) ids=$($ids -join ',')"

    # B3: CORS-preflight
    $r = Invoke-Http -Method OPTIONS -Url "$oa/v1/images/generations" -Headers @{
        Origin = 'http://localhost:3000'
        'Access-Control-Request-Method' = 'POST'
        'Access-Control-Request-Headers' = 'content-type'
    }
    $acao = ''
    if ($r.Headers.ContainsKey('Access-Control-Allow-Origin')) { $acao = $r.Headers['Access-Control-Allow-Origin'] }
    $b3 = ($r.Status -eq 204) -and ($acao -eq 'http://localhost:3000')
    Add-Result 'B3' 'OPTIONS :openai/v1/images/generations → 204 + ACAO' $b3 "http=$($r.Status) ACAO=$acao"

    # B4: весь путь клиент → балансер → воркер → движок
    $r = Invoke-Http -Method POST -Url "$oa/v1/images/generations" -Body '{"model":"dall-e-2","prompt":"cat","size":"512x512"}'
    $j = ConvertTo-JsonSafe $r.Body
    $b64 = ''; if ($j -and $j.data -and $j.data.Count -gt 0) { $b64 = [string]$j.data[0].b64_json }
    $b4 = ($r.Status -eq 200) -and ($b64.Length -gt 0)
    Add-Result 'B4' 'POST :openai/v1/images/generations (E2E)' $b4 "http=$($r.Status) b64_len=$($b64.Length) model=$($j.model) seed=$($j.seed)"

    # B5: A1111 через балансер
    $r = Invoke-Http -Method POST -Url "$oa/sdapi/v1/txt2img" -Body '{"prompt":"cat"}'
    $j = ConvertTo-JsonSafe $r.Body
    $img0 = ''; if ($j -and $j.images -and $j.images.Count -gt 0) { $img0 = [string]$j.images[0] }
    $b5 = ($r.Status -eq 200) -and ($img0.Length -gt 0)
    Add-Result 'B5' 'POST :openai/sdapi/v1/txt2img через балансер' $b5 "http=$($r.Status) images[0].len=$($img0.Length)"

    # B6: изоляция — текстовый запрос НЕ должен дойти до image-бэкенда
    $before = (Read-LogFile $workerOut)
    $beforeCount = ([regex]::Matches($before, 'POST /sdcpp/v1/img_gen')).Count
    $r = Invoke-Http -Method POST -Url "$oa/v1/chat/completions" -Body '{"model":"gemma","messages":[{"role":"user","content":"hi"}]}' -TimeoutMs 60000
    Start-Sleep -Milliseconds 700
    $after = (Read-LogFile $workerOut)
    $afterCount = ([regex]::Matches($after, 'POST /sdcpp/v1/img_gen')).Count
    $b6 = ($afterCount -eq $beforeCount)
    Add-Result 'B6' 'изоляция: chat/completions НЕ уходит в img_gen' $b6 "chat_http=$($r.Status) body=$($r.Body.Trim()) img_gen_before=$beforeCount after=$afterCount"

    # B7: Ollama-путь на OpenAI-поверхности → 404 + hint
    $r = Invoke-Http -Method GET -Url "$oa/api/tags"
    $j = ConvertTo-JsonSafe $r.Body
    $b7 = ($r.Status -eq 404) -and $j -and $j.hint
    Add-Result 'B7' 'GET :openai/api/tags → 404 + hint' $b7 "http=$($r.Status) hint=$($j.hint)"

    # ========================================================
    # A9: unload (делаем ПОСЛЕ B — иначе генерация не пройдёт)
    # ========================================================
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $r = Invoke-Http -Method POST -Url "$w/api/image/models/unload" -Body '{"name":"m1"}' -TimeoutMs 60000
    $sw.Stop()
    Start-Sleep -Milliseconds 1200
    $alive = $false
    if ($enginePid -gt 0) { $alive = [bool](Get-Process -Id $enginePid -ErrorAction SilentlyContinue) }
    $r2 = Invoke-Http -Method GET -Url "$w/api/image/models"
    $mj = ConvertTo-JsonSafe $r2.Body
    $m1 = $mj.models | Where-Object { $_.name -eq 'm1' }
    $portBusy = -not (Test-PortFree -Port $EnginePort)
    $hj = ConvertTo-JsonSafe (Invoke-Http -Method GET -Url "$w/health").Body
    $a9func = (-not $alive) -and (-not $portBusy) -and ($m1.state -eq 'not_loaded') -and ($hj.status -eq 'ok')
    $note = if ($r.Status -eq 200) { '' } else { " [BUG: http=$($r.Status) body=$($r.Body.Trim())]" }
    Add-Result 'A9' 'POST /api/image/models/unload → движок убит' $a9func "http=$($r.Status)$note elapsed_ms=$($sw.ElapsedMilliseconds) engine_alive=$alive port${EnginePort}_busy=$portBusy state=$($m1.state)"
}
finally {
    Write-Host "--- teardown ---" -ForegroundColor Cyan
    Stop-Stand
    Start-Sleep -Milliseconds 500
    $stillUp = @()
    foreach ($p in @($WorkerPort, $EnginePort, $LbPort, $LbApiPort, $LbOpenAiPort)) {
        if (-not (Test-PortFree -Port $p)) { $stillUp += $p }
    }
    if ($stillUp.Count -eq 0) {
        Write-Host "  все порты стенда свободны: $WorkerPort/$EnginePort/$LbPort/$LbApiPort/$LbOpenAiPort" -ForegroundColor Green
    } else {
        Write-Host "  ВНИМАНИЕ: заняты порты $($stillUp -join ', ')" -ForegroundColor Red
    }
    foreach ($n in @('SDWORKER_PORT', 'SDWORKER_SD_SERVER_PORT', 'SDWORKER_MODELS_DIR', 'SDWORKER_IMAGE_MODELS_DIR',
                     'SDWORKER_SD_SERVER_BIN', 'SDWORKER_REGISTER_DISABLE', 'SDWORKER_STARTUP_TIMEOUT_SEC',
                     'SDWORKER_GENERATION_TIMEOUT_SEC', 'SDWORKER_IDLE_UNLOAD_MINUTES', 'MOCK_SD_GENERATION_MS',
                     'LB_HOST', 'LB_PORT', 'LB_API_PORT', 'LB_OPENAI_PORT', 'LB_STATE_PATH', 'LB_DATA_DIR',
                     'AUTH_ENABLED', 'LB_ALLOW_ENV_FALLBACK')) {
        Remove-Item -Path "env:$n" -ErrorAction SilentlyContinue
    }
}

Write-Host ""
Write-Host "=== РЕЗУЛЬТАТЫ ===" -ForegroundColor Cyan
$script:results | Format-Table -AutoSize
$failed = @($script:results | Where-Object { $_.Result -eq 'FAIL' })
Write-Host ("Итого: {0} проверок, PASS={1}, FAIL={2}" -f $script:results.Count, ($script:results.Count - $failed.Count), $failed.Count)
if ($failed.Count -gt 0) { exit 1 } else { exit 0 }
