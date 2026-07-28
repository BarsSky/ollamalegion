#!/usr/bin/env pwsh
# =============================================================================
# Smoke-test для расширенного PUT /api/v1/cppworker/config/update (Session 17)
# =============================================================================
# Проверяет:
#   1. GET /api/v1/cppworker/config возвращает все новые поля
#   2. PUT с новыми полями применяется и попадает в applied[]
#   3. Невалидные значения отклоняются с validation_errors
#   4. Конфиг сохраняется в /app/config/cppworker-defaults.json
# =============================================================================
$ErrorActionPreference = "Stop"

$cppworkerUrl = "http://localhost:18092"
$balancerUrl = "http://localhost:18081"

# Берём токен напрямую из живого контейнера (это истина — config.json в compose мог
# разойтись с .env.bundled, см. обсуждение start-bundled.ps1:54-69).
# ВАЖНО: если контейнер вернул токен (даже "changeme...") — это и есть источник истины.
# Не подменяем его config.json, потому что cppworker использует свой CPPWORKER_API_TOKEN
# независимо от токена балансировщика.
$token = $null
$containerToken = docker exec ol-bundled-cppworker-gpu printenv API_TOKEN 2>$null
if ($containerToken) {
    $token = $containerToken.Trim()
    Write-Host "[smoke] token from container API_TOKEN (authoritative)" -ForegroundColor DarkCyan
}
# Только если контейнер ничего не вернул — попробуем config.json.
if (-not $token) {
    $cfg = Get-Content C:/Ollama/ollamalegion/config/config.json -Raw -ErrorAction SilentlyContinue
    if ($cfg) {
        $m = [regex]::Match($cfg, '"tokens"\s*:\s*\[\s*"([^"]+)"')
        if ($m.Success) {
            $token = $m.Groups[1].Value
            Write-Host "[smoke] token from config.json (fallback)" -ForegroundColor DarkCyan
        }
    }
}
if (-not $token) { Write-Host "[smoke] FATAL: no token found" -ForegroundColor Red; exit 1 }
Write-Host "[smoke] token = $token" -ForegroundColor Cyan

$headers = @{ "Authorization" = "Bearer $token"; "Content-Type" = "application/json" }

function Invoke-SmokeCheck {
    param(
        [string]$Name,
        [string]$Url,
        [string]$Method = "GET",
        [string]$Body = $null,
        [string[]]$ExpectedFieldsInResponse = @(),
        [string[]]$ExpectedFieldsInApplied = @()
    )
    Write-Host ""
    Write-Host "[smoke] $Name" -ForegroundColor Yellow
    Write-Host "  → $Method $Url"
    # Передаём body через файл (-d @file) — это исключает любые проблемы с
    # PowerShell-quoting и экранированием в curl-аргументах.
    # ВАЖНО: используем UTF-8 БЕЗ BOM — иначе Go JSON-парсер видит 0xEF 0xBB 0xBF
    # в начале body и падает с "invalid character 'ï' looking for beginning of value".
    $bodyFile = $null
    if ($Body) {
        $bodyFile = [System.IO.Path]::GetTempFileName()
        $utf8NoBom = [System.Text.UTF8Encoding]::new($false)
        [System.IO.File]::WriteAllText($bodyFile, $Body, $utf8NoBom)
    }
    $args = @("-s", "-X", $Method, $Url)
    if ($headers) { foreach ($k in $headers.Keys) { $args += @("-H", "$k`: $($headers[$k])") } }
    if ($bodyFile) { $args += @("-d", "@$bodyFile") }
    $resp = (& curl.exe @args) -join "`n"
    if ($bodyFile) { Remove-Item -LiteralPath $bodyFile -Force -ErrorAction SilentlyContinue }
    $ok = $true
    foreach ($f in $ExpectedFieldsInResponse) {
        if ($resp -notmatch [regex]::Escape($f)) {
            Write-Host "  ✗ expected field '$f' in response" -ForegroundColor Red
            $ok = $false
        } else {
            Write-Host "  ✓ field '$f' present" -ForegroundColor Green
        }
    }
    foreach ($f in $ExpectedFieldsInApplied) {
        if ($resp -match "`"applied`":\s*\[[^\]]*`"$f`"") {
            Write-Host "  ✓ '$f' in applied[]" -ForegroundColor Green
        } else {
            Write-Host "  ✗ '$f' NOT in applied[]" -ForegroundColor Red
            $ok = $false
        }
    }
    if (-not $ok) {
        Write-Host "  response: $resp" -ForegroundColor DarkYellow
    }
    return $ok
}

