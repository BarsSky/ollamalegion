<#
.SYNOPSIS
  up-stack.ps1 — поднимает стек OllamaLegion одной командой: считает идентичность
  сборки из git, собирает ОТСУТСТВУЮЩИЕ образы и запускает
  `docker compose -f deployments/docker-compose.stack.yml --profile <профиль> up -d`.

.DESCRIPTION
  ЗАЧЕМ ЭТОТ СЛОЙ (долг R486).

  `docker compose ... up -d` на машине без образов уходит в сборку, но падала на
  «страже идентичности»: Dockerfile'ы требуют build-arg'и GIT_COMMIT /
  GIT_COMMIT_FULL / BUILD_DATE (а cppworker — ещё LLAMA_BUILD / RPC_PROTO), так как
  смысл R486 в том, что образ НЕ должен уехать со штампом `unknown`. Вычислять эти
  значения из git умел только scripts/release-all.ps1; deployments/.env их не
  содержит и содержать не может (env_file на build-args не действует, а .git в
  контекст сборки не попадает из-за .dockerignore). То есть «просто подняться» на
  чистой машине было нельзя — и это был дефект, а не осторожность.

  Что делает скрипт (порядок, который теперь работает из коробки):

    1. Проверяет предпосылки: это git-клон (git + HEAD), есть deployments/.env,
       есть docker и docker compose.
    2. Считает идентичность ЕДИНСТВЕННЫМ вычислителем —
       scripts/build-identity.ps1 (git + сабмодуль c/llama.cpp + RPC_PROTO из
       ggml-rpc.h) — и кладёт её в ОКРУЖЕНИЕ процесса. Compose берёт build-arg'и
       только оттуда (`args: GIT_COMMIT: ${GIT_COMMIT:-}` и т.д.).
    3. Показывает, каких образов нет локально (это и есть ответ на «почему
       сборка»), и запускает compose. Сборка идёт ШТАТНО через `build:`-секции
       compose: есть образ — сборки нет, поэтому повторный запуск ничего не
       пересобирает (compose не строит существующие образы без --build).
    4. Стража идентичности не «заглушается»: значение либо настоящее (из git),
       либо скрипт падает. Ручные флаги (-GitCommit/-GitCommitFull/-GitCommitDate/
       -LlamaBuild/-LlamaBuildDate) нужны ровно одному случаю — git недоступен
       (архив исходников без .git); по умолчанию ничего подставлять не нужно.

  Вторая ловушка чистой машины, которую скрипт ловит ДО долгой сборки: CUDA_ARCH
  с архитектурой 100+ (Blackwell) при CUDA_VERSION < 12.8 — nvcc падает на
  «Unsupported gpu architecture 'compute_120'» уже ПОСЛЕ долгой сборки. То же
  правило, что в scripts/release-all.ps1 (R493-арх), теперь действует и здесь.

.PARAMETER ComposeProfile
  Профиль compose. По умолчанию `full` (балансер + webui + cppworker-gpu +
  imageworker на одной машине). Другие: worker, balancer, legacy-agent, rpc-node.

.PARAMETER ComposeFile
  Файл compose внутри deployments/. По умолчанию docker-compose.stack.yml.

.PARAMETER NoBuild
  Не собирать образы: добавить `--no-build`. Нужен, когда образы уже собраны
  (или приходят из registry) и запуск не должен трогать сборку. Если какого-то
  образа нет, скрипт падает ДО compose с перечнем отсутствующих: с --no-build
  compose попытался бы их скачать.

.PARAMETER BuildOnly
  Только собрать (`docker compose ... build`), не запускать. Удобно, чтобы
  проверить сборку на живом стенде, не пересоздавая контейнеры.

.PARAMETER Services
  С какими сервисами работать — только вместе с -BuildOnly (например
  `-Services webui`). С `up` не сочетается намеренно: «поднять половину стека» —
  это не тот порядок, который скрипт защищает.

.PARAMETER Tag
  Переопределить теги образов ТОЛЬКО на этот запуск (BALANCER_TAG,
  CPPWORKER_GPU_TAG, WEBUI_TAG, AGENT_TAG, IMAGE_WORKER_TAG). Для проверочных
  сборок вида `-Tag local-check-webui -BuildOnly -Services webui`: deployments/.env
  не меняется, релизные теги r* не перетираются.

