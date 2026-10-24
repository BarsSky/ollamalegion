# legion-keepalive.ps1 — держит контейнер донора поднятым.
#
# ЗАЧЕМ (живой случай 2026-10-16). У контейнера cppworker-node2 стоит
# Restart=unless-stopped, но это не спасает: Docker Desktop при выходе сам
# останавливает все контейнеры, и для движка это «остановлено вручную» — после
# старта движка он их не поднимает. Итог: пользователь залогинен, движок жив,
# cppworker-node2 лежит с Exit=137 часами, а мастер получает мёртвый донор.
#
# ЧТО ДЕЛАЕМ на каждом запуске (раз в 3 минуты):
#   1. если движок не отвечает — поднимаем Docker Desktop и ждём до 5 минут;
#   2. если контейнер не Running — docker start.
#
# ЗАПУСКАЕТСЯ ИЗ ПЛАНИРОВЩИКА (задача LegionStackDockerKeepalive), в сессии
# пользователя — иначе Docker Desktop не поднимется.
$ErrorActionPreference = 'SilentlyContinue'
$name = 'cppworker-node2'
$log  = Join-Path $env:USERPROFILE 'legion-keepalive.log'

function Write-Log([string]$msg) {
    $line = '{0} {1}' -f (Get-Date -Format 'yyyy-MM-ddTHH:mm:ss'), $msg
    Add-Content -Path $log -Value $line -Encoding UTF8
    # Лог не растёт бесконечно: всё, что больше 200 строк, отрезаем.
    $all = @(Get-Content -Path $log)
    if ($all.Count -gt 200) {
        $all | Select-Object -Last 100 | Set-Content -Path $log -Encoding UTF8
    }
}

# 1. Движок.
& docker info --format '{{.ServerVersion}}' *> $null
if ($LASTEXITCODE -ne 0) {
    Write-Log 'движок не отвечает — поднимаю Docker Desktop'
    $exe = (Get-ItemProperty 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run' -Name 'Docker Desktop').'Docker Desktop'
    if ($exe) {
        $exe = $exe -replace '"', ''
        Start-Process -FilePath $exe
    } else {
        Write-Log 'путь к Docker Desktop не найден в HKCU\...\Run'
    }
    $up = $false
    for ($i = 0; $i -lt 60; $i++) {
        Start-Sleep -Seconds 5
        & docker info --format '{{.ServerVersion}}' *> $null
        if ($LASTEXITCODE -eq 0) { $up = $true; break }
    }
    if (-not $up) {
        Write-Log 'движок не поднялся за 5 минут — выхожу'
        exit 1
    }
    Write-Log 'движок поднялся'
}

# 2. Контейнер.
$running = (& docker inspect -f '{{.State.Running}}' $name 2>$null)
if ($running -ne 'true') {
    & docker start $name *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Log "контейнер $name был не Running — запущен"
    } else {
        Write-Log "контейнер $name не запустился (docker start exit=$LASTEXITCODE)"
    }
}
