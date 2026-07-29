#!/usr/bin/env pwsh
# Quick test: verify Round 15 usage fix after cppworker rebuild.
$ErrorActionPreference = "Stop"
$tok = "changeme-bundled-strong-token-please-change"
$cppworker = "http://localhost:18092"
$balancer = "http://localhost:18080"

function Wait-ModelLoaded {
    param([int]$timeoutSec = 180)
    Write-Host "Waiting for model to load (up to $timeoutSec sec)..."
    $start = Get-Date
    while (((Get-Date) - $start).TotalSeconds -lt $timeoutSec) {
        try {
            $r = Invoke-WebRequest -Uri "$cppworker/api/models" -Headers @{ "X-API-Token" = $tok } -UseBasicParsing -TimeoutSec 3
            $state = ($r.Content | ConvertFrom-Json).models[0].state
            if ($state -eq "loaded") {
                Write-Host "  loaded after $([int]((Get-Date) - $start).TotalSeconds)s"
                return $true
            }
        } catch {}
        Start-Sleep -Seconds 3
    }
    return $false
}

if (-not (Wait-ModelLoaded)) {
    Write-Host "TIMEOUT"
    exit 1
}

Write-Host ""
Write-Host "=== Direct cppworker (port 18092) ==="
$body = '{"model":"Qwen_Qwen3-4B-Thinking-2507-Q4_K_M","messages":[{"role":"user","content":"Hi."}],"max_tokens":15,"stream":true,"temperature":0}'
try {
    $r = Invoke-WebRequest -Uri "$cppworker/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage"*"prompt_tokens"*') {
        Write-Host "  PASS: usage chunk present (Round 15 fix WORKING)"
    } else {
        Write-Host "  FAIL: usage chunk MISSING"
    }
} catch {
    Write-Host "ERR: $_"
}

Write-Host ""
Write-Host "=== Through balancer (port 18080) ==="
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage"*"prompt_tokens"*') {
        Write-Host "  PASS: usage chunk present through balancer"
    } else {
        Write-Host "  FAIL: usage chunk missing through balancer"
    }
} catch {
    Write-Host "ERR: $_"
}

Write-Host ""
Write-Host "=== /v1/completions through balancer ==="
$body2 = '{"model":"Qwen_Qwen3-4B-Thinking-2507-Q4_K_M","prompt":"Hi","max_tokens":15,"stream":true,"temperature":0}'
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body2 -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage"*"prompt_tokens"*') {
        Write-Host "  PASS: /v1/completions usage chunk present"
    } else {
        Write-Host "  FAIL: /v1/completions usage missing"
    }
} catch {
    Write-Host "ERR: $_"
}

Write-Host ""
Write-Host "=== Opt-out: stream_options.include_usage=false ==="
$body3 = '{"model":"Qwen_Qwen3-4B-Thinking-2507-Q4_K_M","messages":[{"role":"user","content":"Hi."}],"max_tokens":15,"stream":true,"temperature":0,"stream_options":{"include_usage":false}}'
try {
    $r = Invoke-WebRequest -Uri "$cppworker/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body3 -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage"*"prompt_tokens"*') {
        Write-Host "  FAIL: usage chunk present despite opt-out"
    } else {
        Write-Host "  PASS: opt-out works (no usage chunk)"
    }
} catch {
    Write-Host "ERR: $_"
}
