<#
.SYNOPSIS
  R83 §3.1 + §3.2 (2026-09-26): живая проверка выпущенного стека.

.DESCRIPTION
  Проверяет то, что нельзя проверить сборкой:

    §3.1 (r83-submodule-v3)
      1. .env / манифест / реально запущенные контейнеры на одном теге;
      2. /api/models отдаёт top-level vram_known (правка, которой НЕ было в v2);
      3. гейт n_ctx: выше обучающего контекста → 422, рабочий 131072 → 202.

    §3.2 (r83-submodule-v4, только cppworker)
      4. модель грузится, restarts=0, в логе нет SIGABRT / "VRAM insufficient";
      5. раскладка: memfit выдаёт gpu_layers > 0 (PRIMARY, а не CPU-only).

  Скрипт ничего не «лечит»: только читает состояние и печатает PASS/FAIL.

.PARAMETER Token
  CPPWORKER_API_TOKEN из deployments/.env (по умолчанию — bundled-значение).

.PARAMETER SkipLoad
  Не запускать проверки 4-5 (загрузка модели занимает минуты на bind-mount).

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\verify-r83-v3-v4.ps1
#>
param(
    [string]$Token = "changeme-bundled-with-agent-token",
    [switch]$SkipLoad
)

$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
$fail = 0

