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
    # Версия CUDA в базовом образе cppworker (Dockerfile.gpu). Пусто = дефолт
    # Dockerfile (12.2.0). Для Blackwell (sm_120) нужна 12.8.0 и новее: в 12.2
    # компилятор не знает такую архитектуру и сборка падает на
    # `Unsupported gpu architecture 'compute_120'`.
    [string]$CudaVersion = $env:CUDA_VERSION,
    # Реестр базового образа CUDA. Умолчание Dockerfile (timeweb) отдаёт
    # devel-слой ~110 КБ/с; mirror.gcr.io/nvidia/cuda тянет его за секунды.
    [string]$CudaBase = $env:CUDA_BASE,
    # Релизный тег. Пусто = `latest` (поведение по умолчанию).
    [string]$Tag = "",
    # R154 (2026-10-16): откуда брать дистрибутив Go для стадии go-builder.
    # ЗАЧЕМ. Живой случай: сборка дважды падала на шаге
    #   wget ${GO_DOWNLOAD_BASE}/go${GO_VERSION}.linux-amd64.tar.gz
    # с exit code 4 (сеть), при том что apt-get на том же шаге ПРОШЁЛ. Дефолт
    # Dockerfile (go.dev/dl) из этой сети недоступен; в самом Dockerfile для таких
    # сетей указано зеркало mirrors.aliyun.com/golang. Пусто = дефолт Dockerfile.
    [string]$GoDownloadBase = $env:GO_DOWNLOAD_BASE,
    # Не добавлять префикс IMAGE_REGISTRY (нужно, если образы должны остаться
    # строго локальными даже при заданном registry).
    [switch]$NoRegistryPrefix,
    # R157: не собирать CPU-вариант cppworker (стенду он не нужен,
    # а именно эта стадия уронила сборку R154 внутренней ошибкой компилятора).
    [switch]$SkipCpu,
    # R148 (2026-10-16): самопроверка проверки CUDA-арок БЕЗ сборки.
    #
    # ЗАЧЕМ. Проверка `Assert-CudaArchBuildable` появилась после живого случая:
    # `-CudaArch "86;120"` на тулчейне 12.2 падал через минуты сборки строкой
    # `nvcc fatal : Unsupported gpu architecture 'compute_120'`. Сама проверка
    # обязана быть проверяемой без 40-минутной сборки — как это сделано у
    # scripts/upgrade-donor-node.ps1 -SelfTest.
    [switch]$SelfTest
)

# ВАЖНО (R85, 2026-10-06): docker пишет ПРОГРЕСС сборки в stderr, и при
# $ErrorActionPreference='Stop' PowerShell превращает первую же строку прогресса
# в NativeCommandError — сборка обрывалась на «#0 building with "desktop-linux"…»
# сразу после старта. Ошибки docker ловим по факту: проверяем $LASTEXITCODE после
# каждого build (он и есть источник истины).
$ErrorActionPreference = "Continue"

# R83: префикс локального репозитория образов — та же переменная, что читает compose.
. (Join-Path $PSScriptRoot 'lib-image-registry.ps1')
$registryPrefix = ''
if (-not $NoRegistryPrefix) {
    $registryPrefix = Get-ImageRegistryPrefix -EnvDir (Join-Path $PSScriptRoot '..\deployments')
}

$imageTag = if ([string]::IsNullOrWhiteSpace($Tag)) { 'latest' } else { $Tag.Trim() }

