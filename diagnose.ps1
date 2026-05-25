Write-Host "========================================" -ForegroundColor Cyan
Write-Host "OllamaLegion — Диагностика + загрузка модели" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan

Write-Host "`n1. Balancer /api/v1/backends:" -ForegroundColor Yellow
try {
    $backends = Invoke-RestMethod -Uri 'http://localhost:18081/api/v1/backends' -Method Get -TimeoutSec 5
    foreach ($b in $backends) {
        Write-Host ("   Backend: " + $b.id + " type=" + $b.backendType + " status=" + $b.status + " hasAgent=" + $b.hasAgent)
    }
} catch {
    Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

Write-Host "`n2. /api/tags (эмуляция запроса OpenWebUI):" -ForegroundColor Yellow
try {
    $tags = Invoke-RestMethod -Uri 'http://localhost:18080/api/tags' -Method Get -TimeoutSec 5
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

Write-Host "`n3. CppWorker — /api/models (загруженные модели):" -ForegroundColor Yellow
try {
    $cm = Invoke-RestMethod -Uri 'http://localhost:18091/api/models' -Method Get -TimeoutSec 5
    Write-Host "   Count: $($cm.count)"
    if ($cm.models.Count -gt 0) {
        foreach ($m in $cm.models) { Write-Host ("   - " + $m.name + " " + $m.architecture) }
    } else {
        Write-Host "   Моделей нет — требуется загрузка" -ForegroundColor Yellow
    }
} catch {
    Write-Host ("   ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

Write-Host "`n4. Файлы .gguf в models/:" -ForegroundColor Yellow
$ggufFiles = Get-ChildItem -Path "c:\Ollama\ollamalegion\models" -Filter "*.gguf" -ErrorAction SilentlyContinue
if ($ggufFiles) {
    foreach ($f in $ggufFiles) {
        Write-Host ("   " + $f.Name + " — " + [math]::Round($f.Length/1GB, 2) + " GB") -ForegroundColor Green
    }
} else {
    Write-Host "   Нет .gguf файлов" -ForegroundColor Yellow
}

# ====== ЗАГРУЗКА МОДЕЛИ ======
Write-Host "`n5. Попытка загрузки модели gemma-4-E4B-it-Q4_K_M.gguf:" -ForegroundColor Cyan
$modelName = "gemma-4-E4B-it-Q4_K_M"
$modelPath = "/app/models/gemma-4-E4B-it-Q4_K_M.gguf"
$loadBody = @{
    name = $modelName
    path = $modelPath
    gpuLayers = 0
    ctxSize = 2048
    batchSize = 512
} | ConvertTo-Json

Write-Host "   POST /api/models/load body: $loadBody"
try {
    $loadResp = Invoke-RestMethod -Uri 'http://localhost:18091/api/models/load' -Method Post -Body $loadBody -ContentType 'application/json' -TimeoutSec 30
    Write-Host "   Ответ: $($loadResp | ConvertTo-Json -Compress)" -ForegroundColor Green
} catch {
    Write-Host ("   ОШИБКА загрузки: " + $_.Exception.Message) -ForegroundColor Red
}

# Дадим время на загрузку
Start-Sleep -Seconds 5

Write-Host "`n6. Проверка после загрузки — /api/models:" -ForegroundColor Yellow
try {
    $cm2 = Invoke-RestMethod -Uri 'http://localhost:18091/api/models' -Method Get -TimeoutSec 5
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

Write-Host "`n7. Проверка /api/tags после загрузки:" -ForegroundColor Yellow
try {
    $tags2 = Invoke-RestMethod -Uri 'http://localhost:18080/api/tags' -Method Get -TimeoutSec 5
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

Write-Host "`n========================================" -ForegroundColor Cyan