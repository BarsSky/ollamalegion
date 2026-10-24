#!/usr/bin/env pwsh
# scripts/verify-autotune-r230.ps1 — R230 (2026-10-24): живая проверка того,
# что AutoTune считает бюджет правильно и не советует неисполнимые значения.
#
# ЗАЧЕМ. Жалоба оператора (A10 24GB, n_ctx=131072, KV q8_0, 16 слоёв): «пишет,
# что свободно 0 GB, рекомендует -2 в num_gpu_layers и q4_0, после перезагрузки
# автотюна стало хуже, чем при первой загрузке». Причины были в единицах
# (МБ как байты), в отсутствии reload-бюджета (free + usage модели) и в
# отсутствии параметров архитектуры для расчёта KV. Этот скрипт проверяет
# ФАКТ на живом стенде — до и после правок.
#
# Проверки (по каждому бэкенду и каждой загруженной модели):
#   1. freeVRAM/gpu memory в отчёте не «0.0GB», когда у бэкенда есть VRAM.
#   2. Нет рекомендации num_gpu_layers == -2 (R230: не советуем auto-offload;
#      конкретное число или «всё влезает»).
#   3. Если рекомендуются все слои — есть бюджет и модель/резерв в reason.
#   4. Рекомендация KV — только если она влезает (в reason есть бюджет).
#   5. Если рекомендаций нет — AutoTune честно говорит «optimal».
#
# Запуск:
#   pwsh -File scripts/verify-autotune-r230.ps1
#   pwsh -File scripts/verify-autotune-r230.ps1 -Api http://127.0.0.1:18081
#   $env:LB_ADMIN_USER / $env:LB_ADMIN_PASSWORD — учётка WebUI (если включён вход).
#
# Код возврата: 0 — PASS, 1 — FAIL.
[CmdletBinding()]
param(
    [string]$Api = "http://127.0.0.1:18081",
    [string]$User = $env:LB_ADMIN_USER,
    [string]$Password = $env:LB_ADMIN_PASSWORD,
    [int]$TimeoutSec = 30
)

$ErrorActionPreference = 'Stop'
$script:failures = @()
$script:checks = 0

function Fail([string]$msg) {
    $script:failures += $msg
    Write-Host ("  FAIL  " + $msg) -ForegroundColor Red
}
function Pass([string]$msg) {
    $script:checks++
    Write-Host ("  PASS  " + $msg) -ForegroundColor Green
}
function Info([string]$msg) {
    Write-Host ("  INFO  " + $msg) -ForegroundColor Cyan
}