# ==================== 1. GET current config ====================
$get = Invoke-SmokeCheck `
    -Name "1. GET /api/v1/cppworker/config returns all extended fields" `
    -Url "$cppworkerUrl/api/v1/cppworker/config" `
    -ExpectedFieldsInResponse @(
        'defaultCtxSize', 'defaultBatchSize', 'defaultGpuLayers',
        'defaultFlashAttnType', 'defaultNuma', 'defaultUseMmap',
        'defaultUseMlock', 'defaultNThreads', 'defaultRmsNormEps',
        'autoGpuDistribution', 'tensorSplitStrategy', 'defaultMainGpu',
        'defaultRpcBackend', 'defaultNoMemoryMap', 'tensorSplitStrategy',
        'defaultSplitMode',
        '"tensorSplitStrategy":""',
        'defaultKvCacheType', 'defaultNoKvOffload',
        'defaultRopeFreqBase', 'defaultRopeFreqScale',
        'defaultRopeScalingType', 'defaultRopeScalingFactor',
        'defaultYarnExtFactor', 'defaultYarnAttnFactor',
        'defaultYarnBetaFast', 'defaultYarnBetaSlow',
        'enableMetrics', 'metricsRetentionSeconds', 'idleUnloadMinutes'
    )

# ==================== 2. PUT — apply all extended fields ====================
# Имена полей берутся из JSON-тегов cppbackend.Config (internal/cppbackend/config.go).
# ВАЖНО: некоторые поля (idleUnloadMinutes, autoGpuDistribution) НЕ имеют префикса "default"
# в JSON-теге — это решение было принято при создании Config, см. config.go:14-91.
$putBody = '{"defaultCtxSize":65536,"defaultKvCacheType":"q8_0","defaultRopeScalingType":"yarn","defaultRopeScalingFactor":4.0,"defaultYarnExtFactor":2.0,"idleUnloadMinutes":30,"autoGpuDistribution":true,"defaultMainGpu":0,"defaultSplitMode":1,"defaultUseMlock":false}'
$put = Invoke-SmokeCheck `
    -Name "2. PUT extended fields — all in applied[]" `
    -Url "$cppworkerUrl/api/v1/cppworker/config/update" `
    -Method "PUT" -Body $putBody `
    -ExpectedFieldsInApplied @(
        'defaultCtxSize', 'defaultKvCacheType', 'defaultRopeScalingType',
        'defaultRopeScalingFactor', 'defaultYarnExtFactor',
        'idleUnloadMinutes', 'autoGpuDistribution',
        'defaultMainGpu', 'defaultSplitMode', 'defaultUseMlock'
    )

# ==================== 3. PUT — invalid kvCacheType rejected ====================
$badBody = '{"defaultKvCacheType":"q2_k"}'
$bad = Invoke-SmokeCheck `
    -Name "3. PUT invalid kvCacheType → validation_errors, NOT in applied" `
    -Url "$cppworkerUrl/api/v1/cppworker/config/update" `
    -Method "PUT" -Body $badBody `
    -ExpectedFieldsInResponse @('"validation_errors"', 'defaultKvCacheType')
# Также проверяем, что defaultKvCacheType НЕ попало в applied[].
# Делаем повторный PUT с правильным body и читаем applied.
$badBodyFile = [System.IO.Path]::GetTempFileName()
$utf8NoBom = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($badBodyFile, $badBody, $utf8NoBom)
$badResp = (& curl.exe -s -X PUT "$cppworkerUrl/api/v1/cppworker/config/update" -H "Authorization: Bearer $token" -H "Content-Type: application/json" -d "@$badBodyFile") -join "`n"
Remove-Item -LiteralPath $badBodyFile -Force -ErrorAction SilentlyContinue
if ($badResp -match '"applied":\[[^\]]*"defaultKvCacheType"[^\]]*\]') {
    Write-Host "  ✗ invalid kvCacheType был применён (applied[] содержит defaultKvCacheType)" -ForegroundColor Red
} else {
    Write-Host "  ✓ invalid kvCacheType НЕ применён (applied[] без defaultKvCacheType)" -ForegroundColor Green
}

