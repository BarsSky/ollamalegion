# rebuid-all-latest.ps1 — R83 (2026-09-29): полная пересборка стека с тегами
# r83-submodule-v23 и latest.
#
# Зачем отдельный скрипт. build-containers.ps1 ставит ОДИН тег, а здесь нужны оба:
# релизный (его читает compose из deployments/.env) и latest (его используют
# dev-конфиги и скрипты). Плюс нужны версии в шапках логов (GIT_COMMIT/BUILD_DATE).
$ErrorActionPreference = 'Continue'
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
$env:DOCKER_BUILDKIT = '1'

$sha = (git rev-parse --short HEAD).Trim()
$date = (Get-Date -Format 'yyyy-MM-dd')
$common = @('--build-arg', "GIT_COMMIT=$sha", '--build-arg', "BUILD_DATE=$date")

function Build-One {
    param([string]$Name, [string]$Dockerfile, [string[]]$ExtraArgs, [string]$Target)
    $tagged = "ollama-legion/$Name`:r83-submodule-v23"
    $latest = "ollama-legion/$Name`:latest"
    Write-Host "[build] $tagged (target=$Target)" -ForegroundColor Cyan
    $args = @('build') + $ExtraArgs + $common
    if ($Target) { $args += @('--target', $Target) }
    $args += @('-t', $tagged, '-t', $latest, '-f', $Dockerfile, '.')
    & docker @args
    if ($LASTEXITCODE -ne 0) { throw "[$Name] build failed (exit $LASTEXITCODE)" }
    Write-Host "[build] ok: $tagged + $latest" -ForegroundColor Green
}

Build-One -Name 'balancer' -Dockerfile 'docker/balancer/Dockerfile' -ExtraArgs @()
Build-One -Name 'agent'    -Dockerfile 'docker/agent/Dockerfile'    -ExtraArgs @('--build-arg','ENABLE_NVML=true') -Target 'agent-gpu'
Build-One -Name 'webui'    -Dockerfile 'docker/webui/Dockerfile'    -ExtraArgs @('--build-arg',"VERSION=r83-submodule-v23")

Write-Host "[build] all done (commit=$sha)" -ForegroundColor Green
