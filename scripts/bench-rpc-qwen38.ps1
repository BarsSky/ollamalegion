#!/usr/bin/env pwsh
# scripts/bench-rpc-qwen38.ps1 — R231 (2026-10-24): сравнение «локально vs RPC-донор»
# на большой модели текущего стенда (Qwen3.8-27B-UD-Q4_K_M, 15.3 GiB).
#
# ЗАЧЕМ. Вопрос оператора: «есть ли выигрыш от раскладки на донора по RPC?».
# Ответ нельзя брать из теории: донор добавляет VRAM (здесь +8 ГБ), но каждый
# токен гоняет активации по сети, поэтому итог зависит от канала и раскладки.
# Скрипт измеряет ФАКТ на одном и том же промпте, одной модели и одинаковых
# параметрах загрузки в двух режимах:
#   off — раскладка выключена (только локальная карта, часть слоёв в RAM);
#   on  — раскладка на донора (RPC, порт 50052).
#
# Что попадает в сравнение:
#   - время загрузки модели (сек);
#   - раскладка слоёв (GPU / CPU / RPC) и свободная VRAM после загрузки;
#   - скорость генерации (eval tokens/s) и prefill (prompt tokens/s);
#   - полное время ответа на фиксированный запрос.
#
# Запуск:
#   $env:LB_ADMIN_USER='...'; $env:LB_ADMIN_PASSWORD='...'
#   pwsh -File scripts/bench-rpc-qwen38.ps1                 # оба режима, 3 замера
#   pwsh -File scripts/bench-rpc-qwen38.ps1 -Mode on -Runs 2
#   pwsh -File scripts/bench-rpc-qwen38.ps1 -ContextSize 32768 -KvCacheType q4_0
#
# ВАЖНО: скрипт реально выгружает и загружает модель (минуты на 15-GB модель)
# и в конце возвращает режим раскладки в "auto".
[CmdletBinding()]
param(
    [string]$Api = "http://127.0.0.1:18081",
    [string]$Worker = "http://127.0.0.1:18092",
    [string]$Backend = "cppworker-gpu-bundled-agent",
    [string]$Model = "Qwen3.8-27B-UD-Q4_K_M",
    [int]$ContextSize = 16384,
    [string]$KvCacheType = "q8_0",
    [int]$NumPredict = 128,
    [int]$Runs = 3,
    [ValidateSet("off", "on", "both")][string]$Mode = "both",
    [string]$User = $env:LB_ADMIN_USER,
    [string]$Password = $env:LB_ADMIN_PASSWORD,
    [int]$LoadTimeoutSec = 900,
    [switch]$KeepLoaded
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$diag = Join-Path $root '_diag'
if (-not (Test-Path $diag)) { New-Item -ItemType Directory -Path $diag | Out-Null }
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$artifact = Join-Path $diag ("bench-rpc-qwen38-{0}.json" -f $stamp)

$jar = Join-Path $diag "bench-cookies-$stamp.txt"
$tmp = Join-Path $diag "bench-tmp-$stamp"
New-Item -ItemType Directory -Path $tmp | Out-Null

function Write-Bytes([string]$path, [string]$content) {
    [System.IO.File]::WriteAllText($path, $content, (New-Object System.Text.UTF8Encoding($false)))
}

function CurlJson([string]$url, [string]$method = 'GET', [string]$bodyFile = '', [string]$csrf = '', [int]$timeoutSec = 30) {
    $respFile = Join-Path $tmp ("curl-" + [guid]::NewGuid().ToString('N') + ".json")
    $curlArgs = @('-s', '--max-time', "$timeoutSec", '-o', "$respFile", '-X', $method, $url)
    if ($bodyFile) { $curlArgs += @('-H', 'Content-Type: application/json', '--data-binary', "@$bodyFile") }
    if ($csrf) { $curlArgs += @('-H', "X-CSRF-Token: $csrf") }
    & curl.exe @curlArgs 2>&1 | Out-Null
    if (-not (Test-Path $respFile)) { return $null }
    $raw = Get-Content $respFile -Raw -ErrorAction SilentlyContinue
    Remove-Item $respFile -ErrorAction SilentlyContinue
    if (-not $raw) { return $null }
    try { return ($raw | ConvertFrom-Json) } catch { return $null }
}

# ── 0. Вход (нужен CSRF для записи: rpc-layout) ─────────────────────────────
$csrf = ''
if ($User -and $Password) {
    $loginFile = Join-Path $tmp 'login.json'
    Write-Bytes $loginFile (@{ name = $User; password = $Password } | ConvertTo-Json -Compress)
    # ОДИН вход: cookie-jar и CSRF обязаны принадлежать ОДНОЙ сессии (иначе
    # PUT rpc-layout отклоняется 403 CSRF, а curl -s это молча проглатывает).
    $loginOut = Join-Path $tmp 'login-resp.json'
    $code = & curl.exe -s -o "$loginOut" -w '%{http_code}' -c $jar -X POST "$Api/api/v1/auth/login" `
        -H 'Content-Type: application/json' --data-binary "@$loginFile" --max-time 15
    if ($code -eq '200' -and (Test-Path $loginOut)) {
        try { $csrf = (Get-Content $loginOut -Raw | ConvertFrom-Json).csrf } catch { }
    }
    if (-not $csrf) {
        Write-Warning "вход не удался (HTTP $code): переключение режима раскладки будет отклонено"
    }
} else {
    Write-Warning 'LB_ADMIN_USER/LB_ADMIN_PASSWORD не заданы: переключение режима раскладки может быть отклонено (403 CSRF).'
}

function Set-RpcMode([string]$mode) {
    $f = Join-Path $tmp "rpc-$mode.json"
    Write-Bytes $f (@{ mode = $mode } | ConvertTo-Json -Compress)
    $resp = Join-Path $tmp "rpc-$mode-resp.json"
    $code = & curl.exe -s -o "$resp" -w '%{http_code}' -X PUT "$Api/api/v1/backends/$Backend/rpc-layout" `
        -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" -b $jar --data-binary "@$f" --max-time 30
    if ($code -ne '200') {
        Write-Warning ("PUT rpc-layout ({0}) → HTTP {1}: {2}" -f $mode, $code, (Get-Content $resp -Raw -ErrorAction SilentlyContinue))
    }
    # Ждём, пока cppworker применит конфигурацию (гард применяет её перед загрузкой
    # и по тику; проверяем ФАКТИЧЕСКОЕ состояние, а не только ответ API).
    $want = ($mode -eq 'on')
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline) {
        $m = CurlJson "$Worker/api/models" 'GET' '' '' 10
        if ($m) {
            $servers = @($m.rpc_servers_active | Where-Object { $_ })
            $enabled = [bool]$m.rpc_enabled
            if ($enabled -eq $want -and (($servers.Count -gt 0) -eq $want)) { return $true }
        }
        Start-Sleep -Milliseconds 1000
    }
    return $false
}

