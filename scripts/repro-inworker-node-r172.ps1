# scripts/repro-inworker-node-r172.ps1 — R172 (2026-10-18).
#
# ЗАЧЕМ. Авария «встроенный узел раскладки падает, а отдельный контейнер с тем же
# образом работает» наблюдалась только когда КЛИЕНТ на другой машине. Донор при
# этом может быть выключен, а гипотезу надо проверять. Скрипт воспроизводит ту же
# топологию ОДНОЙ машиной:
#
#   контейнер-СЕРВЕР (cppworker + дочерний rpc-server)   ← узел раскладки
#   контейнер-КЛИЕНТ (тот же образ, без GPU, только CPU)  ← «мастер»
#
# оба в одной пользовательской docker-сети, поэтому соединение идёт не через
# loopback, а как между машинами (имя контейнера, внешний интерфейс сети).
# Стенд (ol-stack-*) не затрагивается: контейнеры одноразовые, с префиксом r172-.
#
# ЧТО СМОТРИМ. Клиент после загрузки с раскладкой либо отвечает 200 (авария не
# воспроизвелась — значит она зависит от машины: карта/драйвер/VRAM), либо
# умирает с "Remote RPC server crashed or returned malformed response". Во втором
# случае серверная сторона уже объясняет причину: exit-статус и строки лога
# дочернего процесса (R172).
#
# PowerShell 5.1 (pwsh в этом окружении нет).
param(
    [string]$Image = 'ollama-legion/cppworker:gpu-r172-node-diag',
    [string]$Network = 'ollama-legion-stack_default',
    [string]$ModelsDir = 'D:\ollama-legion-models',
    [string]$Model = 'gemma-4-E4B-it-Q4_K_M',
    [int]$GpuLayers = 20,
    [int]$ContextSize = 4096,
    [int]$ServerApiPort = 18095,
    [int]$ClientApiPort = 18096,
    [int]$NodePort = 50052,
    [string]$Token = 'changeme-bundled-with-agent-token',
    # Клиенту можно дать свою карту — это и есть топология аварии: мастер грузит
    # модель на СВОЮ карту и на удалённый узел одновременно. Без этого клиент
    # держит все слои на CPU либо целиком на узле (тоже полезный край).
    [switch]$ClientWithGpu,
    [switch]$KeepContainers
)

$ErrorActionPreference = 'Continue'
$serverName = 'r172-node-server'
$clientName = 'r172-node-client'

function Remove-Containers {
    foreach ($n in @($clientName, $serverName)) {
        docker rm -f $n 2>&1 | Out-Null
    }
}

function Wait-Health([string]$url, [int]$timeoutSec = 90) {
    $deadline = (Get-Date).AddSeconds($timeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            $r = Invoke-WebRequest -Uri $url -TimeoutSec 5 -UseBasicParsing
            if ($r.StatusCode -eq 200) { return $true }
        } catch { }
        Start-Sleep -Milliseconds 700
    }
    return $false
}

Write-Host "=== 1. Проверка образа и сети ===" -ForegroundColor Cyan
$img = docker images --format '{{.Repository}}:{{.Tag}}' | Where-Object { $_ -eq $Image }
if (-not $img) { Write-Host "НЕТ ОБРАЗА $Image — сначала соберите scripts/build-containers.ps1 -CppWorker -Tag <tag>" -ForegroundColor Red; exit 2 }
$net = docker network ls --format '{{.Name}}' | Where-Object { $_ -eq $Network }
if (-not $net) {
    # Имя сети compose зависит от каталога запуска (deployments_default, ol-net и
    # т.п.). Вместо угадывания берём сеть, в которой реально живёт стенд.
    $detected = (docker inspect ol-stack-cppworker-gpu --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}} {{end}}' 2>$null)
    if ($detected) {
        $candidate = ($detected.Trim() -split '\s+')[0]
        if ($candidate) {
            Write-Host "сеть $Network не найдена — использую сеть стенда: $candidate" -ForegroundColor Yellow
            $Network = $candidate
            $net = $Network
        }
    }
}
if (-not $net) { Write-Host "НЕТ СЕТИ $Network (docker compose -f deployments/docker-compose.stack.yml up -d создаёт её)" -ForegroundColor Red; exit 2 }
Write-Host "образ: $Image`nсеть: $Network"

Write-Host "`n=== 2. Контейнер-СЕРВЕР (встроенный узел, отладка протокола) ===" -ForegroundColor Cyan
Remove-Containers
$serverArgs = @(
    'run', '-d', '--name', $serverName, '--network', $Network,
    '--gpus', 'all',
    '-p', "${ServerApiPort}:${ServerApiPort}",
    '-e', "CPPWORKER_PORT=$ServerApiPort",
    '-e', 'CPPWORKER_MODELS_DIR=/app/models',
    '-e', "CPPWORKER_API_TOKEN=$Token",
    '-e', 'CPPWORKER_RPC_SERVER_ENABLED=true',
    '-e', "CPPWORKER_RPC_SERVER_PORT=$NodePort",
    # Отладка протокола: ggml-rpc печатает каждую операцию на СЕРВЕРЕ.
    '-e', 'CPPWORKER_RPC_SERVER_DEBUG=true',
    # Перезапуск выключен на время разбора: новый цикл стирает PID/uptime/exit.
    '-e', 'CPPWORKER_RPC_SERVER_RESTART=false',
    '-e', 'CPPWORKER_REGISTER_DISABLE=true',
    '-v', "${ModelsDir}:/app/models",
    $Image
)
docker @serverArgs | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Host 'сервер не запустился' -ForegroundColor Red; exit 3 }