.PARAMETER GitCommit
  Ручное указание короткого коммита (git недоступен). С ним остальные значения
  берутся из git, если он есть, иначе — из флагов ниже.

.PARAMETER GitCommitFull
  Ручное указание полного коммита (метка OCI .revision). По умолчанию берётся из git.

.PARAMETER GitCommitDate
  Ручное указание даты коммита (ISO-8601, как `git log -1 --format=%cI`).

.PARAMETER LlamaBuild
  Ручное указание коммита llama.cpp (нужен только сборке cppworker-gpu).

.PARAMETER LlamaBuildDate
  Ручное указание даты коммита llama.cpp (ГГГГ-ММ-ДД).

.PARAMETER DryRun
  Ничего не собирать и не запускать: показать вычисленную идентичность, состояние
  образов и ТОЧНУЮ команду compose. Docker при этом только читается.

.EXAMPLE
  pwsh -File scripts/up-stack.ps1
  # профиль full: считает идентичность, собирает отсутствующие образы, up -d

.EXAMPLE
  pwsh -File scripts/up-stack.ps1 -ComposeProfile balancer -NoBuild
  # только управление, образы уже есть: ничего не собирается

.EXAMPLE
  pwsh -File scripts/up-stack.ps1 -BuildOnly -Services webui -Tag local-check-webui
  # проверочная сборка одного webui с временным тегом, стенд не трогается

.EXAMPLE
  pwsh -File scripts/up-stack.ps1 -GitCommit 2e7ea36c -GitCommitFull 2e7ea36c444f6219158088db37b33bed827795a7
  # git недоступен (архив исходников): идентичность указана явно
#>
[CmdletBinding()]
param(
    [string]$ComposeProfile = 'full',
    [string]$ComposeFile = 'docker-compose.stack.yml',
    [switch]$NoBuild,
    [switch]$BuildOnly,
    [string[]]$Services = @(),
    [string]$Tag = '',
    [string]$GitCommit = '',
    [string]$GitCommitFull = '',
    [string]$GitCommitDate = '',
    [string]$LlamaBuild = '',
    [string]$LlamaBuildDate = '',
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$script:composeV2 = $true
$script:composeExit = 0

# --- вывод и отказы --------------------------------------------------------
# Ровно один способ сообщить об отказе: короткий текст + подсказки «что делать».
# Никаких «собралось со штампом unknown»: R486 требует падать, а не врать.
function Fail {
    param(
        [Parameter(Mandatory = $true)][string]$Message,
        [string[]]$Hints = @()
    )
    Write-Host ''
    Write-Host "up-stack: ОШИБКА: $Message" -ForegroundColor Red
    foreach ($h in $Hints) { Write-Host "  $h" -ForegroundColor Yellow }
    Write-Host ''
    exit 1
}

function Test-CommandExists {
    param([Parameter(Mandatory = $true)][string]$Name)
    return ($null -ne (Get-Command $Name -ErrorAction SilentlyContinue))
}

# --- docker compose: v2 (`docker compose`) или v1 (`docker-compose`) --------
# Поддержка обеих форм нужна не «для галочки»: на Linux-хостах без плагина
# compose встречается docker-compose 1.29, а `docker compose` там падает с
# «docker: 'compose' is not a docker command».
function Initialize-Compose {
    if (-not (Test-CommandExists 'docker')) {
        Fail 'не найден docker CLI (docker.exe/docker не в PATH)' @(
            'Установите Docker (или Docker Desktop) и проверьте: docker version',
            'Linux: добавьте пользователя в группу docker, иначе нужен sudo'
        )
    }

    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $v2 = $false
    try {
        & docker compose version *> $null
        $v2 = ($LASTEXITCODE -eq 0)
    }
    catch { $v2 = $false }
    finally { $ErrorActionPreference = $prev }

    if ($v2) { $script:composeV2 = $true; return }

    if (Test-CommandExists 'docker-compose') {
        $prev = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        $v1 = $false
        try {
            & docker-compose version *> $null
            $v1 = ($LASTEXITCODE -eq 0)
        }
        catch { $v1 = $false }
        finally { $ErrorActionPreference = $prev }
        if ($v1) {
            $script:composeV2 = $false
            Write-Host 'up-stack: найден только docker-compose v1 — используем его' -ForegroundColor DarkGray
            return
        }
    }

    Fail 'не найден docker compose (ни плагин `docker compose`, ни docker-compose)' @(
        'Docker Desktop: включите «Docker Compose» в настройках',
        'Linux: установите плагин compose v2 (docker-compose-plugin)'
    )
}

function Invoke-ComposeCommand {
    param([Parameter(Mandatory = $true)][string[]]$ComposeArgs)
    if ($script:composeV2) { & docker compose @ComposeArgs }
    else { & docker-compose @ComposeArgs }
    $script:composeExit = $LASTEXITCODE
}

# Читает только разрешённые compose значения: `config --images` — это тот же
# интерполятор, что и у `up`, поэтому список образов совпадает с реальным.
# Возвращает $null, если старый compose такой команды не знает: тогда проверки
# «чего нет локально» пропускаются, а не выдают себя за выполненные.
function Get-ComposeImages {
    param([Parameter(Mandatory = $true)][string[]]$ComposeArgs)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $raw = $null
    $code = 1
    try {
        if ($script:composeV2) { $raw = & docker compose @ComposeArgs config --images 2>$null }
        else { $raw = & docker-compose @ComposeArgs config --images 2>$null }
        $code = $LASTEXITCODE
    }
    catch { $raw = $null; $code = 1 }
    finally { $ErrorActionPreference = $prev }

    if ($code -ne 0 -or $null -eq $raw) { return $null }
    $list = @($raw | ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ -ne '' })
    if ($list.Count -eq 0) { return $null }
    return $list
}

