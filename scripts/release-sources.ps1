<#
.SYNOPSIS
  Релиз ИСХОДНИКОВ (source-only): архив кода + краткое описание. БЕЗ сборки
  бинарников и БЕЗ Docker-образов.

.DESCRIPTION
  Делает ровно три вещи и ничего больше:
    1. собирает архив текущего дерева (git archive — только отслеживаемые файлы);
    2. кладёт рядом краткое описание (по умолчанию docs/v1.0-release-notes.md);
    3. печатает контрольную сумму и содержимое верхнего уровня архива.

  ЧЕГО НЕ ДЕЛАЕТ (осознанно):
    - не запускает docker build / release-all.ps1 / compose;
    - не компилирует Go и не собирает бинарники;
    - не меняет теги образов, deployments/.env и release-manifest.json;
    - не делает git tag и не пушит (это отдельное, явное действие — см. -TagOnly).

  Почему git archive, а не сжатие каталога: в архиф попадают ТОЛЬКО
  отслеживаемые файлы — значит в релиз не утекут ни .env с токенами, ни
  deployments/data, ни локальные сборки llama.cpp (c/llama.cpp.local-bak*),
  ни _diag. Размер архива при этом остаётся разумным даже с вендоренным
  llama.cpp.

.PARAMETER Tag
  Имя релиза/архива. По умолчанию — из текущей даты: v1.0-rc2-src-<yyyyMMdd>.

.PARAMETER OutDir
  Куда класть артефакты. По умолчанию dist/.

.PARAMETER NotesPath
  Файл с описанием релиза. По умолчанию docs/v1.0-release-notes.md.
  Если файла нет — архив всё равно соберётся, но об этом будет предупреждение.

.PARAMETER Format
  zip (по умолчанию) или tar.gz.

.PARAMETER TagOnly
  Не собирать архив, а только создать аннотированный git-тег (без push).
  Требует чистого рабочего дерева.

.EXAMPLE
  pwsh -File scripts/release-sources.ps1
  pwsh -File scripts/release-sources.ps1 -Tag v1.0-src -Format tar.gz
  pwsh -File scripts/release-sources.ps1 -TagOnly -Tag v1.0
#>
param(
    [string]$Tag = "",
    [string]$OutDir = "dist",
    [string]$NotesPath = "docs/v1.0-release-notes.md",
    [ValidateSet("zip", "tar.gz")]
    [string]$Format = "zip",
    [switch]$TagOnly
)

$ErrorActionPreference = "Stop"

function Info($msg) { Write-Host "  $msg" }
function Ok($msg) { Write-Host "  OK  $msg" -ForegroundColor Green }
function Warn($msg) { Write-Host "  !!  $msg" -ForegroundColor Yellow }

