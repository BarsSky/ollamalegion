# =============================================================================
# stop-bundled-full.ps1 — stop the bundled-full stack
# =============================================================================
# Usage:
#   .\scripts\stop-bundled-full.ps1
#   .\scripts\stop-bundled-full.ps1 -Clean    # also remove volumes
#   .\scripts\stop-bundled-full.ps1 -CleanImages  # also remove images
# =============================================================================

[CmdletBinding()]
param(
    [switch]$Clean,
    [switch]$CleanImages
)

$ErrorActionPreference = "Stop"

$RepoRoot = Resolve-Path (Join-Path $PSScriptRoot "..")
$DeployDir = Join-Path $RepoRoot "deployments"
$ComposeFile = Join-Path $DeployDir "docker-compose.bundled-full.yml"
$EnvFile = Join-Path $DeployDir ".env.bundled-full"
$ProjectName = "ol-bundled-full"

if (-not (Test-Path $ComposeFile)) {
    Write-Host "Compose file not found: $ComposeFile" -ForegroundColor Red
    exit 1
}

Write-Host "Stopping $ProjectName..." -ForegroundColor Yellow
$args = @(
    "-p", $ProjectName
    "-f", $ComposeFile
    "--env-file", $EnvFile
    "down"
    "--remove-orphans"
)
if ($Clean) { $args += "-v" }

docker compose @args
if ($LASTEXITCODE -ne 0) {
    Write-Host "docker compose down failed" -ForegroundColor Red
    exit 1
}

if ($CleanImages) {
    Write-Host "Removing images..." -ForegroundColor Yellow
    docker rmi -f `
        ollama-legion/balancer:cppworker-bundled-full `
        ollama-legion/cppworker:gpu-arch_all `
        ollama-legion/agent:gpu-llamacpp `
        ollama-legion/webui:cppworker-bundled-full `
        2>&1 | Out-String | Write-Host
}

Write-Host ""
Write-Host "✓ $ProjectName stopped" -ForegroundColor Green
