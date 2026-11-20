# live_prefix_cache_check.ps1 — R658 (2026-11-20): живая проверка переиспользования
# KV-префилла на воркере.
#
# ЗАЧЕМ. В R493 в мост добавлен кэш префилла: второй запрос с общим началом промпта
# (обычный ход агента: тот же system+tools, растущая история) должен подхватывать
# уже посчитанный префикс. На гибридных моделях (attention + recurrent, например
# Qwen3.5/3.8, arch=qwen35) частичный откат recurrent-состояния невозможен, и до
# R658 мост игнорировал неудачу обрезки: декод начинался с позиции общего префикса,
# память оставалась от прошлого промпта — llama_decode падал
# `decode: failed to initialize batch`, клиент получал 500
# «llama_decode failed for prompt batch» с ложной подсказкой про VRAM.
#
# Что проверяет этот скрипт: ДВА запроса с одинаковым префиксом промпта подряд
# оба отвечают 200. Именно второй запрос падал до R658.
#
# ЗАПУСК (стенд, воркер напрямую):
#   pwsh -File scripts/live_prefix_cache_check.ps1 `
#        -BaseUrl http://127.0.0.1:18092 -Model Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M `
#        -Token $env:CPPWORKER_API_TOKEN
#
# Модель должна быть уже загружена (скрипт не грузит её сам: окно выбирает стенд).
# Код возврата: 0 — оба запроса 200; 1 — есть отказ (печатается bridge_info).
param(
    [string]$BaseUrl = "http://127.0.0.1:18092",
    [Parameter(Mandatory = $true)][string]$Model,
    [string]$Token = "",
    [int]$NumPredict = 16,
    [int]$FillerRepeats = 70
)

$ErrorActionPreference = "Continue"
$headers = @{ "Content-Type" = "application/json" }
if ($Token) { $headers["X-API-Token"] = $Token }

$prefix = "R658 prefix cache check. " + ("Shared prefix probe sentence. " * $FillerRepeats)

function Send-Chat([string]$tail) {
    $body = @{
        model    = $Model
        messages = @(
            @{ role = "system"; content = $prefix },
            @{ role = "user"; content = ("Tail: " + $tail) }
        )
        stream   = $false
        options  = @{ num_predict = $NumPredict; temperature = 0 }
    } | ConvertTo-Json -Depth 6

    try {
        $r = Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/api/chat" -Method POST -Headers $headers -Body $body -TimeoutSec 300
        return @{ ok = $true; status = $r.StatusCode; detail = "" }
    } catch {
        $detail = $_.Exception.Message
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            try {
                $bi = ($_.ErrorDetails.Message | ConvertFrom-Json).bridge_info
                if ($bi) { $detail = "code=$($bi.code) actual_tokens=$($bi.actual_tokens) n_ctx=$($bi.current_n_ctx): $($bi.message)" }
                else { $detail = $_.ErrorDetails.Message }
            } catch { $detail = $_.ErrorDetails.Message }
        }
        return @{ ok = $false; status = $_.Exception.Response.StatusCode.value__; detail = $detail }
    }
}

Write-Host "R658: два запроса с общим префиксом (второй падал до фикса)" -ForegroundColor Cyan
$first = Send-Chat "alpha"
if (-not $first.ok) {
    Write-Host ("  FAIL первый запрос: HTTP {0} {1}" -f $first.status, $first.detail) -ForegroundColor Red
    exit 1
}
Write-Host ("  OK  первый запрос: HTTP {0}" -f $first.status) -ForegroundColor DarkGray

$second = Send-Chat "beta"
if (-not $second.ok) {
    Write-Host ("  FAIL второй запрос (общий префикс): HTTP {0} {1}" -f $second.status, $second.detail) -ForegroundColor Red
    Write-Host "       Это и есть дефект R658: мост переиспользовал префикс, который память не откатила." -ForegroundColor Yellow
    exit 1
}
Write-Host ("  OK  второй запрос (общий префикс): HTTP {0}" -f $second.status) -ForegroundColor DarkGray

Write-Host ""
Write-Host "R658: переиспользование префилла не ломает декод" -ForegroundColor Green
exit 0
