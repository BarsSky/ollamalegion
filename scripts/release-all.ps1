<#
.SYNOPSIS
  R83 (2026-09-25): единый выпуск образов продового стека (bundled-with-agent).

.DESCRIPTION
  ЗАЧЕМ ЭТОТ СКРИПТ.

  До R83 выпуск состоял из отдельных скриптов (rebuild_balancer_r66a.ps1,
  rebuild_cppworker_r66a.ps1, rebuild_webui_r66d.ps1, deploy-agent-docker.ps1),
  и каждый обновлял версию ОПТУДЬ по-своему:

    - balancer  — переписывал строку `image:` в compose регуляркой;
    - cppworker — писал CPPWORKER_GPU_TAG в .env и .env.bundled-with-agent;
    - webui     — тоже переписывал `image:` в compose;
    - agent     — собирался через `docker compose --build` (тег `latest`).

  Три разных способа означали, что «выпустить всё согласованно» было нечем:
  пересобрали один компонент — три остальных остались на прежних версиях. Это
  ровно тот класс рассинхрона, который ловился в WebUI (?v=R66d рядом с ?v=R77
  в одной странице).

  ТЕПЕРЬ (путь 1+2+3 из плана):
    1. теги всех четырёх образов вынесены в переменные и читаются из
       deployments/.env (`${VAR:-default}` в compose) — compose при выпуске
       НЕ правится;
    2. каждая сборка дополнительно получает «последний» тег-алиас: у balancer/
       agent/webui — `latest` (у них нет взаимоисключающих вариантов), у
       cppworker — `latest-gpu-<arch>`, потому что cpu/stub/gpu-* делят один
       репозиторий и plain `latest` перезаписывался бы последней сборкой;
    3. ЭТОТ скрипт собирает все четыре, пишет теги ОДНИМ заходом и фиксирует
       манифест (тег + image id) — так «что раскатано» перестаёт быть знанием
       одного человека.

.PARAMETER Tag
  Тег выпуска. По умолчанию — r<ГГММДД-ЧЧММ>. Пустой тег не допускается:
  выпуск без версии невозможно ни откатить, ни отличить от предыдущего.

.PARAMETER Services
  Подмножество компонентов: balancer, cppworker, agent, webui (по умолчанию все).

.PARAMETER CudaArch
  CUDA_ARCH для cppworker (по умолчанию 86 = A10/RTX 30xx). Для H100/A100 менять.

.PARAMETER SkipBuild
  Только зафиксировать теги/манифест и пересоздать сервисы (без docker build).

.PARAMETER NoDeploy
  Не делать `docker compose up` — только собрать, протегировать и записать теги.

.PARAMETER NoAlias
  Не вешать «последний» тег-алиас.

.PARAMETER ComposeFile
  Какой compose-файл использовать для раскатки (по умолчанию — рабочий стенд:
  docker-compose.stack.yml). Именно он управляет ТЕКУЩИМ стендом (`ol-stack-*`);
  docker-compose.cppworker-bundled-with-agent.yml — отдельный проект
  (`ol-bundled-*`), который делит те же порты и падает с
  «Bind for 0.0.0.0:18080 failed: port is already allocated».

.PARAMETER ComposeProfile
  Профиль compose для раскатки (по умолчанию full: loadbalancer + webui +
  cppworker-gpu + agent).

.EXAMPLE
  pwsh -File scripts/release-all.ps1 -Tag r83-submodule-v1
  pwsh -File scripts/release-all.ps1 -Tag r83 -Services webui,balancer
  pwsh -File scripts/release-all.ps1 -Tag r83 -SkipBuild -NoDeploy   # только перепин
#>
param(
    [string]$Tag = "",
    [string[]]$Services = @("balancer", "cppworker", "agent", "webui"),
    [int]$CudaArch = 86,
    # Реестр базового образа CUDA для cppworker (ARG CUDA_BASE в Dockerfile.gpu).
    # Оставлено настраиваемым намеренно: зеркало по умолчанию (timeweb) отдаёт
    # слой devel-образа 2.34 GB со скоростью ~110 KB/s, и сборка висит часами.
    # Быстрый обход: -CudaBase mirror.gcr.io/nvidia/cuda (проверено: 77 MB/s).
    [string]$CudaBase = "",
    [switch]$SkipBuild,
    [switch]$NoDeploy,
    [switch]$NoAlias,
    # R83-политика (2026-10-01): раскатка — ОДНИМ рабочим способом, тем же
    # compose, которым живёт стенд. Раньше по умолчанию брался
    # docker-compose.cppworker-bundled-with-agent.yml (проект ol-bundled-*),
    # который делит порты с рабочим стендом ol-stack-* и падал на 18080.
    [string]$ComposeFile = "docker-compose.stack.yml",
    [string]$ComposeProfile = "full"
)