function Get-Models() { return CurlJson "$Worker/api/models" 'GET' '' '' 15 }

function Get-LoadedModel([string]$name) {
    $m = Get-Models
    if (-not $m -or -not $m.models) { return $null }
    foreach ($x in $m.models) {
        if ($x.state -eq 'loaded' -and $x.name -eq $name) { return $x }
    }
    return $null
}

function Unload-Model([string]$name) {
    & curl.exe -s -X POST "$Worker/api/models/unload?force=true&name=$name" --max-time 60 | Out-Null
    for ($i = 0; $i -lt 60; $i++) {
        if (-not (Get-LoadedModel $name)) { return $true }
        Start-Sleep -Seconds 1
    }
    return $false
}

function Load-Model([string]$name) {
    $body = Join-Path $tmp 'load.json'
    $resp = Join-Path $tmp 'load-resp.json'
    Write-Bytes $body (@{
        name        = $name
        contextSize = $ContextSize
        kvCacheType = $KvCacheType
    } | ConvertTo-Json -Compress)
    $t0 = Get-Date
    $code = & curl.exe -s -o "$resp" -w '%{http_code}' -X POST "$Worker/api/models/load" -H 'Content-Type: application/json' --data-binary "@$body" --max-time 120
    $early = Get-Content $resp -Raw -ErrorAction SilentlyContinue
    if ($code -ne '200' -and $code -ne '202') {
        return [pscustomobject]@{ ok = $false; seconds = 0; model = $null; error = "load HTTP ${code}: $early" }
    }
    # R231: ответ «loading» мог прийти от СТАРОЙ загрузки (коллизия с abort/lazy-load):
    # тогда фоновая загрузка не стартует, а мы впустую ждём минуты. loadingStartedAt
    # обязан быть свежим — иначе считаем, что загрузка не началась.
    $started = $null
    if ($early) {
        try {
            $ej = $early | ConvertFrom-Json
            if ($ej.model -and $ej.model.loadingStartedAt -and ([string]$ej.model.loadingStartedAt) -notmatch '^0001-') {
                $started = [datetime]::Parse([string]$ej.model.loadingStartedAt, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::RoundtripKind)
            }
        } catch { }
    }
    if (-not $started -or ((Get-Date).ToUniversalTime() - $started.ToUniversalTime()).TotalSeconds -gt 30) {
        return [pscustomobject]@{ ok = $false; seconds = 0; model = $null; error = "load not started (stale loading state): $early" }
    }
    $deadline = (Get-Date).AddSeconds($LoadTimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $m = Get-Models
        if ($m -and $m.models) {
            $loading = @($m.models | Where-Object { $_.state -eq 'loading' })
            $loaded = @($m.models | Where-Object { $_.state -eq 'loaded' -and $_.name -eq $name })
            if ($loaded.Count -gt 0 -and $loading.Count -eq 0 -and [int]$loaded[0].context_size -gt 0) {
                return [pscustomobject]@{ ok = $true; seconds = ((Get-Date) - $t0).TotalSeconds; model = $loaded[0]; error = '' }
            }
        }
        Start-Sleep -Seconds 2
    }
    return [pscustomobject]@{ ok = $false; seconds = ((Get-Date) - $t0).TotalSeconds; model = $null; error = 'load timeout' }
}

