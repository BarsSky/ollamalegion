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
    # Пусто = автоопределение: deployments/.env, если он есть, иначе
    # deployments/.env.example (см. пояснение ниже).
    [string]$EnvFile = "",
    [string]$ManifestFile = "release-manifest.json"
)

$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
$deploymentsDir = Join-Path $repoRoot "deployments"
$composePath = Join-Path $deploymentsDir $ComposeFile
$manifestPath = Join-Path $deploymentsDir $ManifestFile

# ВАЖНО про выбор файла: `deployments/.env` — ЛОКАЛЬНЫЙ файл (он в .gitignore),
# поэтому в CI его нет. Отслеживаемый контракт — `deployments/.env.example`
# (его копируют в .env). Проверяем .env, если он есть (локально — фактические
# значения), иначе .env.example (в CI — что контракт объявляет все переменные).
$envCandidates = if ($EnvFile) { @($EnvFile) } else { @(".env", ".env.example") }
$envPath = $null
$usedEnvName = ""
foreach ($cand in $envCandidates) {
    $candidatePath = Join-Path $deploymentsDir $cand
    if (Test-Path $candidatePath) {
        $envPath = $candidatePath
        $usedEnvName = $cand
        break
    }
}

$problems = New-Object System.Collections.Generic.List[string]

if (-not (Test-Path $composePath)) { throw "нет файла $composePath" }
if (-not $envPath) {
    throw "нет ни deployments/.env, ни deployments/.env.example — не с чем сверять теги образов"
}

Write-Host "check-image-tags: $ComposeFile (переменные: $usedEnvName)" -ForegroundColor Cyan

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
            $problems.Add("строка ${lineNo}: в $usedEnvName нет переменной $name — compose возьмёт default '$default'. Добавьте '$name=$default' в deployments/$usedEnvName.")
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

# --- R486: идентичность сборки запекается в образ --------------------------
#
# ЗАЧЕМ ЭТОТ БЛОК. Проверка выше видит только строки `image:`. Живой дефект был
# ровно в другом месте: образ собирался с ЧУЖИМ штампом (коммит предыдущего
# релиза), потому что build-arg'и идентичности не передавались, а Dockerfile
# молча брал `ARG GIT_COMMIT=unknown`. Манifest при этом честно писал gitCommit
# из репозитория — то есть расхождение «манифест vs образ» не видел никто.
#
# Что проверяется теперь (текстово, без docker):
#   1) в compose не осталось отменённых переменных идентичности (R486);
#   2) ни один аргумент сборки не имеет дефолта `unknown`;
#   3) у КАЖДОГО build-блока есть аргумент GIT_COMMIT_FULL (count build-блоков
#      = count `GIT_COMMIT_FULL:`), то есть новый сервис не появится без
#      идентичности;
#   4) каждый `dockerfile:` пишет `/app/identity.json` и штампует
#      pkg/version.Commit -ldflags.
$argsSectionSeen = $false
$removedVars = @('BALANCER_GIT_COMMIT', 'BALANCER_BUILD_DATE',
    'CPPWORKER_GIT_COMMIT', 'CPPWORKER_BUILD_DATE',
    'WEBUI_GIT_COMMIT', 'WEBUI_BUILD_DATE',
    'AGENT_GIT_COMMIT', 'AGENT_BUILD_DATE')
foreach ($name in $removedVars) {
    $hits = Select-String -Path $composePath -Pattern ([regex]::Escape($name))
    foreach ($h in $hits) {
        $problems.Add("строка $($h.LineNumber): осталась отменённая переменная $name — идентичность сборки задаётся build-arg'ами из scripts/build-identity.ps1 (R486)")
    }
}

$unknownDefaults = Select-String -Path $composePath -Pattern '^\s*(GIT_COMMIT|GIT_COMMIT_FULL|GIT_COMMIT_DATE|BUILD_DATE|BUILD_AUTHOR|LLAMA_BUILD|LLAMA_BUILD_DATE|RPC_PROTO):.*:-unknown\}'
foreach ($h in $unknownDefaults) {
    $problems.Add("строка $($h.LineNumber): аргумент сборки с дефолтом 'unknown' — образ соберётся с ложным штампом. Оставьте пусто (`$`{VAR:-}) и передавайте значение из scripts/build-identity.ps1")
}

$buildBlocks = @(Select-String -Path $composePath -Pattern '^\s+dockerfile:\s*(\S+)\s*$')
$identityArgs = @(Select-String -Path $composePath -Pattern '^\s*GIT_COMMIT_FULL:')
if ($buildBlocks.Count -ne $identityArgs.Count) {
    $problems.Add("build-блоков в ${ComposeFile}: $($buildBlocks.Count), аргументов GIT_COMMIT_FULL: $($identityArgs.Count) — сервис без идентичности сборки соберётся со штампом 'unknown' (R486)")
}

foreach ($b in $buildBlocks) {
    $dockerfileRel = $b.Matches[0].Groups[1].Value
    # Пути dockerfile: в compose отсчитываются от КОРНЯ репозитория (context: ..),
    # а не от deployments/.
    $dockerfilePath = Join-Path (Split-Path -Parent $PSScriptRoot) $dockerfileRel
    if (-not (Test-Path $dockerfilePath)) {
        $problems.Add("строка $($b.LineNumber): dockerfile '$dockerfileRel' не найден")
        continue
    }
    $df = Get-Content -Raw -LiteralPath $dockerfilePath
    if ($df -notmatch 'identity\.json') {
        $problems.Add("$dockerfileRel не пишет identity.json — образ не сможет доказать свою идентичность (R486)")
    }
    if ($df -match 'go build' -and $df -notmatch 'pkg/version\.Commit=') {
        $problems.Add("$dockerfileRel собирает Go-бинарь без -X pkg/version.Commit — штамп сборки не попадёт в бинарь (R486)")
    }
    if ($df -match 'go build' -and $df -notmatch 'GIT_COMMIT not supplied') {
        $problems.Add("$dockerfileRel собирает Go-бинарь без стража 'GIT_COMMIT not supplied' — пустой build-arg снова даст образ со штампом 'unknown' (R486)")
    }
    $argsSectionSeen = $true
}
if (-not $argsSectionSeen) {
    $problems.Add("в ${ComposeFile} не найдено ни одного build-блока с dockerfile: — проверьте формат")
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