if ($ClientWithGpu) { Write-Host "`n=== 3. Контейнер-КЛИЕНТ (со своей картой + узел) ===" -ForegroundColor Cyan }
else { Write-Host "`n=== 3. Контейнер-КЛИЕНТ (карта скрыта, всё на узел) ===" -ForegroundColor Cyan }
$clientArgs = @(
    'run', '-d', '--name', $clientName, '--network', $Network,
    # GPU-образ линкуется с libcuda.so.1, поэтому без nvidia-рантайма бинарь
    # вообще не стартует (exit 127 "error while loading shared libraries").
    '--gpus', 'all',
    '-p', "${ClientApiPort}:${ClientApiPort}"
)
if (-not $ClientWithGpu) {
    # Карта скрыта: локальных устройств нет, слои уходят на узел через RPC.
    $clientArgs += @('-e', 'CUDA_VISIBLE_DEVICES=')
}
$clientArgs += @(
    '-e', "CPPWORKER_PORT=$ClientApiPort",
    '-e', 'CPPWORKER_MODELS_DIR=/app/models',
    '-e', "CPPWORKER_API_TOKEN=$Token",
    '-e', 'CPPWORKER_RPC_ENABLE=true',
    '-e', "CPPWORKER_RPC_SERVERS=${serverName}:$NodePort",
    '-e', 'CPPWORKER_SPLIT_MODE=1',
    '-e', 'CPPWORKER_REGISTER_DISABLE=true',
    '-v', "${ModelsDir}:/app/models",
    $Image
)
docker @clientArgs | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Host 'клиент не запустился' -ForegroundColor Red; exit 3 }

$serverUrl = "http://127.0.0.1:$ServerApiPort"
$clientUrl = "http://127.0.0.1:$ClientApiPort"
if (-not (Wait-Health "$serverUrl/api/info")) { Write-Host "сервер не ответил на $serverUrl/api/info" -ForegroundColor Red }
if (-not (Wait-Health "$clientUrl/api/info")) { Write-Host "клиент не ответил на $clientUrl/api/info" -ForegroundColor Red }

Write-Host "`n=== 4. Состояние узла у сервера ===" -ForegroundColor Cyan
try {
    # Узел отдаётся в /api/models (rpc_server), а не в /api/info: первое —
    # единственный дешёвый источник факта, и он же проверяет, что слой узла жив.
    $info = Invoke-RestMethod -Uri "$serverUrl/api/models" -Headers @{'X-API-Token'=$Token} -TimeoutSec 20
    $node = $info.rpc_server
    if ($node) { $node | ConvertTo-Json -Depth 4 } else { Write-Host 'в /api/models нет rpc_server' -ForegroundColor Yellow }
} catch { Write-Host "не удалось прочитать /api/models: $($_.Exception.Message)" -ForegroundColor Yellow }

Write-Host "`n=== 5. Загрузка модели КЛИЕНТОМ с раскладкой на узел ===" -ForegroundColor Cyan
$loadBody = @{
    name        = $Model
    contextSize = $ContextSize
    gpuLayers   = $GpuLayers
} | ConvertTo-Json
$loadOk = $false
try {
    $r = Invoke-RestMethod -Method Post -Uri "$clientUrl/api/models/load" -ContentType 'application/json' -Body $loadBody -TimeoutSec 30
    Write-Host ("ответ загрузки: " + ($r | ConvertTo-Json -Depth 3 -Compress))
    $loadOk = $true
} catch {
    Write-Host "загрузка не принята: $($_.Exception.Message)" -ForegroundColor Yellow
}

