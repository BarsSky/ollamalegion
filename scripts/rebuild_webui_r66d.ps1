#!/usr/bin/env pwsh
# rebuild_webui_r66d.ps1 — пересборка и раскатка webui-контейнера.
#
# R66d (2026-09-23): тег по умолчанию r66-submodule-v11. Что изменилось в образе:
#   1) webui/nginx.conf: `location = /js/modules/config.js` с no-store. config.js
#      генерируется entrypoint'ом и содержит API_TOKEN; раньше он попадал под
#      `expires 1y` для *.js, браузер держал СТАРЫЙ токен до года и после смены
#      токена получал 401 на всех действиях — это и был «token problem»;
#   2) docker/webui/entrypoint.sh: при старте пробит балансер тем же токеном и
#      печатает "API token check: OK" или ERROR+инструкцию при 401.
#      Проба на BusyBox-флагах (-S -T), не GNU (--server-response/--timeout).
#
# Зачем отдельный скрипт: у webui нет своего rebuild-скрипта (были только для
# balancer и cppworker), а руками легко забыть две вещи, которые уже приводили
# к «пустым вкладкам»:
#   1) тег образа в compose прописан строкой — его надо обновить, иначе compose
#      пересоздаст контейнер на старом образе;
#   2) build-args VERSION/GIT_COMMIT нужны entrypoint.sh, который инжектит их в
#      config.js (иначе в футере «dev» и неверная версия).
#
# Использование:
#   pwsh -File scripts/rebuild_webui_r66d.ps1
#   pwsh -File scripts/rebuild_webui_r66d.ps1 -Tag r66-submodule-v11
#   pwsh -File scripts/rebuild_webui_r66d.ps1 -SkipBuild      # только пересоздать

param(
    [string]$Tag = "r66-submodule-v11",
    [switch]$SkipBuild
)

# R66c: НЕ 'Stop' — в Windows PowerShell 5.1 docker пишет прогресс в stderr,
# каждая строка становится NativeCommandError, и скрипт умирал бы на первой же.
$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

$imageName = "ollama-legion/webui"
$composeFile = "docker-compose.cppworker-bundled-with-agent.yml"
$composePath = Join-Path $repoRoot "deployments\$composeFile"

$gitCommit = (git rev-parse --short HEAD)
$buildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")

# R66c (2026-09-22): тег пиним ДО сборки. Раньше порядок был обратный, и
# `docker compose build` собирал образ под СТАРЫМ тегом из compose-файла
# (docker compose build использует image: из файла, а не -t), после чего
# `up -d` не находил нужный тег локально, пытался его pull'ить (pull access
# denied для локального имени) и пересобирал образ второй раз. Побочный эффект:
# старый тег молча начинал указывать на новое содержимое.
# Образ в compose прописан строкой (не через ${VAR}) — поэтому и правим файл.
Write-Host "[webui] Pinning image tag in $composeFile ..." -ForegroundColor Cyan
$content = Get-Content $composePath -Raw
$pattern = '(?m)^(\s*image:\s*ollama-legion/webui:)\S+'
if ($content -notmatch $pattern) { throw "не нашёл строку 'image: ollama-legion/webui:...' в $composePath" }
$content = [regex]::Replace($content, $pattern, "`${1}$Tag")
Set-Content -Path $composePath -Value $content -NoNewline

if (-not $SkipBuild) {
    Write-Host "[webui] Building ${imageName}:${Tag} (commit $gitCommit) ..." -ForegroundColor Cyan
    $env:WEBUI_VERSION = $Tag
    $env:WEBUI_GIT_COMMIT = $gitCommit
    $env:WEBUI_BUILD_DATE = $buildDate
    Push-Location (Join-Path $repoRoot "deployments")
    try {
        docker compose -f $composeFile build webui
        if ($LASTEXITCODE -ne 0) { throw "docker compose build webui failed (exit $LASTEXITCODE)" }
    }
    finally {
        Pop-Location
    }
}

Push-Location (Join-Path $repoRoot "deployments")
try {
    Write-Host "[webui] Recreating service webui via compose ..." -ForegroundColor Cyan
    docker compose -f $composeFile up -d --no-deps --force-recreate webui
    if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
}
finally {
    Pop-Location
}

Start-Sleep -Seconds 5
docker ps --filter name=ol-bundled-webui --format '{{.Names}}\t{{.Status}}\t{{.Image}}'

Write-Host "[webui] Проверка страниц (обрати внимание: все html из webui/ должны отдаваться 200):" -ForegroundColor Cyan
$port = "18083"
foreach ($page in @("/", "/monitor.html", "/health.html", "/virtual-models.html", "/rpc-status.html", "/tp-pipeline.html")) {
    $code = curl.exe -s -o NUL -w "%{http_code}" --max-time 10 "http://127.0.0.1:${port}${page}"
    $mark = if ($code -eq "200") { "OK " } else { "!! " }
    Write-Host ("  {0}{1} -> {2}" -f $mark, $page, $code)
}
Write-Host "[webui] DONE." -ForegroundColor Green