# --- repo root (скрипт лежит в scripts/) -------------------------------------
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    if (-not (Test-Path .git)) { throw "не git-репозиторий: $root" }

    if ([string]::IsNullOrWhiteSpace($Tag)) {
        $Tag = "v1.0-rc2-src-" + (Get-Date -Format 'yyyyMMdd')
    }
    if ($Tag -notmatch '^[A-Za-z0-9._-]+$') {
        throw "Недопустимый тег '$Tag': разрешены буквы, цифры, точка, подчёркивание, дефис"
    }

    # --- режим «только тег» --------------------------------------------------
    if ($TagOnly) {
        Write-Host "=== git tag (source-only релиз, без push) ===" -ForegroundColor Cyan
        $dirty = git status --porcelain
        if ($dirty) {
            Warn "рабочее дерево грязное — коммитьте до тега:"
            $dirty | Select-Object -First 10 | ForEach-Object { Info $_ }
            throw "не ставлю тег на незакоммиченные изменения"
        }
        git tag -a $Tag -m "OllamaLegion $Tag — source-only release"
        if ($LASTEXITCODE -ne 0) { throw "git tag не удался" }
        Ok "тег создан: $Tag (локально)"
        Info "пуш — отдельным явным действием: git push origin $Tag"
        return
    }

    Write-Host "=== OllamaLegion: релиз ИСХОДНИКОВ ($Tag) ===" -ForegroundColor Cyan
    $commit = (git rev-parse --short HEAD).Trim()
    $branch = (git rev-parse --abbrev-ref HEAD).Trim()
    $dirty = [bool](git status --porcelain)
    Info "ветка:  $branch"
    Info "коммит: $commit"
    if ($dirty) {
        Warn "рабочее дерево содержит незакоммиченные изменения."
        Warn "Архив соберётся по КОММИТУ (git archive), а не по рабочему дереву —"
        Warn "то есть незакоммиченные правки в релиз НЕ попадут. Это сделано намеренно."
    }

    if (-not (Test-Path $NotesPath)) {
        Warn "файл описания не найден: $NotesPath (архив соберётся без него)"
    }

    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
    $prefix = "ollamalegion-$Tag"
    $archive = Join-Path $OutDir "$prefix.$Format"

    # --- архив (только отслеживаемые файлы) ---------------------------------
    Write-Host ""
    Write-Host "[1/3] git archive (отслеживаемые файлы, без бинарников и образов)" -ForegroundColor Cyan
    if ($Format -eq "zip") {
        git archive --format=zip --prefix="$prefix/" -o $archive HEAD
    } else {
        git archive --format=tar.gz --prefix="$prefix/" -o $archive HEAD
    }
    if ($LASTEXITCODE -ne 0) { throw "git archive не удался" }
    $sizeMB = [math]::Round((Get-Item $archive).Length / 1MB, 1)
    Ok "$archive ($sizeMB МБ)"

    # --- описание рядом с архивом -------------------------------------------
    Write-Host ""
    Write-Host "[2/3] краткое описание" -ForegroundColor Cyan
    if (Test-Path $NotesPath) {
        $notesOut = Join-Path $OutDir "$prefix-README.md"
        Copy-Item $NotesPath $notesOut -Force
        Ok "$notesOut"
    } else {
        Warn "описание пропущено (нет $NotesPath)"
    }

    # --- проверка содержимого ------------------------------------------------
    #
    # ВАЖНО про список: git archive кладёт ТОЛЬКО отслеживаемые файлы, поэтому
    # проверка нужна не «вообще на мусор», а против конкретных классов:
    #   - временные каталоги/файлы (dist/, .tmp*);
    #   - то, что уже отфильтровано .gitignore, но могло попасть случайно;
    #   - секреты: рабочее .env (в отличие от tracked *.example.env, которые
    #     оператору НУЖНЫ и должны быть в релизе);
    #   - персистентное состояние (deployments/data: state.json/config.json — это
    #     рантайм, а не исходники).
    Write-Host ""
    Write-Host "[3/3] проверка архива" -ForegroundColor Cyan

    $forbidden = @(
        '(^|/)dist/',
        '(^|/)\.tmp',
        '(^|/)\.r83-',
        'deployments/data/',
        '(^|/)\.env$'  # рабочее окружение; *.example.env — разрешены и ожидаемы
    )
    $expectedTracked = @('release-manifest.json', 'example.env')
    $entries = @()
    if ($Format -eq "zip") {
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [System.IO.Compression.ZipFile]::OpenRead((Resolve-Path $archive))
        try { $entries = $zip.Entries | ForEach-Object { $_.FullName } } finally { $zip.Dispose() }
    } else {
        $entries = (tar -tzf $archive) 2>$null
        if (-not $entries) { Warn "tar недоступен — проверка содержимого пропущена" }
    }

    if ($entries.Count -gt 0) {
        Info "файлов в архиве: $($entries.Count)"
        $bad = @()
        foreach ($e in $entries) { foreach ($f in $forbidden) { if ($e -match $f) { $bad += $e } } }
        if ($bad.Count -gt 0) {
            Warn "в архиве есть файлы, которых там быть не должно:"
            $bad | Select-Object -First 10 | ForEach-Object { Info $_ }
        } else {
            Ok "запрещённых файлов нет (нет dist/, .tmp*, deployments/data/, рабочего .env)"
        }
        # Ожидаемые метаданные: полезно подтвердить, что они ЕСТЬ, а не отсутствуют.
        foreach ($exp in $expectedTracked) {
            $hit = $entries | Where-Object { $_ -match [regex]::Escape($exp) } | Select-Object -First 1
            if ($hit) { Ok "ожидаемый файл на месте: $($hit -replace "^$prefix/", "")" }
        }
        # Верхний уровень — чтобы глазами видеть состав.
        Write-Host "  верхний уровень:" -ForegroundColor DarkGray
        $entries |
            ForEach-Object { $_ -replace "^$prefix/", "" } |
            Where-Object { $_ -match '^[^/]+/?$' } |
            Sort-Object -Unique | Select-Object -First 25 | ForEach-Object { Write-Host "    $_" -ForegroundColor DarkGray }
    }

    $sha = (Get-FileHash $archive -Algorithm SHA256).Hash
    Write-Host ""
    Write-Host "=== Готово: $Tag (source-only) ===" -ForegroundColor Cyan
    Info "архив:    $archive ($sizeMB МБ)"
    Info "SHA256:   $sha"
    Info "коммит:   $commit ($branch)"
    Write-Host ""
    Info "Бинарники и Docker-образы НЕ собирались — это релиз исходников."
    Info "Тег (когда решите): pwsh -File scripts/release-sources.ps1 -TagOnly -Tag $Tag"
}
finally {
    Pop-Location
}