$ErrorActionPreference = "Continue"
$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location $repoRoot

# Общие операции с тегами — тот же код, что используют rebuild_*.ps1
# (единая реализация вместо трёх разных способов).
. (Join-Path $PSScriptRoot 'lib-image-tags.ps1')

# R83 (2026-09-28): локальный репозиторий образов — та же переменная, что читает
# compose (deployments/.env → IMAGE_REGISTRY). Пусто = стандартный путь.
. (Join-Path $PSScriptRoot 'lib-image-registry.ps1')
$registryPrefix = Get-ImageRegistryPrefix -EnvDir (Join-Path $PSScriptRoot '..\deployments')

if ([string]::IsNullOrWhiteSpace($Tag)) {
    $Tag = "r" + (Get-Date -Format 'yyMMdd-HHmm')
}
if ($Tag -notmatch '^[A-Za-z0-9._-]+$') {
    throw "Недопустимый тег '$Tag': разрешены только буквы, цифры, точка, подчёркивание и дефис"
}

# R83-политика (2026-10-01): файл compose и профиль приходят из параметров.
# По умолчанию — рабочий стенд (docker-compose.stack.yml --profile full), потому
# что «единый рабочий метод» = тот же compose, которым стенд живёт.
$envDir = Join-Path $repoRoot "deployments"
$composePath = Join-Path $envDir $ComposeFile
$envPath = Join-Path $envDir ".env"
$envContainerPath = Join-Path $envDir ".env.bundled-with-agent"
$manifestPath = Join-Path $envDir "release-manifest.json"

# Компонент → (переменная в deployments/.env, image repo, полный тег для образа)
# ВНИМАНИЕ: для cppworker compose добавляет префикс `gpu-`, поэтому переменная
# хранит тег БЕЗ него (совпадает с прежним поведением rebuild_cppworker_*.ps1).
$spec = @{
    balancer  = @{ Var = "BALANCER_TAG";        Repo = "ollama-legion/balancer";  ImageTag = $Tag;                 Alias = "latest" }
    cppworker = @{ Var = "CPPWORKER_GPU_TAG";   Repo = "ollama-legion/cppworker"; ImageTag = "gpu-$Tag";           Alias = "latest-gpu-$CudaArch" }
    agent     = @{ Var = "AGENT_TAG";           Repo = "ollama-legion/agent";     ImageTag = $Tag;                 Alias = "latest" }
    webui     = @{ Var = "WEBUI_TAG";           Repo = "ollama-legion/webui";     ImageTag = $Tag;                 Alias = "latest" }
}

foreach ($svc in $Services) {
    if (-not $spec.ContainsKey($svc)) {
        throw "Неизвестный компонент '$svc'. Доступны: $($spec.Keys -join ', ')"
    }
}

Write-Host ""
Write-Host "======================================================================" -ForegroundColor Cyan
Write-Host "  R83 release: тег '$Tag', компоненты: $($Services -join ', ')" -ForegroundColor Cyan
Write-Host "======================================================================" -ForegroundColor Cyan
Write-Host ""

# Проверяем, что все четыре переменные вообще есть в .env: скрипты пишут тег
# заменой строки `VAR=...`, поэтому отсутствие строки = молчаливый no-op
# (переменная не выставится, compose возьмёт default — и выпуск «не доедет»).
$envLines = Get-Content $envPath
foreach ($svc in @("balancer", "cppworker", "agent", "webui")) {
    $varName = $spec[$svc].Var
    if (-not ($envLines | Where-Object { $_ -match "^$varName=" })) {
        throw "В deployments/.env нет строки '$varName='. Добавьте её (см. комментарий R83 в .env) — иначе тег не запишется."
    }
}

$results = @()

