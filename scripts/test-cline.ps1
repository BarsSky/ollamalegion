#!/usr/bin/env pwsh
# Cline-style smoke test through balancer (Round 15, 2026-07-29).
# Tests that streaming chat returns usage chunk by default (без явного
# stream_options.include_usage).

$ErrorActionPreference = "Stop"
$tok = "changeme-bundled-strong-token-please-change"
$balancer = "http://localhost:18080"
$cppworker = "http://localhost:18092"

function Wait-ModelLoaded {
    Write-Host "Waiting for model to load..."
    for ($i = 0; $i -lt 30; $i++) {
        try {
            $resp = Invoke-WebRequest -Uri "$cppworker/api/models" -Headers @{ "X-API-Token" = $tok } -UseBasicParsing -TimeoutSec 3
            $state = ($resp.Content | ConvertFrom-Json).models[0].state
            if ($state -eq "loaded") {
                Write-Host "  loaded after $((($i+1)*3))s"
                return $true
            }
        } catch {}
        Start-Sleep -Seconds 3
    }
    return $false
}

if (-not (Wait-ModelLoaded)) {
    Write-Host "TIMEOUT: model did not load in 90s"
    exit 1
}

# ============================================================
# TEST 1: Cline-style streaming chat (без stream_options)
# Ожидаем usage в финальном чанке (Round 15 default = true)
# ============================================================
Write-Host ""
Write-Host "=== TEST 1: /v1/chat/completions stream (default include_usage) ==="
$body = '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Count from 1 to 3."}],"max_tokens":30,"stream":true,"temperature":0}'
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage":{"prompt_tokens"*') {
        Write-Host "  PASS: usage chunk found by default"
    } else {
        Write-Host "  FAIL: usage chunk MISSING (Round 15 fix needs rebuild)"
    }
} catch {
    Write-Host "ERR: $_"
}

# ============================================================
# TEST 2: opt-out через stream_options.include_usage=false
# Ожидаем ОТСУТСТВИЕ usage chunk
# ============================================================
Write-Host ""
Write-Host "=== TEST 2: explicit include_usage=false (opt-out) ==="
$body2 = '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Count from 1 to 3."}],"max_tokens":30,"stream":true,"temperature":0,"stream_options":{"include_usage":false}}'
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body2 -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    if ($r.Content -like '*"usage"*') {
        Write-Host "  FAIL: usage chunk present despite opt-out"
    } else {
        Write-Host "  PASS: opt-out works (no usage chunk)"
    }
} catch {
    Write-Host "ERR: $_"
}

# ============================================================
# TEST 3: non-streaming /v1/chat/completions
# Ожидаем usage в JSON ответе
# ============================================================
Write-Host ""
Write-Host "=== TEST 3: non-streaming (usage в JSON) ==="
$body3 = '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"Say hi."}],"max_tokens":20,"stream":false}'
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/chat/completions" -Method POST -Headers @{ Authorization = "Bearer $tok"; "Content-Type" = "application/json" } -Body $body3 -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
    $json = $r.Content | ConvertFrom-Json
    if ($json.usage) {
        Write-Host "  PASS: usage = prompt=$($json.usage.prompt_tokens), completion=$($json.usage.completion_tokens), total=$($json.usage.total_tokens)"
    } else {
        Write-Host "  FAIL: usage missing"
    }
} catch {
    Write-Host "ERR: $_"
}

# ============================================================
# TEST 4: Cline-style headers (X-Title, HTTP-Referer)
# Cline identifies itself via these headers for OpenRouter-compatible APIs.
# ============================================================
Write-Host ""
Write-Host "=== TEST 4: Cline-style request with identifying headers ==="
$body4 = '{"model":"gemma-4-E4B-it-Q4_K_M","messages":[{"role":"user","content":"What is 2+2?"}],"max_tokens":20,"stream":false}'
try {
    $r = Invoke-WebRequest -Uri "$balancer/v1/chat/completions" -Method POST -Headers @{
        Authorization = "Bearer $tok"
        "Content-Type" = "application/json"
        "X-Title" = "Cline"
        "HTTP-Referer" = "https://vscode.local/"
        "User-Agent" = "Cline/1.0.0"
    } -Body $body4 -UseBasicParsing -TimeoutSec 30
    Write-Host "STATUS: $($r.StatusCode)"
    Write-Host $r.Content
} catch {
    Write-Host "ERR: $_"
}
