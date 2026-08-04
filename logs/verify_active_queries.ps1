# Проверяем busy state — запустим generation и одновременно спросим active-queries
$token = "changeme-bundled-full-token-min-32-chars-please"
$body = @{
    model = "Qwen3-Instruct-2507-q4km"
    messages = @(@{ role = "user"; content = "Write a long story about a knight in 2000 words" })
    max_tokens = 2000
    stream = $true
} | ConvertTo-Json -Compress
$utf8NoBom = New-Object System.Text.UTF8Encoding $false
[System.IO.File]::WriteAllText("C:\Ollama\ollamalegion\logs\stream_qwen.json", $body, $utf8NoBom)

# Запустим streaming запрос в фоне
Write-Host "Starting streaming generation..."
$streamJob = Start-Job -ScriptBlock {
    $token = $args[0]
    $bodyPath = $args[1]
    $url = $args[2]
    $headers = @{ Authorization = "Bearer $token" }
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 600 -Uri $url -Method Post -Headers $headers -ContentType "application/json" -Body (Get-Content $bodyPath -Raw)
        Write-Output "Status: $($r.StatusCode) Length: $($r.Content.Length)"
    } catch {
        Write-Output "ERROR: $($_.Exception.Message)"
    }
} -ArgumentList $token, "C:\Ollama\ollamalegion\logs\stream_qwen.json", "http://192.168.13.20:18092/v1/chat/completions"

# Подождём немного чтобы streaming начался
Start-Sleep -Seconds 3

# Проверим active-queries
$hdr = @{ Authorization = "Bearer $token" }
Write-Host "`n=== Active queries during generation ==="
for ($i = 0; $i -lt 5; $i++) {
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri "http://192.168.13.20:18092/api/models/active-queries?model=Qwen3-Instruct-2507-q4km" -Headers $hdr
        Write-Host "  Poll $i`: $($r.Content)"
    } catch {
        Write-Host "  Poll $i error: $($_.Exception.Message)"
    }
    Start-Sleep -Seconds 2
}

# Дождёмся streaming
Write-Host "`nWaiting for streaming to finish..."
$streamResult = Receive-Job -Job $streamJob -Wait
Write-Host "Stream result: $streamResult"

# Cleanup
Remove-Job -Job $streamJob -Force
