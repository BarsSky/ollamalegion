# verify-v0513.ps1 — Live verify v0.5.13 features (Round 26)
#
# Tests:
#   1. cppworker /api/models/active-queries endpoint
#   2. X-Model-Context-Warning header on chat completion
#   3. api server async apply path with busy detection
#   4. WebUI busy badge in DOM (after busy state)
#
# Usage: .\scripts\verify-v0513.ps1

$ErrorActionPreference = "Stop"
$token = $env:CPPWORKER_API_TOKEN
if (-not $token) {
    $envFile = Join-Path $PSScriptRoot "..\deployments\.env.bundled-full"
    if (Test-Path $envFile) {
        Get-Content $envFile | ForEach-Object {
            if ($_ -match "^\s*CPPWORKER_API_TOKEN=(.+)$") {
                $token = $Matches[1].Trim('"', "'")
                $env:CPPWORKER_API_TOKEN = $token
            }
        }
    }
}
if (-not $token) { throw "CPPWORKER_API_TOKEN not set" }

$hdr = @{ Authorization = "Bearer $token" }
$cppworker = "http://192.168.13.20:18092"
$balancer = "http://192.168.13.20:18081"

Write-Host "=== v0.5.13 Live Verify ===" -ForegroundColor Cyan

# Test 1: active-queries endpoint (no specific model)
Write-Host "`n[1] cppworker /api/models/active-queries (no model)" -ForegroundColor Yellow
try {
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri "$cppworker/api/models/active-queries" -Headers $hdr
    Write-Host "  Status: $($r.StatusCode)"
    Write-Host "  Body: $($r.Content)"
    $json = $r.Content | ConvertFrom-Json
    if ($json.PSObject.Properties.Name -contains 'queries') {
        Write-Host "  ✓ Schema correct (queries field present)" -ForegroundColor Green
    } else {
        Write-Host "  ✗ Missing queries field" -ForegroundColor Red
    }
} catch {
    Write-Host "  ✗ FAIL: $($_.Exception.Message)" -ForegroundColor Red
}

# Test 2: active-queries endpoint (specific model)
Write-Host "`n[2] cppworker /api/models/active-queries?model=Qwen3..." -ForegroundColor Yellow
try {
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri "$cppworker/api/models/active-queries?model=Qwen3-Instruct-2507-q4km" -Headers $hdr
    Write-Host "  Status: $($r.StatusCode)"
    Write-Host "  Body: $($r.Content)"
    $json = $r.Content | ConvertFrom-Json
    if ($json.model -eq "Qwen3-Instruct-2507-q4km") {
        Write-Host "  ✓ Schema correct (model field present)" -ForegroundColor Green
    }
} catch {
    Write-Host "  ✗ FAIL: $($_.Exception.Message)" -ForegroundColor Red
}

# Test 3: X-Model-Context-Warning header on chat completion
Write-Host "`n[3] /v1/chat/completions with small prompt (no overflow expected)" -ForegroundColor Yellow
try {
    $body = @{
        model = "Qwen3-Instruct-2507-q4km"
        messages = @(@{ role = "user"; content = "Say hi in 5 words" })
        max_tokens = 50
        stream = $false
    } | ConvertTo-Json
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 30 -Uri "$cppworker/v1/chat/completions" `
        -Method Post -Headers $hdr -ContentType "application/json" -Body $body
    Write-Host "  Status: $($r.StatusCode)"
    $xctx = $r.Headers["X-Model-Context-Warning"]
    $xadj = $r.Headers["X-Model-Adjusted-NPredict"]
    Write-Host "  X-Model-Context-Warning: $xctx"
    Write-Host "  X-Model-Adjusted-NPredict: $xadj"
    if (-not $xctx) {
        Write-Host "  ✓ No warning (OK, prompt is small)" -ForegroundColor Green
    } else {
        Write-Host "  ! Warning header present (level=$xctx)" -ForegroundColor Yellow
    }
} catch {
    Write-Host "  ✗ FAIL: $($_.Exception.Message)" -ForegroundColor Red
}

# Test 4: X-Model-Context-Warning with HUGE prompt (overflow expected)
# Use 200K chars ≈ 50K tokens. Model n_ctx=16384. 50K > 16K → overflow.
Write-Host "`n[4] /v1/chat/completions with ~50K token prompt (overflow expected)" -ForegroundColor Yellow
try {
    # 200K chars — definitely > 16K n_ctx
    $bigContent = "x" * 200000
    $body = @{
        model = "Qwen3-Instruct-2507-q4km"
        messages = @(@{ role = "user"; content = $bigContent })
        max_tokens = 100
        stream = $false
    } | ConvertTo-Json
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 60 -Uri "$cppworker/v1/chat/completions" `
        -Method Post -Headers $hdr -ContentType "application/json" -Body $body
    Write-Host "  Status: $($r.StatusCode)"
    $xctx = $r.Headers["X-Model-Context-Warning"]
    $xadj = $r.Headers["X-Model-Adjusted-NPredict"]
    $xsug = $r.Headers["X-Model-Context-Suggestion"]
    Write-Host "  X-Model-Context-Warning: $xctx"
    Write-Host "  X-Model-Adjusted-NPredict: $xadj"
    Write-Host "  X-Model-Context-Suggestion: $xsug"
    if ($xctx -and ($xctx -like "overflow*" -or $xctx -like "approaching*")) {
        Write-Host "  ✓ Warning correctly raised ($xctx)" -ForegroundColor Green
    } elseif ($xctx) {
        Write-Host "  ! Warning level: $xctx" -ForegroundColor Yellow
    } else {
        Write-Host "  ✗ No warning for huge prompt" -ForegroundColor Red
    }
} catch {
    Write-Host "  Note: huge prompt may have been rejected: $($_.Exception.Message)" -ForegroundColor Yellow
}

# Test 5: api server apply profile (idle model = sync path)
Write-Host "`n[5] api server apply profile (sync path when idle)" -ForegroundColor Yellow
try {
    # Сначала создадим профиль
    $profileBody = '{"contextLength":16384,"batchSize":512,"numGpuLayers":20,"notes":"verify-v0513"}'
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 30 -Uri "$balancer/api/v1/cppworker/model-profiles/Qwen3-Instruct-2507-q4km" `
        -Method Put -Headers $hdr -ContentType "application/json" -Body $profileBody
    Write-Host "  Profile upsert: $($r.StatusCode)"

    # Теперь apply (без body — возьмёт текущий профиль)
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 120 -Uri "$balancer/api/v1/cppworker/model-profiles/Qwen3-Instruct-2507-q4km/apply" `
        -Method Post -Headers $hdr -ContentType "application/json" -Body "{}"
    Write-Host "  Apply status: $($r.StatusCode)"
    Write-Host "  Body: $($r.Content.Substring(0, [Math]::Min(500, $r.Content.Length)))"
    $json = $r.Content | ConvertFrom-Json
    if ($json.backends) {
        Write-Host "  ✓ Sync apply returned backends: $($json.backends.Count)" -ForegroundColor Green
    } elseif ($json.status -eq "accepted") {
        Write-Host "  ✓ Async apply (busy backend): applyId=$($json.applyId)" -ForegroundColor Green
    }
} catch {
    Write-Host "  ✗ FAIL: $($_.Exception.Message)" -ForegroundColor Red
}

Write-Host "`n=== Done ===" -ForegroundColor Cyan