function Get-Placement($model) {
    $gpu = 0; $cpu = 0; $rpc = 0
    foreach ($d in @($model.device_layers_exact)) {
        if (-not $d) { continue }
        $dev = [string]$d.device
        if ($dev -like '*RPC*') { $rpc += [int]$d.count }
        elseif ($dev -like 'CPU*') { $cpu += [int]$d.count }
        else { $gpu += [int]$d.count }
    }
    return [pscustomobject]@{ gpu = $gpu; cpu = $cpu; rpc = $rpc }
}

function Invoke-Generate([int]$numPredict) {
    $body = Join-Path $tmp "gen-$numPredict.json"
    $respFile = Join-Path $tmp "gen-$numPredict-resp.json"
    Write-Bytes $body (@{
        model    = $Model
        stream   = $false
        messages = @(
            @{ role = 'system'; content = 'Ты — ассистент-программист. Отвечай кратко, только по делу.' },
            @{ role = 'user'; content = 'Напиши на Go функцию sortByLen, которая сортирует срез строк по длине, и приведи пример её вызова. Только код, без пояснений.' }
        )
        options  = @{ num_predict = $numPredict; temperature = 0 }
    } | ConvertTo-Json -Depth 6 -Compress)
    $t0 = Get-Date
    & curl.exe -s -o "$respFile" -X POST "$Worker/api/chat" -H 'Content-Type: application/json' --data-binary "@$body" --max-time 900 | Out-Null
    $sec = ((Get-Date) - $t0).TotalSeconds
    $raw = Get-Content $respFile -Raw -ErrorAction SilentlyContinue
    Remove-Item $respFile -ErrorAction SilentlyContinue
    if (-not $raw) { Write-Warning '  generate: пустой ответ'; return $null }
    try { $j = $raw | ConvertFrom-Json } catch { Write-Warning ('  generate: не JSON: ' + $raw.Substring(0, [Math]::Min(120, $raw.Length))); return $null }
    if ($j.error) { Write-Warning ('  generate: ошибка воркера: ' + $j.error); return $null }
    $evalN = 0.0; $promptN = 0.0; $evalCount = 0; $promptCount = 0
    if ($null -ne $j.eval_duration) { $evalN = [double]$j.eval_duration }
    if ($null -ne $j.prompt_eval_duration) { $promptN = [double]$j.prompt_eval_duration }
    if ($null -ne $j.eval_count) { $evalCount = [int]$j.eval_count }
    if ($null -ne $j.prompt_eval_count) { $promptCount = [int]$j.prompt_eval_count }
    $evalTokS = 0.0
    if ($evalN -gt 0) { $evalTokS = [math]::Round($evalCount / ($evalN / 1e9), 2) }
    $prefillTokS = 0.0
    if ($promptN -gt 0) { $prefillTokS = [math]::Round($promptCount / ($promptN / 1e9), 2) }
    $totalSec = 0.0
    if ($null -ne $j.total_duration) { $totalSec = [math]::Round([double]$j.total_duration / 1e9, 2) }
    return [pscustomobject]@{
        wall_sec      = [math]::Round($sec, 1)
        eval_count    = $evalCount
        eval_tok_s    = $evalTokS
        prompt_count  = $promptCount
        prefill_tok_s = $prefillTokS
        total_sec     = $totalSec
        content_len   = ([string]$j.message.content).Length
        reasoning_len = ([string]$j.message.reasoning).Length
    }
}

