# scripts/auto-continue-modes.ps1 — R118.17 (2026-10-15).
#
# ЖИВАЯ ПРОВЕРКА авто-продолжения балансера при обрыве ответа по капу.
#
# ЗАЧЕМ. Флаг LB_AUTO_CONTINUE_ON_TRUNCATION (R60.21) — операторский opt-in:
# когда upstream обрывает ответ по лимиту токенов (done_reason=length, например
# незакрытый блок ```), балансер сам досылает «продолжи» и склеивает ответ,
# чтобы клиент (Cline/OpenWebUI) не считал оборванную середину полным ответом.
# Проверка «в двух режимах» = прогнать ОДИН И ТОТ ЖЕ запрос с флагом 0 и 1 и
# сравнить: длина, done_reason, закрыт ли блок.
#
# Запуск (флаг берётся из контейнера балансера, менять его так:
#   deployments: docker compose -f docker-compose.stack.yml -f <override> up -d --no-deps loadbalancer
#             (в override — environment: LB_AUTO_CONTINUE_ON_TRUNCATION=0)):
#   & .\scripts\auto-continue-modes.ps1
#   & .\scripts\auto-continue-modes.ps1 -NumPredict 96 -Model 'Qwen3.8-27B-UD-Q4_K_M'
#
# Скрипт НИЧЕГО не меняет: только шлёт запрос и печатает факты. Проверять надо
# на ЗАГРУЖЕННОЙ модели (иначе запрос сначала её поднимет).

[CmdletBinding()]
param(
    [string]$Balancer = 'http://127.0.0.1:18080',
    [string]$Token = 'changeme-bundled-with-agent-token',
    [string]$Model = 'gemma-4-E4B-it-Q4_K_M',
    [int]$NumPredict = 48,
    [string]$Container = 'ol-stack-balancer',
    [int]$TimeoutSec = 600
)

$ErrorActionPreference = 'Stop'

function Get-FlagFromContainer {
    param([string]$Name)
    try {
        $envLine = (docker exec $Name env 2>$null | Select-String -Pattern '^LB_AUTO_CONTINUE_ON_TRUNCATION=' | Select-Object -First 1)
        if ($envLine) { return ($envLine.Line -split '=', 2)[1].Trim() }
        return '(не задан → выключено)'
    } catch {
        return '(docker недоступен)'
    }
}

$flag = Get-FlagFromContainer -Name $Container
Write-Host "=== авто-продолжение: живая проверка ===" -ForegroundColor Cyan
Write-Host "балансер      : $Balancer"
Write-Host "модель        : $Model"
Write-Host "num_predict   : $NumPredict (кап, на котором ожидается обрыв)"
Write-Host "флаг в контейнере $Container : LB_AUTO_CONTINUE_ON_TRUNCATION=$flag"
Write-Host ''

$prompt = @'
Напиши полный код на Python для баллистической симуляции с сопротивлением воздуха.
Только код в одном блоке ```python ... ```, без пояснений. Код должен быть длинным:
классы, функции, графики, валидация входных данных.
'@

$body = @{
    model    = $Model
    messages = @(@{ role = 'user'; content = $prompt })
    stream   = $true
    options  = @{ num_predict = $NumPredict }
} | ConvertTo-Json -Depth 6

# ВАЖНО: тело и ответ гоняем через файлы (UTF-8 без BOM) и curl.exe.
# Invoke-RestMethod в PowerShell 5.1 отправляет строку как Latin-1 и декодирует
# ответ по кодовой странице консоли — русский промпт превращался в мусор, и
# модель отвечала не на тот вопрос (проверка теряла смысл).
$tmpDir = Join-Path $env:TEMP ("auto-continue-{0}" -f (Get-Date -Format 'HHmmss'))
New-Item -ItemType Directory -Path $tmpDir | Out-Null
$reqFile = Join-Path $tmpDir 'req.json'
$respFile = Join-Path $tmpDir 'resp.json'
[System.IO.File]::WriteAllText($reqFile, $body, (New-Object System.Text.UTF8Encoding($false)))

$sw = [Diagnostics.Stopwatch]::StartNew()
$status = & curl.exe -s -o $respFile -w '%{http_code}' -X POST "$Balancer/api/chat" `
    -H "X-API-Token: $Token" -H 'Content-Type: application/json; charset=utf-8' `
    --data-binary "@$reqFile" --max-time $TimeoutSec
$elapsed = [int]$sw.Elapsed.TotalSeconds

if ($status -ne '200') {
    $raw = if (Test-Path $respFile) { [System.IO.File]::ReadAllText($respFile, [System.Text.Encoding]::UTF8) } else { '' }
    Write-Host ("HTTP {0}: {1}" -f $status, $raw) -ForegroundColor Red
    exit 1
}

# ВАЖНО: ответ читаем построчно (NDJSON). Авто-продолжение живёт в СТРИМИНГОВОМ
# транспорте (llamacpp_transport.go: proxyRequestLlamaCpp) — на не-стриминговом
# пути его нет, поэтому проверять надо потоком (Cline/OpenCode и так стримят).
$raw = [System.IO.File]::ReadAllText($respFile, [System.Text.Encoding]::UTF8)
$contentParts = New-Object System.Collections.Generic.List[string]
$reason = ''
$done = $false
foreach ($line in ($raw -split "`n")) {
    $t = $line.Trim()
    if (-not $t) { continue }
    try { $obj = $t | ConvertFrom-Json } catch { continue }
    if ($obj.message -and $obj.message.content) { $contentParts.Add([string]$obj.message.content) }
    if ($obj.done) { $done = $true; if ($obj.done_reason) { $reason = [string]$obj.done_reason } }
}
$content = ($contentParts -join '')
$backticks = ([regex]::Matches($content, '```')).Count
$closed = ($backticks % 2 -eq 0)
$reason = $reason
$tail = if ($content.Length -gt 200) { $content.Substring($content.Length - 200) } else { $content }

Write-Host '--- факт ---'
Write-Host ("chars        : {0}" -f $content.Length)
Write-Host ("elapsed      : {0}s" -f $elapsed)
Write-Host ("done_reason  : {0}" -f $reason)
Write-Host ("блоков ```   : {0} (чётное = блок закрыт: {1})" -f $backticks, $closed)
Write-Host ("хвост ответа : {0}" -f ($tail -replace "`r?`n", ' | '))
Write-Host ''
Write-Host 'Сравнение режимов:' -ForegroundColor Cyan
Write-Host '  флаг выключен → клиент видит обрыв (done_reason=length, блок не закрыт);'
Write-Host '  флаг включён  → балансер досылает «продолжи», ответ длиннее и закрыт.'
