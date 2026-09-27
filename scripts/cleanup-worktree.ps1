<#
.SYNOPSIS
  Уборка рабочего дерева репозитория от артефактов, не относящихся к исходникам
  и сборке: логи прогонов, дампы падений, локальные бинарники, временные каталоги.

.DESCRIPTION
  ВАЖНО ПОНИМАТЬ, ЧТО ИМЕННО ЧИСТИТСЯ. Проверено: весь этот хлам в git НЕ
  попадает (он уже покрыт .gitignore), поэтому «очистка репозитория» здесь —
  это уборка ЛОКАЛЬНОГО рабочего дерева (диск, а не история коммитов). Сами
  файлы в git никогда не коммитились, поэтому удалять их безопасно: история
  коммитов не меняется, `git status` остаётся чистым.

  Режимы:
    (по умолчанию)  ПЕРЕМЕСТИТЬ в карантин <Repo>\.cleanup_<yyyyMMdd>\ —
                    обратимо: вернули папку на место и всё как было.
    -HardDelete     удалить сразу (быстро, необратимо).
    -DryRun         только показать, что было бы убрано, ничего не трогая.

  Категории (можно отключать флагами):
    1. логи прогонов/загрузок/релизов и дампы падений  (*.log в корне)
    2. локальные бинарники и архивы сборки в корне
       (agent.exe, balancer.exe, cppworker.exe, cppworker-stub, *.7z)
    3. каталоги-артефакты сборки и прогонов
       (bin/, ggml/, playwright-report/, test-results/, .pytest_cache/)
    4. локальные бэкапы и сборки C-кода (c/llama.cpp.local-bak*, c/bridge/build)
    5. временные файлы в корне (tmp_*.txt/json, test_toolcall.json,
       apply_test.json, apply_audit_fixes.py, test_user_parallel.py)

  ЧЕГО СКРИПТ НЕ ТРОГАЕТ (осознанно):
    * _diag/            — диагностические артефакты живых прогонов, на них
                          ссылается документация и разборы инцидентов;
    * deployments/      — данные и конфиги стека;
    * models/           — GGUF-модели (гигабайты, но это ваш рабочий контент);
    * dist/             — артефакты релиза исходников (свежие, нужны для выдачи);
    * .cline/, .vscode/ — отслеживаемые/рабочие настройки инструментов;
    * node_modules/, c/llama.cpp/ (сабмодуль) — нужны для сборки;
    * всё, что отслеживается git (проверяется через `git ls-files`).

.EXAMPLE
  pwsh -File scripts/cleanup-worktree.ps1 -DryRun
  pwsh -File scripts/cleanup-worktree.ps1                 # в карантин
  pwsh -File scripts/cleanup-worktree.ps1 -HardDelete      # сразу удалить
  pwsh -File scripts/cleanup-worktree.ps1 -SkipCBackups     # без 855 МБ бэкапа llama.cpp
