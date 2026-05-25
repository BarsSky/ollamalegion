Write-Host "=== Прямой запрос к CppWorker /api/chat ===" -ForegroundColor Cyan

$payload = @{
    model = "gemma-4-E4B-it-Q4_K_M"
    messages = @(
        @{
            role = "user"
            content = "напиши код программы для возведения в степень на c++"
        }
    )
    stream = $false
} | ConvertTo-Json -Depth 3

Write-Host ("Body: " + $payload)

Write-Host "`nPOST http://localhost:18091/api/chat ...`n"
try {
    $r = Invoke-RestMethod -Uri 'http://localhost:18091/api/chat' -Method Post -Body $payload -ContentType 'application/json' -TimeoutSec 120
    Write-Host "=== УСПЕХ ===" -ForegroundColor Green
    Write-Host ("Model: " + $r.model)
    Write-Host ("`nContent:`n" + $r.message.content)
    Write-Host ("`nDuration: " + ($r.total_duration/1e9) + "s")
} catch {
    Write-Host ("ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

Write-Host "`n=== Прямой запрос к CppWorker /api/generate ===" -ForegroundColor Cyan
$g = @{
    model = "gemma-4-E4B-it-Q4_K_M"
    prompt = "напиши код программы для возведения в степень на c++"
    stream = $false
} | ConvertTo-Json -Compress

Write-Host ("Body: " + $g)
Write-Host "`nPOST http://localhost:18091/api/generate ...`n"
try {
    $r2 = Invoke-RestMethod -Uri 'http://localhost:18091/api/generate' -Method Post -Body $g -ContentType 'application/json' -TimeoutSec 120
    Write-Host "=== УСПЕХ ===" -ForegroundColor Green
    Write-Host ("Model: " + $r2.model)
    Write-Host ("`nResponse:`n" + $r2.response)
} catch {
    Write-Host ("ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
}

Write-Host "`n=== Через балансер (порт 18080) /api/generate ===" -ForegroundColor Cyan
Write-Host "POST http://localhost:18080/api/generate ...`n"
try {
    $r3 = Invoke-RestMethod -Uri 'http://localhost:18080/api/generate' -Method Post -Body $g -ContentType 'application/json' -TimeoutSec 120
    Write-Host "=== УСПЕХ через балансер ===" -ForegroundColor Green
    Write-Host ("Model: " + $r3.model)
    Write-Host ("`nResponse:`n" + $r3.response)
} catch {
    Write-Host ("ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
    try {
        $errBody = $_.ErrorDetails.Message
        if ($errBody) { Write-Host ("Body: " + $errBody) }
    } catch {}
}