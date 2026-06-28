# check-runner.ps1 — диагностика self-hosted runner'а и CI prerequisites
#
# Использование (без admin-прав):
#   .\scripts\check-runner.ps1
#
# Проверяет:
#   1. CI prerequisites (Go >= 1.21, Docker, git, node, cmake).
#   2. Self-hosted runner service (status, метки, last activity).
#   3. Persistent build cache (GOMODCACHE, GOCACHE, ~/.cargo).
#   4. Права на запись в $RunnerDir/_work.
#   5. GitHub API connectivity.
#   6. Workflow файлы (.github/workflows/ci.yml) — наличие.
#
# Exit codes:
#   0 — всё ОК
#   1 — есть проблемы (выводит список)
#   2 — критическая ошибка (runner service не работает)
# -----------------------------------------------------------------------

[CmdletBinding()]
param(
    [string]$RunnerDir = "C:\actions-runner",
    [string]$RepoOwner = "BarsSky",
    [string]$RepoName = "ollamalegion"
)

$ErrorActionPreference = "Continue"
$ProgressPreference = "SilentlyContinue"

# Счётчики
$script:OkCount = 0
$script:WarnCount = 0
$script:ErrCount = 0

function Write-Success { Write-Host $args[0] -ForegroundColor Green; $script:OkCount++ }
function Write-Warn    { Write-Host $args[0] -ForegroundColor Yellow; $script:WarnCount++ }
function Write-Err     { Write-Host $args[0] -ForegroundColor Red; $script:ErrCount++ }
function Write-Step    { Write-Host "`n=== $($args[0]) ===" -ForegroundColor Cyan }

# -----------------------------------------------------------------------
# 1. CI prerequisites
# -----------------------------------------------------------------------
Write-Step "1/6 CI prerequisites"

function Test-Command {
    param([string]$Cmd, [string]$Name, [string]$MinVersion = "")
    try {
        $out = & $Cmd --version 2>$null
        if ($LASTEXITCODE -eq 0) {
            $ver = ($out | Select-Object -First 1).Trim()
            Write-Success "  ✅ $Name : $ver"
            return $true
        }
    } catch {}
    Write-Err "  ❌ $Name : НЕ НАЙДЕН ($Cmd)"
    return $false
}

$prereqOk = $true
$prereqOk = (Test-Command "go" "Go") -and $prereqOk
$prereqOk = (Test-Command "docker" "Docker") -and $prereqOk
$prereqOk = (Test-Command "git" "Git") -and $prereqOk
$prereqOk = (Test-Command "node" "Node.js") -and $prereqOk
$prereqOk = (Test-Command "cmake" "CMake (опционально, для 7.2 GPU)") -and $prereqOk

# Go version check
try {
    $goVer = (& go version) -replace "go version go", "" -replace " windows/amd64", ""
    if ([version]$goVer -ge [version]"1.21.0") {
        Write-Success "  ✅ Go >= 1.21.0 (текущая: $goVer)"
    } else {
        Write-Err "  ❌ Go >= 1.21 требуется (текущая: $goVer)"
    }
} catch {}

# Docker daemon
try {
    $dockerInfo = docker info 2>&1 | Select-String "Server Version"
    if ($dockerInfo) {
        Write-Success "  ✅ Docker daemon работает ($($dockerInfo.Line.Trim()))"
    } else {
        Write-Warn "  ⚠️ Docker daemon не отвечает. Запустите Docker Desktop."
    }
} catch {
    Write-Warn "  ⚠️ Не удалось проверить Docker daemon: $_"
}

# -----------------------------------------------------------------------
# 2. Self-hosted runner service
# -----------------------------------------------------------------------
Write-Step "2/6 Self-hosted runner service"

$runnerFound = $false
try {
    $services = Get-Service -Name "actions.runner.*" -ErrorAction SilentlyContinue
    if ($services) {
        foreach ($svc in $services) {
            $runnerFound = $true
            $statusEmoji = switch ($svc.Status) {
                "Running" { "✅" }
                "Stopped" { "⚠️" }
                default   { "❌" }
            }
            $color = switch ($svc.Status) {
                "Running" { "Green" }
                "Stopped" { "Yellow" }
                default   { "Red" }
            }
            Write-Host "  $statusEmoji Service: $($svc.Name) — Status: $($svc.Status)" -ForegroundColor $color
            if ($svc.Status -ne "Running") {
                Write-Warn "    Запустить: Start-Service '$($svc.Name)'"
            }
        }
    } else {
        Write-Err "  ❌ Service 'actions.runner.*' не найден."
        Write-Warn "    Установите: .\scripts\setup-runner.ps1 -GitHubToken '<PAT>'"
    }
} catch {
    Write-Err "  ❌ Ошибка при проверке service: $_"
}

# Проверка .runner файла (содержит конфигурацию)
if (Test-Path "$RunnerDir\.runner") {
    Write-Success "  ✅ Runner config: $RunnerDir\.runner"
    try {
        $runnerConfig = Get-Content "$RunnerDir\.runner" -Raw | ConvertFrom-Json
        Write-Host "    Agent: $($runnerConfig.agentName)"
        Write-Host "    Pool:  $($runnerConfig.poolName)"
        Write-Host "    URL:   $($runnerConfig.serverUrl)"
    } catch {}
} else {
    Write-Warn "  ⚠️ Runner config не найден: $RunnerDir\.runner"
    Write-Warn "    Запустите setup-runner.ps1 для регистрации."
}

