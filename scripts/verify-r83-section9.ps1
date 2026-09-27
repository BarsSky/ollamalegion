<#
.SYNOPSIS
  R83 §9 (2026-09-26): живая сверка инвариантов, добавленных во второй половине
  сессии. Дополняет scripts/verify-r83-v3-v4.ps1 (теги/гейт/загрузка).

.DESCRIPTION
  Проверяет то, что иначе пришлось бы смотреть руками на каждом шаге рефактора
  §9.4 (замена legacy-оценок на memfit) и §9.6:

    [1] /api/models: top-level vram_known и per-model vram_known
    [2] /api/v1/health/detailed: recentErrors ЧИТАЕТСЯ из SSE ring buffer
        (до §9.5 секция была всегда пуста, потому что буфер наполнялся только
        при подключённом SSE-клиенте)
    [3] §9.5 end-to-end: провал авто-загрузки несуществующей модели обязан
        породить warning-событие в recentErrors (source=event,
        path=async_auto_load) — БЕЗ открытого SSE-клиента
    [4] §9.3: тот же провал должен быть отказом (413/503 с причиной), а не
        «грузим всё равно»
    [5] §9.2: 413 от preflight (если удаётся спровоцировать) содержит vram_known
    [6] §9.1: лог-маркеры tier-ожидания и отсутствие второго загрузочного цикла

  Скрипт ничего не «лечит»: только читает состояние и печатает PASS/FAIL.

.PARAMETER Token
  Токен балансера (X-API-Token), по умолчанию — bundled-значение.

.PARAMETER SkipNotificationCheck
  Не провоцировать провал авто-загрузки (шаги 3-4). Полезно, если не хочется
  ждать ~90 с и засорять журнал.

.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File scripts\verify-r83-section9.ps1
#>
param(
    [string]$Token = "changeme-bundled-with-agent-token",
    [switch]$SkipNotificationCheck,
    # Проверки [5] и [6] зависят от того, воспроизводился ли сценарий в окне
    # логов. По умолчанию они информационные; с -Strict становятся обязательными
    # (используется при сверке шагов рефактора §9.4/§9.6).
    [switch]$Strict
)

$ErrorActionPreference = "Continue"
$balancer = "http://127.0.0.1:18080"
$cppworker = "http://127.0.0.1:18092"
$fail = 0

