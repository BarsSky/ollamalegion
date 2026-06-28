# setup-runner.ps1 — регистрация этой Windows-машины как GitHub Actions self-hosted runner
#
# Использование (от администратора):
#   .\scripts\setup-runner.ps1 -RepoOwner "BarsSky" -RepoName "ollamalegion" -GitHubToken "<PAT>"
#
# Параметры:
#   -RepoOwner    GitHub owner (default: BarsSky)
#   -RepoName     GitHub repo name (default: ollamalegion)
#   -GitHubToken  Personal Access Token с правом `repo` + `admin:org` (read:actions)
#   -RunnerName   Имя runner'а (default: <hostname>-ci)
#   -Labels       Метки через запятую (default: self-hosted,windows,ollamalegion-ci)
#   -RunnerDir    Директория установки (default: C:\actions-runner)
#   -RunnerVersion Версия runner'а (default: 2.319.1)
#   -Unattended   Не задавать вопросов (default: false)
#   -SkipDownload Пропустить download (если уже скачано в $RunnerDir)
#
# Что делает:
#   1. Проверяет prerequisites (chocolatey, Go 1.21, Docker, git, node).
#   2. Скачивает GitHub Actions runner в $RunnerDir (если не SkipDownload).
#   3. Получает registration token через GitHub API.
#   4. Регистрирует runner с labels [self-hosted, windows, ollamalegion-ci].
#   5. Устанавливает как Windows service `actions.runner.*-ci` (auto-start).
#   6. Запускает runner.
#
# После установки:
#   - Проверить: .\scripts\check-runner.ps1
#   - Удалить:  cd C:\actions-runner && .\config.cmd remove --token <TOKEN>
#   - Остановить: Stop-Service "actions.runner.*-ci"
#   - Логи:    Get-EventLog -LogName Application -Source "Actions Runner" -Newest 20
# -----------------------------------------------------------------------

[CmdletBinding()]
param(
    [string]$RepoOwner = "BarsSky",
    [string]$RepoName = "ollamalegion",
    [string]$GitHubToken = "",
    [string]$RunnerName = "",
    [string]$Labels = "self-hosted,windows,ollamalegion-ci",
    [string]$RunnerDir = "C:\actions-runner",
    [string]$RunnerVersion = "2.319.1",
    [switch]$Unattended,
    [switch]$SkipDownload
)

$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# Цвета для вывода
function Write-Success { Write-Host $args[0] -ForegroundColor Green }
function Write-Warn    { Write-Host $args[0] -ForegroundColor Yellow }
function Write-Err     { Write-Host $args[0] -ForegroundColor Red }
function Write-Step    { Write-Host "`n=== $($args[0]) ===" -ForegroundColor Cyan }

# Дополняем PATH: гарантируем наличие Go (часто бывает в C:\Program Files\Go\bin,
# но не в PATH для PowerShell-сессии, особенно при запуске из cmd через -Command).
# Это backward-compatible: если Go уже в PATH — повторное добавление безвредно.
$goCandidates = @(
    "C:\Program Files\Go\bin",
    "C:\Program Files (x86)\Go\bin",
    "$env:LOCALAPPDATA\Go\bin"
)
foreach ($goDir in $goCandidates) {
    if ((Test-Path (Join-Path $goDir "go.exe")) -and ($env:Path -notlike "*$goDir*")) {
        $env:Path = "$goDir;$env:Path"
    }
}

# -----------------------------------------------------------------------
# 1. Проверка prerequisites
# -----------------------------------------------------------------------
Write-Step "1/6 Проверка prerequisites"

function Test-Tool {
    param([string]$Cmd, [string]$Name, [string[]]$VersionArgs = @("--version"))
    # Разные тулзы используют разные способы вывода версии:
    #   go (1.25+)      → subcommand `version` (флага --version больше нет)
    #   go (1.21..1.24) → флаг `--version`
    #   node            → флаг `--version`
    #   docker          → флаг `--version`
    #   git             → флаг `--version`
    #   cmake           → флаг `--version` (но multi-line)
    # Поэтому пробуем переданные аргументы по очереди. По умолчанию --version,
    # но для go передаём сначала `version` (subcommand).
    foreach ($arg in $VersionArgs) {
        $version = $null
        try {
            $version = & $Cmd $arg 2>$null
            if ($LASTEXITCODE -eq 0) {
                $ver = ($version | Select-Object -First 1).Trim()
                if ($ver) {
                    Write-Success "  ✅ $Name : $ver"
                    return $true
                }
            }
        } catch {}
    }
    Write-Err "  ❌ $Name : НЕ НАЙДЕН ($Cmd)"
    return $false
}

