# build-identity.ps1 — R486 (2026-11-10): ЕДИНСТВЕННЫЙ вычислитель идентичности сборки.
#
# ЗАЧЕМ. Раньше коммит/дата приходили в образ из deployments/.env через
# BALANCER_GIT_COMMIT / CPPWORKER_GIT_COMMIT, а сам скрипт выпуска эти build-arg'и
# вообще не передавал: в Dockerfile срабатывал дефолт ARG GIT_COMMIT=unknown, и
# образ молча уезжал на стенд с ЧУЖИМ штампом (живой случай: образ с коммитом
# 850923d7 при HEAD 8b246ffe; узнали случайно, по /api/version).
#
# ПОЧЕМУ СКРИПТ, А НЕ DOCKERFILE. `.git` в контекст сборки не попадает
# (.dockerignore), поэтому вычислить коммит внутри `RUN` невозможно. Значит
# значения считаются ЗДЕСЬ, на хосте, из git, и передаются build-arg'ами; а
# Dockerfile обязан УПАСТЬ, если arg не пришёл (guard в builder-стадии).
#
# Использование:
#   . scripts/build-identity.ps1            # только функции (Get-BuildIdentity и др.)
#   pwsh scripts/build-identity.ps1 -Service cppworker -Version 0.7.246 -ImageTag 261110-1900
#       -> JSON в stdout
#   pwsh scripts/build-identity.ps1 -Service balancer -BuildArgs
#       -> строки "--build-arg K=V" (для передачи в docker build)
param(
    [string]$Service = '',
    [string]$Version = '',
    [string]$ImageTag = '',
    [string]$Author = '',
    [string]$RepoRoot = '',
    [switch]$BuildArgs
)

$ErrorActionPreference = 'Stop'

function ConvertTo-Rfc3339Utc {
    param([datetime]$Value)
    return $Value.ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

# Get-GitValueR486 — значение из git; $null, если git не смог.
function Get-GitValueR486 {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot,
        [Parameter(Mandatory = $true)][string[]]$GitArgs
    )
    $out = & git -C $RepoRoot @GitArgs 2>$null
    if ($LASTEXITCODE -ne 0 -or -not $out) { return $null }
    return ($out | Out-String).Trim()
}

# Read-RpcProtoR486 — версия протокола RPC из заголовка llama.cpp.
#
# Единственный АВТОРИТЕТНЫЙ источник: c/llama.cpp/ggml/include/ggml-rpc.h
# (`#define RPC_PROTO_MAJOR 6` и т.д.). Своих констант не выдумываем: если
# заголовка нет или формат изменился — это ошибка сборки, а не «unknown».
function Read-RpcProtoR486 {
    param([Parameter(Mandatory = $true)][string]$RepoRoot)
    $header = Join-Path $RepoRoot 'c/llama.cpp/ggml/include/ggml-rpc.h'
    if (-not (Test-Path $header)) {
        throw "R486: нет $header — не могу запечь версию протокола RPC (сабмодуль c/llama.cpp не инициализирован?)"
    }
    $text = Get-Content -Raw -LiteralPath $header
    $nums = @{}
    foreach ($part in @('MAJOR', 'MINOR', 'PATCH')) {
        # Имена в заголовке: RPC_PROTO_MAJOR_VERSION / _MINOR_VERSION / _PATCH_VERSION.
        $m = [regex]::Match($text, "#define\s+RPC_PROTO_$part(?:_VERSION)?\s+(\d+)")
        if (-not $m.Success) {
            throw "R486: в $header нет RPC_PROTO_$part(_VERSION) — формат заголовка изменился, версию протокола не угадываю"
        }
        $nums[$part] = $m.Groups[1].Value
    }
    return "$($nums['MAJOR']).$($nums['MINOR']).$($nums['PATCH'])"
}

<#
.SYNOPSIS
Вычисляет идентичность сборки из git и сабмодуля llama.cpp.