# R148: самопроверка проверки CUDA-арок — БЕЗ сборки и БЕЗ docker.
# Каждый случай ниже — либо живой дефект, либо правило, которое обязано держаться.
function Assert-CudaArchBuildable {
    param(
        [string]$ArchList,
        [string]$ToolkitVersion
    )

    $toolkit = if ([string]::IsNullOrWhiteSpace($ToolkitVersion)) { '12.2.0' } else { $ToolkitVersion.Trim() }
    $toolkitVer = $null
    try { $toolkitVer = [version]($toolkit -replace '[^0-9\.].*$', '') } catch { }

    $normalized = $ArchList.Trim().ToLower()
    if ($normalized -in @('all', '*', 'any', 'native', 'arch_all')) {
        Write-Host "  ВНИМАНИЕ: CUDA_ARCH=all — это 50;61;70;75;80;86;89;90, БЕЗ sm_120." -ForegroundColor Yellow
        Write-Host "  На RTX 50xx (Blackwell) такой образ упадёт на первом GPU-ядре." -ForegroundColor Yellow
        Write-Host "  Для Blackwell: -CudaArch `"86;120`" -CudaVersion `"12.8.0`"" -ForegroundColor Yellow
        return
    }

    # Минимальная версия тулчейна для архитектуры. В таблице — только то, что
    # подтверждено живыми сборками; остальные считаем совместимыми с 11.x.
    $minFor = @{
        '120' = '12.8'; '100' = '12.8'; '101' = '12.8'; '103' = '12.8'
        '90'  = '11.8'; '89' = '11.8'; '87' = '11.4'
    }
    $arches = @(($ArchList -replace '[;,\s]+', ';').Trim(';').Split(';') |
        Where-Object { -not [string]::IsNullOrWhiteSpace($_) } |
        ForEach-Object { $_.Trim().ToLower() -replace '^sm_', '' })

    foreach ($a in $arches) {
        if (-not $minFor.ContainsKey($a)) { continue }
        $need = [version]$minFor[$a]
        if ($toolkitVer -ne $null -and $toolkitVer -lt $need) {
            throw ("CUDA_ARCH=$a требует CUDA $need или новее, а выбран тулчейн $toolkit. " +
                "nvcc упадёт с «Unsupported gpu architecture 'compute_$a'» через несколько минут сборки. " +
                "Пересоберите с -CudaVersion `"$need.0`" (или поправьте CUDA_VERSION в deployments/.env).")
        }
    }

    Write-Host "  CUDA_ARCH=$ArchList совместим с тулчейном $toolkit" -ForegroundColor DarkGray
    Write-Host ("  Образ будет работать только на картах: sm_" + ($arches -join ', sm_') +
        " — на любой другой карте первое GPU-ядро убьёт процесс") -ForegroundColor DarkGray
}

if ($SelfTest) {
    Write-Host '=== SelfTest: Assert-CudaArchBuildable ===' -ForegroundColor Cyan
    $failed = 0
    function Expect-Throw([string]$Arch, [string]$Cuda, [string]$Why) {
        try {
            Assert-CudaArchBuildable -ArchList $Arch -ToolkitVersion $Cuda | Out-Null
            Write-Host ("  FAIL  {0} + CUDA {1}: исключения не было ({2})" -f $Arch, $Cuda, $Why) -ForegroundColor Red
            $script:failed++
        } catch {
            Write-Host ("  PASS  {0} + CUDA {1}: отказ с объяснением" -f $Arch, $Cuda) -ForegroundColor Green
        }
    }
    function Expect-Pass([string]$Arch, [string]$Cuda, [string]$Why) {
        try {
            Assert-CudaArchBuildable -ArchList $Arch -ToolkitVersion $Cuda | Out-Null
            Write-Host ("  PASS  {0} + CUDA {1}: совместимо ({2})" -f $Arch, $Cuda, $Why) -ForegroundColor Green
        } catch {
            Write-Host ("  FAIL  {0} + CUDA {1}: {2}" -f $Arch, $Cuda, $_.Exception.Message) -ForegroundColor Red
            $script:failed++
        }
    }

    # Живой случай: sm_120 требует CUDA 12.8, на 12.2 nvcc не знает архитектуру.
    Expect-Throw '86;120' '12.2.0' 'sm_120 появился в CUDA 12.8'
    Expect-Pass  '86;120' '12.8.0' 'Blackwell на 12.8 собирается'
    Expect-Pass  '75;86'  '12.2.0' 'старые архитектуры на 12.2 собираются'
    Expect-Throw '90'     '11.7'   'sm_90 требует 11.8'
    Expect-Pass  '75;86;120' '12.8.0' 'объединённый образ: все три машины одним образом'
    # all не включает sm_120 — проверка обязана предупредить, но не падать.
    Expect-Pass  'all'    '12.2.0' 'all = без sm_120, но это предупреждение, а не отказ'

    Write-Host ''
    if ($failed -gt 0) { Write-Host ("ИТОГ SelfTest: провалов " + $failed) -ForegroundColor Red; exit 1 }
    Write-Host 'ИТОГ SelfTest: все случаи верны' -ForegroundColor Green
    exit 0
}

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

# R135 (2026-10-16): совместимость запрошенных CUDA-архитектур с тулчейном.
#
# ЗАЧЕМ ДО СБОРКИ. Живой случай на стенде: `-CudaArch "86;120"` без
# `-CudaVersion` берёт дефолт Dockerfile (12.2.0), а sm_120 (Blackwell,
# RTX 50xx) поддерживается только с CUDA 12.8. nvcc падает через несколько
# минут сборки строкой
#     nvcc fatal : Unsupported gpu architecture 'compute_120'
# внутри многокилобайтного вывода, а оператор видит «CppWorker GPU build failed».
# Проверка называет причину и точную команду сразу, до компиляции.
#
# ВТОРОЕ, ЧТО ПРОВЕРЯЕМ. `all` НЕ включает sm_120: список в Dockerfile.gpu —
# 50;61;70;75;80;86;89;90. Образ, собранный с `all`, на RTX 50xx загрузит модель
# и упадёт на первом ядре с «CUDA error: no kernel image is available for
# execution on the device» — то самое падение, от которого страхует рантайм-гард
# (internal/cppbackend/cuda_arch_guard.go). Предупреждаем здесь, а не после
# SIGABRT.

    if ($CppWorker) {
        # Определяем архитектуру CUDA. Пустая/не задана = "all" (по умолчанию в Dockerfile).
        if ([string]::IsNullOrWhiteSpace($CudaArch)) {
            $CudaArch = "all"
        }

        # R135: падать сразу и с объяснением, а не через nvcc посреди сборки.
        Assert-CudaArchBuildable -ArchList $CudaArch -ToolkitVersion $CudaVersion

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

        # R167 (2026-10-17): КОММИТ llama.cpp ПЕЧЁМ В ОБРАЗ.
        #
        # ЗАЧЕМ. Раньше образ собирался с LLAMA_BUILD=unknown: значение приходило
        # только через compose (environment), то есть лежало в .env и не было
        # свойством образа. Из-за этого сверка «что собрано <-> что заявлено»
        # сравнивала env с env и всегда соглашалась (на стенде так и разошлись:
        # донор отдавал «тот же коммит», а бинарник был старше).
        # Теперь факт берётся из сабмодуля и печётся в ENV CPPWORKER_LLAMA_BUILD_BAKED,
        # который compose НЕ переопределяет.
        $llamaCommit = (& git -C c/llama.cpp rev-parse --short HEAD 2>$null)
        if ($LASTEXITCODE -eq 0 -and -not [string]::IsNullOrWhiteSpace($llamaCommit)) {
            $llamaCommit = $llamaCommit.Trim()
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "LLAMA_BUILD=$llamaCommit"
            Write-Host "  llama.cpp commit: $llamaCommit" -ForegroundColor DarkGray
        } else {
            Write-Host "  ВНИМАНИЕ: не удалось прочитать коммит c/llama.cpp — образ соберётся с LLAMA_BUILD=unknown" -ForegroundColor Yellow
        }
        # Версия и реестр CUDA — только когда заданы явно: дефолты Dockerfile
        # остаются дефолтами (иначе прежние сборки под sm_86 поехали бы на
        # другую базовую версию молча).
        if (-not [string]::IsNullOrWhiteSpace($CudaVersion)) {
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "CUDA_VERSION=$($CudaVersion.Trim())"
        }
        if (-not [string]::IsNullOrWhiteSpace($CudaBase)) {
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "CUDA_BASE=$($CudaBase.Trim())"
        }
        # R154: зеркало дистрибутива Go (см. параметр -GoDownloadBase выше).
        # Без него сборка дважды падала на wget go.tar.gz (exit 4), когда дефолтное
        # go.dev/dl из этой сети недоступно — при полностью прошедшем apt-get.
        if (-not [string]::IsNullOrWhiteSpace($GoDownloadBase)) {
            $gpuBuildArgs += "--build-arg"
            $gpuBuildArgs += "GO_DOWNLOAD_BASE=$($GoDownloadBase.Trim())"
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

        # R157 (2026-10-16): CPU-вариант собирается отдельной стадией и на стенде не
        # используется — а именно он уронил сборку R154 внутренней ошибкой компилятора
        # (Segmentation fault в nlohmann/json.hpp, exit code 2) уже ПОСЛЕ успешной
        # сборки GPU-образа. Переключатель позволяет собрать только нужное и не
        # тратить время на чужой сбой.
        if ($SkipCpu) {
            Write-Host "=== CPU-вариант пропущен (-SkipCpu) ===" -ForegroundColor DarkGray
        } else {
        Write-Host "=== Building $cpuImage ===" -ForegroundColor Cyan
        docker build @versionArgs -t $cpuImage -f docker/cppworker/Dockerfile.cpu --target runtime .
        if ($LASTEXITCODE -ne 0) { throw "CppWorker CPU build failed" }
        }
    }

    Write-Host "`n=== All builds completed ===" -ForegroundColor Green
}
finally {
    Pop-Location
}
