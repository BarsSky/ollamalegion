#!/usr/bin/env pwsh
# =============================================================================
# Сборка Docker-образов OllamaLegion
# =============================================================================
# Использование:
#   .\scripts\build-containers.ps1                    # все образы
#   .\scripts\build-containers.ps1 -Balancer           # только balancer
#   .\scripts\build-containers.ps1 -WebUI              # только webui
#   .\scripts\build-containers.ps1 -Agent              # только agent cppworker
#   .\scripts\build-containers.ps1 -CppWorker          # только cppworker
#   .\scripts\build-containers.ps1 -CppWorker -CudaArch "86"   # только cppworker:gpu с тегом :gpu-86
#   .\scripts\build-containers.ps1 -CudaArch "all"    # все поддерживаемые архитектуры (тэг :gpu-arch_all)
#   .\scripts\build-containers.ps1 -Tag r83-submodule-v23      # собрать с РЕЛИЗНЫМ тегом (не latest)
#
# R83 (2026-09-28): ЛОКАЛЬНЫЙ РЕПОЗИТОРИЙ.
# Префикс берётся из `IMAGE_REGISTRY` в deployments/.env (или из окружения):
#   IMAGE_REGISTRY=                      → ollama-legion/balancer:latest
#   IMAGE_REGISTRY=local-docker-hub:5000/ → local-docker-hub:5000/ollama-legion/balancer:latest
# Скрипт добавляет слэш сам и превращает заглушки (local/docker.io) в пустой
# префикс, поэтому значение можно писать «как принято в вашем registry».
# Собранный образ получает ТО ЖЕ имя, которое ищет compose, — иначе `up -d`
# пошёл бы в registry за образом, только что собранным локально под другим именем.
# =============================================================================
param(
    [switch]$Balancer,
    [switch]$WebUI,
    [switch]$Agent,
    [switch]$CppWorker,
    [string]$CudaArch = $env:CUDA_ARCH,
    # Релизный тег. Пусто = `latest` (поведение по умолчанию).
    [string]$Tag = "",
    # Не добавлять префикс IMAGE_REGISTRY (нужно, если образы должны остаться
    # строго локальными даже при заданном registry).
    [switch]$NoRegistryPrefix
)

$ErrorActionPreference = "Stop"

# R83: префикс локального репозитория образов — та же переменная, что читает compose.
. (Join-Path $PSScriptRoot 'lib-image-registry.ps1')
$registryPrefix = ''
if (-not $NoRegistryPrefix) {
    $registryPrefix = Get-ImageRegistryPrefix -EnvDir (Join-Path $PSScriptRoot '..\deployments')
}

$imageTag = if ([string]::IsNullOrWhiteSpace($Tag)) { 'latest' } else { $Tag.Trim() }

# R83: версия/коммит/дата вшиваются в бинарники (pkg/version) — их печатает
# стартовая шапка логов. Значения те же, что в Dockerfile'ах по умолчанию.
$gitCommit = 'unknown'
$gitDescribe = 'unknown'
try {
    $sha = & git rev-parse --short HEAD 2>$null
    if ($LASTEXITCODE -eq 0 -and $sha) { $gitCommit = $sha.Trim() }
    $desc = & git describe --tags --always 2>$null
    if ($LASTEXITCODE -eq 0 -and $desc) { $gitDescribe = $desc.Trim() }
} catch { }
$buildDate = (Get-Date -Format 'yyyy-MM-dd')
$versionArgs = @('--build-arg', "GIT_COMMIT=$gitCommit", '--build-arg', "BUILD_DATE=$buildDate")

# Round 23 (2026-08-04): ВКЛЮЧАЕМ BuildKit глобально.
# Без этого:
#   - `--mount=type=cache,target=/root/.ccache` в docker/cppworker/Dockerfile.gpu
#     ИГНОРИРУЕТСЯ → ccache всегда пустой → каждый билд = полная перекомпиляция
#     CUDA (15-20 мин вместо 1-2 мин с инкрементной пересборкой).
#   - `--mount=type=cache,target=/go/pkg/mod` для Go modules тоже не работает.
#   - BuildKit cache mounts требуют `DOCKER_BUILDKIT=1` (или docker buildx).
#
# Безопасно для всех Dockerfile'ов: legacy builders без mount'ов работают как раньше,
# просто добавляется BuildKit-монтирование для ccache/Go modules.
$env:DOCKER_BUILDKIT = "1"

