#!/usr/bin/env pwsh
# =============================================================================
# Запуск bundled стека Ollama Legion: cppworker-gpu + balancer + webui
# =============================================================================
# Использование:
#   .\scripts\start-bundled.ps1
#   .\scripts\start-bundled.ps1 -Rebuild        # с пересборкой образов
#   .\scripts\start-bundled.ps1 -Down           # остановить стек
#   .\scripts\start-bundled.ps1 -Logs           # показать логи
# =============================================================================

[CmdletBinding()]
param(
    [switch]$Rebuild,
    [switch]$Down,
    [switch]$Logs,
    [switch]$NoDetach
)

$ErrorActionPreference = "Stop"
$scriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$deployDir = Join-Path $scriptDir "..\deployments"
$deployDir = (Resolve-Path $deployDir).Path
$envFile = Join-Path $deployDir ".env.bundled"
$envExample = Join-Path $deployDir ".env.bundled.example"
$composeFile = Join-Path $deployDir "docker-compose.cppworker-bundled.yml"
$bundledConfig = Join-Path $scriptDir "..\config\config.bundled.json"
$defaultConfig = Join-Path $scriptDir "..\config\config.example.json"

Set-Location $deployDir

function Write-Step($msg) {
    Write-Host ""
    Write-Host "==> $msg" -ForegroundColor Cyan
}

# -----------------------------------------------------------------------------
# .env.bundled — инициализация
# -----------------------------------------------------------------------------
if (-not (Test-Path $envFile)) {
    Write-Step "Создаю .env.bundled из .env.bundled.example"
    Copy-Item $envExample $envFile
    Write-Host "    Скопирован $envFile" -ForegroundColor Green
    Write-Host "    Обязательно смените CPPWORKER_API_TOKEN на свой (или оставьте для теста)!" -ForegroundColor Yellow
}

# -----------------------------------------------------------------------------
# config.bundled.json → config.json (для балансировщика)
# -----------------------------------------------------------------------------
$defaultConfigPath = Join-Path $scriptDir "..\config\config.json"
if (Test-Path $bundledConfig) {
    if ((Test-Path $defaultConfigPath) -and -not $env:OL_BUNDLED_FORCE_OVERWRITE_CONFIG) {
        $existing = Get-Content $defaultConfigPath -Raw
        if ($existing -notmatch '"_comment":\s*"Ollama Legion — bundled config') {
            Write-Step "config.json уже существует и не выглядит как bundled"
            Write-Host "    Использую существующий config.json" -ForegroundColor Yellow
        }
    } else {
        Write-Step "Копирую config.bundled.json → config.json"
        Copy-Item $bundledConfig $defaultConfigPath -Force
        # Подставляем токен из .env.bundled
        $token = (Select-String -Path $envFile -Pattern '^CPPWORKER_API_TOKEN=(.+)$').Matches.Groups[1].Value
        if ($token) {
            $json = Get-Content $defaultConfigPath -Raw | ConvertFrom-Json
            $json.auth.tokens[0].name = $token
            $json | ConvertTo-Json -Depth 20 | Set-Content $defaultConfigPath
            Write-Host "    auth.tokens[0].name = $token" -ForegroundColor Green
        }
    }
}

# -----------------------------------------------------------------------------
# Команды
# -----------------------------------------------------------------------------
$composeArgs = @("-f", $composeFile, "--env-file", $envFile)

if ($Down) {
    Write-Step "Останавливаю стек"
    docker compose @composeArgs down
    exit $LASTEXITCODE
}

if ($Logs) {
    Write-Step "Логи (Ctrl+C для выхода)"
    docker compose @composeArgs logs -f
    exit $LASTEXITCODE
}

$upArgs = $composeArgs + @("up")
if ($Rebuild) { $upArgs += "--build" }
if (-not $NoDetach) { $upArgs += "-d" }

Write-Step "Запускаю bundled стек"
docker compose @upArgs

if ($LASTEXITCODE -eq 0 -and -not $NoDetach) {
    Write-Step "Стек запущен"

    # -----------------------------------------------------------------------------
    # Smoke-check: убедиться, что DNS bundled-net зарегистрирован корректно
    # (защита от регрессии 2026-06-08: при внешнем `docker run` контейнер
    # loadbalancer не оказывался в `ol-bundled-net`, и webui получал 502 на
    # /ollama/api/* при первом запросе браузера OpenWebUI).
    # -----------------------------------------------------------------------------
    Write-Step "Smoke-check: проверяю DNS bundled-net и путь /ollama/* → balancer"
    Start-Sleep -Seconds 5
    $dnsOk = $false
    $httpOk = $false
    try {
        $dnsLookup = docker exec ol-bundled-webui nslookup loadbalancer 127.0.0.11 2>&1
        $dnsOk = $dnsLookup -match 'Address:\s+172\.\d+\.\d+\.\d+'
    } catch { $dnsOk = $false }
    try {
        $webuiPort = (Get-Content $envFile | Select-String -Pattern '^WEBUI_PORT=' | Select-Object -First 1) -replace 'WEBUI_PORT=', ''
        if (-not $webuiPort) { $webuiPort = '18083' }
        $httpCode = (curl.exe -s -o $null -w '%{http_code}' -H 'Origin: http://localhost' "http://localhost:$webuiPort/ollama/api/version" 2>&1)
        $httpOk = ($httpCode -eq '200')
    } catch { $httpOk = $false }
    if ($dnsOk -and $httpOk) {
        Write-Host "    DNS loadbalancer в bundled-net: OK" -ForegroundColor Green
        Write-Host "    /ollama/api/version → 200 OK: OK" -ForegroundColor Green
    } else {
        Write-Host "    DNS loadbalancer в bundled-net: $(if ($dnsOk) {'OK'} else {'FAIL'})" -ForegroundColor $(if ($dnsOk) {'Green'} else {'Yellow'})
        Write-Host "    /ollama/api/version → 200 OK: $(if ($httpOk) {'OK'} else {'FAIL'})" -ForegroundColor $(if ($httpOk) {'Green'} else {'Yellow'})
        Write-Host ""
        Write-Host "    ВНИМАНИЕ: nginx в webui не может достучаться до балансера по DNS." -ForegroundColor Yellow
        Write-Host "    Типичное решение: 'docker network connect --alias loadbalancer ollama-legion-bundled-net ol-bundled-balancer'" -ForegroundColor Yellow
        Write-Host "    Подробности: docs/test-report-2026-06-08-cppworker-multiclient.md (Phase 10)" -ForegroundColor Yellow
    }

    Write-Host ""
    Write-Host "Полезные адреса:" -ForegroundColor Green
    Write-Host "  WebUI:          http://localhost:18083"
    Write-Host "  Balancer API:   http://localhost:18080 (OpenAI-совместимый)"
    Write-Host "  Balancer admin: http://localhost:18081 (X-API-Token required)"
    Write-Host "  CppWorker:      http://localhost:18092 (прямой доступ)"
    Write-Host ""
    Write-Host "Проверить регистрацию cppworker в балансировщике:"
    Write-Host "  curl -H 'X-API-Token: <your token>' http://localhost:18081/api/v1/backends"
    Write-Host ""
    Write-Host "Посмотреть логи:" -ForegroundColor Green
    Write-Host "  .\scripts\start-bundled.ps1 -Logs"
}