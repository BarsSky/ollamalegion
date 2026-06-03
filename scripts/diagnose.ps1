#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Диагностика OllamaLegion + опциональная автозагрузка GGUF-модели в CppWorker.

.DESCRIPTION
    Делает последовательные проверки: бэкенды в balancer, /api/tags (как видит OpenWebUI),
    загруженные модели в CppWorker, GGUF-файлы в каталоге моделей. Затем пытается
    загрузить модель через POST /api/models/load и повторяет проверки.

    Параметры по умолчанию ориентированы на локальный dev-стек. Все адреса и
    пути можно переопределить через -Admin, -Balancer, -Cppworker, -ModelsDir, -LoadName.

.EXAMPLE
    pwsh scripts/diagnose.ps1
    pwsh scripts/diagnose.ps1 -LoadName "llama-3.1-8b-instruct" -ModelPath "/app/models/llama-3.1-8b.gguf"
    pwsh scripts/diagnose.ps1 -Cppworker http://cppworker.local:18091 -Balancer http://balancer.local:18080
#>

[CmdletBinding()]
param(
    [string]$Admin = "http://localhost:18081",
    [string]$Balancer = "http://localhost:18080",
    [string]$Cppworker = "http://localhost:18091",
    [string]$ModelsDir,
    [string]$LoadName = "gemma-4-E4B-it-Q4_K_M",
    [string]$ModelPath = "/app/models/gemma-4-E4B-it-Q4_K_M.gguf",
    [int]$GpuLayers = 0,
    [int]$CtxSize = 2048,
    [int]$BatchSize = 512,
    [switch]$SkipLoad
)

if ([string]::IsNullOrWhiteSpace($ModelsDir)) {
    # По умолчанию — каталог models в корне репозитория (рядом со scripts)
    $ModelsDir = Join-Path (Split-Path -Parent $PSScriptRoot) "models"
}

Write-Host "========================================" -ForegroundColor Cyan
Write-Host "OllamaLegion — Диагностика + загрузка модели" -ForegroundColor Cyan
Write-Host "Admin:        $Admin" -ForegroundColor DarkGray
Write-Host "Balancer:     $Balancer" -ForegroundColor DarkGray
Write-Host "CppWorker:    $Cppworker" -ForegroundColor DarkGray
Write-Host "Models dir:   $ModelsDir" -ForegroundColor DarkGray
Write-Host "========================================" -ForegroundColor Cyan

# 1) Бэкенды
Write-Host "`n1. Balancer /api/v1/backends:" -ForegroundColor Yellow
try {
    $backends = Invoke-RestMethod -Uri "$Admin/api/v1/backends" -Method Get -TimeoutSec 5
    foreach ($b in $backends) {
        Write-Host ("   Backend: " + $b.id + " type=" + $b.backendType + " status=" + $b.status + " hasAgent=" + $b.hasAgent)
    }
} catch {
    Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

# 2) /api/tags (что увидит OpenWebUI)
Write-Host "`n2. /api/tags (эмуляция запроса OpenWebUI):" -ForegroundColor Yellow
try {
    $tags = Invoke-RestMethod -Uri "$Balancer/api/tags" -Method Get -TimeoutSec 5
    if ($tags.models.Count -gt 0) {
        Write-Host "   Найдено моделей: $($tags.models.Count)" -ForegroundColor Green
        foreach ($m in $tags.models) {
            Write-Host ("   - " + $m.name + " (" + $m.model + ") size=" + $m.size)
        }
    } else {
        Write-Host "   ПУСТО — OpenWebUI не увидит ни одной модели!" -ForegroundColor Red
    }
} catch {
    Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

# 3) Загруженные модели в CppWorker
Write-Host "`n3. CppWorker — /api/models (загруженные модели):" -ForegroundColor Yellow
try {
    $cm = Invoke-RestMethod -Uri "$Cppworker/api/models" -Method Get -TimeoutSec 5
    Write-Host "   Count: $($cm.count)"
    if ($cm.models.Count -gt 0) {
        foreach ($m in $cm.models) { Write-Host ("   - " + $m.name + " " + $m.architecture) }
    } else {
        Write-Host "   Моделей нет — требуется загрузка" -ForegroundColor Yellow
    }
} catch {
    Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

# 4) GGUF-файлы в каталоге моделей
Write-Host "`n4. Файлы .gguf в $ModelsDir :" -ForegroundColor Yellow
$ggufFiles = Get-ChildItem -Path $ModelsDir -Filter "*.gguf" -ErrorAction SilentlyContinue
if ($ggufFiles) {
    foreach ($f in $ggufFiles) {
        Write-Host ("   " + $f.Name + " — " + [math]::Round($f.Length/1GB, 2) + " GB") -ForegroundColor Green
    }
} else {
    Write-Host "   Нет .gguf файлов" -ForegroundColor Yellow
}

# 5) Попытка автозагрузки модели
if (-not $SkipLoad) {
    Write-Host "`n5. Попытка загрузки модели $LoadName :" -ForegroundColor Cyan
    $loadBody = @{
        name      = $LoadName
        path      = $ModelPath
        gpuLayers = $GpuLayers
        ctxSize   = $CtxSize
        batchSize = $BatchSize
    } | ConvertTo-Json

    Write-Host "   POST $Cppworker/api/models/load body: $loadBody"
    try {
        $loadResp = Invoke-RestMethod -Uri "$Cppworker/api/models/load" -Method Post -Body $loadBody -ContentType 'application/json' -TimeoutSec 30
        Write-Host "   Ответ: $($loadResp | ConvertTo-Json -Compress)" -ForegroundColor Green
    } catch {
        Write-Host ("   ОШИБКА загрузки: " + $_.Exception.Message) -ForegroundColor Red
    }

    Start-Sleep -Seconds 5

    # 6) Проверка после загрузки
    Write-Host "`n6. Проверка после загрузки — /api/models:" -ForegroundColor Yellow
    try {
        $cm2 = Invoke-RestMethod -Uri "$Cppworker/api/models" -Method Get -TimeoutSec 5
        Write-Host "   Count: $($cm2.count)"
        if ($cm2.models.Count -gt 0) {
            foreach ($m in $cm2.models) {
                Write-Host ("   + ЗАГРУЖЕНА: " + $m.name + " " + $m.architecture) -ForegroundColor Green
            }
        } else {
            Write-Host "   Всё ещё пусто" -ForegroundColor Yellow
        }
    } catch {
        Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
    }

    # 7) Проверка /api/tags после загрузки
    Write-Host "`n7. Проверка /api/tags после загрузки:" -ForegroundColor Yellow
    try {
        $tags2 = Invoke-RestMethod -Uri "$Balancer/api/tags" -Method Get -TimeoutSec 5
        if ($tags2.models.Count -gt 0) {
            Write-Host "   УСПЕХ! Найдено моделей: $($tags2.models.Count)" -ForegroundColor Green
            foreach ($m in $tags2.models) {
                Write-Host ("   - " + $m.name + " (" + $m.model + ") size=" + $m.size)
            }
        } else {
            Write-Host "   Всё ещё пусто" -ForegroundColor Red
        }
    } catch {
        Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
    }
}
else {
    Write-Host "`n5-7. Загрузка модели пропущена (-SkipLoad)" -ForegroundColor DarkGray
}

Write-Host "`n========================================" -ForegroundColor Cyan