Push-Location $PSScriptRoot\..
try {
    $registryLabel = 'локальный (без префикса)'
    if ($registryPrefix) {
        $registryLabel = $registryPrefix.TrimEnd('/')
    }
    Write-Host "=== Репозиторий образов: $registryLabel | тег: $imageTag ===" -ForegroundColor Gray
    Write-Host "=== Версия сборки: commit=$gitCommit buildDate=$buildDate ===" -ForegroundColor Gray

    if (-not $Balancer -and -not $WebUI -and -not $Agent -and -not $CppWorker) {
        $Balancer = $true
        $WebUI = $true
        $Agent = $true
        $CppWorker = $true
    }

    if ($Balancer) {
        $balancerImage = Resolve-ImageRef -Prefix $registryPrefix -Name 'ollama-legion/balancer' -Tag $imageTag
        Write-Host "=== Building $balancerImage ===" -ForegroundColor Cyan
        docker build @versionArgs -t $balancerImage -f docker/balancer/Dockerfile .
        if ($LASTEXITCODE -ne 0) { throw "Balancer build failed" }
    }

    if ($WebUI) {
        Write-Host "=== Building webui ===" -ForegroundColor Cyan
        # Sprint 30 (2026-07-30): пробрасываем VERSION / GIT_COMMIT / BUILD_DATE
        # в WebUI build как --build-arg. entrypoint.sh инжектит их в config.js,
        # JS в index.html показывает версию в sidebar footer (id=appVersion).
        $webuiVersion = $gitDescribe
        $webuiBuildDate = (Get-Date -Format 'yyyy-MM-ddTHH:mm:ssZ')
        Write-Host "  version=$webuiVersion commit=$gitCommit" -ForegroundColor Gray
        # R83: имя образа при сборке через compose задаётся в самом compose, поэтому
        # здесь передаём его явно — иначе при заданном IMAGE_REGISTRY тег внутри
        # compose и фактически собранный разойдутся.
        $webuiImage = Resolve-ImageRef -Prefix $registryPrefix -Name 'ollama-legion/webui' -Tag $imageTag
        docker build `
            --build-arg "VERSION=$webuiVersion" `
            --build-arg "GIT_COMMIT=$gitCommit" `
            --build-arg "BUILD_DATE=$webuiBuildDate" `
            -t $webuiImage -f docker/webui/Dockerfile .
        if ($LASTEXITCODE -ne 0) { throw "WebUI build failed" }
    }

    if ($Agent) {
        $agentImage = Resolve-ImageRef -Prefix $registryPrefix -Name 'ollama-legion/agent' -Tag $imageTag
        Write-Host "=== Building $agentImage ===" -ForegroundColor Cyan
        docker build @versionArgs --build-arg ENABLE_NVML=true -t $agentImage -f docker/agent/Dockerfile .
        if ($LASTEXITCODE -ne 0) { throw "Agent build failed" }
    }

    if ($CppWorker) {
        # Определяем архитектуру CUDA. Пустая/не задана = "all" (по умолчанию в Dockerfile).
        if ([string]::IsNullOrWhiteSpace($CudaArch)) {
            $CudaArch = "all"
        }

        # Нормализуем значение для тега:
        #   "all"        -> arch_all
        #   "75;86;89"   -> arch_75-86-89
        #   "86"         -> 86
        #   "ALL"        -> arch_all
        $tagArchSegment = ""
        $lowerArch = $CudaArch.Trim().ToLower()
        if ($lowerArch -in @("all", "*", "any", "native", "arch_all")) {
            $tagArchSegment = "arch_all"
        } else {
            # заменяем разделители ; , пробел на дефис
            $normalized = ($CudaArch -replace '[;,\s]+', '-').Trim('-')
            if ([string]::IsNullOrEmpty($normalized)) {
                $tagArchSegment = "arch_all"
            } else {
                $tagArchSegment = $normalized
            }
        }

        # Тег cppworker: релизный, если задан -Tag, иначе архитектурный (как раньше).
        $gpuTag = if ($imageTag -ne 'latest') { "gpu-$imageTag" } else { "gpu-$tagArchSegment" }
        $cpuTag = if ($imageTag -ne 'latest') { $imageTag } else { 'cpu' }
        $gpuImage = Resolve-ImageRef -Prefix $registryPrefix -Name 'ollama-legion/cppworker' -Tag $gpuTag
        $cpuImage = Resolve-ImageRef -Prefix $registryPrefix -Name 'ollama-legion/cppworker' -Tag $cpuTag

        Write-Host "=== Building $gpuImage (CUDA_ARCH=$CudaArch) ===" -ForegroundColor Cyan
        $gpuBuildArgs = @()
        if ($lowerArch -ne "arch_all") {
            # передаём --build-arg только если архитектура не "all" (иначе используем дефолт в Dockerfile)
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "CUDA_ARCH=$CudaArch"
        }
        # Round 14a follow-up (2026-07-29): для одиночной архитектуры (напр. "86")
        # используем Dockerfile.gpu.<arch> — у него более полный COPY в go-builder
        # stage и короче компиляция (только один CUDA arch). Для "all" или
        # нескольких arch — Dockerfile.gpu (универсальный).
        $dockerfile = "docker/cppworker/Dockerfile.gpu"
        if ($lowerArch -notmatch "all" -and $lowerArch -notmatch "-" -and $lowerArch -notmatch ";") {
            $archSpecific = "docker/cppworker/Dockerfile.gpu.$CudaArch"
            if (Test-Path $archSpecific) {
                $dockerfile = $archSpecific
                Write-Host "Using arch-specific Dockerfile: $dockerfile" -ForegroundColor Yellow
            }
        }
        & docker build @gpuBuildArgs @versionArgs -t $gpuImage -f $dockerfile --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker GPU build failed" }

        Write-Host "=== Building $cpuImage ===" -ForegroundColor Cyan
        docker build @versionArgs -t $cpuImage -f docker/cppworker/Dockerfile.cpu --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker CPU build failed" }
    }

    Write-Host "`n=== All builds completed ===" -ForegroundColor Green
}
finally {
    Pop-Location
}
