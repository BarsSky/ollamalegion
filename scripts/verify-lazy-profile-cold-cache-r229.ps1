# scripts/verify-lazy-profile-cold-cache-r229.ps1 — R229 (2026-10-23).
#
# ПРОВЕРКА СИТУАЦИИ, которой не было в автотестах: «воркер пересоздан, пока
# балансер ещё не отвечал → кэш профилей пуст → ПЕРВАЯ ЖЕ ленивая загрузка обязана
# подтянуть профили (R228, 91d0852) и применить их (R229, ctx=12288, а не env-дефолт
# 32768)».
#
# ПОЧЕМУ ОТДЕЛЬНЫЙ СКРИПТ, А НЕ UNIT-ТЕСТ. Сценарий по определению живёт на стенде:
# он ОСТАНАВЛИВАЕТ балансер и перезапускает воркер. Unit-тесты проверяют логику
# fetchNow на httptest-балансере; здесь проверяется настоящая последовательность
# событий и настоящий лог воркера.
#
# Что делает (и как читать вывод):
#   1. выгружает модель (загрузка должна быть именно ленивой);
#   2. останавливает балансер и перезапускает воркер → в логе должно появиться
#      «profileSyncer: initial fetch failed» (кэш пуст);
#   3. поднимает балансер и ждёт healthy;
#   4. шлёт ПЕРВЫЙ запрос напрямую в воркер (ленивая загрузка);
#   5. проверяет лог: «profiles fetched» ПОСЛЕ неудачного старта, «окно загрузки
#      взято из профиля модели» и «применяю профиль модели» с contextLength=12288;
#   6. проверяет /api/models: state=loaded, context_size=12288;
#   7. выгружает модель (стенд остаётся чистым).
#
# Запуск: pwsh -File scripts/verify-lazy-profile-cold-cache-r229.ps1
# Код возврата: 0 — PASS, 1 — FAIL (с причиной в выводе).
#
# ВНИМАНИЕ: скрипт меняет состояние стенда (стоп балансера ~15-25 с, рестарт
# воркера). Не запускайте параллельно с ci.ps1/живыми проверками.
[CmdletBinding()]
param(
    [string]$Model = 'gemma-4-E4B-it-Q4_K_M',
    [string]$WorkerContainer = 'ol-stack-cppworker-gpu',
    [string]$BalancerContainer = 'ol-stack-balancer',
    [string]$WorkerUrl = 'http://127.0.0.1:18092',
    [int]$LoadTimeoutSec = 300,
    [int]$HealthyWaitSec = 60
)

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root

# Токен берём из deployments/.env (единый источник на стенде), не хардкодим.
$token = ''
$envFile = Join-Path $root 'deployments/.env'
if (Test-Path $envFile) {
    $line = Select-String -Path $envFile -Pattern '^CPPWORKER_API_TOKEN=' | Select-Object -First 1
    if ($line) { $token = ($line.Line -split '=', 2)[1].Trim() }
}
$headers = @{ 'Content-Type' = 'application/json' }
if ($token) { $headers['X-API-Token'] = $token }

$fails = @()
function Check([bool]$Ok, [string]$What, [string]$Detail) {
    if ($Ok) {
        Write-Host ('  PASS  ' + $What) -ForegroundColor Green
    } else {
        Write-Host ('  FAIL  ' + $What + ' :: ' + $Detail) -ForegroundColor Red
        $script:fails += ($What + ' :: ' + $Detail)
    }
}
function Api([string]$Method, [string]$Path, $Body) {
    $params = @{ Uri = ($script:WorkerUrl + $Path); Method = $Method; Headers = $script:headers; UseBasicParsing = $true; TimeoutSec = $script:LoadTimeoutSec + 30 }
    if ($null -ne $Body) { $params['Body'] = ($Body | ConvertTo-Json -Depth 6) }
    try { return (Invoke-WebRequest @params).Content } catch { return $null }
}
function WaitHealthy([string]$Container, [int]$Sec) {
    $deadline = (Get-Date).AddSeconds($Sec)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 2
        $st = (docker inspect $Container --format '{{.State.Health.Status}}' 2>$null) -join ''
        if ($st.Trim() -eq 'healthy') { return $true }
    }
    return $false
}

Write-Host '=== R229: холодный кэш профилей → первая ленивая загрузка применяет профиль ===' -ForegroundColor Cyan

