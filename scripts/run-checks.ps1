# scripts/run-checks.ps1 — R148 (2026-10-16).
#
# ЕДИНАЯ ПРОВЕРКА СТЕНДА: гоняет все тесты, которые ловят найденные ранее дефекты,
# чтобы регрессию было видно одной командой, а не по памяти.
#
# ЗАЧЕМ. За сессию найдены и починены дефекты в разных слоях: имена моделей с .gguf
# (Go + WebUI), сессия, вытеснявшая себя с прогретой копии, недостоверный отказ по
# окну, молчаливая инъекция image-инструмента, потеря duration_ms в собранном
# ответе, гард CUDA-архитектур (рантайм и сборка), роль бэкенда и раскладка,
# помощники инструмента проверки параметров. У каждого теперь есть тест; этот скрипт
# запускает их все.
#
# Запуск:
#   pwsh -File scripts/run-checks.ps1              # всё
#   pwsh -File scripts/run-checks.ps1 -SkipGo      # только JS и скрипты
#   pwsh -File scripts/run-checks.ps1 -SkipJs
#
# Код возврата: 0 — всё зелёное, 1 — есть провалы.
[CmdletBinding()]
param(
    [switch]$SkipGo,
    [switch]$SkipJs,
    [switch]$Quiet
)

$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
$failures = @()
$passed = 0

function Section([string]$title) {
    Write-Host ''
    Write-Host ("=== " + $title + " ===") -ForegroundColor Cyan
}

# ── Go: пакеты, где живут найденные дефекты ─────────────────────────────────
#
# ПАКЕТЫ ГОНЯЕМ ПО ОЧЕРЕДИ, А НЕ ОДНОЙ КОМАНДОЙ. `go test pkg1 pkg2 pkg3` запускает
# пакеты ПАРАЛЛЕЛЬНО, и на Windows это дважды дало средовое падение рантайма
# (`unexpected signal during runtime execution`, `net.(*netFD).Read`) при зелёных
# тестах по отдельности. Один пакет — один процесс: результат воспроизводим.
if (-not $SkipGo) {
    Section 'Go: балансер, cppbackend, API (по очереди)'
    $goFail = 0
    foreach ($pkg in @('./internal/balancer/', './internal/cppbackend/', './internal/api/', './cmd/cppworker/', './cmd/balancer/')) {
        $out = & go test -tags llama_stub $pkg -count=1 2>&1
        $ok = ($LASTEXITCODE -eq 0)
        if (-not $ok) {
            $goFail++
            $failures += ('go test ' + $pkg)
            Write-Host ('  FAIL  ' + $pkg) -ForegroundColor Red
            $out | Select-String -Pattern '^--- FAIL|^    --- FAIL|^FAIL|panic|Error:' |
                Select-Object -First 8 | ForEach-Object { Write-Host ('        ' + $_) }
        } else {
            Write-Host ('  PASS  ' + $pkg + '  ' + (($out | Select-Object -Last 1) -replace '\s+', ' '))
        }
    }
    if ($goFail -eq 0) { $passed++ }
}

# ── JS: модули WebUI и модуль инструмента проверки параметров ───────────────
if (-not $SkipJs) {
    Section 'JS: модули WebUI и помощники инструмента'
    $jsTests = @()
    $jsTests += Get-ChildItem -Path 'webui/js/modules' -Filter '*.test.js' -File -ErrorAction SilentlyContinue
    $jsTests += Get-ChildItem -Path 'scripts/webui-e2e/tests' -Filter '*.test.js' -File -ErrorAction SilentlyContinue
    $jsFail = 0
    foreach ($t in $jsTests) {
        $out = & node $t.FullName 2>&1
        if ($LASTEXITCODE -ne 0) {
            $jsFail++
            $failures += ('JS-тест ' + $t.Name)
            Write-Host ("  FAIL  " + $t.Name) -ForegroundColor Red
            $out | Select-Object -Last 6 | ForEach-Object { Write-Host ('        ' + $_) }
        } elseif (-not $Quiet) {
            Write-Host ("  PASS  " + $t.Name)
        }
    }
    Write-Host ("  JS-тестов: " + $jsTests.Count + ", провалов: " + $jsFail)
    if ($jsFail -eq 0) { $passed++ }
}

# ── Скрипты: проверка CUDA-арок без сборки ──────────────────────────────────
#
# Отдельным процессом: build-containers.ps1 в режиме -SelfTest делает `exit 0`,
# а это завершило бы и сам раннер. powershell.exe (не pwsh) — потому что pwsh в
# этом окружении может отсутствовать.
Section 'Скрипты: проверка CUDA-арок (без сборки)'
& powershell.exe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'build-containers.ps1') -SelfTest 2>&1 |
    Select-Object -Last 10 | ForEach-Object { Write-Host ('  ' + $_) }
if ($LASTEXITCODE -ne 0) { $failures += 'build-containers.ps1 -SelfTest' } else { $passed++ }

# ── Итог ────────────────────────────────────────────────────────────────────
Section 'ИТОГ'
Write-Host ("  шагов пройдено: " + $passed + ", провалов: " + $failures.Count)
if ($failures.Count -gt 0) {
    $failures | ForEach-Object { Write-Host ('   - ' + $_) -ForegroundColor Red }
    Pop-Location
    exit 1
}
Write-Host '  всё зелёное' -ForegroundColor Green
Pop-Location
exit 0