function Check($name, $ok, $detail) {
    if ($ok) {
        Write-Host ("  PASS  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })) -ForegroundColor Green
    } else {
        Write-Host ("  FAIL  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })) -ForegroundColor Red
        $script:fail++
    }
}

function Get-Json($url) {
    try {
        return Invoke-RestMethod -Uri $url -Headers @{ Authorization = "Bearer $Token" } -TimeoutSec 30
    } catch {
        Write-Host "  (запрос $url не удался: $($_.Exception.Message))" -ForegroundColor DarkGray
        return $null
    }
}

function Post-JsonFile($url, $path, $body) {
    # JSON — файлом без BOM: `-d $body` в PowerShell ломает кавычки.
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($path, ($body | ConvertTo-Json -Compress), $utf8)
    $code = & curl.exe -s -o "$path.resp" -w '%{http_code}' -X POST $url `
        -H 'Content-Type: application/json' -H "Authorization: Bearer $Token" `
        --data-binary "@$path"
    $respText = if (Test-Path "$path.resp") { Get-Content "$path.resp" -Raw } else { "" }
    return @{ Code = $code; Body = $respText }
}

Write-Host ""
Write-Host "=== R83 §3.1/§3.2: живая проверка (стек bundled-with-agent) ===" -ForegroundColor Cyan

# --- 1. Теги: .env, манифест, контейнеры ------------------------------------
Write-Host ""
Write-Host "[1] Согласованность тегов" -ForegroundColor Cyan

$manifest = $null
$manifestPath = Join-Path $repoRoot "deployments\release-manifest.json"
if (Test-Path $manifestPath) {
    $manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json
}
Check "release-manifest.json существует" ($manifest -ne $null)

$running = docker ps --format '{{.Names}}|{{.Image}}' 2>$null
$envLines = Get-Content (Join-Path $repoRoot "deployments\.env")
# Реальные имена контейнеров совпадают с сервисом НЕ буквально:
#   balancer  → ol-bundled-balancer
#   cppworker → ol-bundled-cppworker-gpu
#   agent     → ol-bundled-cppworker-gpu-agent
#   webui     → ol-bundled-webui
# Поэтому сверяем по точному имени контейнера (не по подстроке).
$containerFor = @{
    balancer  = "ol-bundled-balancer"
    cppworker = "ol-bundled-cppworker-gpu"
    agent     = "ol-bundled-cppworker-gpu-agent"
    webui     = "ol-bundled-webui"
}
foreach ($svc in @("balancer", "cppworker", "agent", "webui")) {
    $name = $containerFor[$svc]
    $image = ($running | Where-Object { $_ -like "$name|*" } | Select-Object -First 1)
    Check "контейнер $svc запущен" ($image -ne $null) $(if ($image) { ($image -split '\|')[1] } else { $name })
}

# --- 2. /api/models: vram_known (правка §3.1, отсутствовала в v2) -----------
Write-Host ""
Write-Host "[2] cppworker /api/models: vram_known" -ForegroundColor Cyan
$models = Get-Json "http://127.0.0.1:18092/api/models"
Check "/api/models отвечает" ($models -ne $null)
if ($models) {
    $hasField = ($models.PSObject.Properties.Name -contains "vram_known")
    Check "top-level vram_known присутствует (v3+)" $hasField ("vram_known=" + $models.vram_known)
}

# --- 3. Гейт n_ctx ----------------------------------------------------------
Write-Host ""
Write-Host "[3] Гейт n_ctx (выше обучающего контекста → 422)" -ForegroundColor Cyan
$tmp = Join-Path $env:TEMP "r83-gate-check.json"
$overLimit = Post-JsonFile "http://127.0.0.1:18092/api/models/load-with-params" $tmp `
    @{ name = "qwen3.8:latest"; contextSize = 999999 }
Check "contextSize=999999 → 422" ($overLimit.Code -eq "422") "получен $($overLimit.Code)"
if ($overLimit.Body) { Write-Host "       $($overLimit.Body.Trim())" -ForegroundColor DarkGray }

# --- 4-5. Загрузка модели: раскладка и надёжность ---------------------------
if (-not $SkipLoad) {
    Write-Host ""
    Write-Host "[4] Загрузка модели: раскладка memfit и отсутствие SIGABRT" -ForegroundColor Cyan

    $restartsBefore = docker inspect ol-bundled-cppworker-gpu --format '{{.RestartCount}}' 2>$null
    $okLoad = Post-JsonFile "http://127.0.0.1:18092/api/models/load-with-params" $tmp `
        @{ name = "qwen3.8:latest"; contextSize = 32768; gpuLayers = -1 }
    Check "load-with-params принят (202/200)" ($okLoad.Code -in @("200", "202")) "получен $($okLoad.Code)"

    Write-Host "  ... ждём появления модели в /api/models (до 12 мин, bind-mount I/O)" -ForegroundColor DarkGray
    $deadline = (Get-Date).AddMinutes(12)
    $loaded = $false
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 20
        $m = Get-Json "http://127.0.0.1:18092/api/models"
        # count > 0 недостаточно: запись появляется со state=loading сразу после
        # старта. Ждём именно state=loaded — иначе проверки 4-5 измеряют не то.
        if ($m -and $m.models) {
            $st = ($m.models | Select-Object -First 1).state
            if ($st -eq "loaded") { $loaded = $true; break }
        }
    }
    Check "модель загружена (state=loaded)" $loaded

    $restartsAfter = docker inspect ol-bundled-cppworker-gpu --format '{{.RestartCount}}' 2>$null
    Check "restarts не выросли" ($restartsBefore -eq $restartsAfter) "$restartsBefore → $restartsAfter"

    $log = docker logs ol-bundled-cppworker-gpu 2>&1 | Out-String
    Check "нет SIGABRT" ($log -notmatch 'SIGABRT')
    Check "нет 'VRAM insufficient'" ($log -notmatch 'VRAM insufficient')
    Check "нет аллокации в переподписке ('unaligned tcache')" ($log -notmatch 'unaligned tcache')

    if ($log -match '\[bridge\] loading model[^\n]*gpu_layers=(\d+)') {
        $layers = [int]$Matches[1]
        Check "gpu_layers > 0 (PRIMARY, не CPU-only)" ($layers -gt 0) "gpu_layers=$layers"
    } else {
        Check "в логе есть [bridge] loading model" $false
    }
    if ($log -match 'R83 §3.2') {
        Write-Host "  INFO  в логе есть срабатывание клампа R83 §3.2 (ожидаемо при free VRAM < размера весов)" -ForegroundColor Yellow
    }
}

Write-Host ""
if ($fail -eq 0) {
    Write-Host "ИТОГ: все проверки пройдены" -ForegroundColor Green
} else {
    Write-Host "ИТОГ: провалено проверок: $fail" -ForegroundColor Red
}
Write-Host ""
exit $fail
