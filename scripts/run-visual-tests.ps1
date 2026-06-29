<#
OllamaLegion - Playwright visual-regression runner (PowerShell).

Usage:
    .\scripts\run-visual-tests.ps1                # compare against baseline
    .\scripts\run-visual-tests.ps1 -Update        # regenerate baseline
    .\scripts\run-visual-tests.ps1 -Install       # first-time setup
    .\scripts\run-visual-tests.ps1 -InstallBrowsers
    $env:BASE_URL = "http://localhost:18081"; .\scripts\run-visual-tests.ps1

Required: node (>= 18), npm.
First-time use:
    .\scripts\run-visual-tests.ps1 -InstallBrowsers
#>

[CmdletBinding()]
param(
    [switch]$Update,
    [switch]$Install,
    [switch]$InstallBrowsers,
    [switch]$Help
)

$ErrorActionPreference = "Stop"

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = (Resolve-Path "$ScriptDir\..").Path
Set-Location -Path $RepoRoot

if ($Help) {
    Get-Content "$PSCommandPath" -TotalCount 25
    return
}

$BaseUrl = if ($env:BASE_URL) { $env:BASE_URL } else { "http://localhost:18083" }
$env:PLAYWRIGHT_BASE_URL = $BaseUrl

function Step($msg) { Write-Host "[run-visual-tests] $msg" -ForegroundColor Cyan }

if ($Install) {
    Step "installing npm dependencies..."
    npm install --no-audit --no-fund
    Step "installing Playwright browsers (chromium)..."
    npx playwright install chromium
    Step "done."
    return
}

if ($InstallBrowsers) {
    Step "installing Playwright browsers (chromium)..."
    npx playwright install chromium
    Step "done."
    return
}

if ($Update) {
    Step "regenerating baseline screenshots against $BaseUrl..."
    npx playwright test --config=playwright.config.js --update-snapshots
    return
}

Step "running visual-regression suite against $BaseUrl..."
npx playwright test --config=playwright.config.js