if ($loadOk) {
    Write-Host "`n=== 6. Ожидание загрузки (прогресс) ===" -ForegroundColor Cyan
    $deadline = (Get-Date).AddMinutes(6)
    $state = 'unknown'
    while ((Get-Date) -lt $deadline) {
        try {
            $p = Invoke-RestMethod -Uri "$clientUrl/api/models/load/progress?model=$Model" -TimeoutSec 15
            $state = $p.status
            Write-Host ("  статус: $state")
            if ($state -in @('loaded', 'error', 'failed')) { break }
        } catch {
            # 404 на прогресс — не ошибка: часть сборок отдаёт состояние только
            # через /api/models. Не засоряем вывод повтором одной строки.
            Start-Sleep -Seconds 3
            continue
        }
        Start-Sleep -Seconds 3
    }

    # ФАКТ раскладки: куда реально легли слои. Живой случай (прогон B): узел
    # доступен, но клиент кладёт ВСЁ на CPU и молчит об этом — «загрузилось» и
    # «загрузилось с раскладкой» не одно и то же.
    Write-Host "`n=== 6b. Куда легли слои (факт) ===" -ForegroundColor Cyan
    try {
        $facts = Invoke-RestMethod -Uri "$clientUrl/api/models" -Headers @{'X-API-Token'=$Token} -TimeoutSec 20
        Write-Host ("rpc_enabled={0} servers={1} gpu_count={2} vram_remote_free_mb={3}" -f `
            $facts.rpc_enabled, ($facts.rpc_servers_active -join ','), $facts.gpu_count, $facts.vram_remote_free_mb)
        $fm = $facts.models | Where-Object { $_.name -eq $Model } | Select-Object -First 1
        if ($fm) {
            Write-Host ("state={0} size_vram={1} layers={2}" -f $fm.state, $fm.size_vram, `
                (($fm.device_layers_exact | ForEach-Object { "$($_.device):$($_.from)-$($_.to)" }) -join ' '))
            if (($fm.device_layers_exact | Where-Object { $_.device -like 'RPC*' } | Measure-Object).Count -eq 0) {
                Write-Host 'ВНИМАНИЕ: на узле НЕТ ни одного слоя — это не раскладка, а тихий откат на CPU' -ForegroundColor Yellow
            }
        }
    } catch {
        Write-Host ("факт раскладки недоступен: " + $_.Exception.Message) -ForegroundColor Yellow
    }

    Write-Host "`n=== 7. Операция чата (на ней клиент и падал) ===" -ForegroundColor Cyan
    $chatBody = @{
        model    = $Model
        messages = @(@{ role = 'user'; content = '2+2' })
        stream   = $false
        options  = @{ num_predict = 8 }
    } | ConvertTo-Json -Depth 6
    $chatOk = $false
    try {
        $c = Invoke-RestMethod -Method Post -Uri "$clientUrl/api/chat" -ContentType 'application/json' -Body $chatBody -TimeoutSec 180
        Write-Host ("ответ чата: " + ($c | ConvertTo-Json -Depth 4 -Compress))
        $chatOk = $true
    } catch {
        Write-Host "чат не прошёл: $($_.Exception.Message)" -ForegroundColor Yellow
    }
} else {
    $chatOk = $false
}

Write-Host "`n=== 8. Живы ли контейнеры ===" -ForegroundColor Cyan
docker ps -a --filter "name=r172-node" --format '{{.Names}} {{.Status}}' | ForEach-Object { Write-Host "  $_" }

Write-Host "`n=== 9. СЕРВЕРНАЯ СТОРОНА: причина и лог узла (R172) ===" -ForegroundColor Cyan
$headers = @{ 'X-API-Token' = $Token }
$verdict = 'НЕ ВОСПРОИЗВЕЛОСЬ'
try {
    $log = Invoke-RestMethod -Uri "$serverUrl/api/v1/cppworker/rpc-server/log?lines=120" -Headers $headers -TimeoutSec 25
    Write-Host ("enabled={0} running={1} pid={2} lastExit='{3}' lastExitAt={4} restarts={5} logLines={6} logDropped={7}" -f `
        $log.enabled, $log.running, $log.pid, $log.lastExit, $log.lastExitAt, $log.restarts, $log.logLines, $log.logDropped)
    Write-Host '--- последние строки узла ---'
    foreach ($ln in ($log.lines | Select-Object -Last 40)) { Write-Host ("  " + $ln.at + "  " + $ln.text) }
    if ($log.lastExit -and $log.lastExit -ne '') {
        $verdict = 'УЗЕЛ ПАДАЛ: ' + $log.lastExit
    } elseif ($log.restarts -gt 0) {
        $verdict = 'УЗЕЛ ПЕРЕЗАПУСКАЛСЯ: раз ' + $log.restarts + ', exit ' + $log.lastExit
    } elseif (-not $chatOk) {
        $verdict = 'УЗЕЛ ЖИВ, НО ОПЕРАЦИЯ НЕ ПРОШЛА (смотреть строки выше)'
    }
} catch {
    Write-Host "лог узла недоступен: $($_.Exception.Message)" -ForegroundColor Yellow
    $verdict = 'ЛОГ НЕДОСТУПЕН'
}

Write-Host "`n=== ВЕРДИКТ: $verdict ===" -ForegroundColor Cyan
if ($chatOk) { Write-Host 'чат прошёл — этой топологией авария не воспроизводится' -ForegroundColor Green }

if (-not $KeepContainers) {
    Write-Host "`n--- уборка (сохранить: -KeepContainers) ---"
    Remove-Containers
} else {
    Write-Host "`nконтейнеры оставлены: $serverName, $clientName (убрать: docker rm -f $serverName $clientName)"
}
exit $(if ($chatOk) { 0 } else { 1 })