function Test-ImageExists {
    param([Parameter(Mandatory = $true)][string]$ImageRef)
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $ok = $false
    try {
        & docker image inspect $ImageRef *> $null
        $ok = ($LASTEXITCODE -eq 0)
    }
    catch { $ok = $false }
    finally { $ErrorActionPreference = $prev }
    return $ok
}

# --- чтение deployments/.env (ТОЛЬКО ЧТЕНИЕ!) ------------------------------
# Файл не меняем ни при каких условиях: он — состояние живого стенда, и
# перезапись тегов отсюда сломала бы «какой образ сейчас работает».
function Get-EnvFileValue {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Name
    )
    if (-not (Test-Path -LiteralPath $Path)) { return '' }
    $value = ''
    $pattern = '^\s*' + [regex]::Escape($Name) + '\s*=\s*(.*)$'
    foreach ($line in Get-Content -LiteralPath $Path) {
        if ($line -match $pattern) { $value = $Matches[1].Trim() }
    }
    # Кавычки compose снимает сам; здесь — чтобы значение сравнить, а не переписать.
    if ($value.Length -ge 2) {
        $q = $value.Substring(0, 1)
        if (($q -eq '"' -or $q -eq "'") -and $value.EndsWith($q)) {
            $value = $value.Substring(1, $value.Length - 2)
        }
    }
    return $value
}

# Приоритет ровно как у compose: окружение процесса > deployments/.env > default.
function Get-EffectiveComposeVar {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [string]$Default = '',
        [Parameter(Mandatory = $true)][string]$EnvFilePath
    )
    $fromProcess = [Environment]::GetEnvironmentVariable($Name)
    if (-not [string]::IsNullOrWhiteSpace($fromProcess)) { return $fromProcess.Trim() }
    $fromFile = Get-EnvFileValue -Path $EnvFilePath -Name $Name
    if (-not [string]::IsNullOrWhiteSpace($fromFile)) { return $fromFile.Trim() }
    return $Default
}

