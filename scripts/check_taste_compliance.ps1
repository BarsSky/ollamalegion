# TASTE-compliance pre-flight check (PowerShell).
# Usage: powershell -ExecutionPolicy Bypass -File scripts/check_taste_compliance.ps1
# Exits non-zero on any violation found.

$ErrorActionPreference = 'Stop'
$BaseDir = Split-Path -Parent $PSCommandPath | Split-Path -Parent
Set-Location $BaseDir

$violations = 0

function Add-Violation($message) {
    Write-Host "FAIL: $message" -ForegroundColor Red
    $script:violations++
}

function Note-OK($message) {
    Write-Host "OK:   $message" -ForegroundColor Green
}

# 1) Em-dash (\u2014) in webui sources (комментарии + user-facing).
$emDashPattern = '[\u2014]'
$emDashFiles = @()
foreach ($path in @('webui\index.html', 'webui\js\i18n\ru.js', 'webui\js\i18n\en.js')) {
    if (Test-Path $path) {
        $matches = Select-String -Path $path -Pattern $emDashPattern -List -ErrorAction SilentlyContinue
        if ($matches) {
            foreach ($m in $matches) { $emDashFiles += "$($m.Path):$($m.LineNumber): $($m.Line.Trim())" }
        }
    }
}
foreach ($cssFile in Get-ChildItem -Path 'webui\css' -Filter '*.css') {
    $matches = Select-String -Path $cssFile.FullName -Pattern $emDashPattern -List -ErrorAction SilentlyContinue
    if ($matches) {
        foreach ($m in $matches) { $emDashFiles += "$($m.Path):$($m.LineNumber): $($m.Line.Trim())" }
    }
}
if ($emDashFiles.Count -gt 0) {
    Add-Violation ("em-dash found in {0} location(s):" -f $emDashFiles.Count)
    foreach ($f in $emDashFiles) { Write-Host "       $f" }
} else {
    Note-OK "no em-dash in webui sources"
}

# 2) Hardcoded rgba( in CSS (исключения — legacy / var()-wrap).
$rgbaInCss = @()
foreach ($cssFile in Get-ChildItem -Path 'webui\css' -Filter '*.css') {
    $matches = Select-String -Path $cssFile.FullName -Pattern 'rgba\(' -ErrorAction SilentlyContinue
    if ($matches) {
        foreach ($m in $matches) {
            $line = $m.Line
            $allowed = ($line -match 'legacy') -or ($line -match 'backdrop-filter') -or ($line -match 'var\(')
            if (-not $allowed) {
                $rgbaInCss += "$($m.Path):$($m.LineNumber): $($line.Trim())"
            }
        }
    }
}
if ($rgbaInCss.Count -gt 0) {
    Add-Violation ("hardcoded rgba() in {0} CSS location(s):" -f $rgbaInCss.Count)
    foreach ($f in $rgbaInCss) { Write-Host "       $f" }
} else {
    Note-OK "no hardcoded rgba() in CSS (legacy/backdrop-filter excluded)"
}

# 3) Эмодзи в webui/index.html user-facing текстах.
$emojiPattern = '[\u00A0-\uFFFF]'  # 2026-06-29: PS не поддерживает \u{...}
# fallback: ищем все non-ASCII, потом отфильтровываем кириллицу через
$emojiLocations = @()
if (Test-Path 'webui\index.html') {
    $matches = Select-String -Path 'webui\index.html' -Pattern $emojiPattern -ErrorAction SilentlyContinue
    if ($matches) {
        foreach ($m in $matches) {
            $emojiLocations += "$($m.Path):$($m.LineNumber): $($m.Line.Trim().Substring(0, [Math]::Min(80, $m.Line.Trim().Length)))..."
        }
    }
}
$nonASCIIStrings = $emojiLocations  # 2026-06-29: PS -match над emoji ненадёжен
# Все non-ASCII строки в index.html — review-only, не auto-fail.
# Используйте grep -P '\p{Extended_Pictographic}' или emojis.json для точной проверки.
$nonFlagEmoji = $emojiLocations | Where-Object { $_ -notmatch '(?:RU|EN|рус|English)' }
if ($nonFlagEmoji.Count -gt 0) {
    Add-Violation ("non-language emoji in index.html ({0} locations):" -f $nonFlagEmoji.Count)
    foreach ($f in $nonFlagEmoji) { Write-Host "       $f" }
} else {
    Note-OK "no non-language emoji in index.html (only lang flags remain if any)"
}

# 4) Every CSS файл использует typography scale вместо hardcoded font-size.
$nonTokenFontSize = @()
foreach ($cssFile in Get-ChildItem -Path 'webui\css' -Filter '*.css') {
    $matches = Select-String -Path $cssFile.FullName -Pattern 'font-size:\s*\d' -ErrorAction SilentlyContinue
    if ($matches) {
        foreach ($m in $matches) {
            $line = $m.Line.Trim()
            if ($line -notmatch 'var\(--font-size') {
                $nonTokenFontSize += "$($m.Path):$($m.LineNumber): $line"
            }
        }
    }
}
if ($nonTokenFontSize.Count -gt 5) {
    Add-Violation ("{0} uses of hardcoded font-size px values (review):" -f $nonTokenFontSize.Count)
    foreach ($f in ($nonTokenFontSize | Select-Object -First 10)) { Write-Host "       $f" }
    if ($nonTokenFontSize.Count -gt 10) { Write-Host "       ... and $($nonTokenFontSize.Count - 10) more" }
} else {
    Note-OK "font-size mostly tokenised (review-only threshold)"
}

Write-Host ""
if ($violations -eq 0) {
    Write-Host "TASTE compliance: PASSED" -ForegroundColor Green
    exit 0
} else {
    Write-Host "TASTE compliance: $violations violation group(s) found" -ForegroundColor Red
    exit 1
}