# -----------------------------------------------------------------------
# 3. Persistent build cache
# -----------------------------------------------------------------------
Write-Step "3/6 Persistent build cache"

$caches = @(
    @{ Name = "GOMODCACHE"; Path = $env:GOMODCACHE; Default = Join-Path $env:USERPROFILE "go\pkg\mod" },
    @{ Name = "GOCACHE";    Path = $env:GOCACHE;    Default = Join-Path $env:LOCALAPPDATA "go-build" },
    @{ Name = "CARGO_HOME"; Path = $env:CARGO_HOME; Default = Join-Path $env:USERPROFILE ".cargo" }
)

foreach ($c in $caches) {
    $path = if ($c.Path) { $c.Path } else { $c.Default }
    if (Test-Path $path) {
        $size = (Get-ChildItem $path -Recurse -ErrorAction SilentlyContinue | Measure-Object -Property Length -Sum).Sum
        $sizeMb = [math]::Round($size / 1MB, 1)
        Write-Success "  ✅ $($c.Name) = $path ($sizeMb MB)"
    } else {
        Write-Warn "  ⚠️ $($c.Name) = $path (не существует, будет создан при первом build)"
    }
}

# -----------------------------------------------------------------------
# 4. Права на запись в work directory
# -----------------------------------------------------------------------
Write-Step "4/6 Права на запись в work directory"

$workDir = Join-Path $RunnerDir "_work"
if (Test-Path $workDir) {
    try {
        $testFile = Join-Path $workDir ".ci-write-test-$PID"
        "test" | Out-File -FilePath $testFile -ErrorAction Stop
        Remove-Item $testFile -Force
        Write-Success "  ✅ Write OK: $workDir"
    } catch {
        Write-Err "  ❌ Нет прав на запись в $workDir : ${_}"
    }
} else {
    Write-Warn "  ⚠️ Work directory не существует: $workDir"
    Write-Warn "    Будет создан автоматически при первом job."
}

# -----------------------------------------------------------------------
# 5. GitHub API connectivity
# -----------------------------------------------------------------------
Write-Step "5/6 GitHub API connectivity"

try {
    $apiUrl = "https://api.github.com/repos/$RepoOwner/$RepoName"
    $headers = @{
        "Accept"                = "application/vnd.github+json"
        "X-GitHub-Api-Version"  = "2022-11-28"
    }
    $response = Invoke-RestMethod -Method GET -Uri $apiUrl -Headers $headers -TimeoutSec 10
    Write-Success "  ✅ GitHub API: $RepoOwner/$RepoName доступен"
    Write-Host "    Stars: $($response.stargazers_count), Forks: $($response.forks_count)"
} catch {
    $code = $_.Exception.Response.StatusCode.value__
    if ($code -eq "Unauthorized") {
        Write-Warn "  ⚠️ GitHub API: 401 (rate limit, добавьте -GitHubToken для аутентификации)"
    } else {
        Write-Err "  ❌ GitHub API: $code — $_"
    }
}

# -----------------------------------------------------------------------
# 6. Workflow файлы
# -----------------------------------------------------------------------
Write-Step "6/6 Workflow файлы"

$repoRoot = (Get-Location).Path
if (-not (Test-Path ".github\workflows")) {
    # Попробуем подняться на уровень выше
    Set-Location ..
    $repoRoot = (Get-Location).Path
}

$workflowsDir = Join-Path $repoRoot ".github\workflows"
if (Test-Path $workflowsDir) {
    $workflows = Get-ChildItem $workflowsDir -Filter "*.yml" -ErrorAction SilentlyContinue
    if ($workflows) {
        foreach ($wf in $workflows) {
            Write-Success "  ✅ Workflow: $($wf.Name)"
        }
    } else {
        Write-Warn "  ⚠️ Нет workflow файлов в $workflowsDir"
        Write-Warn "    Создайте .github/workflows/ci.yml для запуска CI."
    }
} else {
    Write-Warn "  ⚠️ Директория workflows не найдена: $workflowsDir"
}

# -----------------------------------------------------------------------
# Итог
# -----------------------------------------------------------------------
Write-Step "Итог"
Write-Host "  ✅ OK:     $script:OkCount" -ForegroundColor Green
if ($script:WarnCount -gt 0) {
    Write-Host "  ⚠️  WARN:   $script:WarnCount" -ForegroundColor Yellow
}
if ($script:ErrCount -gt 0) {
    Write-Host "  ❌ ERRORS: $script:ErrCount" -ForegroundColor Red
}

if ($script:ErrCount -gt 0) {
    Write-Host ""
    Write-Host "❌ Найдены критические проблемы. Исправьте и перезапустите." -ForegroundColor Red
    exit 1
} elseif ($script:WarnCount -gt 0) {
    Write-Host ""
    Write-Host "⚠️ Есть предупреждения. CI может работать, но не оптимально." -ForegroundColor Yellow
    exit 0
} else {
    Write-Host ""
    Write-Host "✅ Всё ОК. Runner готов к работе." -ForegroundColor Green
    exit 0
}