# Проверка всех основных Ollama API endpoints через балансер
$base = "http://127.0.0.1:18080"
$model = "gemma-4-E4B-it-Q4_K_M.gguf"

function Test-Endpoint {
    param(
        [string]$Method,
        [string]$Path,
        [string]$Body = "",
        [int]$Timeout = 30
    )
    $url = "$base$Path"
    Write-Host "[$Method] $url" -NoNewline
    try {
        $args = @("-s", "-w", " HTTP_STATUS:%{http_code}", "--max-time", $Timeout, "-X", $Method)
        if ($Body -ne "") {
            $args += @("-H", "Content-Type: application/json", "-d", $Body)
        }
        $output = & cmd /c curl $args $url 2`>`&1
        $lines = $output -split "`n"
        $last = $lines[-1]
        if ($last -match "HTTP_STATUS:(\d+)") {
            $status = $matches[1]
        } else {
            $status = "???"
        }
        $bodyOut = ($lines | Select-Object -First ($lines.Count - 1)) -join "`n"
        if ($status -match "^2") {
            Write-Host " -> $status" -ForegroundColor Green
        } else {
            Write-Host " -> $status" -ForegroundColor Red
        }
        if ($bodyOut.Trim()) {
            Write-Host $bodyOut
        }
    } catch {
        Write-Host " -> ERROR: $($_.Exception.Message)" -ForegroundColor Red
    }
    Write-Host ""
}

Write-Host "=== Ollama API endpoint check via balancer ($base) ===" 
Write-Host ""

# GET endpoints
Test-Endpoint -Method GET -Path "/api/version"
Test-Endpoint -Method GET -Path "/api/tags"
Test-Endpoint -Method GET -Path "/api/ps"

# POST /api/show
Test-Endpoint -Method POST -Path "/api/show" -Body (@{model=$model} | ConvertTo-Json -Depth 2)

# POST /api/generate (non-stream)
$body = @{model=$model; prompt="Say yes if you work."; stream=$false; options=@{num_predict=5; temperature=0.1}} | ConvertTo-Json -Depth 3
Test-Endpoint -Method POST -Path "/api/generate" -Body $body -Timeout 60

# POST /api/chat (non-stream)
$body = @{model=$model; messages=@(@{role="user"; content="Say yes if you work."}); stream=$false; options=@{num_predict=5; temperature=0.1}} | ConvertTo-Json -Depth 3
Test-Endpoint -Method POST -Path "/api/chat" -Body $body -Timeout 60

# POST /api/embeddings
$body = @{model=$model; prompt="hello world"} | ConvertTo-Json -Depth 2
Test-Endpoint -Method POST -Path "/api/embeddings" -Body $body -Timeout 60

# POST /api/pull
$body = @{name="hf:dummy/dummy.gguf"} | ConvertTo-Json -Depth 2
Test-Endpoint -Method POST -Path "/api/pull" -Body $body -Timeout 10

# POST /api/delete
$body = @{name=$model} | ConvertTo-Json -Depth 2
Test-Endpoint -Method POST -Path "/api/delete" -Body $body -Timeout 10

# POST /api/copy
$body = @{source=$model; destination="copy-test.gguf"} | ConvertTo-Json -Depth 2
Test-Endpoint -Method POST -Path "/api/copy" -Body $body -Timeout 10

# POST /api/create
$body = @{name="test-model"; modelfile="FROM $model"} | ConvertTo-Json -Depth 2
Test-Endpoint -Method POST -Path "/api/create" -Body $body -Timeout 10

Write-Host ""
Write-Host "=== Done ==="