$allOk = $true
# go 1.25+ использует subcommand `version`, go 1.21..1.24 — флаг `--version`
$allOk = (Test-Tool "go" "Go" @("version", "--version")) -and $allOk
$allOk = (Test-Tool "docker" "Docker") -and $allOk
$allOk = (Test-Tool "git" "Git") -and $allOk
$allOk = (Test-Tool "node" "Node.js") -and $allOk
# cmake иногда выводит warning в stderr на первой строке — не считаем это ошибкой
$allOk = (Test-Tool "cmake" "CMake (опционально, для 7.2 GPU)" @("--version")) -and $allOk

if (-not $allOk) {
    Write-Err "`nНе все prerequisites установлены. Установите недостающие и перезапустите."
    Write-Warn "См. docs/ci/self-hosted-runner.md для инструкций."
    exit 1
}

# Проверка Go-версии
$goVer = (& go version) -replace "go version go", "" -replace " windows/amd64", ""
if ([version]$goVer -lt [version]"1.21.0") {
    Write-Err "  ❌ Go >= 1.21 требуется (текущая: $goVer)"
    exit 1
}
Write-Success "  ✅ Go >= 1.21.0"

# -----------------------------------------------------------------------
# 2. Проверка admin-прав
# -----------------------------------------------------------------------
Write-Step "2/6 Проверка admin-прав"

$currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Err "  ❌ Скрипт требует прав администратора (для установки Windows service)."
    Write-Warn "  Запустите PowerShell от администратора."
    exit 1
}
Write-Success "  ✅ Запущено от администратора"

# -----------------------------------------------------------------------
# 3. Запрос GitHub Token (если не передан)
# -----------------------------------------------------------------------
Write-Step "3/6 GitHub Token"

if (-not $GitHubToken) {
    if ($Unattended) {
        Write-Err "  ❌ -GitHubToken обязателен в unattended режиме."
        exit 1
    }
    $secure = Read-Host "  Введите GitHub Personal Access Token (repo + admin:org scope)" -AsSecureString
    $GitHubToken = [Runtime.InteropServices.Marshal]::PtrToStringAuto(
        [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
    )
}

if ([string]::IsNullOrWhiteSpace($GitHubToken)) {
    Write-Err "  ❌ Токен пустой. Прерывание."
    exit 1
}
Write-Success "  ✅ Токен получен (длина: $($GitHubToken.Length) символов)"

# -----------------------------------------------------------------------
# 4. Имя runner'а
# -----------------------------------------------------------------------
if (-not $RunnerName) {
    $RunnerName = "$($env:COMPUTERNAME.ToLower())-ci"
}
Write-Success "  Runner name: $RunnerName"

# -----------------------------------------------------------------------
# 5. Скачивание GitHub Actions runner
# -----------------------------------------------------------------------
Write-Step "4/6 Скачивание GitHub Actions runner v$RunnerVersion"

if (-not (Test-Path $RunnerDir)) {
    New-Item -ItemType Directory -Path $RunnerDir -Force | Out-Null
}

$zipPath = Join-Path $RunnerDir "actions-runner.zip"
$extractDir = Join-Path $RunnerDir "runner"

if (-not $SkipDownload) {
    $url = "https://github.com/actions/runner/releases/download/v$RunnerVersion/actions-runner-win-x64-$RunnerVersion.zip"
    Write-Host "  URL: $url"
    Write-Host "  Скачиваю..."
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing
    } catch {
        Write-Err "  ❌ Не удалось скачать: $_"
        exit 1
    }
    Write-Success "  ✅ Скачано: $zipPath ($([math]::Round((Get-Item $zipPath).Length / 1MB, 1)) MB)"

    Write-Host "  Распаковываю..."
    Expand-Archive -Path $zipPath -DestinationPath $RunnerDir -Force
    Write-Success "  ✅ Распаковано в $RunnerDir"
} else {
    Write-Warn "  ⏭ SkipDownload: использую существующую $RunnerDir"
}

