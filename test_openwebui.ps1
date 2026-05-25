Write-Host "========================================" -ForegroundColor Cyan
Write-Host "Имитационный тест OpenWebUI — /api/chat" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan

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

Write-Host "`nЗапрос:" -ForegroundColor Yellow
Write-Host $payload

Write-Host "`nОтправка POST /api/chat на порт 18080 (балансер)...`n" -ForegroundColor Yellow

try {
    $response = Invoke-RestMethod -Uri 'http://localhost:18080/api/chat' -Method Post -Body $payload -ContentType 'application/json' -TimeoutSec 120
    Write-Host "=== ОТВЕТ ===" -ForegroundColor Green
    Write-Host ("Model: " + $response.model)
    Write-Host ("Created at: " + $response.created_at)
    Write-Host ("Role: " + $response.message.role)
    Write-Host ("`nContent:`n" + $response.message.content)
    Write-Host ("`nTotal duration: " + ($response.total_duration/1e9) + "s")
    Write-Host "========================================" -ForegroundColor Green
} catch {
    Write-Host ("ОШИБКА: " + $_.Exception.Message) -ForegroundColor Red
    if ($_.Exception.Response) {
        $reader = New-Object System.IO.StreamReader($_.Exception.Response.GetResponseStream())
        Write-Host ("Body: " + $reader.ReadToEnd())
    }
}