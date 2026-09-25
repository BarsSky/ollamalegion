<#
.SYNOPSIS
  R83 (2026-09-25): проверка согласованности тегов образов в продовом compose.

.DESCRIPTION
  Проверяет БЕЗ docker (можно запускать где угодно, в т.ч. на CI):

    1. каждый `image:` в docker-compose.cppworker-bundled-with-agent.yml
       параметризован через `${VAR:-default}` (а не литералом);
    2. для каждой найденной переменной есть строка `VAR=...` в deployments/.env
       (иначе compose возьмёт default, и выпуск «не доедет»);
    3. если есть deployments/release-manifest.json — фактически подставляемый
       тег совпадает с тегом из манифеста для каждого компонента.

  ЗАЧЕМ. До R83 теги жили в трёх разных местах и обновлялись тремя разными
  способами, поэтому «пересобрали один компонент, три остались на старых
  версиях» было штатной ситуацией. Пункт 3 ловит именно её: подняли webui, но
  забыли balancer → манифест и .env расходятся → проверка падает.

.EXAMPLE
  pwsh -File scripts/check-image-tags.ps1
  pwsh -File scripts/check-image-tags.ps1 -Verbose
#>
param(
    [string]$ComposeFile = "docker-compose.cppworker-bundled-with-agent.yml",
    [string]$EnvFile = ".env",
    [string]$ManifestFile = "release-manifest.json"
)

$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
$deploymentsDir = Join-Path $repoRoot "deployments"
$composePath = Join-Path $deploymentsDir $ComposeFile
$envPath = Join-Path $deploymentsDir $EnvFile
$manifestPath = Join-Path $deploymentsDir $ManifestFile

$problems = New-Object System.Collections.Generic.List[string]

if (-not (Test-Path $composePath)) { throw "нет файла $composePath" }
if (-not (Test-Path $envPath)) { throw "нет файла $envPath" }

Write-Host "check-image-tags: $ComposeFile" -ForegroundColor Cyan

# --- читаем .env -----------------------------------------------------------
$envMap = @{}
foreach ($line in Get-Content $envPath) {
    if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        $envMap[$Matches[1]] = $Matches[2].Trim()
    }
}

# --- разбираем image: ------------------------------------------------------
$imageLines = Select-String -Path $composePath -Pattern '^\s*image:\s*(\S+)'
if (-not $imageLines) {
    throw "в $ComposeFile не найдено ни одной строки 'image:' — проверьте формат"
}

$effective = @{}   # repo → фактически подставляемый тег
$varSeen = @{}

foreach ($m in $imageLines) {
    $image = $m.Matches[0].Groups[1].Value
    $lineNo = $m.LineNumber

    $vars = [regex]::Matches($image, '\$\{([A-Za-z_][A-Za-z0-9_]*):-([^}]*)\}')
    if ($vars.Count -eq 0) {
        $problems.Add("строка ${lineNo}: тег не параметризован — image: $image. Замените на `$`{VAR:-<текущий тег>}, чтобы выпуск не требовал правки compose.")
        continue
    }

    $resolved = $image
    foreach ($v in $vars) {
        $name = $v.Groups[1].Value
        $default = $v.Groups[2].Value
        $varSeen[$name] = $lineNo

        $value = $default
        if ($envMap.ContainsKey($name) -and $envMap[$name] -ne "") {
            $value = $envMap[$name]
        } elseif (-not $envMap.ContainsKey($name)) {
            $problems.Add("строка ${lineNo}: в $EnvFile нет переменной $name — compose возьмёт default '$default'. Добавьте '$name=$default' в deployments/$EnvFile.")
        }
        $resolved = $resolved.Replace($v.Value, $value)
    }

    $idx = $resolved.LastIndexOf(':')
    if ($idx -lt 1) {
        $problems.Add("строка ${lineNo}: не удалось разобрать образ '$resolved'")
        continue
    }
    $repo = $resolved.Substring(0, $idx)
    $tag = $resolved.Substring($idx + 1)
    $effective[$repo] = $tag
    Write-Host ("  {0,-30} → {1}" -f $repo, $tag)
}

# --- сверка с манифестом ---------------------------------------------------
if (Test-Path $manifestPath) {
    Write-Host ""
    Write-Host "сверка с $ManifestFile (тег выпуска: $((Get-Content $manifestPath -Raw | ConvertFrom-Json).tag))" -ForegroundColor Cyan
    $manifest = Get-Content $manifestPath -Raw | ConvertFrom-Json
    foreach ($img in $manifest.images) {
        $full = [string]$img.image
        $i = $full.LastIndexOf(':')
        if ($i -lt 1) { continue }
        $repo = $full.Substring(0, $i)
        $tag = $full.Substring($i + 1)

        if (-not $effective.ContainsKey($repo)) {
            $problems.Add("манифест содержит $full, но такого образа нет в $ComposeFile — выпуск неполный или манифест устарел")
            continue
        }
        if ($effective[$repo] -ne $tag) {
            $problems.Add("рассинхрон выпуска: compose фактически возьмёт ${repo}:$($effective[$repo]), а манифест фиксирует ${repo}:${tag} — какой-то компонент пересобрали, а тег не зафиксировали")
        } else {
            Write-Host ("  OK  {0}:{1}" -f $repo, $tag) -ForegroundColor DarkGray
        }
    }
} else {
    Write-Host ""
    Write-Host "манифеста $ManifestFile нет — сверка выпуска пропущена (появится после scripts/release-all.ps1)" -ForegroundColor DarkGray
}

# --- итог ------------------------------------------------------------------
Write-Host ""
if ($problems.Count -gt 0) {
    Write-Host "НАЙДЕНО ПРОБЛЕМ: $($problems.Count)" -ForegroundColor Red
    foreach ($p in $problems) { Write-Host "  - $p" -ForegroundColor Red }
    exit 1
}
Write-Host "OK: теги параметризованы, переменные ($($varSeen.Keys -join ', ')) есть в $EnvFile" -ForegroundColor Green
exit 0