# Версия — из CHANGELOG.md (верхняя запись `## [0.7.477 - ...]`), если git
# недоступен: `git describe --tags --always` в этом случае не работает, а версия
# нужна для внятного сообщения и для будущих build-arg'ов.
function Get-ChangelogVersion {
    param([Parameter(Mandatory = $true)][string]$RepoRoot)
    $path = Join-Path $RepoRoot 'CHANGELOG.md'
    if (-not (Test-Path -LiteralPath $path)) { return '' }
    $text = Get-Content -Raw -LiteralPath $path
    $m = [regex]::Match($text, '(?m)^##\s*\[(\d+\.\d+\.\d+)')
    if ($m.Success) { return $m.Groups[1].Value }
    return ''
}

# ==========================================================================
# 0. Где мы и всё ли на месте
# ==========================================================================
$repoRoot = Split-Path -Parent $PSScriptRoot
$deploymentsDir = Join-Path $repoRoot 'deployments'
$composePath = Join-Path $deploymentsDir $ComposeFile
$envPath = Join-Path $deploymentsDir '.env'

if (-not (Test-Path -LiteralPath (Join-Path $repoRoot 'go.mod'))) {
    Fail "не найден корень репозитория: в '$repoRoot' нет go.mod" @(
        'Скрипт должен лежать в <репозиторий>/scripts/up-stack.ps1 — не копируйте его отдельно'
    )
}
if (-not (Test-Path -LiteralPath $composePath)) {
    Fail "нет compose-файла: $composePath" @(
        'Проверьте -ComposeFile (по умолчанию docker-compose.stack.yml)'
    )
}
if (-not (Test-Path -LiteralPath $envPath)) {
    Fail "нет $envPath — compose не сможет интерполировать переменные стека" @(
        'Скопируйте шаблон и заново запустите скрипт:',
        '  Copy-Item deployments\.env.example deployments\.env',
        'и задайте в deployments/.env единый токен стека CPPWORKER_API_TOKEN'
    )
}

if ($NoBuild -and $BuildOnly) {
    Fail '-NoBuild и -BuildOnly одновременно бессмысленны: выберите одно' @(
        '-NoBuild  — воспользоваться готовыми образами и поднять стек',
        '-BuildOnly — только собрать образы, не поднимая контейнеры'
    )
}
if ($Services.Count -gt 0 -and -not $BuildOnly) {
    Fail '-Services поддерживается только вместе с -BuildOnly' @(
        'Подъём стека идёт одним порядком: профиль целиком (--profile <профиль> up -d)',
        'Проверочная сборка одного сервиса: -BuildOnly -Services webui'
    )
}
if ($Tag -ne '' -and $Tag -notmatch '^[A-Za-z0-9._-]+$') {
    Fail "недопустимый -Tag '$Tag': разрешены только буквы, цифры, точка, подчёркивание и дефис" @(
        'Пример проверочного тега: -Tag local-check-webui'
    )
}

Initialize-Compose

# Токен стека обязателен и для простого `up -d` (compose падает на `${VAR:?}`):
# проверяем заранее, чтобы сообщение было человеческим.
$token = Get-EffectiveComposeVar -Name 'CPPWORKER_API_TOKEN' -Default '' -EnvFilePath $envPath
if ([string]::IsNullOrWhiteSpace($token)) {
    Fail "в deployments/.env не задан CPPWORKER_API_TOKEN" @(
        'Это единый токен стека (балансер, воркеры, webui). Впишите свою строку:',
        '  CPPWORKER_API_TOKEN=<ваш-токен>',
        'Значение по умолчанию из .env.example — плейсхолдер, его нужно заменить'
    )
}

# ==========================================================================
# 1. Идентичность сборки (R486) — ЕДИНСТВЕННЫЙ вычислитель: build-identity.ps1
# ==========================================================================
. (Join-Path $PSScriptRoot 'build-identity.ps1')

$gitAvailable = Test-CommandExists 'git'
$commitShort = ''
$commitFull = ''
$commitDate = ''
$buildDate = ''
$buildAuthor = ''
$buildVersion = ''
$buildBranch = ''

# Идентичность llama.cpp нужна только тому образу, который её линкует —
# cppworker-gpu (в профилях full/worker), и только когда возможна сборка.
$profileHasCppworker = ($ComposeProfile -eq 'full' -or $ComposeProfile -eq 'worker')
$mayBuildCppworker = $profileHasCppworker -and (-not $NoBuild)
if ($BuildOnly -and $Services.Count -gt 0 -and -not ($Services -contains 'cppworker-gpu')) {
    $mayBuildCppworker = $false
}

