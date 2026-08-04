# Тест async apply во время busy
$token = "changeme-bundled-full-token-min-32-chars-please"
$hdr = @{ Authorization = "Bearer $token" }

# Сначала создадим профиль
Write-Host "Creating profile..."
$body = '{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"notes":"verify-async-apply"}'
$null = Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 -Uri "http://192.168.13.20:18081/api/v1/cppworker/model-profiles/Qwen3-Instruct-2507-q4km" `
    -Method Put -Headers $hdr -ContentType "application/json" -Body $body
Write-Host "Profile created"

# Запустим streaming в фоне
$streamBody = @{
    model = "Qwen3-Instruct-2507-q4km"
    messages = @(@{ role = "user"; content = "Tell me a very long story about a journey" })
    max_tokens = 2000
    stream = $true
} | ConvertTo-Json -Compress
$utf8NoBom = New-Object System.Text.UTF8Encoding $false
[System.IO.File]::WriteAllText("C:\Ollama\ollamalegion\logs\stream_qwen2.json", $streamBody, $utf8NoBom)

Write-Host "Starting streaming generation..."
$streamJob = Start-Job -ScriptBlock {
    $headers = @{ Authorization = "Bearer $($args[0])" }
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 600 -Uri $args[1] -Method Post -Headers $headers -ContentType "application/json" -Body (Get-Content $args[2] -Raw)
    Write-Output "Status: $($r.StatusCode) Length: $($r.Content.Length)"
} -ArgumentList $token, "http://192.168.13.20:18092/v1/chat/completions", "C:\Ollama\ollamalegion\logs\stream_qwen2.json"

# Подождём чтобы streaming начался
Start-Sleep -Seconds 5

# Apply profile (должен вернуть 202 + applyId, потому что модель busy)
Write-Host "`n=== Apply during busy ==="
try {
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 10 -Uri "http://192.168.13.20:18081/api/v1/cppworker/model-profiles/Qwen3-Instruct-2507-q4km/apply" `
        -Method Post -Headers $hdr -ContentType "application/json" -Body "{}"
    Write-Host "Status: $($r.StatusCode)"
    Write-Host "Body: $($r.Content)"
    $json = $r.Content | ConvertFrom-Json
    if ($r.StatusCode -eq 202 -and $json.status -eq "accepted") {
        Write-Host "`n✓ ASYNC PATH: applyId=$($json.applyId), progressUrl=$($json.progressUrl)" -ForegroundColor Green
        Write-Host "  busyBackends: $($json.busyBackends -join ', ')"
        Write-Host "  initialActiveQueries: $($json.initialActiveQueries)"

        # Проверим что streaming ещё идёт
        Start-Sleep -Seconds 2
        $aq = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri "http://192.168.13.20:18092/api/models/active-queries?model=Qwen3-Instruct-2507-q4km" -Headers $hdr
        Write-Host "  Active queries now: $($aq.Content)"
    } else {
        Write-Host "  ! Sync path (model not busy): status=$($r.StatusCode)" -ForegroundColor Yellow
    }
} catch {
    Write-Host "Error: $($_.Exception.Message)" -ForegroundColor Red
}

# Дождёмся streaming
Write-Host "`nWaiting for streaming..."
$streamResult = Receive-Job -Job $streamJob -Wait -Timeout 60
Write-Host "Stream: $streamResult"
Remove-Job -Job $streamJob -Force
