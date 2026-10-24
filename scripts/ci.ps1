# scripts/ci.ps1 — R226 (2026-10-22): один прогон всех проверок стенда.
#
# ЗАЧЕМ. Перед коммитом надо было помнить четыре команды (unit, JS внутри unit,
# живая регрессия, e2e WebUI), и легко было забыть одну — так уже случалось:
# `run-checks.ps1` не запускал пакеты, где лежали тесты на RPC-патчи, и регрессия
# жила незамеченной. Здесь один вход, один код возврата и лог в `_diag/ci-*.log`.
#
# Запуск:
#   pwsh -File scripts/ci.ps1                 # unit + e2e (быстро, без GPU-нагрузки)
#   pwsh -File scripts/ci.ps1 -Live           # + живая регрессия балансера
#   pwsh -File scripts/ci.ps1 -Live -Clients  # + обязательный контракт клиентов (OpenAI+Ollama)
#   pwsh -File scripts/ci.ps1 -Live -Model "Qwen3.8-27B-UD-Q4_K_M.gguf"
#   pwsh -File scripts/ci.ps1 -SkipUnit -SkipE2E   # только то, что указано
#
# Код возврата: 0 — всё зелёное, 1 — есть провалы (в конце печатается список).
[CmdletBinding()]
param(
    [switch]$Live,
    # R229: обязательный прогон контракта клиентов (contract-suite) против стенда.
    [switch]$Clients,
    [switch]$SkipUnit,
    [switch]$SkipE2E,
    [string]$Model = "gemma-4-E4B-it-Q4_K_M.gguf",
    [string]$Base = "http://127.0.0.1:18080",
    [string]$WebUI = "http://127.0.0.1:18083",
    [int]$TimeoutSec = 300
)

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root

$diag = Join-Path $root '_diag'
if (-not (Test-Path $diag)) { New-Item -ItemType Directory -Path $diag | Out-Null }
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$log = Join-Path $diag ("ci-{0}.log" -f $stamp)

$failures = @()
$passed = 0

function Section([string]$title) {
    Write-Host ''
    Write-Host ("=== " + $title + " ===") -ForegroundColor Cyan
    Add-Content -Path $log -Value ("=== " + $title + " ===")
}

function Run([string]$name, [scriptblock]$cmd) {
    Section $name
    $out = & $cmd 2>&1
    $ok = ($LASTEXITCODE -eq 0)
    $out | Add-Content -Path $log
    if (-not $ok) {
        $script:failures += $name
        Write-Host ("  FAIL  " + $name) -ForegroundColor Red
        $out | Select-Object -Last 12 | ForEach-Object { Write-Host ("        " + $_) }
    } else {
        $script:passed++
        Write-Host ("  PASS  " + $name) -ForegroundColor Green
    }
}

# ── 1. Юнит-тесты + JS (scripts/run-checks.ps1) ────────────────────────────
if (-not $SkipUnit) {
    Run 'unit (Go + JS)' { powershell -NoProfile -File scripts/run-checks.ps1 }
}

# ── 2. Живая регрессия балансера (нужен поднятый стенд) ────────────────────
if ($Live) {
    Run 'live regression' {
        powershell -NoProfile -File scripts/run-live-checks.ps1 -Model $Model -Base $Base -TimeoutSec $TimeoutSec
    }
}

# ── 2b. Контракт клиентов (R229): contract-suite — ОБЯЗАТЕЛЬНЫЙ шаг ────────
#
# ЗАЧЕМ ОТДЕЛЬНЫЙ ФЛАГ. Живая регрессия проверяет балансер, но не проверяет то,
# что важно клиенту: история доезжает, инструменты приходят в обоих API, стрим не
# врёт, ошибки приходят ошибками. `scripts/clients/contract-suite.js` — именно
# этот набор. Пока он запускался вручную, «зелёный» прогон мог не проверить
# клиентский путь вовсе (живой случай R228: сломанный apply и флейк истории
# всплыли уже после «проверок»). Теперь флаг -Clients делает его обязательным:
# любой FAIL — провал прогона.
#
# Запуск по правилу «на каждый образ»:
#   pwsh -File scripts/ci.ps1 -Live -Clients
if ($Clients) {
    Run 'contract-suite (клиенты: OpenAI + Ollama)' {
        $prevModel = $env:MODEL
        $prevBase = $env:BASE_OPENAI
        try {
            # MODEL в contract-suite — имя модели без .gguf (так её зовёт воркер).
            $env:MODEL = ($Model -replace '\.gguf$', '')
            # -Base — порт Ollama; OpenAI-поверхность живёт на 18079 (Base-1).
            if ($Base -match '^(http://[^:]+):(\d+)$') {
                $env:BASE_OPENAI = $Matches[1] + ':' + ([int]$Matches[2] - 1)
            }
            node scripts/clients/contract-suite.js
        } finally {
            $env:MODEL = $prevModel
            $env:BASE_OPENAI = $prevBase
        }
    }
}

# ── 3. WebUI: вход и роли (Playwright, нужен веб-стенд) ────────────────────
if (-not $SkipE2E) {
    if (Test-Path 'node_modules/@playwright/test') {
        Run 'webui auth/roles e2e' { node scripts/webui-e2e/check-auth-roles-r222.js }
    } else {
        Write-Host '  SKIP  webui auth/roles e2e (playwright не установлен)' -ForegroundColor Yellow
        Add-Content -Path $log -Value 'SKIP webui e2e: playwright не установлен'
    }
}

# ── Итог ───────────────────────────────────────────────────────────────────
Section 'ИТОГ'
Write-Host ("  шагов пройдено: {0}, провалов: {1}" -f $passed, $failures.Count)
Write-Host ("  лог: {0}" -f $log)
foreach ($f in $failures) { Write-Host ("   - " + $f) -ForegroundColor Red }

Pop-Location
if ($failures.Count -gt 0) { exit 1 }
Write-Host '  всё зелёное'
exit 0