# manual: явный коммит означает «git не используем для этого поля». Так
# оператор на архиве исходников без .git указывает штамп осознанно — но не
# получает подстановку `unknown` по умолчанию (стража остаётся стра́жей).
$manual = (($GitCommit -ne '') -or ($GitCommitFull -ne '') -or ($GitCommitDate -ne ''))

if (-not $manual) {
    # Отдельное сообщение «git нет в PATH» вместо исключения PowerShell: оператор
    # должен видеть причину и что делать, а не «The term 'git' is not recognized».
    if (-not $gitAvailable) {
        Fail 'git не найден в PATH — идентичность сборки (коммит/дата) вычислить нечем' @(
            'Без неё образы не собираются: Dockerfile-стража R486 отвергает пустой/unknown коммит',
            'Установите git (https://git-scm.com) или задайте идентичность явно:',
            '  -GitCommit <короткий-sha> -GitCommitFull <полный-sha> [-GitCommitDate <ISO-8601>]'
        )
    }
    try {
        $base = Get-BuildIdentity -Service 'stack'
        $commitShort = [string]$base.GitCommit
        $commitFull = [string]$base.GitCommitFull
        $commitDate = [string]$base.GitCommitDate
        $buildDate = [string]$base.BuildDate
        $buildAuthor = [string]$base.Author
        $buildVersion = [string]$base.Version
    }
    catch {
        $reason = $_.Exception.Message
        Fail "не удалось вычислить идентичность сборки из git: $reason" @(
            'Без неё образы не собираются: Dockerfile-стража R486 отвергает пустой/unknown коммит',
            'Проверьте, что это git-клон (нужен каталог .git) и git есть в PATH:',
            '  git -C <репозиторий> rev-parse --short HEAD',
            'Если git недоступен (архив исходников), задайте идентичность явно:',
            '  -GitCommit <короткий-sha> -GitCommitFull <полный-sha> [-GitCommitDate <ISO-8601>]'
        )
    }
    if ($gitAvailable) {
        $buildBranch = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('rev-parse', '--abbrev-ref', 'HEAD')
    }
}
else {
    # Ручной путь: поле из флага, иначе — из git, если он есть.
    if ($GitCommit) { $commitShort = $GitCommit }
    elseif ($gitAvailable) { $commitShort = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('rev-parse', '--short', 'HEAD') }

    if ($GitCommitFull) { $commitFull = $GitCommitFull }
    elseif ($gitAvailable) { $commitFull = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('rev-parse', 'HEAD') }

    if ($GitCommitDate) { $commitDate = $GitCommitDate }
    elseif ($gitAvailable) { $commitDate = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('log', '-1', '--format=%cI') }

    $buildDate = ConvertTo-Rfc3339Utc -Value (Get-Date)

    if ($env:BUILD_AUTHOR) { $buildAuthor = $env:BUILD_AUTHOR }
    elseif ($gitAvailable) { $buildAuthor = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('config', 'user.name') }

    if ($gitAvailable) { $buildVersion = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('describe', '--tags', '--always') }
    if (-not $buildVersion) { $buildVersion = Get-ChangelogVersion -RepoRoot $repoRoot }
    if (-not $buildVersion) { $buildVersion = 'dev' }

    if ($gitAvailable) { $buildBranch = Get-GitValueR486 -RepoRoot $repoRoot -GitArgs @('rev-parse', '--abbrev-ref', 'HEAD') }

    # Честность важнее удобства: полный коммит в метке .revision не выдумываем.
    if (-not $commitFull -and $commitShort) {
        $commitFull = $commitShort
        Write-Host 'up-stack: внимание: -GitCommitFull не задан, в метку .revision пойдёт короткий коммит' -ForegroundColor Yellow
    }
    if (-not $commitDate) {
        Write-Host 'up-stack: внимание: дата коммита неизвестна (git недоступен, -GitCommitDate не задан) — поле commit_date в identity.json будет пустым' -ForegroundColor Yellow
    }
}

