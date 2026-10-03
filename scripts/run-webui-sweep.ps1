# run-webui-sweep.ps1 — полный прогон JS-тестов WebUI с честными кодами возврата.
#
# ЗАЧЕМ ОТДЕЛЬНЫЙ СКРИПТ: в интерактивной сессии PowerShell `$LASTEXITCODE` после
# длинной цепочки команд с пайпами врёт (например, показывает 1 у прошедшего
# теста). Здесь каждый прогон пишет вывод в файл, а код берётся сразу после
# запуска — так результат воспроизводим.
$ErrorActionPreference = 'Continue'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root
$logDir = Join-Path $env:TEMP 'ol-webui-sweep'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null

$failed = @()
$run = 0

function Invoke-Test([string]$rel) {
    $script:run++
    $name = [System.IO.Path]::GetFileNameWithoutExtension($rel)
    $out = Join-Path $logDir ($name + '.out')
    & node $rel > $out 2>&1
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        $script:failed += $rel
        Write-Host ("FAIL  {0} (exit {1})" -f $rel, $code) -ForegroundColor Red
        Get-Content $out | Select-Object -Last 12 | ForEach-Object { Write-Host ("      | " + $_) }
    } else {
        $last = (Get-Content $out | Select-Object -Last 1)
        Write-Host ("ok    {0} :: {1}" -f $rel, $last)
    }
}

Get-ChildItem webui/js/modules/*.test.js | Sort-Object Name | ForEach-Object { Invoke-Test ('webui/js/modules/' + $_.Name) }
Get-ChildItem webui/tests/*.test.js | Sort-Object Name | ForEach-Object { Invoke-Test ('webui/tests/' + $_.Name) }

Write-Host ''
Write-Host ("прогонов: {0}, провалов: {1}" -f $run, $failed.Count)
if ($failed.Count) { Write-Host ($failed -join ', '); exit 1 }
exit 0