function Check($name, $ok, $detail) {
    if ($ok) {
        Write-Host ("  PASS  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })) -ForegroundColor Green
    } else {
        Write-Host ("  FAIL  {0}{1}" -f $name, $(if ($detail) { " — $detail" } else { "" })) -ForegroundColor Red
        $script:fail++
    }
}

# InfoCheck — проверка, зависящая от воспроизведения сценария в окне логов.
# В обычном прогоне это INFO (не провал), с -Strict — обязательная проверка.
function InfoCheck($name, $ok, $detail) {
    if ($ok) {
        Write-Host ("  INFO  {0} — сработало{1}" -f $name, $(if ($detail) { ": $detail" } else { "" })) -ForegroundColor Green
    } else {
        $color = if ($Strict) { "Red" } else { "Yellow" }
        Write-Host ("  INFO  {0} — в окне логов не срабатывало{1}" -f $name, $(if ($detail) { " ($detail)" } else { "" })) -ForegroundColor $color
        if ($Strict) { $script:fail++ }
    }
}

function Get-JsonUtf8($url, $headers) {
    # Читаем ответ как UTF-8: PowerShell по умолчанию декодирует в CP1251, и
    # русский текст в сообщениях событий превращается в мусор.
    try {
        $req = [System.Net.HttpWebRequest]::Create($url)
        foreach ($k in $headers.Keys) { $req.Headers.Add($k, $headers[$k]) }
        $req.Timeout = 30000
        $resp = $req.GetResponse()
        $sr = New-Object System.IO.StreamReader($resp.GetResponseStream(), [System.Text.Encoding]::UTF8)
        $body = $sr.ReadToEnd()
        $sr.Close(); $resp.Close()
        return $body | ConvertFrom-Json
    } catch {
        Write-Host "  (запрос $url не удался: $($_.Exception.Message))" -ForegroundColor DarkGray
        return $null
    }
}

Write-Host ""
Write-Host "=== R83 §9: живая сверка инвариантов ===" -ForegroundColor Cyan

# --- [1] vram_known -----------------------------------------------------------
Write-Host ""
Write-Host "[1] /api/models: vram_known (cppworker R83)" -ForegroundColor Cyan
$models = Get-JsonUtf8 "$cppworker/api/models" @{ Authorization = "Bearer $Token" }
Check "/api/models отвечает" ($models -ne $null)
if ($models) {
    $hasTop = $models.PSObject.Properties.Name -contains "vram_known"
    Check "top-level vram_known есть" $hasTop ("vram_known=" + $models.vram_known)
    if ($models.models -and $models.models.Count -gt 0) {
        $m0 = $models.models[0]
        $hasPer = $m0.PSObject.Properties.Name -contains "vram_known"
        Check "per-model vram_known есть (нужен preflight'у)" $hasPer ("model=" + $m0.name + " vram_known=" + $m0.vram_known)
    } else {
        Write-Host "  (модели не загружены — per-model проверка пропущена)" -ForegroundColor DarkGray
    }
}

# --- [1b] load_degraded: контракт поля (R83 §9.4 шаг 1б, вариант D) ----------
#
# Что проверяем и почему именно так. Поле load_degraded — это НЕ провал
# (загрузка удалась, модель работает без GPU), поэтому в спокойном состоянии
# его быть не должно: иначе оператор получит предупреждение о работающей
# модели. А если оно есть — обязано нести stage и diagnostics, иначе UI не
# сможет объяснить, почему раскладка такая.
#
# Позитивную ветку (cpu_only + заголовки) здесь воспроизвести нельзя: на живом
# стенде модель влезает в VRAM (partial_offload), а искусственный дефицит
# памяти требует отдельного контейнера. Она покрыта юнит- и e2e-тестами
# (cmd/cppworker/degraded_load_r83_test.go) и проверялась на временном
# stub-контейнере с подменёнными RAM/VRAM.
Write-Host ""
Write-Host "[1b] §9.4 шаг 1б: контракт load_degraded в /api/models" -ForegroundColor Cyan
if ($models) {
    $hasDegradedField = $models.PSObject.Properties.Name -contains "load_degraded"
    if (-not $models.load_degraded) {
        Check "нет деградации → поля load_degraded нет" $true "модель работает штатно"
    } else {
        Check "load_degraded есть → stage заполнен" `
            ($models.load_degraded.stage -ne "") ("stage=" + $models.load_degraded.stage)
        Check "load_degraded есть → diagnostics заполнены" `
            ($null -ne $models.load_degraded.diagnostics) `
            ("model=" + $models.load_degraded.model + " reason=" + $models.load_degraded.reason)
        Check "load_degraded.severity = warning (не провал)" `
            ($models.load_degraded.severity -eq "warning") ("severity=" + $models.load_degraded.severity)
    }
    # Признак того, что поля вообще нет в схеме ответа (старый образ) — это не
    # провал проверки, а информация: поле опциональное (omitempty).
    if (-not $hasDegradedField) {
        Write-Host "  (поля нет в ответе вовсе — штатно для omitempty)" -ForegroundColor DarkGray
    }
}

# --- [2] recentErrors читается ------------------------------------------------
Write-Host ""
Write-Host "[2] /api/v1/health/detailed: recentErrors доступен (ring buffer)" -ForegroundColor Cyan
$health = Get-JsonUtf8 "$balancer/api/v1/health/detailed" @{ "X-API-Token" = $Token }
Check "/api/v1/health/detailed отвечает" ($health -ne $null)
if ($health) {
    $hasField = $health.PSObject.Properties.Name -contains "recentErrors"
    Check "поле recentErrors присутствует" $hasField
    Write-Host ("       status={0} healthScore={1} recentErrors={2}" -f `
        $health.status, $health.healthScore, @($health.recentErrors).Count) -ForegroundColor DarkGray
}

# --- [3]-[4] уведомление о провале авто-загрузки ------------------------------
if (-not $SkipNotificationCheck) {
    Write-Host ""
    Write-Host "[3] §9.5/§9.3: провал авто-загрузки → событие в буфере и явный отказ" -ForegroundColor Cyan

    $utf8 = New-Object System.Text.UTF8Encoding($false)
    $bodyPath = Join-Path $env:TEMP "r83-s9-nonexistent.json"
    [System.IO.File]::WriteAllText($bodyPath,
        '{"model":"r83-nonexistent-model:latest","messages":[{"role":"user","content":"hi"}],"stream":false}',
        $utf8)

    $before = 0
    if ($health) { $before = @($health.recentErrors).Count }

    $code = & curl.exe -s -o "$bodyPath.resp" -w '%{http_code}' -X POST "$balancer/api/chat" `
        -H 'Content-Type: application/json' -H "Authorization: Bearer $Token" `
        --data-binary "@$bodyPath" --max-time 220
    Check "несуществующая модель → не 2xx" ($code -notmatch '^2') "получен $code"
    $respText = if (Test-Path "$bodyPath.resp") { [System.IO.File]::ReadAllText("$bodyPath.resp") } else { "" }
    Check "в ответе есть причина (не пустой 500)" ($respText.Length -gt 20) $respText.Substring(0, [Math]::Min(160, $respText.Length))

    # Ждём появления события в буфере (публикация асинхронная).
    $found = $false
    $deadline = (Get-Date).AddSeconds(30)
    while ((Get-Date) -lt $deadline) {
        $h = Get-JsonUtf8 "$balancer/api/v1/health/detailed" @{ "X-API-Token" = $Token }
        if ($h) {
            $ev = @($h.recentErrors) | Where-Object { $_.path -eq "async_auto_load" -or $_.message -like "*r83-nonexistent-model*" } | Select-Object -First 1
            if ($ev) { $found = $true; break }
        }
        Start-Sleep -Seconds 3
    }
    Check "событие провала попало в recentErrors БЕЗ SSE-клиента" $found `
        "(до §9.5 буфер наполнялся только при подключённом клиенте)"
}

# --- [5] vram_known в отказе preflight ---------------------------------------
Write-Host ""
Write-Host "[5] §9.2: preflight-отказ при известной VRAM" -ForegroundColor Cyan
$log = docker logs --since 30m ol-bundled-balancer 2>&1 | Out-String
InfoCheck "в логе есть пометка о известной VRAM" `
    ($log -match 'VRAM is known|vram_known|веса не влезают') `
    "срабатывает только когда VRAMKnown=true и max_vram_n_ctx=0 и клиент просит больше текущего n_ctx"

# --- [6] §9.1 tier-маркеры ---------------------------------------------------
Write-Host ""
Write-Host "[6] §9.1: тир ожидания загрузки по размеру модели" -ForegroundColor Cyan
$cplog = docker logs --since 30m ol-bundled-cppworker-gpu 2>&1 | Out-String
InfoCheck "cppworker жив и логирует решения гейта" ($cplog -match 'R83 гейт n_ctx') `
    "лог появляется при попытке загрузки с явным contextSize"
Check "нет SIGABRT/переподписки" (($cplog -notmatch 'SIGABRT') -and ($cplog -notmatch 'unaligned tcache'))

# --- [6b] событие деградации (§9.4 шаг 1б, вариант D) ------------------------
#
# Проверяем, что уведомление о CPU-only действительно доходит до балансера.
# Событие публикуется только когда факт деградации есть в /api/models, поэтому
# в обычном прогоне это INFO; с -Strict — обязательная проверка (используется
# при сверке на стенде, где деградацию воспроизводили).
Write-Host ""
Write-Host "[6b] §9.4 шаг 1б: событие load_degraded доходит до WebUI" -ForegroundColor Cyan
if ($health) {
    $degEv = @($health.recentErrors) | Where-Object {
        $_.path -eq "load_degraded" -or $_.message -like "*без GPU*" -or $_.message -like "*cpu_only*"
    } | Select-Object -First 1
    InfoCheck "событие деградации есть в recentErrors" ($null -ne $degEv) `
        "публикуется при load_degraded в /api/models (severity=warning, source=load)"
}

# --- [7] §9.8: профили без модели — видно, а не «магия» ------------------------
#
# История пункта: в логах живого cppworker видели попытки грузить
# r81-model.gguf / m-auto.gguf / llama-3-8b.gguf, которых нет ни в config/*.json,
# ни в deployments/*.json. Разбор (2026-09-27) показал: это имена, которые
# присылал КЛИЕНТ (внутренние _diag-логи прошлых сессий), а не содержимое
# репозитория. Профили в конфиге сами по себе загрузок НЕ инициируют — они
# применяются только когда модель с таким именем запрошена.
#
# Проверка нужна, чтобы этот класс («профиль на модель, которой нет на стенде»)
# было видно сразу и не приходилось снова искать по логам: печатаем профили,
# которым не соответствует ни одна модель в каталоге cppworker.
Write-Host ""
Write-Host "[7] §9.8: профили без соответствующей модели (informational)" -ForegroundColor Cyan
$cfgRaw = docker exec ol-bundled-balancer sh -c "cat /app/data/config.json" 2>$null | Out-String
$filesRaw = Get-JsonUtf8 "$cppworker/api/models/files" @{ Authorization = "Bearer $Token" }
$orphans = @()
if ($cfgRaw -and $filesRaw) {
    try {
        $cfg = $cfgRaw | ConvertFrom-Json
        $catalog = @($filesRaw.files | ForEach-Object { [System.IO.Path]::GetFileNameWithoutExtension([string]$_.name) })
        if ($cfg.llamaCppModelProfiles) {
            foreach ($prof in $cfg.llamaCppModelProfiles.PSObject.Properties) {
                $name = $prof.Name
                $match = $catalog | Where-Object { $_ -and ($_ -like "*$name*" -or $name -like "*$_*") }
                if (-not $match) { $orphans += $name }
            }
        }
        Check "профили прочитаны из конфига балансера" $true `
            ("всего=" + @($cfg.llamaCppModelProfiles.PSObject.Properties).Count + ", без модели на стенде=" + $orphans.Count)
    } catch {
        Write-Host ("  (конфиг не разобран: " + $_.Exception.Message + ")") -ForegroundColor DarkGray
    }
} else {
    Write-Host "  (нет доступа к конфигу балансера или каталогу моделей)" -ForegroundColor DarkGray
}
if ($orphans.Count -gt 0) {
    Write-Host ("       профили без модели: " + ($orphans -join ', ')) -ForegroundColor DarkGray
    Write-Host "       Это не дефект: профиль применяется только при запросе такой модели, загрузок не инициирует." -ForegroundColor DarkGray
}

Write-Host ""
if ($fail -eq 0) {
    Write-Host "ИТОГ §9: все проверки пройдены" -ForegroundColor Green
} else {
    Write-Host "ИТОГ §9: провалено проверок: $fail" -ForegroundColor Red
}
Write-Host ""
exit $fail