if ([string]::IsNullOrWhiteSpace($commitShort)) {
    Fail 'идентичность сборки пуста: коммит получить не удалось' @(
        'git недоступен и -GitCommit не задан — образ собрать нельзя (стража R486)',
        'Задайте явно: -GitCommit <sha> -GitCommitFull <sha> [-GitCommitDate <ISO-8601>]'
    )
}
if ([string]::IsNullOrWhiteSpace($commitFull)) { $commitFull = $commitShort }

# llama.cpp: коммит/дата/протокол нужны только сборке cppworker-gpu.
$llamaCommit = ''
$llamaDate = ''
$rpcProto = ''
if ($mayBuildCppworker) {
    $llamaDir = Join-Path $repoRoot 'c/llama.cpp'
    $llamaHeader = Join-Path $llamaDir 'ggml/include/ggml-rpc.h'

    if ($LlamaBuild) { $llamaCommit = $LlamaBuild }
    elseif (-not (Test-Path -LiteralPath $llamaDir)) {
        Fail 'нет c/llama.cpp, а профиль собирает cppworker-gpu' @(
            'Сабмодуль не инициализирован. Выполните:',
            '  git submodule update --init --recursive',
            'Если образы уже собраны и нужен только запуск — добавьте -NoBuild'
        )
    }
    elseif ($gitAvailable) { $llamaCommit = Get-GitValueR486 -RepoRoot $llamaDir -GitArgs @('rev-parse', '--short', 'HEAD') }

    if ($LlamaBuildDate) { $llamaDate = $LlamaBuildDate }
    elseif ($gitAvailable) { $llamaDate = Get-GitValueR486 -RepoRoot $llamaDir -GitArgs @('log', '-1', '--format=%cs') }

    if ([string]::IsNullOrWhiteSpace($llamaCommit) -or [string]::IsNullOrWhiteSpace($llamaDate)) {
        Fail 'не удалось вычислить идентичность llama.cpp (нужна сборке cppworker-gpu)' @(
            'Выполните: git submodule update --init --recursive',
            'Либо укажите явно: -LlamaBuild <sha> -LlamaBuildDate <ГГГГ-ММ-ДД>',
            'Если образы уже собраны и нужен только запуск — добавьте -NoBuild'
        )
    }
    # RPC_PROTO — из заголовка, не из git: формат проверяет Read-RpcProtoR486
    # (единственный авторитетный источник, c/llama.cpp/ggml/include/ggml-rpc.h).
    try { $rpcProto = Read-RpcProtoR486 -RepoRoot $repoRoot }
    catch { Fail $_.Exception.Message @('Сабмодуль c/llama.cpp инициализирован полностью? git submodule update --init --recursive') }
    if ([string]::IsNullOrWhiteSpace($rpcProto)) { Fail 'RPC_PROTO пуст — протокол RPC не вычислен (ggml-rpc.h)' }
}

# ==========================================================================
# 2. Что из образов уже есть (это и есть настоящий ответ «почему сборка»)
# ==========================================================================
$composeBaseArgs = @(
    '-f', $composePath,
    '--env-file', $envPath,
    '--profile', $ComposeProfile
)

# -Tag — ТОЛЬКО на этот запуск: теги живут в окружении процесса, deployments/.env
# не переписывается (compose даёт окружению приоритет над .env).
if ($Tag -ne '') {
    $env:BALANCER_TAG = $Tag
    $env:CPPWORKER_GPU_TAG = $Tag
    $env:WEBUI_TAG = $Tag
    $env:AGENT_TAG = $Tag
    $env:IMAGE_WORKER_TAG = $Tag
    Write-Host ("up-stack: теги образов на этот запуск = '{0}' (deployments/.env не изменяется)" -f $Tag) -ForegroundColor DarkGray
}

$images = Get-ComposeImages -ComposeArgs $composeBaseArgs
$missingImages = @()
if ($null -ne $images) {
    Write-Host ''
    Write-Host 'образы стека:' -ForegroundColor Cyan
    foreach ($img in $images) {
        if (Test-ImageExists -ImageRef $img) {
            Write-Host ("  есть  {0}" -f $img) -ForegroundColor DarkGray
        }
        else {
            $missingImages += $img
            if ($NoBuild) { Write-Host ("  НЕТ   {0}" -f $img) -ForegroundColor Red }
            else { Write-Host ("  НЕТ   {0}  -> будет собрана" -f $img) -ForegroundColor Yellow }
        }
    }
}