$jar = Join-Path ([System.IO.Path]::GetTempPath()) ("ol-autotune-r230-" + [guid]::NewGuid().ToString('N') + ".cookies")
try {
    # ── 0. Вход (если WebUI-вход включён) ─────────────────────────────────────
    $loginBody = @{ name = $User; password = $Password } | ConvertTo-Json -Compress
    $loginFile = Join-Path ([System.IO.Path]::GetTempPath()) ("ol-autotune-login-" + [guid]::NewGuid().ToString('N') + ".json")
    # UTF-8 без BOM: JSON с BOM строгий декодер cppworker/балансера отвергает.
    # (PowerShell 5.1 не знает -Encoding utf8NoBOM, поэтому пишем через .NET.)
    [System.IO.File]::WriteAllText($loginFile, $loginBody, (New-Object System.Text.UTF8Encoding($false)))
    $loginOut = Join-Path ([System.IO.Path]::GetTempPath()) ("ol-autotune-loginout-" + [guid]::NewGuid().ToString('N') + ".json")
    $loginCode = & curl.exe -s -o "$loginOut" -w '%{http_code}' -c $jar -X POST "$Api/api/v1/auth/login" `
        -H 'Content-Type: application/json' --data-binary "@$loginFile" --max-time $TimeoutSec
    Remove-Item $loginFile, $loginOut -ErrorAction SilentlyContinue
    if ($loginCode -eq '200') {
        Info "вход выполнен ($User)"
    } elseif ($loginCode -eq '503') {
        Info "WebUI-вход не включён (нет пользователей) — читаем без авторизации"
    } else {
        Fail "login вернул HTTP $loginCode (учётка: $User). Задайте LB_ADMIN_USER/LB_ADMIN_PASSWORD."
        throw "login failed"
    }

    # ── 1. Отчёт /api/v1/backends ────────────────────────────────────────────
    $raw = & curl.exe -s -b $jar --max-time $TimeoutSec "$Api/api/v1/backends"
    if (-not $raw) { Fail "пустой ответ /api/v1/backends"; throw "no response" }
    $data = $raw | ConvertFrom-Json

    $backends = @($data.backends)
    if ($backends.Count -eq 0) { Fail "нет бэкендов в ответе"; throw "no backends" }

    $anyModel = $false
    foreach ($b in $backends) {
        $gpuTotalMB = 0
        if ($b.gpu -and $b.gpu.memoryTotal) { $gpuTotalMB = [int64]$b.gpu.memoryTotal }
        $loaded = @($b.loadedModels | Where-Object { $_.state -eq 'loaded' })
        if ($loaded.Count -eq 0) { continue }
        Info ("бэкенд {0}: VRAM {1} MB, loaded={2}" -f $b.id, $gpuTotalMB, $loaded.Count)

        $report = $b.autoTune
        if (-not $report -or -not $report.models) {
            # Нет отчёта при загруженной модели — это подозрительно, но не всегда
            # ошибка (например, AutoTune выключен глобально). Фиксируем как INFO.
            Info ("  AutoTune-отчёта нет (AutoTune выключен?)")
            continue
        }

        foreach ($m in @($report.models)) {
            $anyModel = $true
            $recs = @($m.recommendations | Where-Object { $_ })
            Info ("  модель {0}: subOptimal={1}, рекомендаций={2}" -f $m.modelName, $m.isSubOptimal, $recs.Count)

            if (-not $m.isSubOptimal -and $recs.Count -eq 0) {
                Pass ("{0}: рекомендаций нет — состояние optimal" -f $m.modelName)
            }

            foreach ($r in $recs) {
                $text = [string]$r.suggestion
                $cat = [string]$r.category

                # 1) «0.0GB» — признак неверных единиц/бюджета.
                if ($gpuTotalMB -gt 1024 -and ($text -match 'free=0\.0GB' -or $text -match 'бюджет 0\.0GB')) {
                    Fail ("{0}: в reason фигурирует 0.0GB при VRAM {1} MB — единицы/бюджет: {2}" -f $m.modelName, $gpuTotalMB, $text)
                }

                # 2) -2 в рекомендации слоёв.
                if ($cat -eq 'layers' -and [int]$r.recommendedNumGpuLayers -eq -2) {
                    Fail ("{0}: рекомендация num_gpu_layers=-2 (R230 запрещает auto-offload советовать)" -f $m.modelName)
                }

                # 3) Рекомендации KV/слоёв должны ссылаться на бюджет.
                if ($cat -eq 'kv_cache' -or $cat -eq 'layers') {
                    if ($text -notmatch 'бюджет') {
                        Fail ("{0}: рекомендация {1} без расчёта бюджета: {2}" -f $m.modelName, $cat, $text)
                    } else {
                        Pass ("{0}: {1} посчитан от бюджета памяти" -f $m.modelName, $cat)
                    }
                }

                # 4) «все слои» — только вместе с KV и резервом в reason.
                if ($cat -eq 'layers' -and [int]$r.recommendedNumGpuLayers -eq -1) {
                    if ($text -match 'KV' -and $text -match 'резерв') {
                        Pass ("{0}: -1 (all layers) объяснён через model+KV+резерв" -f $m.modelName)
                    } else {
                        Fail ("{0}: -1 без расчёта model+KV+резерв: {1}" -f $m.modelName, $text)
                    }
                }
            }
        }
    }

    if (-not $anyModel) {
        Fail "ни одной загруженной модели с AutoTune-отчётом — сначала загрузите модель"
    }
}
catch {
    if ($_.Exception.Message -notin @('login failed', 'no response', 'no backends')) {
        Fail ("исключение: " + $_.Exception.Message)
    }
}
finally {
    Remove-Item $jar -ErrorAction SilentlyContinue
}

Write-Host ''
if ($script:failures.Count -eq 0) {
    Write-Host ("ИТОГ: PASS ({0} проверок)" -f $script:checks) -ForegroundColor Green
    exit 0
}
Write-Host ("ИТОГ: FAIL ({0})" -f $script:failures.Count) -ForegroundColor Red
foreach ($f in $script:failures) { Write-Host ("  - " + $f) -ForegroundColor Red }
exit 1
