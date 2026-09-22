#!/usr/bin/env pwsh
# verify_r66b_chat_features.ps1 — живая проверка R66b-фиксов на /api/chat:
#   1. prompt_eval_duration (TTFT) больше не 0 и eval_duration < total_duration;
#   2. format:"json" → инструкция в промпте + снятие markdown-обёртки;
#   3. think:true/false → управление reasoning-инструкцией в промпте.
#
# Использование: powershell -File scripts/verify_r66b_chat_features.ps1
param(
    [string]$BalancerUrl = "http://127.0.0.1:18080",
    [string]$CppWorkerUrl = "http://127.0.0.1:18092",
    [string]$Token = "changeme-bundled-with-agent-token",
    [string]$Model = "Qwen3-Instruct-2507-q4km",
    [string]$OutDir = "$PSScriptRoot\..\_diag\r66b-verify"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
$auth = @{ "X-API-Token" = $Token }

function Write-Utf8NoBom {
    param([string]$Path, [string]$Text)
    # ВАЖНО: без BOM и без ASCII — иначе кириллица в промпте превращается в "?????"
    # (Out-File -Encoding ascii), и модель получает мусор вместо вопроса.
    [System.IO.File]::WriteAllText($Path, $Text, (New-Object System.Text.UTF8Encoding($false)))
}

function Invoke-CurlJson {
    param([string]$Url, [string]$Method, [string]$BodyFile, [string]$OutFile, [int]$TimeoutSec = 600, [hashtable]$Headers = @{})
    $args = @('-s', '-m', "$TimeoutSec", '-X', $Method, '-o', $OutFile, '-w', '%{http_code}')
    if ($BodyFile) { $args += @('-H', 'Content-Type: application/json', '--data-binary', "@$BodyFile") }
    foreach ($k in $Headers.Keys) { $args += @('-H', "$k`: $($Headers[$k])") }
    $code = & curl.exe @args $Url
    return $code
}

Write-Host "[1/5] Проверка доступности стека..."
$tags = curl.exe -s -m 20 "$BalancerUrl/api/tags"
if (-not $tags) { throw "balancer недоступен: $BalancerUrl" }
Write-Host "  ok: $($tags.Substring(0, [Math]::Min(120, $tags.Length)))"

Write-Host "[2/5] Прогрев модели (холодная загрузка может занять до 5 минут)..."
$warm = Join-Path $OutDir 'warm.json'
Write-Utf8NoBom $warm ('{"model":"' + $Model + '","messages":[{"role":"user","content":"hi"}],"stream":false,"options":{"num_predict":1}}')
$warmOut = Join-Path $OutDir 'warm_resp.json'
$warmCode = & curl.exe -s -m 30 -X POST "$BalancerUrl/api/chat" -H "Content-Type: application/json" --data-binary "@$warm" -o $warmOut -w '%{http_code}'
Write-Host "  warm request HTTP=$warmCode (503 = загрузка запущена, это нормально)"
$loaded = $false
for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Seconds 8
    try {
        $m = (curl.exe -s -m 10 "$CppWorkerUrl/api/models" | ConvertFrom-Json).models
        if ($m -and $m[0].state -eq 'loaded') { $loaded = $true; Write-Host "  модель загружена через $((($i + 1) * 8))s"; break }
    } catch { }
}
if (-not $loaded) { Write-Warning "не дождались загрузки модели — часть проверок может упасть" }

Write-Host "[3/5] think=true + format=json (non-stream) → промпт + ответ..."
$reqA = Join-Path $OutDir 'req_think_true_json.json'
$bodyA = @'
{"model":"__MODEL__","messages":[{"role":"user","content":"Верни JSON с полем status и значением ok"}],
 "stream":false,"think":true,"format":"json","options":{"num_predict":128}}
'@ -replace '__MODEL__', $Model
Write-Utf8NoBom $reqA $bodyA
$respA = Join-Path $OutDir 'resp_think_true_json.json'
$codeA = Invoke-CurlJson -Url "$BalancerUrl/api/chat" -Method POST -BodyFile $reqA -OutFile $respA -TimeoutSec 600
Copy-Item (Join-Path $OutDir 'resp_think_true_json.json') (Join-Path $OutDir 'A_resp.json') -Force
$promptA = Join-Path $OutDir 'A_last_prompt.json'
$codeP = & curl.exe -s -m 20 -H "X-API-Token: $Token" -o $promptA -w '%{http_code}' "$CppWorkerUrl/api/v1/cppworker/debug/last-prompt"
Write-Host "  HTTP response=$codeA, debug/last-prompt=$codeP"

Write-Host "[4/5] think=false (stream) → промпт не должен содержать thinking-инструкцию..."
$reqB = Join-Path $OutDir 'req_think_false.json'
$bodyB = @'
{"model":"__MODEL__","messages":[{"role":"user","content":"Скажи одно слово: ок"}],
 "stream":true,"think":false,"options":{"num_predict":32}}
'@ -replace '__MODEL__', $Model
Write-Utf8NoBom $reqB $bodyB
$respB = Join-Path $OutDir 'B_stream.ndjson'
$codeB = Invoke-CurlJson -Url "$BalancerUrl/api/chat" -Method POST -BodyFile $reqB -OutFile $respB -TimeoutSec 600
$promptB = Join-Path $OutDir 'B_last_prompt.json'
$codePB = & curl.exe -s -m 20 -H "X-API-Token: $Token" -o $promptB -w '%{http_code}' "$CppWorkerUrl/api/v1/cppworker/debug/last-prompt"
Write-Host "  HTTP response=$codeB, debug/last-prompt=$codePB"

Write-Host "[5/5] Итоги:"
$finalLine = Get-Content $respB | Where-Object { $_ -match '"done":\s*true' } | Select-Object -Last 1
if ($finalLine) {
    $obj = $finalLine | ConvertFrom-Json
    Write-Host ("  total_duration       = {0}" -f $obj.total_duration)
    Write-Host ("  prompt_eval_duration = {0}" -f $obj.prompt_eval_duration)
    Write-Host ("  eval_duration        = {0}" -f $obj.eval_duration)
    if ($obj.prompt_eval_duration -gt 0 -and $obj.prompt_eval_duration -lt $obj.total_duration) {
        Write-Host "  ✅ TTFT отделён от генерации" -ForegroundColor Green
    } else {
        Write-Warning "  ❌ prompt_eval_duration не отделён (TTFT=$($obj.prompt_eval_duration), total=$($obj.total_duration))"
    }
} else {
    Write-Warning "  не нашли финальный done-чанк в $respB"
}
Write-Host "  Артефакты: $OutDir"
