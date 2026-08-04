# scripts/fix-gemma4-retry-loop.ps1 — Helper script for Cline + gemma-4
# Issue: gemma-4 with n_ctx=65536 → upstream GGML_ASSERT → cppworker crash
#         → infinite retry loop (v0.5.13)
# Fix:    circuit breaker (v0.5.14) prevents infinite loop
#         profile gemma-4 n_ctx=8192 for stable load
#
# Usage: .\scripts\fix-gemma4-retry-loop.ps1

$ErrorActionPreference = "Stop"
$token = $env:CPPWORKER_API_TOKEN
if (-not $token) { $envFile = Join-Path $PSScriptRoot "..\deployments\.env.bundled-full"; if (Test-Path $envFile) { Get-Content $envFile | ForEach-Object { if ($_ -match "^\s*CPPWORKER_API_TOKEN=(.+)$") { $token = $Matches[1].Trim('"', "'") } } } }
if (-not $token) { throw "CPPWORKER_API_TOKEN not set" }

Write-Host "=== Cline + gemma-4 diagnostic + fix ===" -ForegroundColor Cyan
Write-Host ""

# 1. Check if gemma-4 profile exists
$hdr = @{ Authorization = "Bearer $token" }
try {
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://192.168.13.20:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M" -Headers $hdr
    $gemmaProfile = $r.Content | ConvertFrom-Json
    Write-Host "[1] gemma-4 profile EXISTS:" -ForegroundColor Green
    Write-Host "    contextLength: $($gemmaProfile.profile.contextLength)"
    Write-Host "    numGpuLayers:  $($gemmaProfile.profile.numGpuLayers)"
    Write-Host "    notes:         $($gemmaProfile.profile.notes)"
} catch {
    Write-Host "[1] gemma-4 profile NOT FOUND" -ForegroundColor Yellow
    Write-Host "    Creating profile with contextLength=8192 (avoids upstream bug)..."
    $body = '{"contextLength":8192,"batchSize":512,"numGpuLayers":-1,"notes":"gemma-4 Cline tool use (n_ctx reduced to avoid upstream GGML_ASSERT)"}'
    try {
        $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri "http://192.168.13.20:18081/api/v1/cppworker/model-profiles/gemma-4-E4B-it-Q4_K_M" `
            -Method Put -Headers $hdr -ContentType "application/json" -Body $body
        Write-Host "    Created OK" -ForegroundColor Green
    } catch {
        Write-Host "    Failed: $($_.Exception.Message)" -ForegroundColor Red
    }
}

# 2. Check current model state
Write-Host ""
try {
    $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "http://192.168.13.20:18092/api/models" -Headers $hdr
    $j = $r.Content | ConvertFrom-Json
    $gemma = $j.models | Where-Object { $_.name -eq "gemma-4-E4B-it-Q4_K_M" } | Select-Object -First 1
    if ($gemma) {
        Write-Host "[2] gemma-4 state: $($gemma.state), n_ctx=$($gemma.contextSize)" -ForegroundColor Yellow
        if ($gemma.state -eq "loading") {
            Write-Host "    NOTE: gemma-4 is currently loading. Wait for completion or restart."
        }
    } else {
        Write-Host "[2] gemma-4: NOT LOADED" -ForegroundColor Green
    }
} catch {
    Write-Host "[2] cppworker not responding" -ForegroundColor Yellow
}

# 3. Check circuit breaker state via balancer logs
Write-Host ""
Write-Host "[3] Recent balancer events (last 30 lines):" -ForegroundColor Yellow
cmd /c "docker logs ol-bundled-full-balancer --tail 30" 2>&1 | Out-File -Encoding utf8 -FilePath "$PSScriptRoot\..\logs\balancer_recent.log"
$lines = Get-Content "$PSScriptRoot\..\logs\balancer_recent.log"
$breaker = $lines | Select-String "circuit breaker|loadBackoff" | Select-Object -Last 5
if ($breaker) {
    $breaker | ForEach-Object { Write-Host "    $_" }
} else {
    Write-Host "    (no recent circuit breaker events)"
}

# 4. Recommendations
Write-Host ""
Write-Host "=== RECOMMENDATIONS ===" -ForegroundColor Cyan
Write-Host ""
Write-Host "Option A: Use Qwen3-Instruct-2507-q4km (RECOMMENDED for Cline tool use)"
Write-Host "  - 2.5GB, loads in ~30s, no upstream bugs"
Write-Host "  - Set in Cline config: model = qwen3-instruct-2507-q4km"
Write-Host "  - Or just use the alias 'qwen3' via balancer"
Write-Host ""
Write-Host "Option B: Use gemma-4 with smaller n_ctx (WORKAROUND)"
Write-Host "  - Set Cline config: num_ctx = 8192 (or lower)"
Write-Host "  - OR apply profile via WebUI: gguf models → backend → Settings →"
Write-Host "    Edit profile gemma-4-E4B-it-Q4_K_M → Save + Apply"
Write-Host ""
Write-Host "Option C: Pre-load gemma-4 manually (TEST ONLY)"
Write-Host "  - Load via WebUI: gguf models → click on gemma-4 file → Load"
Write-Host "  - Model stays in VRAM for keep_alive (default 30m)"
Write-Host "  - Cline can use it without auto-load"
Write-Host ""
Write-Host "If Cline shows 'circuit breaker open' error:"
Write-Host "  - WAIT 60 seconds for breaker to expire"
Write-Host "  - Use a different model (Qwen3) to avoid the loop"
Write-Host "  - Or fix upstream llama.cpp (out of our scope)"
