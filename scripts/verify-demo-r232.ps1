#!/usr/bin/env pwsh
# scripts/verify-demo-r232.ps1 — R232 (2026-10-24): живая проверка демо-режима.
#
# Поднимает ВРЕМЕННЫЙ контейнер webui с WEBUI_DEMO_MODE=1 (порт по умолчанию
# 18084), ждёт готовности, гоняет Playwright-проверку
# scripts/webui-e2e/check-demo-mode-r232.js и удаляет контейнер.
# Рабочий стенд при этом не трогается: настоящий webui продолжает работать на
# своём порту с выключенным демо.
#
# Скрипт совместим с Windows PowerShell 5.1 и pwsh 7: вывод docker/node не
# поднимается до исключений (у нативных команд stderr — это норма).
#
# Запуск:
#   pwsh -File scripts/verify-demo-r232.ps1
#   pwsh -File scripts/verify-demo-r232.ps1 -Image ollama-legion/webui:r232-demo -Port 18084
#   pwsh -File scripts/verify-demo-r232.ps1 -Keep   # оставить контейнер для ручного просмотра
#
# Код возврата: 0 — PASS, 1 — FAIL.
[CmdletBinding()]
param(
    [string]$Image = "ollama-legion/webui:r232-demo",
    [int]$Port = 18084,
    [string]$Container = "ol-demo-webui-r232",
    [string]$Network = "ol-net",
    [switch]$Keep,
    [int]$WaitSec = 90
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
$url = "http://127.0.0.1:$Port"
$failures = @()

function Fail([string]$msg) { $script:failures += $msg; Write-Host ("  FAIL  " + $msg) -ForegroundColor Red }

# Нативные команды пишут в stderr постоянно (docker при отсутствии объекта,
# node при предупреждениях). Под $ErrorActionPreference='Stop' такой stderr
# в PS 5.1 становится исключением, поэтому здесь он не поднимается вообще.
function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$Exe,
        [string[]]$Arguments = @()
    )
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $lines = & $Exe @Arguments 2>&1 | ForEach-Object { "$_" }
        return [pscustomobject]@{ Code = $LASTEXITCODE; Output = @($lines) }
    }
    finally { $ErrorActionPreference = $prev }
}

function Test-NameListed {
    param([string[]]$Names, [string]$Name)
    foreach ($n in $Names) { if ($n.Trim() -eq $Name) { return $true } }
    return $false
}

function Remove-TempContainer {
    $listed = Invoke-Native docker @('ps', '-a', '--format', '{{.Names}}')
    if ($listed.Code -eq 0 -and (Test-NameListed -Names $listed.Output -Name $Container)) {
        Invoke-Native docker @('rm', '-f', $Container) | Out-Null
    }
}

try {
    # 0. Образ и сеть
    $images = Invoke-Native docker @('image', 'ls', '--format', '{{.Repository}}:{{.Tag}}')
    if (-not (Test-NameListed -Names $images.Output -Name $Image)) {
        throw "образ $Image не найден: соберите scripts/build-containers.ps1 -WebUI -Tag <tag>"
    }
    $networks = Invoke-Native docker @('network', 'ls', '--format', '{{.Name}}')
    if (-not (Test-NameListed -Names $networks.Output -Name $Network)) {
        throw "сеть $Network не найдена: сначала поднимите стенд (docker compose ... up -d)"
    }

    # 1. Временный контейнер (если остался от прошлого прогона — пересоздаём)
    Remove-TempContainer
    Write-Host "Старт демо-контейнера: $Image → $url (WEBUI_DEMO_MODE=1)"
    $run = Invoke-Native docker @(
        'run', '-d', '--name', $Container, '--network', $Network, '-p', "${Port}:80",
        '-e', 'WEBUI_DEMO_MODE=1',
        '-e', 'NGINX_PORT=80',
        '-e', 'API_HOST=loadbalancer',
        '-e', 'API_PORT=18081',
        '-e', 'LB_PORT=18080',
        '-e', 'CPPWORKER_HOST=cppworker-gpu',
        '-e', 'CPPWORKER_PORT=18092',
        $Image
    )
    if ($run.Code -ne 0) { throw ("docker run не удался: " + ($run.Output -join ' ')) }

    # 2. Готовность nginx
    $ready = $false
    $deadline = (Get-Date).AddSeconds($WaitSec)
    while ((Get-Date) -lt $deadline) {
        $code = Invoke-Native curl.exe @('-s', '-o', "$env:TEMP\demo-health.html", '-w', '%{http_code}', '--max-time', '3', "$url/health.html")
        if (($code.Output -join '') -eq '200') { $ready = $true; break }
        Start-Sleep -Seconds 1
    }
    if (-not $ready) { Fail "webui не ответил на $url/health.html за $WaitSec с"; throw 'not ready' }

    # 3. Конфиг действительно демо
    $cfgResp = Invoke-Native curl.exe @('-s', '--max-time', '5', "$url/js/modules/config.js")
    $cfg = ($cfgResp.Output -join "`n")
    if ($cfg -notmatch 'DEMO_MODE:\s*true') { Fail "config.js не содержит DEMO_MODE: true" }
    else { Write-Host '  PASS  config.js: DEMO_MODE=true' }

    # 4. Playwright-сценарий
    $env:WEBUI = $url
    $env:NODE_PATH = (Join-Path $root 'node_modules')
    $node = Invoke-Native node @((Join-Path $root 'scripts/webui-e2e/check-demo-mode-r232.js'))
    foreach ($line in $node.Output) { Write-Host $line }
    if ($node.Code -ne 0) { Fail 'Playwright-проверка демо-режима не прошла' }
}
catch {
    if ($_.Exception.Message -notin @('not ready')) {
        Fail ("исключение: " + $_.Exception.Message)
    }
}
finally {
    if (-not $Keep) {
        Remove-TempContainer
    } else {
        Write-Host "  контейнер $Container оставлен: $url (удалить: docker rm -f $Container)"
    }
}

Write-Host ''
if ($failures.Count -eq 0) {
    Write-Host 'ИТОГ: PASS' -ForegroundColor Green
    exit 0
}
Write-Host ("ИТОГ: FAIL ({0})" -f $failures.Count) -ForegroundColor Red
foreach ($f in $failures) { Write-Host ('  - ' + $f) -ForegroundColor Red }
exit 1