#>
param(
    [switch]$DryRun,
    [switch]$HardDelete,
    # Категории
    [switch]$SkipLogs,
    [switch]$SkipBinaries,
    [switch]$SkipBuildDirs,
    [switch]$SkipCBackups,
    [switch]$SkipTempFiles
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    if (-not (Test-Path .git)) { throw "не git-репозиторий: $root" }

    $stamp = Get-Date -Format 'yyyyMMdd'
    $quarantine = Join-Path $root ".cleanup_$stamp"

    function Tracked([string]$rel) {
        $n = (git ls-files -- $rel 2>$null | Measure-Object).Count
        return $n -gt 0
    }
    function SizeMB([string]$p) {
        if (-not (Test-Path $p)) { return 0 }
        $s = (Get-ChildItem $p -Recurse -File -Force -ErrorAction SilentlyContinue |
              Measure-Object -Property Length -Sum).Sum
        if (-not $s) { $s = (Get-Item $p -Force).Length }
        return [math]::Round($s / 1MB, 1)
    }

    $targets = New-Object System.Collections.Generic.List[object]

    if (-not $SkipLogs) {
        Get-ChildItem -File -Filter '*.log' -ErrorAction SilentlyContinue |
            ForEach-Object { $targets.Add([pscustomobject]@{ Path = $_.Name; Group = 'логи прогонов/дампы' }) }
    }
    if (-not $SkipBinaries) {
        @('agent.exe', 'balancer.exe', 'cppworker.exe', 'cppworker-stub', 'ollamalegion.7z') |
            ForEach-Object { if (Test-Path $_) { $targets.Add([pscustomobject]@{ Path = $_; Group = 'локальные бинарники' }) } }
    }
    if (-not $SkipBuildDirs) {
        @('bin', 'ggml', 'playwright-report', 'test-results', '.pytest_cache') |
            ForEach-Object { if (Test-Path $_) { $targets.Add([pscustomobject]@{ Path = $_; Group = 'каталоги-артефакты' }) } }
    }
    if (-not $SkipCBackups) {
        Get-ChildItem -Directory -Filter 'llama.cpp.local-bak*' -Path 'c' -ErrorAction SilentlyContinue |
            ForEach-Object { $targets.Add([pscustomobject]@{ Path = "c/$($_.Name)"; Group = 'локальные сборки C' }) }
        if (Test-Path 'c/bridge/build') { $targets.Add([pscustomobject]@{ Path = 'c/bridge/build'; Group = 'локальные сборки C' }) }
    }
    if (-not $SkipTempFiles) {
        @('tmp_commit_msg.txt', 'tmp_load_qwen38.json', 'test_toolcall.json',
          'apply_test.json', 'apply_audit_fixes.py', 'test_user_parallel.py') |
            ForEach-Object { if (Test-Path $_) { $targets.Add([pscustomobject]@{ Path = $_; Group = 'временные файлы' }) } }
    }

    Write-Host "=== Уборка рабочего дерева ($root) ===" -ForegroundColor Cyan
    Write-Host ("режим: " + $(if ($DryRun) { 'DRY-RUN (ничего не меняю)' } elseif ($HardDelete) { 'УДАЛЕНИЕ' } else { "карантин → $quarantine" })) -ForegroundColor Yellow
    Write-Host ""

    $totalMB = 0.0
    $removed = 0
    $skipped = 0
    foreach ($t in $targets) {
        if (Tracked $t.Path) {
            Write-Host ("  ПРОПУСК (отслеживается git): {0}" -f $t.Path) -ForegroundColor Red
            $skipped++
            continue
        }
        $mb = SizeMB $t.Path
        $totalMB += $mb
        $removed++
        Write-Host ("  {0,-26} {1,8:N1} МБ  {2}" -f $t.Group, $mb, $t.Path)

        if ($DryRun) { continue }

        if ($HardDelete) {
            Remove-Item -Recurse -Force $t.Path -ErrorAction SilentlyContinue
        } else {
            # Карантин: сохраняем структуру путей, чтобы вернуть «как было».
            $dest = Join-Path $quarantine ($t.Path -replace '/', '\')
            $destDir = Split-Path -Parent $dest
            New-Item -ItemType Directory -Force -Path $destDir | Out-Null
            Move-Item -Force $t.Path $dest -ErrorAction SilentlyContinue
        }
    }

    Write-Host ""
    Write-Host ("--- итог: объектов {0}, объём {1:N1} МБ, пропущено {2} ---" -f $removed, $totalMB, $skipped) -ForegroundColor Cyan

    if (-not $DryRun) {
        if (-not $HardDelete) {
            Write-Host "Карантин: $quarantine" -ForegroundColor Yellow
            Write-Host "Вернуть всё назад:  Move-Item '$quarantine\\*' . -Force    (по категориям — вручную)"
            Write-Host "Удалить карантин:   Remove-Item -Recurse -Force '$quarantine'"
        }
        # Проверка, что состояние репозитория не изменилось.
        $dirty = git status --porcelain
        if ($dirty) {
            Write-Host "! git status НЕ пуст — уборка затронула отслеживаемое, разберитесь:" -ForegroundColor Red
            $dirty | Select-Object -First 10 | ForEach-Object { Write-Host "    $_" }
        } else {
            Write-Host "OK  git status чист: отслеживаемые файлы не затронуты" -ForegroundColor Green
        }
    }
}
finally {
    Pop-Location
}