foreach ($svc in $Services) {
    $s = $spec[$svc]
    # R83: имя образа собирается с префиксом репозитория из IMAGE_REGISTRY —
    # ровно так же, как его ищет compose. Иначе `up -d` ушёл бы в registry за
    # образом, который только что собран локально под другим именем.
    $imageRef = Resolve-ImageRef -Prefix $registryPrefix -Name $s.Repo -Tag $s.ImageTag

    if (-not $SkipBuild) {
        Write-Host "[$svc] build $imageRef ..." -ForegroundColor Cyan
        switch ($svc) {
            "balancer" {
                docker build -t $imageRef -f docker/balancer/Dockerfile .
            }
            "cppworker" {
                # CUDA_ARCH — только нужная архитектура: default "all" в Dockerfile.gpu
                # собирает 9 архитектур (~9x дольше) и нужен лишь для multi-arch.
                # CUDA_BASE — реестр базового образа; пусто = дефолт Dockerfile.
                if ($CudaBase -ne "") {
                    docker build --build-arg CUDA_ARCH=$CudaArch --build-arg CUDA_BASE=$CudaBase -t $imageRef -f docker/cppworker/Dockerfile.gpu .
                } else {
                    docker build --build-arg CUDA_ARCH=$CudaArch -t $imageRef -f docker/cppworker/Dockerfile.gpu .
                }
            }
            "agent" {
                # Продовый стек использует GPU-агента (nvidia runtime) → target agent-gpu.
                docker build -t $imageRef -f docker/agent/Dockerfile --target agent-gpu --build-arg ENABLE_NVML=true .
            }
            "webui" {
                # VERSION попадает и в config.js (window.WEBUI_CONFIG.VERSION), и —
                # благодаря R83 — в токен cache-busting `?v=` внутри образа, поэтому
                # HTML при выпуске править не нужно.
                docker build --build-arg VERSION=$Tag -t $imageRef -f docker/webui/Dockerfile .
            }
        }
        if ($LASTEXITCODE -ne 0) {
            throw "[$svc] docker build failed (exit $LASTEXITCODE) — .env НЕ изменён, ничего не раскатано"
        }
    }

    if (-not $NoAlias) {
        # «Последний» тег-алиас — через общий helper (для cppworker он вариантный,
        # см. lib-image-tags.ps1: plain `latest` перезаписывается сборкой другого
        # варианта, потому что cpu/stub/gpu-* делят один репозиторий).
        Add-ImageAlias -ImageRef $imageRef -AliasRef (Resolve-ImageRef -Prefix $registryPrefix -Name $s.Repo -Tag $s.Alias)
    }

    $imgID = Get-ImageID -ImageRef $imageRef
    $results += [pscustomobject]@{
        service  = $svc
        variable = $s.Var
        image    = $imageRef
        imageTag = $s.ImageTag
        imageId  = $imgID
        alias    = if ($NoAlias) { "" } else { Resolve-ImageRef -Prefix $registryPrefix -Name $s.Repo -Tag $s.Alias }
    }
}

# --- фиксируем теги в deployments/.env -------------------------------------
# Это ЕДИНСТВЕННОЕ место, где хранится «что раскатано»: compose интерполирует
# ${VAR:-default} именно из .env (env_file на подстановку не влияет).
Write-Host ""
foreach ($r in $results) {
    # R83: compose сам добавляет префикс `gpu-` к CPPWORKER_GPU_TAG
# (`image: ollama-legion/cppworker:gpu-${CPPWORKER_GPU_TAG:-...}`), поэтому в .env
# пишем тег БЕЗ него — иначе получается образ `gpu-gpu-<tag>` и расходится с
# манифестом/check-image-tags.ps1.
Set-ImageTagVar -VariableName $r.variable -Value ($r.imageTag -replace '^gpu-', '') -EnvDir $envDir
}

# --- деплой ----------------------------------------------------------------
if (-not $NoDeploy) {
    Push-Location $envDir
    try {
        Write-Host ""
        Write-Host "docker compose -f $ComposeFile --profile $ComposeProfile up -d (пересоздание изменённых сервисов) ..." -ForegroundColor Cyan
        docker compose -f $ComposeFile --profile $ComposeProfile up -d
        if ($LASTEXITCODE -ne 0) { throw "docker compose up failed (exit $LASTEXITCODE)" }
    } finally {
        Pop-Location
    }
}

# --- манифест --------------------------------------------------------------
# Зачем: тег в .env отвечает на вопрос «что должно быть», манифест — «чем это
# собрано». Вместе они дают воспроизводимость и откат даже после перезаписи тега
# (образ остаётся в локальном демоне по image id).
$gitCommit = (git rev-parse --short HEAD 2>$null | Select-Object -First 1)
$manifest = [ordered]@{
    tag = $Tag
    builtAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
    gitCommit = $gitCommit
    cudaArch = $CudaArch
    composeFile = $ComposeFile
    composeProfile = $ComposeProfile
    images = $results
}
$manifest | ConvertTo-Json -Depth 5 | Set-Content -Path $manifestPath
Write-Host "[release] манифест: $manifestPath" -ForegroundColor Cyan

Write-Host ""
Write-Host "======================================================================" -ForegroundColor Green
Write-Host "  Готово: тег $Tag" -ForegroundColor Green
Write-Host "======================================================================" -ForegroundColor Green
$results | Format-Table service, variable, image, imageId -AutoSize

Write-Host "Проверка согласованности compose и .env:"
Write-Host "  pwsh -File scripts/check-image-tags.ps1" -ForegroundColor DarkGray
Write-Host "Откат: верните нужные теги в deployments/.env и выполните"
Write-Host "  docker compose -f deployments/$ComposeFile --profile $ComposeProfile up -d" -ForegroundColor DarkGray
