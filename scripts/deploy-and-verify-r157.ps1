# deploy-and-verify-r157.ps1 — раскатка образа с канонизацией имени и приёмка бага
# «алиас ломает обслуживание».
#
# ЧТО ПРОВЕРЯЕМ (живой дефект 2026-10-16, доказан на обоих бэкендах):
#   загрузка под алиасом `gemma-4` регистрировала модель в воркере под алиасом, и
#   запрос по КАНОНИЧЕСКОМУ имени падал: HTTP 500 «model gemma-4-E4B-it-Q4_K_M not
#   found» → клиент получал 502. R157 канонизирует имя по владельцу файла ДО
#   регистрации и сообщает замену заголовками X-Model-Canonical/X-Model-Requested.
#
# КРИТЕРИИ ПРИЁМКИ:
#   1. контейнер поднялся на новом образе и healthy;
#   2. load {"name":"gemma-4"} → заголовок X-Model-Canonical = gemma-4-E4B-it-Q4_K_M;
#   3. /api/v1/models знает КАНОНИЧЕСКОЕ имя (алиаса в реестре нет);
#   4. chat по каноническому имени → 200 с ответом (было 500/502);
#   5. в логе воркера есть «канонизировано по владельцу файла».
$ErrorActionPreference = 'Continue'
$root = 'C:\Ollama\ollamalegion'
Set-Location $root

$worker   = 'http://127.0.0.1:18092'   # воркер напрямую
$balancer = 'http://127.0.0.1:18080'   # клиентский вход балансера (Ollama API)
$alias     = 'gemma-4'
$canonical = 'gemma-4-E4B-it-Q4_K_M'
$img = 'ollama-legion/cppworker:gpu-r157-canonical'

function Step($t) { Write-Host "`n=== $t ===" }

Step '0. Образ на месте'
$have = docker images --format '{{.Repository}}:{{.Tag}}' | Where-Object { $_ -eq $img }
if (-not $have) { Write-Host "НЕТ ОБРАЗА $img — приёмка невозможна"; exit 2 }
Write-Host "ok: $img"

Step '1. Тег в .env и пересоздание контейнера'
$envPath = Join-Path $root 'deployments\.env'
$raw = Get-Content $envPath -Raw
$raw2 = $raw -replace '(?m)^CPPWORKER_GPU_TAG=.*$', 'CPPWORKER_GPU_TAG=r157-canonical'
if ($raw2 -ne $raw) { Set-Content -Path $envPath -Value $raw2 -NoNewline; Write-Host 'CPPWORKER_GPU_TAG=r157-canonical' }
docker compose -f deployments\docker-compose.stack.yml up -d cppworker-gpu 2>&1 | Select-Object -Last 3

Step '2. Ждём healthy'
$ok = $false
for ($i = 0; $i -lt 60; $i++) {
    Start-Sleep -Seconds 3
    $st = docker inspect -f '{{.State.Health.Status}}' ol-stack-cppworker-gpu 2>$null
    if ($st -eq 'healthy') { $ok = $true; break }
}
if (-not $ok) { Write-Host 'контейнер не стал healthy'; docker logs --tail 20 ol-stack-cppworker-gpu; exit 3 }
Write-Host 'healthy'

Step '3. Выгружаем текущий инстанс'
try {
    $r = Invoke-WebRequest -Method Post -Uri "$worker/api/models/unload?name=$canonical" -TimeoutSec 60
    Write-Host ("unload -> " + [int]$r.StatusCode + " " + $r.Content)
} catch { Write-Host ("unload: " + $_.Exception.Message) }
Start-Sleep -Seconds 3

Step '4. Загрузка ПОД АЛИАСОМ (это и был дефект)'
$loadHdr = $null
try {
    $r = Invoke-WebRequest -Method Post -Uri "$worker/api/models/load" -TimeoutSec 300 `
        -ContentType 'application/json' -Body ('{"name":"' + $alias + '"}') -UseBasicParsing
    $loadHdr = $r.Headers
    Write-Host ("load alias -> " + [int]$r.StatusCode)
} catch {
    Write-Host ("load alias ОШИБКА: " + $_.Exception.Message)
    if ($_.Exception.Response) {
        Write-Host ("  статус: " + [int]$_.Exception.Response.StatusCode)
        $loadHdr = $_.Exception.Response.Headers
    }
}
if ($loadHdr) {
    Write-Host ("  X-Model-Canonical: " + ($loadHdr['X-Model-Canonical'] -join ','))
    Write-Host ("  X-Model-Requested: " + ($loadHdr['X-Model-Requested'] -join ','))
}
Start-Sleep -Seconds 8

Step '5. Что в реестре воркера'
try {
    $m = Invoke-RestMethod -Uri "$worker/api/models" -TimeoutSec 30
    $names = @()
    foreach ($x in $m.models) { $names += $x.name }
    if (-not $names -and $m.data) { foreach ($x in $m.data) { $names += $x.id } }
    Write-Host ("имена: " + ($names -join ', '))
    if ($names -contains $canonical) { Write-Host 'ok: каноническое имя в реестре' }
    else { Write-Host 'ПЛОХО: канонического имени в реестре нет' }
    if ($names -contains $alias) { Write-Host 'ВНИМАНИЕ: в реестре остался алиас' }
} catch { Write-Host ("models: " + $_.Exception.Message) }

Step '6. Запрос по КАНОНИЧЕСКОМУ имени (раньше 500/502)'
$body = '{"model":"' + $canonical + '","messages":[{"role":"user","content":"2+2"}],"stream":false,"options":{"num_predict":8}}'
try {
    $r = Invoke-WebRequest -Method Post -Uri "$balancer/api/chat" -ContentType 'application/json' `
        -Body $body -TimeoutSec 180
    Write-Host ("chat canonical -> " + [int]$r.StatusCode)
    $txt = $r.Content
    Write-Host ('  ответ: ' + $txt.Substring(0, [Math]::Min(200, $txt.Length)))
} catch {
    Write-Host ("chat canonical ОШИБКА: " + $_.Exception.Message)
    if ($_.Exception.Response) { Write-Host ("  статус: " + [int]$_.Exception.Response.StatusCode) }
}

Step '7. Запрос по АЛИАСУ (для полноты картины)'
try {
    $r = Invoke-WebRequest -Method Post -Uri "$balancer/api/chat" -ContentType 'application/json' `
        -Body ($body -replace [regex]::Escape($canonical), $alias) -TimeoutSec 180
    Write-Host ("chat alias -> " + [int]$r.StatusCode)
} catch {
    Write-Host ("chat alias: " + $_.Exception.Message)
    if ($_.Exception.Response) { Write-Host ("  статус: " + [int]$_.Exception.Response.StatusCode) }
}

Step '8. Лог воркера: факт канонизации'
docker logs --since 15m ol-stack-cppworker-gpu 2>&1 | Select-String -Pattern 'канонизировано|владельца файла' |
    Select-Object -Last 4 | ForEach-Object { '  ' + $_.Line.Trim() }

Write-Host "`nГОТОВО"