if ($NoBuild -and $missingImages.Count -gt 0) {
    Fail 'образов нет, а сборка запрещена флагом -NoBuild' @(
        'Отсутствуют:',
        ($missingImages -join ', '),
        'Уберите -NoBuild — compose соберёт отсутствующие образы штатно (build: в compose)'
    )
}

# CUDA_ARCH с архитектурой 100+ требует CUDA 12.8+: иначе nvcc упадёт на
# «Unsupported gpu architecture 'compute_120'» уже после долгой сборки.
# Проверяем только когда cppworker-gpu действительно будет собираться.
if ($mayBuildCppworker) {
    $cppRef = $null
    if ($null -ne $images) {
        $cppMatches = @($images | Where-Object { $_ -like '*cppworker*' })
        if ($cppMatches.Count -gt 0) { $cppRef = [string]$cppMatches[0] }
    }
    $cppWillBuild = $true
    if ($null -ne $cppRef) {
        if (Test-ImageExists -ImageRef $cppRef) { $cppWillBuild = $false }
    }
    if ($cppWillBuild) {
        $cudaArchRaw = Get-EffectiveComposeVar -Name 'CUDA_ARCH' -Default 'all' -EnvFilePath $envPath
        $cudaVersionRaw = Get-EffectiveComposeVar -Name 'CUDA_VERSION' -Default '12.2.0' -EnvFilePath $envPath
        $cudaArchClean = ($cudaArchRaw -replace '#.*$', '').Trim()
        $archTokens = @(($cudaArchClean -split '[;,]') |
            ForEach-Object { ($_ -replace '^sm_', '').Trim() } |
            Where-Object { $_ -match '^\d+$' })
        $needsBlackwell = @($archTokens | Where-Object { [int]$_ -ge 100 }).Count -gt 0
        $verMatch = [regex]::Match($cudaVersionRaw, '(\d+)\.(\d+)')
        if ($needsBlackwell -and $verMatch.Success) {
            $cudaMajorMinor = [double]::Parse(("{0}.{1}" -f $verMatch.Groups[1].Value, $verMatch.Groups[2].Value),
                [System.Globalization.CultureInfo]::InvariantCulture)
            if ($cudaMajorMinor -lt 12.8) {
                Fail ("CUDA_ARCH='{0}' содержит архитектуру 100+ (Blackwell/sm_120), а CUDA_VERSION='{1}' её не знает" -f $cudaArchClean, $cudaVersionRaw) @(
                    'Сборка cppworker-gpu упала бы на «Unsupported gpu architecture compute_120» после долгой сборки',
                    'Исправьте в deployments/.env одно из двух:',
                    '  CUDA_VERSION=12.8.0   (базовый образ CUDA 12.8 умеет sm_120)',
                    '  CUDA_ARCH=86          (или список без 100/120 — под вашу карту)',
                    'Либо задайте на этот запуск: $env:CUDA_VERSION="12.8.0"'
                )
            }
        }
    }
}

# ==========================================================================
# 3. План: печатаем то, что реально уйдёт в docker (без догадок)
# ==========================================================================
$identityEnv = [ordered]@{
    GIT_COMMIT      = $commitShort
    GIT_COMMIT_FULL = $commitFull
    GIT_COMMIT_DATE = $commitDate
    BUILD_DATE      = $buildDate
    BUILD_AUTHOR    = $buildAuthor
    VERSION         = $buildVersion
}
if ($mayBuildCppworker) {
    $identityEnv['LLAMA_BUILD'] = $llamaCommit
    $identityEnv['LLAMA_BUILD_DATE'] = $llamaDate
    $identityEnv['RPC_PROTO'] = $rpcProto
}