function Measure-Mode([string]$mode) {
    Write-Host ''
    Write-Host ("=== режим раскладки: {0} ===" -f $mode) -ForegroundColor Cyan
    try {
        if (-not (Set-RpcMode $mode)) {
            Write-Warning "cppworker не применил режим $mode за отведённое время — замер пропущен"
            return [pscustomobject]@{ mode = $mode; ok = $false; error = 'rpc mode not applied' }
        }
        Start-Sleep -Seconds 1
        # force-unload заодно отменяет случайную ленивую загрузку (минуты на 15-GB модель).
        Unload-Model $Model | Out-Null
        Start-Sleep -Seconds 2
        $load = Load-Model $Model
        if (-not $load.ok) {
            Write-Warning ("модель не загрузилась в режиме {0}: {1}" -f $mode, $load.error)
            return [pscustomobject]@{ mode = $mode; ok = $false; error = $load.error }
        }
        Write-Host ("  загрузка: {0:N1} с" -f $load.seconds)
        $pl = Get-Placement $load.model
        Write-Host ("  слои: GPU={0} CPU={1} RPC={2}, всего={3}" -f $pl.gpu, $pl.cpu, $pl.rpc, $load.model.n_layers)
        $snap = Get-Models
        Write-Host ("  VRAM free: local={0} MB, remote={1} MB (source={2}), rpc_servers={3}" -f `
            $snap.vram_local_free_mb, $snap.vram_remote_free_mb, $snap.vram_remote_source, (($snap.rpc_servers_active) -join ','))

        # Прогрев (не в замер): первый запрос может включать аллокации/кэш.
        Invoke-Generate 8 | Out-Null
        $samples = @()
        for ($i = 1; $i -le $Runs; $i++) {
            $g = Invoke-Generate $NumPredict
            if ($g) {
                Write-Host ("  замер {0}: eval={1} tok/s, prefill={2} tok/s, всего={3} с, content={4} симв." -f `
                    $i, $g.eval_tok_s, $g.prefill_tok_s, $g.total_sec, $g.content_len)
                $samples += $g
            } else {
                Write-Warning "  замер $i не удался"
            }
        }
        if ($samples.Count -eq 0) {
            return [pscustomobject]@{ mode = $mode; ok = $false; error = 'no successful generations' }
        }
        $avg = [math]::Round((($samples | Measure-Object -Property eval_tok_s -Average).Average), 2)
        $avgPrefill = [math]::Round((($samples | Measure-Object -Property prefill_tok_s -Average).Average), 2)
        $avgTotal = [math]::Round((($samples | Measure-Object -Property total_sec -Average).Average), 2)
        return [pscustomobject]@{
            mode           = $mode
            ok             = $true
            error          = ''
            load_sec       = [math]::Round($load.seconds, 1)
            gpu_layers     = $pl.gpu
            cpu_layers     = $pl.cpu
            rpc_layers     = $pl.rpc
            total_layers   = $load.model.n_layers
            vram_local_mb  = $snap.vram_local_free_mb
            vram_remote_mb = $snap.vram_remote_free_mb
            rpc_servers    = @($snap.rpc_servers_active)
            eval_tok_s     = $avg
            prefill_tok_s  = $avgPrefill
            total_sec      = $avgTotal
            runs           = $samples
        }
    } catch {
        Write-Warning ("режим {0}: исключение: {1}" -f $mode, $_.Exception.Message)
        return [pscustomobject]@{ mode = $mode; ok = $false; error = $_.Exception.Message }
    }
}