# 1. Выгружаем модель, чтобы загрузка была ленивой.
$un = Api 'POST' ('/api/models/unload?name=' + [uri]::EscapeDataString($Model) + '&force=true') $null
Write-Host ('  модель выгружена: ' + $(if ($null -eq $un) { '404/уже выгружена' } else { 'HTTP 200' })) -ForegroundColor DarkGray

# 2. Балансер — стоп, воркер — restart (кэш профилей пуст).
docker stop $BalancerContainer | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Host 'FAIL: не удалось остановить балансер' -ForegroundColor Red; Pop-Location; exit 1 }
# Docker-логи мечены в UTC: маркер времени тоже обязан быть UTC, иначе --since
# уедет на часовой пояс и «в логе ничего нет» будет ложным выводом.
$restartAt = [DateTime]::UtcNow
docker restart $WorkerContainer | Out-Null
Start-Sleep -Seconds 15

$startLog = (docker logs $WorkerContainer --since ($restartAt.ToString('yyyy-MM-ddTHH:mm:ssZ')) 2>&1 | Out-String)
$initialFailed = $startLog -match 'initial fetch failed'
$initialFetched = ($startLog -match 'profiles fetched')
Check $initialFailed 'старт без балансера: профили не подтянулись (кэш пуст)' ($startLog | Select-String -Pattern 'initial|profileSyncer' | Select-Object -Last 3 | Out-String)
Check (-not $initialFetched) 'до подъёма балансера профилей в кэше нет' 'в стартовом логе уже есть profiles fetched — сценарий недостоверен'

# 3. Поднимаем балансер.
docker start $BalancerContainer | Out-Null
$healthy = WaitHealthy $BalancerContainer $HealthyWaitSec
Check $healthy 'балансер снова healthy' ('не стал healthy за ' + $HealthyWaitSec + ' с')

# 4. Первый запрос — ленивая загрузка (напрямую в воркер, без участия балансера в маршрутизации).
$loadAt = [DateTime]::UtcNow
$chat = Api 'POST' '/api/chat' @{ model = $Model; stream = $false; messages = @(@{ role = 'user'; content = 'Ответь одним словом: ок.' }); options = @{ num_predict = 5 } }
Check ($null -ne $chat) 'первый запрос обслужен (ленивая загрузка)' 'нет ответа/таймаут'

# 5. Лог загрузки: профили подтянуты ПОСЛЕ неудачи старта + профиль применён.
$loadLog = (docker logs $WorkerContainer --since ($loadAt.AddSeconds(-2).ToString('yyyy-MM-ddTHH:mm:ssZ')) 2>&1 | Out-String)
$fetchedAtLoad = $loadLog -match 'profiles fetched'
$windowFromProfile = $loadLog -match 'окно загрузки взято из профиля модели'
$applied = $loadLog -match 'применяю профиль модели'
$ctx12288 = $loadLog -match '"contextLength":12288'
Check $fetchedAtLoad 'профили подтянуты по требованию загрузки (fetchNow)' 'в логе загрузки нет «profiles fetched»'
Check ($windowFromProfile -and $applied) 'загрузка применила профиль модели (окно и параметры)' 'нет строк про профиль модели'
Check $ctx12288 'профиль модели — contextLength=12288, а не env 32768' 'в логе нет contextLength=12288'

# 6. Факт воркера.
$modelsRaw = Api 'GET' '/api/models' $null
$ctx = $null
$state = $null
if ($modelsRaw) {
    $j = $modelsRaw | ConvertFrom-Json
    $m = ($j.models | Select-Object -First 1)
    if ($m) { $state = $m.state; $ctx = $m.context_size }
}
Check ($state -eq 'loaded') 'модель загружена' ('state=' + $state)
Check ($ctx -eq 12288) 'окно модели = 12288 (из профиля)' ('context_size=' + $ctx)

# 7. Чистим стенд.
Api 'POST' ('/api/models/unload?name=' + [uri]::EscapeDataString($Model) + '&force=true') $null | Out-Null

Write-Host ''
if ($fails.Count -gt 0) {
    Write-Host ('ИТОГ: ПРОВАЛ (' + $fails.Count + ')') -ForegroundColor Red
    $fails | ForEach-Object { Write-Host ('  - ' + $_) -ForegroundColor Red }
    Pop-Location
    exit 1
}
Write-Host 'ИТОГ: PASS — пустой кэш профилей дотягивается при загрузке, профиль применён (ctx=12288)' -ForegroundColor Green
Pop-Location
exit 0
