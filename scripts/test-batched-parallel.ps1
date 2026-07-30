# test-batched-parallel.ps1 — Round 15.1 step 3.5 integration test.
#
# Measures wall-time for 4 concurrent /v1/chat requests через
# BatchedScheduler (Round 15.1 batched parallel path) vs sequential
# baseline (Round 13 multi-slot, default).
#
# Ожидаемый результат: 4 parallel < 4 sequential × 60% (target per
# design doc: real GPU matmul sharing).
#
# Использование:
#   .\scripts\test-batched-parallel.ps1
#
# Требования:
#   - bundled stack запущен (cppworker, balancer)
#   - модель Qwen3-4B-Thinking gguf в /app/models/

$ErrorActionPreference = "Stop"

$cppworkerUrl = "http://localhost:18092"
$balancerUrl  = "http://localhost:18080"
$token = "changeme-bundled-strong-token-please-change"
$headers = @{
    "X-API-Token" = $token
    "Content-Type" = "application/json"
}

$modelPath = "/app/models/Qwen_Qwen3-4B-Thinking-2507-Q4_K_M.gguf"
$modelNameBatched = "qwen3-test-batched"
$modelNameLegacy  = "qwen3-test-legacy"

# ============================================================================
# Helpers
# ============================================================================

function Unload-All {
    Write-Host "[cleanup] unloading test models..." -ForegroundColor Yellow
    foreach ($name in @($modelNameBatched, $modelNameLegacy, "qwen3-4b-batched", "qwen3-4b")) {
        try {
            Invoke-WebRequest -Uri "$cppworkerUrl/api/models/unload?name=$name" -Method POST -Headers $headers -UseBasicParsing -TimeoutSec 5 -ErrorAction SilentlyContinue | Out-Null
        } catch {}
    }
}

function Load-Model {
    param([string]$name, [int]$parallel, [bool]$batched)
    $body = @{
        name                   = $name
        path                   = $modelPath
        n_ctx                  = 4096
        n_gpu_layers           = -1
        parallel               = $parallel
    } | ConvertTo-Json
    if ($batched) {
        $body = $body.Substring(0, $body.Length - 1) + ', "enableBatchedParallel": true}'
    }
    Write-Host "[load] $name (parallel=$parallel, batched=$batched)..." -ForegroundColor Cyan
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $resp = Invoke-WebRequest -Uri "$cppworkerUrl/api/models/load-with-params" -Method POST -Headers $headers -Body $body -UseBasicParsing -TimeoutSec 90
    $sw.Stop()
    $json = $resp.Content | ConvertFrom-Json
    if ($resp.StatusCode -ne 200) {
        throw "load failed: $($resp.StatusCode) $($resp.Content)"
    }
    Write-Host ("[load]   ok in {0}ms, batchedParallel={1}" -f $sw.ElapsedMilliseconds, $json.model.batchedParallel) -ForegroundColor Green
    return $json.model
}

function Send-Chat {
    param([string]$name, [string]$message, [int]$maxTokens, [int]$requestId)
    $body = @{
        model      = $name
        messages   = @(@{role = "user"; content = $message})
        max_tokens = $maxTokens
        stream     = $false
    } | ConvertTo-Json
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    try {
        $resp = Invoke-WebRequest -Uri "$cppworkerUrl/v1/chat/completions" -Method POST -Headers $headers -Body $body -UseBasicParsing -TimeoutSec 120
        $sw.Stop()
        $json = $resp.Content | ConvertFrom-Json
        $usage = $json.usage
        return @{
            RequestId      = $requestId
            StatusCode     = $resp.StatusCode
            WallMs         = $sw.ElapsedMilliseconds
            PromptTokens   = $usage.prompt_tokens
            CompletionTokens = $usage.completion_tokens
            FinishReason   = $json.choices[0].finish_reason
            ContentPreview = ($json.choices[0].message.content -replace "`n", " ") | Select-Object -First 1
        }
    } catch {
        $sw.Stop()
        return @{
            RequestId  = $requestId
            StatusCode = 0
            WallMs     = $sw.ElapsedMilliseconds
            Error      = $_.Exception.Message
        }
    }
}

# 4 distinct prompts to make sure results are independent.
$prompts = @(
    "List 3 primary colors in one short sentence.",
    "Name 2 continents starting with letter A.",
    "What is 7 multiplied by 8? Reply in one short sentence.",
    "Reply with the word HELLO in capitals."
)
$maxTokens = 20

# ============================================================================
# Test 1: Sequential baseline (Round 13 path, parallel=1)
# ============================================================================

Write-Host "`n=== Test 1: Sequential (4 requests, 1 at a time, parallel=1) ===" -ForegroundColor Yellow
Unload-All
Start-Sleep -Seconds 2
Load-Model -name $modelNameLegacy -parallel 1 -batched $false | Out-Null
Start-Sleep -Seconds 2