# ── 1. Замеры ────────────────────────────────────────────────────────────────
$modes = if ($Mode -eq 'both') { @('off', 'on') } else { @($Mode) }
$results = @()
try {
    foreach ($m in $modes) { $results += Measure-Mode $m }
}
finally {
    # Возвращаем авто-режим, чтобы не оставить стенд в ручном выборе оператора.
    # Без ожидания: гард применит «auto» сам по тику.
    try {
        $auto = Join-Path $tmp 'rpc-auto.json'
        Write-Bytes $auto '{"mode":"auto"}'
        & curl.exe -s -o $null -X PUT "$Api/api/v1/backends/$Backend/rpc-layout" `
            -H 'Content-Type: application/json' -H "X-CSRF-Token: $csrf" -b $jar --data-binary "@$auto" | Out-Null
    } catch { }
    if (-not $KeepLoaded) { Unload-Model $Model | Out-Null }
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
    Remove-Item -Force $jar -ErrorAction SilentlyContinue
}

# ── 2. Итоговая таблица ─────────────────────────────────────────────────────
Write-Host ''
Write-Host '=== СРАВНЕНИЕ ===' -ForegroundColor Cyan
$ok = @($results | Where-Object { $_.ok })
$table = $ok | Select-Object mode, load_sec, gpu_layers, cpu_layers, rpc_layers, total_layers, vram_local_mb, vram_remote_mb, eval_tok_s, prefill_tok_s, total_sec
$table | Format-Table -AutoSize | Out-String -Width 200 | Write-Host

$artifactData = [pscustomobject]@{
    measuredAt  = (Get-Date).ToUniversalTime().ToString('o')
    model       = $Model
    contextSize = $ContextSize
    kvCacheType = $KvCacheType
    numPredict  = $NumPredict
    runs        = $Runs
    results     = $results
}
$artifactData | ConvertTo-Json -Depth 8 | Out-File -FilePath $artifact -Encoding UTF8
Write-Host ("артефакт: {0}" -f $artifact)

if ($ok.Count -eq 2) {
    $a = $ok[0]; $b = $ok[1]
    $diff = if ($a.eval_tok_s -gt 0) { [math]::Round((($b.eval_tok_s - $a.eval_tok_s) / $a.eval_tok_s) * 100, 1) } else { 0 }
    Write-Host ''
    if ($b.eval_tok_s -gt $a.eval_tok_s) {
        Write-Host ("ВЫИГРЫШ RPC: {0} → {1} tok/s (+{2}%), prefill {3} → {4} tok/s, загрузка {5:N1} → {6:N1} с" -f `
            $a.eval_tok_s, $b.eval_tok_s, $diff, $a.prefill_tok_s, $b.prefill_tok_s, $a.load_sec, $b.load_sec) -ForegroundColor Green
    } else {
        Write-Host ("RPC НЕ ДАЁТ ВЫИГРЫША по скорости: {0} → {1} tok/s ({2}%), prefill {3} → {4} tok/s" -f `
            $a.eval_tok_s, $b.eval_tok_s, $diff, $a.prefill_tok_s, $b.prefill_tok_s) -ForegroundColor Yellow
    }
    Write-Host ("раскладка off: GPU={0} CPU={1} RPC={2}; on: GPU={3} CPU={4} RPC={5}" -f `
        $a.gpu_layers, $a.cpu_layers, $a.rpc_layers, $b.gpu_layers, $b.cpu_layers, $b.rpc_layers)
} else {
    Write-Host 'Сравнение неполное: не все режимы измерены (см. предупреждения выше).'
}
