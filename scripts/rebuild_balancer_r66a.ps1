#!/usr/bin/env pwsh
# rebuild_balancer_r66a.ps1 — пересборка и раскатка balancer-контейнера.
#
# ИСТОРИЯ (почему скрипт переписан 2026-09-22, R66b):
#   Раньше скрипт делал `docker rm` + `docker run ... --network ol-bundled-network`
#   руками. Это (а) указывало на НЕсуществующую сеть (реальная —
#   ollama-legion-bundled-net), т.е. после `docker rm` балансер вообще не
#   стартовал — полный отказ API; (б) теряло DNS-алиас `loadbalancer`, от
#   которого зависят webui (http://loadbalancer:18081) и agent
#   (BALANCER_URL=http://loadbalancer:18081); (в) теряло env
#   LB_API_TOKEN/LB_CONFIG_PATH и подсовывало /app/data/config.json вместо
#   смонтированного config/config.json (то есть бандл-конфиг с
#   backendEngine=llama_cpp и профилями моделей не применялся).
#   Теперь единственный источник истины — docker-compose, скрипт только
#   собирает образ и просит compose пересоздать сервис.
#
# Использование:
#   pwsh -File scripts/rebuild_balancer_r66a.ps1                 # tag по умолчанию (R66b)
#   pwsh -File scripts/rebuild_balancer_r66a.ps1 -Tag r66-submodule-v5
#   pwsh -File scripts/rebuild_balancer_r66a.ps1 -SkipBuild      # только пересоздать

param(
    [string]$Tag = "r66-submodule-v5",
    [switch]$SkipBuild
)

# R66c (2026-09-22): НЕ 'Stop'. В Windows PowerShell 5.1 (в котором этот скрипт
# и запускается — pwsh на машине нет) docker пишет прогресс сборки в stderr, а
# каждая такая строка превращается в NativeCommandError. С ErrorActionPreference
# = 'Stop' скрипт умирал на первой же строке прогресса, ещё до проверки
# $LASTEXITCODE, то есть сборка не выполнялась вообще. Ошибки ловим явными
# проверками $LASTEXITCODE (они ниже) и throw'ами.
$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$imageName = "ollama-legion/balancer"
$composeFile = "docker-compose.cppworker-bundled-with-agent.yml"
$composePath = Join-Path $repoRoot "deployments\$composeFile"

if (-not $SkipBuild) {
    Write-Host "[balancer] Building ${imageName}:${Tag} ..." -ForegroundColor Cyan
    docker build -t "${imageName}:${Tag}" -f docker/balancer/Dockerfile .
    if ($LASTEXITCODE -ne 0) { throw "docker build failed (exit $LASTEXITCODE)" }
}

# Образ балансера в compose прописан строкой (не через ${VAR}), поэтому тег
# обновляем прямо в compose-файле — иначе compose пересоздаст контейнер на
# СТАРОМ образе и фиксы не доедут.
Write-Host "[balancer] Pinning image tag in $composeFile ..." -ForegroundColor Cyan
$content = Get-Content $composePath -Raw
$pattern = '(?m)^(\s*image:\s*ollama-legion/balancer:)\S+'
if ($content -notmatch $pattern) { throw "не нашёл строку 'image: ollama-legion/balancer:...' в $composePath" }
$content = [regex]::Replace($content, $pattern, "`${1}$Tag")
Set-Content -Path $composePath -Value $content -NoNewline

Push-Location (Join-Path $repoRoot "deployments")
try {
    Write-Host "[balancer] Recreating service loadbalancer via compose (alias + env + config mount) ..." -ForegroundColor Cyan
    docker compose -f $composeFile up -d --no-deps --force-recreate loadbalancer
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
}
finally {
    Pop-Location
}

Start-Sleep -Seconds 8
docker ps --filter name=ol-bundled-balancer --format '{{.Names}}\t{{.Status}}\t{{.Image}}'
Write-Host "[balancer] Проверка API (нужен X-API-Token из deployments/.env.bundled-with-agent):" -ForegroundColor Cyan
try {
    $token = (Select-String -Path (Join-Path $repoRoot "deployments\.env.bundled-with-agent") -Pattern '^CPPWORKER_API_TOKEN=(.*)$').Matches[0].Groups[1].Value
    $r = Invoke-WebRequest -Uri "http://127.0.0.1:18081/api/v1/cluster/config" -Headers @{ "X-API-Token" = $token } -TimeoutSec 15 -UseBasicParsing
    $cfg = $r.Content | ConvertFrom-Json
    Write-Host ("  backendEngine={0} operatingMode={1}" -f $cfg.backendEngine, $cfg.operatingMode)
    if ($cfg.backendEngine -ne "llama_cpp") {
        Write-Warning "backendEngine=$($cfg.backendEngine): метрики и read-endpoints (/api/ps) будут пустыми для llama.cpp-бэкендов. Проверь config/config.json."
    }
}
catch {
    Write-Warning "не удалось проверить /api/v1/cluster/config: $($_.Exception.Message)"
}
Write-Host "[balancer] DONE." -ForegroundColor Green
