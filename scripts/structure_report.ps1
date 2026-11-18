# structure_report.ps1 — R506 (2026-11-19), план
# docs/plans/2026-11-19-readability-refactor.md, шаг R1.
#
# ЧТО ДЕЛАЕТ. Печатает измеримые показатели структуры кода и сравнивает их с
# критериями §5 плана. Нужен, чтобы шаги R2–R7 проверялись одной командой, а не
# «на глаз», и чтобы долг был виден в числах.
#
# ЗАПУСК:
#   powershell -NoProfile -ExecutionPolicy Bypass -File scripts/structure_report.ps1
#   powershell ... -File scripts/structure_report.ps1 -FailOnRegression   # CI-режим
#
# КОД ВОЗВРАТА: 0 — всё в пределах цели; 1 — есть показатели хуже цели
# (с -FailOnRegression) или ниже порогов из плана.
#
# Пороги (из §5 плана):
#   функций > 1000 строк ........................ 0
#   функций > 300 строк ......................... <= 5
#   функций >= 150 строк ........................ <= 60
#   файлов >= 1500 строк (наш код) .............. <= 3
#   internal/cppbackend/backend.go .............. <= 800
#   боевых файлов с _rNNN ....................... 0
param(
    [switch]$FailOnRegression,
    [int]$TopLines = 20
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$skipDirPattern = 'node_modules|\\c\\llama\.cpp\\|\\dist\\|test-results|\\\.git\\'
$skipFilePattern = '_test\.go$|\.test\.js$'

function Get-CodeFiles {
    Get-ChildItem -Recurse -File -Include *.go,*.c,*.h,*.js |
        Where-Object { $_.FullName -notmatch $skipDirPattern }
}

# --- 1. Файлы: строки ---------------------------------------------------------
$files = Get-CodeFiles | ForEach-Object {
    [pscustomobject]@{
        Lines = (Get-Content $_.FullName | Measure-Object -Line).Lines
        Path  = $_.FullName.Replace($PWD.Path + '\', '')
    }
}

Write-Output '=== Файлы: топ по строкам ==='
$files | Sort-Object Lines -Descending | Select-Object -First $TopLines |
    ForEach-Object { '{0,6}  {1}' -f $_.Lines, $_.Path }

# --- 2. Функции Go/C: длина --------------------------------------------------
$funcs = @()
foreach ($f in (Get-CodeFiles | Where-Object { $_.Extension -in '.go', '.c', '.h' })) {
    if ($f.Name -match $skipFilePattern) { continue }
    $lines = Get-Content $f.FullName
    $starts = @()
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match '^(func |export )' -or $lines[$i] -match '^[A-Za-z_][A-Za-z0-9_ \*]*\([^;]*\)\s*\{$') {
            $starts += $i
        }
    }
    for ($j = 0; $j -lt $starts.Count; $j++) {
        $end = if ($j + 1 -lt $starts.Count) { $starts[$j + 1] } else { $lines.Count }
        $len = $end - $starts[$j]
        if ($len -ge 150) {
            $funcs += [pscustomobject]@{
                Lines = $len
                Path  = $f.FullName.Replace($PWD.Path + '\', '')
                Line  = $starts[$j] + 1
                Name  = $lines[$starts[$j]].Trim()
            }
        }
    }
}

Write-Output ''
Write-Output '=== Функции >= 150 строк (топ) ==='
$funcs | Sort-Object Lines -Descending | Select-Object -First $TopLines |
    ForEach-Object { '{0,6}  {1}:{2}  {3}' -f $_.Lines, $_.Path, $_.Line, $_.Name.Substring(0, [Math]::Min(70, $_.Name.Length)) }

# --- 3. Показатели против целей §5 -------------------------------------------
$over1000 = ($funcs | Where-Object { $_.Lines -gt 1000 }).Count
$over300 = ($funcs | Where-Object { $_.Lines -gt 300 }).Count
$ge150 = $funcs.Count
$bigFiles = ($files | Where-Object { $_.Lines -ge 1500 }).Count
$backendFile = $files | Where-Object { $_.Path -eq 'internal\cppbackend\backend.go' } | Select-Object -First 1
$backendLines = if ($backendFile) { $backendFile.Lines } else { 0 }

$runnFiles = Get-CodeFiles |
    Where-Object { $_.Name -notmatch $skipFilePattern -and $_.Name -match '_r\d{3,}' }
$rnnnCount = $runnFiles.Count

$checks = @(
    [pscustomobject]@{ Metric = 'функций > 1000 строк'; Now = $over1000; Target = '0';       Ok = ($over1000 -eq 0) }
    [pscustomobject]@{ Metric = 'функций > 300 строк';  Now = $over300;  Target = '<= 5';    Ok = ($over300 -le 5) }
    [pscustomobject]@{ Metric = 'функций >= 150 строк'; Now = $ge150;    Target = '<= 60';   Ok = ($ge150 -le 60) }
    [pscustomobject]@{ Metric = 'файлов >= 1500 строк'; Now = $bigFiles; Target = '<= 3';    Ok = ($bigFiles -le 3) }
    [pscustomobject]@{ Metric = 'cppbackend/backend.go'; Now = $backendLines; Target = '<= 800'; Ok = ($backendLines -le 800) }
    [pscustomobject]@{ Metric = 'боевых файлов с _rNNN'; Now = $rnnnCount; Target = '0';     Ok = ($rnnnCount -eq 0) }
)

Write-Output ''
Write-Output '=== Показатели против целей §5 плана ==='
$checks | ForEach-Object {
    $mark = if ($_.Ok) { 'OK  ' } else { 'ДОЛГ' }
    '{0}  {1,-28} сейчас: {2,5}   цель: {3}' -f $mark, $_.Metric, $_.Now, $_.Target
}

$regressions = ($checks | Where-Object { -not $_.Ok }).Count
Write-Output ''
if ($regressions -eq 0) {
    Write-Output 'ИТОГ: все критерии §5 выполнены.'
} else {
    Write-Output ("ИТОГ: не выполнено критериев — {0} (см. docs/plans/2026-11-19-readability-refactor.md)" -f $regressions)
}

if ($FailOnRegression -and $regressions -gt 0) { exit 1 }
exit 0