.DESCRIPTION
Бросает исключение, если значения получить не удалось: вызывающий
(build-containers.ps1, release-all.ps1) обязан упасть, а не собрать образ
с штампом «unknown».
#>
function Get-BuildIdentity {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory = $true)][string]$Service,
        [string]$Version = '',
        [string]$ImageTag = '',
        [string]$Author = '',
        [string]$RepoRoot = ''
    )

    if (-not $RepoRoot) { $RepoRoot = Split-Path -Parent $PSScriptRoot }
    if (-not (Test-Path (Join-Path $RepoRoot 'go.mod'))) {
        throw "R486: $RepoRoot не похож на корень репозитория (нет go.mod)"
    }

    $commitShort = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('rev-parse', '--short', 'HEAD')
    if (-not $commitShort) { throw "R486: git rev-parse HEAD недоступен в $RepoRoot — штамп сборки вычислить нечем" }
    $commitFull = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('rev-parse', 'HEAD')
    if (-not $commitFull) { throw "R486: git rev-parse HEAD (полный) недоступен" }
    $commitDate = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('log', '-1', '--format=%cI')
    if (-not $commitDate) { throw "R486: дату коммита получить не удалось" }

    if (-not $Author) { $Author = $env:BUILD_AUTHOR }
    if (-not $Author) {
        $name = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('config', 'user.name')
        $email = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('config', 'user.email')
        if ($name -and $email) { $Author = "$name <$email>" }
        elseif ($name) { $Author = $name }
        elseif ($email) { $Author = $email }
    }

    if (-not $Version) {
        $Version = Get-GitValueR486 -RepoRoot $RepoRoot -GitArgs @('describe', '--tags', '--always')
        if (-not $Version) { $Version = 'dev' }
    }
    if (-not $ImageTag) { $ImageTag = $Version }

    $llamaCommit = ''
    $llamaDate = ''
    $rpcProto = ''
    # Значения llama.cpp нужны только образу, который его линкует — cppworker.
    # Для остальных пусто, и в ldflags их не передаём: пустое не выдаём за факт.
    if ($Service -eq 'cppworker') {
        $llamaDir = Join-Path $RepoRoot 'c/llama.cpp'
        if (-not (Test-Path $llamaDir)) {
            throw "R486: нет $llamaDir — не могу запечь коммит llama.cpp"
        }
        $llamaCommit = Get-GitValueR486 -RepoRoot $llamaDir -GitArgs @('rev-parse', '--short', 'HEAD')
        if (-not $llamaCommit) { throw "R486: git -C c/llama.cpp rev-parse недоступен — коммит llama.cpp не вычислить" }
        $llamaDate = Get-GitValueR486 -RepoRoot $llamaDir -GitArgs @('log', '-1', '--format=%cs')
        if (-not $llamaDate) { throw "R486: дату коммита llama.cpp получить не удалось" }
        $rpcProto = Read-RpcProtoR486 -RepoRoot $RepoRoot
    }

    return [pscustomobject]@{
        Service        = $Service
        Version        = $Version
        ImageTag       = $ImageTag
        GitCommit      = $commitShort
        GitCommitFull  = $commitFull
        GitCommitDate  = $commitDate
        BuildDate      = (ConvertTo-Rfc3339Utc (Get-Date))
        Author         = $Author
        LlamaBuild     = $llamaCommit
        LlamaBuildDate = $llamaDate
        RpcProto       = $rpcProto
    }
}

# Get-BuildArgLinesR486 — те же значения в виде аргументов docker build.
#
# Пустые значения передаются явно: Dockerfile-страж должен получить пустую
# строку и упасть с внятным текстом, а не молча собрать образ без штампа.
function Get-BuildArgLinesR486 {
    param([Parameter(Mandatory = $true)]$Identity)
    $pairs = [ordered]@{
        SERVICE         = $Identity.Service
        VERSION         = $Identity.Version
        IMAGE_TAG       = $Identity.ImageTag
        GIT_COMMIT      = $Identity.GitCommit
        GIT_COMMIT_FULL = $Identity.GitCommitFull
        GIT_COMMIT_DATE = $Identity.GitCommitDate
        BUILD_DATE      = $Identity.BuildDate
        BUILD_AUTHOR    = $Identity.Author
    }
    if ($Identity.Service -eq 'cppworker') {
        $pairs['LLAMA_BUILD'] = $Identity.LlamaBuild
        $pairs['LLAMA_BUILD_DATE'] = $Identity.LlamaBuildDate
        $pairs['RPC_PROTO'] = $Identity.RpcProto
    }
    $lines = New-Object System.Collections.Generic.List[string]
    foreach ($k in $pairs.Keys) {
        $lines.Add('--build-arg')
        $lines.Add(("{0}={1}" -f $k, $pairs[$k]))
    }
    return $lines.ToArray()
}

# Точка входа скрипта: при dot-source Service пуст, и ничего не выполняется.
if ($MyInvocation.InvocationName -ne '.' -and $Service) {
    $params = @{ Service = $Service }
    if ($Version) { $params['Version'] = $Version }
    if ($ImageTag) { $params['ImageTag'] = $ImageTag }
    if ($Author) { $params['Author'] = $Author }
    if ($RepoRoot) { $params['RepoRoot'] = $RepoRoot }
    $identity = Get-BuildIdentity @params
    if ($BuildArgs) {
        Get-BuildArgLinesR486 -Identity $identity | ForEach-Object { $_ }
    }
    else {
        $identity | ConvertTo-Json -Depth 3
    }
}