# -----------------------------------------------------------------------
# 6. Получение registration token
# -----------------------------------------------------------------------
Write-Step "5/6 Получение registration token из GitHub API"

$apiUrl = "https://api.github.com/repos/$RepoOwner/$RepoName/actions/runners/registration-token"
$headers = @{
    "Authorization" = "Bearer $GitHubToken"
    "Accept"        = "application/vnd.github+json"
    "X-GitHub-Api-Version" = "2022-11-28"
}

try {
    $response = Invoke-RestMethod -Method POST -Uri $apiUrl -Headers $headers
    $regToken = $response.token
    Write-Success "  ✅ Registration token получен (expires: $($response.expires_at))"
} catch {
    Write-Err "  ❌ Не удалось получить token: $_"
    Write-Warn "  Проверьте:"
    Write-Warn "    - PAT имеет scope 'repo' + 'admin:org' (для org runner'ов)"
    Write-Warn "    - $RepoOwner/$RepoName существует и доступен"
    exit 1
}

# -----------------------------------------------------------------------
# 7. Регистрация runner'а
# -----------------------------------------------------------------------
Write-Step "6/6 Регистрация и запуск runner'а"

Set-Location $RunnerDir

# Удаляем предыдущую регистрацию (если есть)
if (Test-Path ".\.runner") {
    Write-Warn "  ⏭ Runner уже зарегистрирован. Перерегистрирую..."
    & .\config.cmd remove --token $regToken 2>$null | Out-Null
}

# Регистрируем
$regArgs = @(
    "--url", "https://github.com/$RepoOwner/$RepoName",
    "--token", $regToken,
    "--name", $RunnerName,
    "--labels", $Labels,
    "--work", "_work",
    "--replace"
)
if ($Unattended) { $regArgs += "--unattended" }

Write-Host "  Регистрирую: $RunnerName с labels [$Labels]"
& .\config.cmd @regArgs
if ($LASTEXITCODE -ne 0) {
    Write-Err "  ❌ Регистрация провалилась (exit $LASTEXITCODE)"
    exit 1
}
Write-Success "  ✅ Runner зарегистрирован"

# Установка как Windows service
Write-Host "  Устанавливаю как Windows service..."
& .\svc.cmd install
if ($LASTEXITCODE -ne 0) {
    Write-Err "  ❌ svc install провалился (exit $LASTEXITCODE)"
    exit 1
}
Write-Success "  ✅ Service установлен"

# Запуск service
Write-Host "  Запускаю service..."
& .\svc.cmd start
if ($LASTEXITCODE -ne 0) {
    Write-Err "  ❌ svc start провалился (exit $LASTEXITCODE)"
    exit 1
}
Write-Success "  ✅ Service запущен"

# -----------------------------------------------------------------------
# Финальная проверка
# -----------------------------------------------------------------------
Write-Step "Готово!"

$serviceName = "actions.runner.$RepoOwner-$RepoName.$RunnerName"
$svc = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if ($svc -and $svc.Status -eq "Running") {
    Write-Success "  ✅ Service '$serviceName' работает (Status: $($svc.Status))"
} else {
    Write-Warn "  ⚠️ Service '$serviceName' не запущен. Проверьте: Get-Service $serviceName"
}

Write-Host ""
Write-Host "Следующие шаги:" -ForegroundColor Cyan
Write-Host "  1. Проверить runner:" -ForegroundColor White
Write-Host "     .\scripts\check-runner.ps1" -ForegroundColor Gray
Write-Host "  2. Открыть в GitHub:" -ForegroundColor White
Write-Host "     https://github.com/$RepoOwner/$RepoName/settings/actions/runners" -ForegroundColor Gray
Write-Host "  3. Создать PR для проверки workflow:" -ForegroundColor White
Write-Host "     git push origin feature/<branch>" -ForegroundColor Gray
Write-Host ""
Write-Host "Логи service:" -ForegroundColor Cyan
Write-Host "  Get-EventLog -LogName Application -Source 'Actions Runner' -Newest 20" -ForegroundColor Gray
Write-Host ""
Write-Host "Удаление runner'а (если нужно):" -ForegroundColor Cyan
Write-Host "  cd $RunnerDir && .\config.cmd remove --token <REG_TOKEN>" -ForegroundColor Gray