Write-Host ''
Write-Host 'идентичность сборки (печётся в образ, R486):' -ForegroundColor Cyan
Write-Host ("  git commit    : {0}" -f $commitShort)
Write-Host ("  git commit full: {0}" -f $commitFull)
$commitDateText = if ($commitDate) { $commitDate } else { 'неизвестна (git недоступен)' }
Write-Host ("  commit date   : {0}" -f $commitDateText)
Write-Host ("  build date    : {0}" -f $buildDate)
$authorText = if ($buildAuthor) { $buildAuthor } else { '(не задан)' }
Write-Host ("  author        : {0}" -f $authorText)
Write-Host ("  version       : {0}" -f $buildVersion)
if ($buildBranch) { Write-Host ("  branch        : {0}" -f $buildBranch) }
if ($mayBuildCppworker) {
    Write-Host ("  llama.cpp     : {0} ({1})" -f $llamaCommit, $llamaDate)
    Write-Host ("  rpc proto     : {0}" -f $rpcProto)
}

$composeRunArgs = @($composeBaseArgs)
if ($BuildOnly) {
    $composeRunArgs += @('build')
    if ($Services.Count -gt 0) { $composeRunArgs += $Services }
}
else {
    $composeRunArgs += @('up', '-d')
    if ($NoBuild) { $composeRunArgs += @('--no-build') }
}
$composeText = if ($script:composeV2) { 'docker compose' } else { 'docker-compose' }
$displayCommand = $composeText + ' ' + ($composeRunArgs -join ' ')

Write-Host ''
Write-Host 'команда:' -ForegroundColor Cyan
Write-Host ("  {0}" -f $displayCommand) -ForegroundColor White

if ($DryRun) {
    Write-Host ''
    Write-Host 'DRY-RUN: ничего не собрано и не запущено; docker только читался.' -ForegroundColor Yellow
    exit 0
}

# ==========================================================================
# 4. Экспорт в окружение процесса и запуск compose
# ==========================================================================
# Compose берёт build-arg'и (`${GIT_COMMIT:-}` и т.д.) из ОКРУЖЕНИЯ процесса:
# env_file на build-args не действует, поэтому единственный способ донести
# идентичность до сборки — эти переменные.
foreach ($name in $identityEnv.Keys) {
    Set-Item -Path ("Env:" + $name) -Value ([string]$identityEnv[$name])
}

$sw = [System.Diagnostics.Stopwatch]::StartNew()
Write-Host ''
Write-Host 'запуск...' -ForegroundColor Cyan
Invoke-ComposeCommand -ComposeArgs $composeRunArgs
$sw.Stop()

if ($script:composeExit -ne 0) {
    Fail ("docker compose завершился с кодом {0}" -f $script:composeExit) @(
        'Полный вывод выше. Частые причины на чистой машине:',
        '  * нет доступа к базовому образу (NGINX_BASE / CUDA_BASE) — сеть или зеркало;',
        '  * стража идентичности: «FATAL: GIT_COMMIT not supplied» — сообщите, это дефект проводки (R486);',
        '  * docker без прав (Linux: группа docker / sudo).'
    )
}

Write-Host ''
if ($BuildOnly) {
    Write-Host ("готово: сборка заняла {0:N1} с" -f $sw.Elapsed.TotalSeconds) -ForegroundColor Green
    Write-Host ('Проверить штамп в образе: docker image inspect --format ''{{index .Config.Labels "org.opencontainers.image.revision"}}'' <образ>') -ForegroundColor DarkGray
    exit 0
}

Write-Host ("готово: стек поднят ({0:N1} с)" -f $sw.Elapsed.TotalSeconds) -ForegroundColor Green
Write-Host ''
Write-Host 'проверка здоровья:' -ForegroundColor Cyan
Write-Host ("  {0} -f {1} --profile {2} ps" -f $composeText, $ComposeFile, $ComposeProfile) -ForegroundColor DarkGray
Write-Host '  клиентский API (Ollama/OpenAI): http://localhost:18080'
Write-Host '  admin API + метрики         : http://localhost:18081/api/v1/health'
Write-Host '  WebUI оператора             : http://localhost:18083'
Write-Host 'Порты переопределяются в deployments/.env (LB_PORT / LB_ADMIN_PORT / WEBUI_PORT).' -ForegroundColor DarkGray