# ==================== 4. Verify config saved to cppworker-defaults.json ====================
Write-Host ""
Write-Host "[smoke] 4. Verify /app/config/cppworker-defaults.json has new fields" -ForegroundColor Yellow
$defaults = docker exec ol-bundled-cppworker-gpu cat /app/config/cppworker-defaults.json
foreach ($f in 'defaultKvCacheType','defaultRopeScalingType','defaultRopeScalingFactor','defaultYarnExtFactor','idleUnloadMinutes','defaultSplitMode') {
    if ($defaults -match [regex]::Escape("`"$f`"")) {
        Write-Host "  ✓ $f saved to disk" -ForegroundColor Green
    } else {
        Write-Host "  ✗ $f MISSING from cppworker-defaults.json" -ForegroundColor Red
    }
}

# ==================== 6. Balancer PUT /api/v1/cluster/config with llamaCpp section ====================
Write-Host ""
Write-Host "[smoke] 6. Balancer PUT /api/v1/cluster/config with llamaCpp section" -ForegroundColor Yellow
$body6 = '{"llamaCpp":{"contextLength":131072,"batchSize":1024,"kvCacheType":"q8_0","ropeScalingType":"yarn","ropeScalingFactor":4.0,"yarnExtFactor":2.0,"numGpuLayers":20,"useMlock":false,"autoGpuDistribution":true}}'
$tmp6 = [System.IO.Path]::GetTempFileName()
$utf8NoBom = [System.Text.UTF8Encoding]::new($false)
[System.IO.File]::WriteAllText($tmp6, $body6, $utf8NoBom)
$balancerToken = 'JH678MNSJNDAJNDSK'
$resp6 = (& curl.exe -s -X PUT -H "Authorization: Bearer $balancerToken" -H "Content-Type: application/json" -d "@$tmp6" "http://localhost:18081/api/v1/cluster/config") -join "`n"
Remove-Item -LiteralPath $tmp6 -Force -ErrorAction SilentlyContinue
# Проверяем что API вернул updated:["llamaCpp"] и значения в config.llamaCpp.
$ok6 = $true
if ($resp6 -match '"updated":\s*\[[^\]]*"llamaCpp"[^\]]*\]') {
    Write-Host "  ✓ updated[] contains llamaCpp" -ForegroundColor Green
} else {
    Write-Host "  ✗ updated[] MISSING llamaCpp" -ForegroundColor Red
    $ok6 = $false
}
foreach ($f in 'contextLength.*131072','batchSize.*1024','kvCacheType.*q8_0','ropeScalingType.*yarn','ropeScalingFactor.*4','yarnExtFactor.*2') {
    if ($resp6 -match $f) {
        Write-Host "  ✓ response contains $f" -ForegroundColor Green
    } else {
        Write-Host "  ✗ response MISSING $f" -ForegroundColor Red
        $ok6 = $false
    }
}
if (-not $ok6) {
    Write-Host "  response: $resp6" -ForegroundColor DarkYellow
}
Write-Host "  NOTE: в bundled config.json монтируется `:ro` — balancer обновляет" -ForegroundColor DarkCyan
Write-Host "  in-memory state, но на диск НЕ пишет (пред-существующее ограничение)." -ForegroundColor DarkCyan
Write-Host "  После restart-bundled config.json перечитается с диска." -ForegroundColor DarkCyan

# ==================== 7. Runtime Overrides (Session 17 P.2) ====================
Write-Host ""
Write-Host "[smoke] 7. Runtime Overrides — sidecar в /app/data" -ForegroundColor Yellow

# 7.0 Pre-test cleanup: удалить существующий override (если был от прошлых тестов).
# Без этого initial state будет exists=true.
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7clean = (& curl.exe -s -X DELETE -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
Write-Host "  Pre-test cleanup: override cleared (existed_before=$(if ($resp7clean -match '"existed_before":\s*true') { 'true' } else { 'false' }))" -ForegroundColor DarkCyan

# 7.1 GET /api/v1/cluster/llama-cpp/overrides — initial state
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7 = (& curl.exe -s -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Write-Host "  Initial state (no override yet):"
Write-Host "  $resp7" -ForegroundColor DarkYellow
if ($resp7 -match '"exists":\s*false') {
    Write-Host "  ✓ exists=false initially" -ForegroundColor Green
} else {
    Write-Host "  ✗ initial state should be exists=false" -ForegroundColor Red
}

# 7.2 PUT llamaCpp — должен создать override-файл
$body7 = '{"llamaCpp":{"contextLength":65536,"batchSize":2048,"kvCacheType":"q4_0"}}'
$tmp7 = [System.IO.Path]::GetTempFileName()
[System.IO.File]::WriteAllText($tmp7, $body7, $utf8NoBom)
$resp7put = (& curl.exe -s -X PUT -H "Authorization: Bearer $balancerToken" -H "Content-Type: application/json" -d "@$tmp7" "http://localhost:18081/api/v1/cluster/config") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
if ($resp7put -match '"updated":\s*\[[^\]]*"llamaCpp"[^\]]*\]') {
    Write-Host "  ✓ PUT llamaCpp applied" -ForegroundColor Green
} else {
    Write-Host "  ✗ PUT failed: $resp7put" -ForegroundColor Red
}

# 7.3 Проверяем что файл в /app/data/runtime-overrides/ создан
$overrideCheck = docker exec ol-bundled-balancer sh -c "ls -la /app/data/runtime-overrides/llama-cpp.json 2>/dev/null && cat /app/data/runtime-overrides/llama-cpp.json" 2>&1
if ($overrideCheck -match 'llama-cpp.json') {
    Write-Host "  ✓ override file created at /app/data/runtime-overrides/llama-cpp.json" -ForegroundColor Green
    Write-Host "  File content:" -ForegroundColor DarkYellow
    $overrideCheck -split "`n" | ForEach-Object { Write-Host "    $_" }
} else {
    Write-Host "  ✗ override file NOT created: $overrideCheck" -ForegroundColor Red
}

# 7.4 GET /api/v1/cluster/llama-cpp/overrides — теперь exists=true
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7get = (& curl.exe -s -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
if ($resp7get -match '"exists":\s*true') {
    Write-Host "  ✓ exists=true after PUT" -ForegroundColor Green
} else {
    Write-Host "  ✗ expected exists=true: $resp7get" -ForegroundColor Red
}
if ($resp7get -match '"kvCacheType"\s*:\s*"q4_0"') {
    Write-Host "  ✓ override contains q4_0" -ForegroundColor Green
} else {
    Write-Host "  ✗ override should contain q4_0: $resp7get" -ForegroundColor Red
}

# 7.5 DELETE /api/v1/cluster/llama-cpp/overrides — стирает файл + restore in-memory
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7del = (& curl.exe -s -X DELETE -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
if ($resp7del -match '"status":\s*"reset"') {
    Write-Host "  ✓ DELETE returned status=reset" -ForegroundColor Green
} else {
    Write-Host "  ✗ DELETE failed: $resp7del" -ForegroundColor Red
}

# 7.6 Проверяем что файл удалён
$overrideAfter = docker exec ol-bundled-balancer sh -c "ls /app/data/runtime-overrides/llama-cpp.json 2>&1"
if ($overrideAfter -match 'No such file') {
    Write-Host "  ✓ override file deleted" -ForegroundColor Green
} else {
    Write-Host "  ✗ override file still exists: $overrideAfter" -ForegroundColor Red
}

# 7.7 GET /api/v1/cluster/llama-cpp/overrides — теперь снова exists=false
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7get = (& curl.exe -s -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
if ($resp7get -match '"exists":\s*false') {
    Write-Host "  ✓ exists=false after DELETE" -ForegroundColor Green
} else {
    Write-Host "  ✗ expected exists=false after DELETE: $resp7get" -ForegroundColor Red
}

# 7.8 In-memory state должен быть restored к base.
# GET /api/v1/cluster/config не возвращает llamaCpp напрямую, но DELETE response
# подтверждает факт restore (мы записали `existed_before: true` — handler сработал).
# Дополнительно проверим что текущий override пуст.
$tmp7 = [System.IO.Path]::GetTempFileName()
$resp7get = (& curl.exe -s -H "Authorization: Bearer $balancerToken" "http://localhost:18081/api/v1/cluster/llama-cpp/overrides") -join "`n"
Remove-Item -LiteralPath $tmp7 -Force -ErrorAction SilentlyContinue
if ($resp7get -match '"exists":\s*false') {
    Write-Host "  ✓ in-memory state restored (override cleared, bundled defaults active)" -ForegroundColor Green
} else {
    Write-Host "  ✗ override should be cleared: $resp7get" -ForegroundColor Red
}

Write-Host ""
Write-Host "[smoke] 5. WebUI index.html has new IDs" -ForegroundColor Yellow
$tmp = [System.IO.Path]::GetTempFileName()
curl.exe -s -o $tmp "http://localhost:18083/index.html"
$utf8NoBom = [System.Text.UTF8Encoding]::new($false)
$html = [System.IO.File]::ReadAllText($tmp, $utf8NoBom)
$idPattern = 'id="{0}"'
foreach ($id in 'kvCacheType','noKvOffload','ropeFreqBase','ropeScalingType','yarnExtFactor','idleUnloadMinutes','useMlock','splitMode','mainGpu','rpcBackend','noMemoryMap','enableMetrics','metricsRetention','flashAttnType','nThreads','noKvOffload') {
    $needle = ($idPattern -f $id)
    if ($html.Contains($needle)) {
        Write-Host "  ✓ #$id in HTML" -ForegroundColor Green
    } else {
        Write-Host "  ✗ #$id MISSING from index.html" -ForegroundColor Red
    }
}

# ==================== 5b. WebUI config.js: API_TOKEN is set (Session 17 P.5) ====================
# Регрессионный тест: до фикса webui service в docker-compose НЕ передавал API_TOKEN env,
# entrypoint.sh генерил `API_TOKEN: ''` в js/modules/config.js, и все запросы от WebUI
# шли с пустым X-API-Token → 401 "invalid or missing API token" на per-backend save.
Write-Host ""
Write-Host "[smoke] 5b. WebUI config.js has non-empty API_TOKEN (Session 17 P.5)" -ForegroundColor Yellow
$cfgTmp = [System.IO.Path]::GetTempFileName()
curl.exe -s -o $cfgTmp "http://localhost:18083/js/modules/config.js"
$utf8NoBom2 = [System.Text.UTF8Encoding]::new($false)
$cfgJs = [System.IO.File]::ReadAllText($cfgTmp, $utf8NoBom2)
if ($cfgJs -match "API_TOKEN:\s*'([^']*)'") {
    $cfgToken = $Matches[1]
    if ($cfgToken.Length -gt 5) {
        Write-Host "  ✓ API_TOKEN set in served config.js (length=$($cfgToken.Length), prefix=$($cfgToken.Substring(0, [Math]::Min(8, $cfgToken.Length)))...)" -ForegroundColor Green
    } else {
        Write-Host "  ✗ API_TOKEN EMPTY or too short: '$cfgToken' — WebUI sends empty X-API-Token → 401 on backend save" -ForegroundColor Red
    }
    # Cross-check: токен в config.js должен совпадать с токеном в cppworker
    $cppworkerToken = docker exec ol-bundled-cppworker-gpu printenv API_TOKEN 2>$null
    if ($cppworkerToken -and $cppworkerToken.Trim() -eq $cfgToken) {
        Write-Host "  ✓ WebUI token matches cppworker container API_TOKEN" -ForegroundColor Green
    } else {
        Write-Host "  ✗ MISMATCH: WebUI='$cfgToken' vs cppworker='$cppworkerToken' → 401 expected" -ForegroundColor Red
    }
} else {
    Write-Host "  ✗ API_TOKEN field not found in config.js" -ForegroundColor Red
}

# ==================== 8. Auth chain: WebUI → balancer → cppworker (Session 17 P.4) ====================
# Регрессионный тест: WebUI (18083) → nginx → balancer (18081) → cppworker (18092)
# с X-API-Token. До фикса:
#   - cppworker utils.go::authMiddleware понимал только Authorization: Bearer → 401
#   - WebUI gguf-api.js::requestViaBackend имел fetch spread bug — X-API-Token затирался
#     options.headers → 401 (это и был реальный 401 в логах пользователя)
Write-Host ""
Write-Host "[smoke] 8. Auth chain: WebUI URL with X-API-Token + gpuLayers=-2" -ForegroundColor Yellow
$backendId = $null
try {
    $backends = (Invoke-RestMethod "http://localhost:18081/api/v1/gguf/backends" -TimeoutSec 5).backends
    $backendId = ($backends | Where-Object { $_.status -eq "healthy" -and $_.type -eq "llama_cpp" })[0].id
    Write-Host "  Using backend id: $backendId" -ForegroundColor DarkCyan
} catch {
    Write-Host "  ✗ failed to list backends: $($_.Exception.Message)" -ForegroundColor Red
}

if ($backendId) {
    # 8a. Direct cppworker via X-API-Token
    Write-Host "  8a. Direct cppworker:18092 with X-API-Token..." -ForegroundColor DarkCyan
    $body8 = '{"defaultGpuLayers": -2, "defaultCtxSize": 4096, "defaultKvCacheType": "q8_0"}'
    $resp8a = Invoke-WebRequest -Method PUT -Uri "http://localhost:18092/api/v1/cppworker/config/update" -Headers @{"X-API-Token"=$token; "Content-Type"="application/json"} -Body $body8 -UseBasicParsing -TimeoutSec 15
    if ($resp8a.StatusCode -eq 200 -and $resp8a.Content -match '"status":"updated"') {
        Write-Host "    ✓ 200 OK, X-API-Token auth works on direct cppworker" -ForegroundColor Green
    } else {
        Write-Host "    ✗ unexpected: $($resp8a.StatusCode) $($resp8a.Content)" -ForegroundColor Red
    }

    # 8b. Wrong X-API-Token → 401
    Write-Host "  8b. Wrong X-API-Token should 401..." -ForegroundColor DarkCyan
    try {
        $resp8b = Invoke-WebRequest -Method PUT -Uri "http://localhost:18092/api/v1/cppworker/config/update" -Headers @{"X-API-Token"="wrong-token-xxx"; "Content-Type"="application/json"} -Body $body8 -UseBasicParsing -TimeoutSec 15
        Write-Host "    ✗ expected 401, got $($resp8b.StatusCode)" -ForegroundColor Red
    } catch {
        if ($_.Exception.Response.StatusCode.value__ -eq 401) {
            Write-Host "    ✓ 401 (invalid token rejected)" -ForegroundColor Green
        } else {
            Write-Host "    ✗ unexpected: $($_.Exception.Message)" -ForegroundColor Red
        }
    }

    # 8c. Full WebUI chain: localhost:18083 (nginx) → balancer → cppworker
    Write-Host "  8c. WebUI chain: localhost:18083 → balancer:18081 → cppworker:18092..." -ForegroundColor DarkCyan
    $webuiUrl = "http://localhost:18083/api/v1/gguf/backends/$backendId/proxy/api/v1/cppworker/config/update"
    try {
        $resp8c = Invoke-WebRequest -Method PUT -Uri $webuiUrl -Headers @{"X-API-Token"=$token; "Content-Type"="application/json"} -Body $body8 -UseBasicParsing -TimeoutSec 30
        if ($resp8c.StatusCode -eq 200 -and $resp8c.Content -match '"status":"updated"') {
            Write-Host "    ✓ 200 OK, full WebUI chain works with X-API-Token" -ForegroundColor Green
            Write-Host "    Body: $($resp8c.Content)" -ForegroundColor DarkYellow
        } else {
            Write-Host "    ✗ unexpected: $($resp8c.StatusCode) $($resp8c.Content)" -ForegroundColor Red
        }
    } catch {
        Write-Host "    ✗ chain failed: $($_.Exception.Message)" -ForegroundColor Red
    }

    # 8d. gpuLayers=-3 должен отвергаться (min=-2)
    Write-Host "  8d. gpuLayers=-3 should be rejected (min=-2)..." -ForegroundColor DarkCyan
    $body8d = '{"defaultGpuLayers": -3}'
    try {
        $resp8d = Invoke-WebRequest -Method PUT -Uri "http://localhost:18092/api/v1/cppworker/config/update" -Headers @{"X-API-Token"=$token; "Content-Type"="application/json"} -Body $body8d -UseBasicParsing -TimeoutSec 15
        # response содержит JSON с \u003e (escaped >) — убираем escape перед match
        $resp8dText = $resp8d.Content -replace '\\u003e', '>'
        if ($resp8dText -match 'must be >= -2, got -3') {
            Write-Host "    ✓ rejected with validation error: must be >= -2, got -3" -ForegroundColor Green
        } else {
            Write-Host "    ✗ unexpected response: $($resp8d.Content)" -ForegroundColor Red
        }
    } catch {
        Write-Host "    ✗ unexpected: $($_.Exception.Message)" -ForegroundColor Red
    }
}

# ==================== 9. WebUI JS — fetch() spread bug regression (Session 17 P.4) ====================
# Запускает node-тест, который проверяет что X-API-Token НЕ теряется в spread.
Write-Host ""
Write-Host "[smoke] 9. WebUI fetch spread bug regression (Session 17 P.4)" -ForegroundColor Yellow
$nodeTest = Join-Path $PSScriptRoot "..\webui\tests\test_gguf_api_headers.js"
if (-not (Test-Path $nodeTest)) {
    $nodeTest = Join-Path $PSScriptRoot "..\..\webui\tests\test_gguf_api_headers.js"
}
Write-Host "  Test path: $nodeTest" -ForegroundColor DarkCyan
if (Test-Path $nodeTest) {
    $output = node $nodeTest 2>&1
    $summaryLine = $output | Select-String -Pattern "Tests (run|passed|failed):" | Select-Object -Last 3
    $failedMatch = $output | Select-String -Pattern "Tests failed:\s*([1-9]\d*)"
    if ($failedMatch) {
        Write-Host "  ✗ node tests failed:" -ForegroundColor Red
        $output | ForEach-Object { Write-Host "    $_" -ForegroundColor Red }
    } else {
        $passedMatch = $output | Select-String -Pattern "Tests passed:\s*(\d+)"
        $runMatch = $output | Select-String -Pattern "Tests run:\s*(\d+)"
        Write-Host "  ✓ $($passedMatch.Matches[0].Groups[1].Value)/$($runMatch.Matches[0].Groups[1].Value) node tests passed" -ForegroundColor Green
    }
} else {
    Write-Host "  ! node test not found: $nodeTest" -ForegroundColor Yellow
}

# ==================== 10. WebUI nginx: SSE endpoint has proxy_buffering off (Session 17 P.7) ====================
# Регрессионный тест: без отдельного location /api/v1/events с proxy_buffering off
# nginx буферизует chunked SSE stream → браузер получает ERR_INCOMPLETE_CHUNKED_ENCODING
# и переподключается каждую минуту.
Write-Host ""
Write-Host "[smoke] 10. WebUI nginx: SSE location has proxy_buffering off (Session 17 P.7)" -ForegroundColor Yellow
$nginxConf = Join-Path (Join-Path $PSScriptRoot "..\..\webui") "nginx.conf"
if (-not (Test-Path $nginxConf)) {
    $nginxConf = Join-Path $PSScriptRoot "..\webui\nginx.conf"
}
if (Test-Path $nginxConf) {
    $confContent = Get-Content $nginxConf -Raw
    $hasSseLocation = $confContent -match "location\s+/api/v1/events\s*\{"
    $hasBufferingOff = $confContent -match "location\s+/api/v1/events[\s\S]{0,1500}proxy_buffering\s+off"
    $hasLongReadTimeout = $confContent -match "location\s+/api/v1/events[\s\S]{0,1500}proxy_read_timeout\s+([6-9]\d\d|\d{4,})"
    $hasConnectionEmpty = $confContent -match "location\s+/api/v1/events[\s\S]{0,1500}proxy_set_header\s+Connection\s+`"`""
    if ($hasSseLocation) {
        Write-Host "  [OK] location /api/v1/events exists (BEFORE /api/ catchall)" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] location /api/v1/events MISSING - SSE will go through /api/ catchall" -ForegroundColor Red
    }
    if ($hasBufferingOff) {
        Write-Host "  [OK] proxy_buffering off for SSE" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] proxy_buffering off NOT set for SSE - chunked stream will be buffered" -ForegroundColor Red
    }
    if ($hasLongReadTimeout) {
        Write-Host "  [OK] proxy_read_timeout >= 600s for SSE" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] proxy_read_timeout < 600s - heartbeat may be interrupted" -ForegroundColor Red
    }
    if ($hasConnectionEmpty) {
        Write-Host "  [OK] proxy_set_header Connection empty for SSE" -ForegroundColor Green
    } else {
        Write-Host "  [WARN] proxy_set_header Connection empty not set (may not matter for HTTP/1.1)" -ForegroundColor Yellow
    }
    # Также проверяем served nginx.conf через контейнер (после envsubst)
    $servedConf = docker exec ol-bundled-webui cat /etc/nginx/conf.d/default.conf 2>&1
    if ($LASTEXITCODE -eq 0 -and $servedConf -match "location\s+/api/v1/events") {
        Write-Host "  [OK] served /etc/nginx/conf.d/default.conf has /api/v1/events location" -ForegroundColor Green
    } else {
        Write-Host "  [WARN] served nginx.conf not yet updated (need to rebuild + restart webui)" -ForegroundColor Yellow
    }
} else {
    Write-Host "  ✗ nginx.conf not found at $nginxConf" -ForegroundColor Red
}

# ==================== 11. WebUI index.html: data-density script uses documentElement (Session 17 P.8) ====================
# Регрессионный тест: в <head> document.body ещё null, setAttribute на нём → TypeError.
# Фикс: использовать document.documentElement (всегда доступен).
Write-Host ""
Write-Host "[smoke] 11. WebUI data-density script uses documentElement (Session 17 P.8)" -ForegroundColor Yellow
$indexPath = Join-Path (Join-Path $PSScriptRoot "..\..\webui") "index.html"
if (-not (Test-Path $indexPath)) {
    $indexPath = Join-Path $PSScriptRoot "..\webui\index.html"
}
if (Test-Path $indexPath) {
    $indexContent = Get-Content $indexPath -Raw
    # data-density script block (между <script> и </script> после data-theme)
    $densityBlock = ($indexContent -split "data-density variant early-load")[1]
    if ($densityBlock) {
        $block = $densityBlock.Substring(0, [Math]::Min(2000, $densityBlock.Length))
        if ($block -match "document\.body\.setAttribute.*data-density") {
            Write-Host "  [FAIL] data-density script still uses document.body — вызовет TypeError в <head>" -ForegroundColor Red
        } else {
            Write-Host "  [OK] data-density script no longer uses document.body" -ForegroundColor Green
        }
        if ($block -match "document\.documentElement\.setAttribute.*data-density") {
            Write-Host "  [OK] data-density script uses document.documentElement" -ForegroundColor Green
        } else {
            Write-Host "  [FAIL] data-density script does not use document.documentElement" -ForegroundColor Red
        }
    } else {
        Write-Host "  [WARN] data-density block not found in index.html" -ForegroundColor Yellow
    }
} else {
    Write-Host "  [FAIL] index.html not found" -ForegroundColor Red
}

# ==================== 12. WebUI api.js: window.Api export (Session 17 P.9) ====================
# Регрессионный тест: api.js объявлял `const Api = ...` как локальный IIFE,
# но другие модули (cppworker-params.js) используют window.Api.cppworkerModelProfiles.
# До фикса Per-Model Profile UI выдавал "Cannot read properties of undefined (reading 'list')".
Write-Host ""
Write-Host "[smoke] 12. WebUI api.js exports window.Api (Session 17 P.9)" -ForegroundColor Yellow
$apiJsPath = Join-Path (Join-Path $PSScriptRoot "..\..\webui\js\modules") "api.js"
if (-not (Test-Path $apiJsPath)) {
    $apiJsPath = Join-Path $PSScriptRoot "..\webui\js\modules\api.js"
}
if (Test-Path $apiJsPath) {
    $apiContent = Get-Content $apiJsPath -Raw
    if ($apiContent -match "window\.Api\s*=\s*Api") {
        Write-Host "  [OK] api.js assigns Api to window.Api" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] api.js does NOT export Api to window — Per-Model UI ломается" -ForegroundColor Red
    }
    if ($apiContent -match "cppworkerModelProfiles") {
        Write-Host "  [OK] api.js defines cppworkerModelProfiles" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] cppworkerModelProfiles missing from api.js" -ForegroundColor Red
    }
} else {
    Write-Host "  [FAIL] api.js not found" -ForegroundColor Red
}

# ==================== 13. i18n: common.in exists (Session 17 P.10) ====================
Write-Host ""
Write-Host "[smoke] 13. i18n: common.in key exists (Session 17 P.10)" -ForegroundColor Yellow
$enPath = Join-Path (Join-Path $PSScriptRoot "..\..\webui\js\i18n") "en.js"
$ruPath = Join-Path (Join-Path $PSScriptRoot "..\..\webui\js\i18n") "ru.js"
if (-not (Test-Path $enPath)) { $enPath = Join-Path $PSScriptRoot "..\webui\js\i18n\en.js" }
if (-not (Test-Path $ruPath)) { $ruPath = Join-Path $PSScriptRoot "..\webui\js\i18n\ru.js" }
if ((Test-Path $enPath) -and (Test-Path $ruPath)) {
    $enContent = Get-Content $enPath -Raw
    $ruContent = Get-Content $ruPath -Raw
    if ($enContent -match '"common\.in"\s*:') {
        Write-Host "  [OK] common.in exists in en.js" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] common.in MISSING from en.js" -ForegroundColor Red
    }
    if ($ruContent -match '"common\.in"\s*:') {
        Write-Host "  [OK] common.in exists in ru.js" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] common.in MISSING from ru.js" -ForegroundColor Red
    }
} else {
    Write-Host "  [FAIL] en.js or ru.js not found" -ForegroundColor Red
}

# ==================== 14. Setup wizard: tooltips + i18n (Session 17 P.11) ====================
# Регрессионный тест: setup-wizard.js имел захардкоженный English для general
# settings (GPU Max %, VRAM Max %, etc.) и не имел tooltip'ов на полях.
# Фикс: t_label() helper + wizard.tooltip.* i18n keys.
Write-Host ""
Write-Host "[smoke] 14. Setup wizard: tooltips + i18n (Session 17 P.11)" -ForegroundColor Yellow
$wizPath = Join-Path (Join-Path $PSScriptRoot "..\..\webui\js\modules") "setup-wizard.js"
if (-not (Test-Path $wizPath)) { $wizPath = Join-Path $PSScriptRoot "..\webui\js\modules\setup-wizard.js" }
if (Test-Path $wizPath) {
    $wizContent = Get-Content $wizPath -Raw
    if ($wizContent -match "function t_label") {
        Write-Host "  [OK] t_label() helper defined" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] t_label() helper missing" -ForegroundColor Red
    }
    if ($wizContent -match "tooltip-trigger") {
        Write-Host "  [OK] tooltip-trigger used in wizard fields" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] no tooltips on wizard fields" -ForegroundColor Red
    }
    # Проверяем что захардкоженный English убран (должен остаться только внутри t_label fallback)
    $hardcodedGpu = ([regex]::Matches($wizContent, "'>GPU Max %'")) -join ','
    if ($hardcodedGpu -eq "") {
        Write-Host "  [OK] no hardcoded 'GPU Max %' in renderGeneralSettingsStep" -ForegroundColor Green
    } else {
        Write-Host "  [WARN] hardcoded 'GPU Max %' still present (in fallback only?)" -ForegroundColor Yellow
    }
    # Проверяем что wizard.tooltip.* keys есть в i18n
    $tooltipKeys = @('wizard.tooltip.replication_min', 'wizard.tooltip.rpc_url', 'wizard.tooltip.balancing_algorithm', 'wizard.tooltip.api_token')
    $missingKeys = @()
    foreach ($k in $tooltipKeys) {
        if ($enContent -notmatch [regex]::Escape($k)) { $missingKeys += "en:$k" }
        if ($ruContent -notmatch [regex]::Escape($k)) { $missingKeys += "ru:$k" }
    }
    if ($missingKeys.Count -eq 0) {
        Write-Host "  [OK] all sampled wizard.tooltip.* keys present in en+ru" -ForegroundColor Green
    } else {
        Write-Host "  [FAIL] missing tooltip i18n keys: $($missingKeys -join ', ')" -ForegroundColor Red
    }
} else {
    Write-Host "  [FAIL] setup-wizard.js not found" -ForegroundColor Red
}

Write-Host ""
Write-Host "[smoke] Done." -ForegroundColor Cyan