$seqResults = @()
$totalSw = [System.Diagnostics.Stopwatch]::StartNew()
for ($i = 0; $i -lt 4; $i++) {
    Write-Host "[seq] request $($i+1)/4..." -ForegroundColor Gray
    $r = Send-Chat -name $modelNameLegacy -message $prompts[$i] -maxTokens $maxTokens -requestId ($i+1)
    $seqResults += $r
    Write-Host ("[seq]   req=$($r.RequestId) status=$($r.StatusCode) wallMs=$($r.WallMs) prompt=$($r.PromptTokens) comp=$($r.CompletionTokens) finish=$($r.FinishReason)") -ForegroundColor Gray
}
$totalSw.Stop()
$seqTotalMs = $totalSw.ElapsedMilliseconds
$seqTokensTotal = ($seqResults | ForEach-Object { $_.CompletionTokens } | Measure-Object -Sum).Sum
Write-Host ("[seq] TOTAL: {0}ms, {1} tokens total" -f $seqTotalMs, $seqTokensTotal) -ForegroundColor Yellow

# ============================================================================
# Test 2: Parallel через BatchedScheduler (Round 15.1, parallel=4, batched=true)
# ============================================================================

Write-Host "`n=== Test 2: Parallel BatchedScheduler (4 concurrent, parallel=4, batched=true) ===" -ForegroundColor Yellow
Unload-All
Start-Sleep -Seconds 2
Load-Model -name $modelNameBatched -parallel 4 -batched $true | Out-Null
Start-Sleep -Seconds 2

# Запускаем 4 запроса параллельно через Start-ThreadJob (если доступен) или jobs.
$jobs = @()
$totalSw = [System.Diagnostics.Stopwatch]::StartNew()
for ($i = 0; $i -lt 4; $i++) {
    $idx = $i
    $j = Start-Job -ScriptBlock {
        param($url, $headers, $model, $msg, $maxTok, $id)
        $body = @{
            model      = $model
            messages   = @(@{role = "user"; content = $msg})
            max_tokens = $maxTok
            stream     = $false
        } | ConvertTo-Json
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        try {
            $resp = Invoke-WebRequest -Uri "$url/v1/chat/completions" -Method POST -Headers $headers -Body $body -UseBasicParsing -TimeoutSec 120
            $sw.Stop()
            $json = $resp.Content | ConvertFrom-Json
            return @{
                RequestId      = $id
                StatusCode     = $resp.StatusCode
                WallMs         = $sw.ElapsedMilliseconds
                PromptTokens   = $json.usage.prompt_tokens
                CompletionTokens = $json.usage.completion_tokens
                FinishReason   = $json.choices[0].finish_reason
            }
        } catch {
            $sw.Stop()
            return @{
                RequestId  = $id
                StatusCode = 0
                WallMs     = $sw.ElapsedMilliseconds
                Error      = $_.Exception.Message
            }
        }
    } -ArgumentList $cppworkerUrl, $headers, $modelNameBatched, $prompts[$idx], $maxTokens, ($idx+1)
    $jobs += $j
}
$parResults = @()
foreach ($j in $jobs) {
    $r = Wait-Job $j | Receive-Job
    $parResults += $r
    Remove-Job $j -Force
    Write-Host ("[par]   req=$($r.RequestId) status=$($r.StatusCode) wallMs=$($r.WallMs) prompt=$($r.PromptTokens) comp=$($r.CompletionTokens) finish=$($r.FinishReason)") -ForegroundColor Gray
}
$totalSw.Stop()
$parTotalMs = $totalSw.ElapsedMilliseconds
$parTokensTotal = ($parResults | ForEach-Object { $_.CompletionTokens } | Measure-Object -Sum).Sum
Write-Host ("[par] TOTAL: {0}ms, {1} tokens total" -f $parTotalMs, $parTokensTotal) -ForegroundColor Yellow

# ============================================================================
# Summary
# ============================================================================

Write-Host "`n=== Summary ===" -ForegroundColor Cyan
$seqAllOk = ($seqResults | Where-Object { $_.StatusCode -eq 200 }).Count -eq 4
$parAllOk = ($parResults | Where-Object { $_.StatusCode -eq 200 }).Count -eq 4
Write-Host ("Sequential (Round 13): {0}ms, 4/4 ok={1}" -f $seqTotalMs, $seqAllOk)
Write-Host ("Parallel   (Round 15.1): {0}ms, 4/4 ok={1}" -f $parTotalMs, $parAllOk)
if ($parTotalMs -gt 0) {
    $speedup = [math]::Round($seqTotalMs / $parTotalMs, 2)
    $pct = [math]::Round(100.0 * $parTotalMs / $seqTotalMs, 1)
    Write-Host ("Speedup: {0}x  (parallel = {1}% of sequential)" -f $speedup, $pct) -ForegroundColor Green
    if ($parAllOk -and $seqAllOk) {
        if ($pct -lt 60) {
            Write-Host "TARGET MET: parallel <60% of sequential" -ForegroundColor Green
        } else {
            Write-Host ("TARGET NOT MET (parallel = {0}% vs target <60%%)") -ForegroundColor Yellow
        }
    }
}

# Cleanup
Unload-All
Write-Host "`n[done]" -ForegroundColor Cyan
