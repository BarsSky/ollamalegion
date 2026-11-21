<#
.SYNOPSIS
  R83 (2026-09-25): проверка согласованности тегов образов в продовом compose.

.DESCRIPTION
  Проверяет БЕЗ docker (можно запускать где угодно, в т.ч. на CI):

    1. каждый `image:` в docker-compose.stack.yml
       параметризован через `${VAR:-default}` (а не литералом);
    2. для каждой найденной переменной есть строка `VAR=...` в deployments/.env
       (иначе compose возьмёт default, и выпуск «не доедет»);
    3. если есть deployments/release-manifest.json — фактически подставляемый
       тег совпадает с тегом из манифеста для каждого компонента.

  ЗАЧЕМ. До R83 теги жили в трёх разных местах и обновлялись тремя разными
  способами, поэтому «пересобрали один компонент, три остались на старых
  версиях» было штатной ситуацией. Пункт 3 ловит именно её: подняли webui, но
  забыли balancer → манифест и .env расходятся → проверка падает.

  R675 (2026-11-21): дефолтный compose — docker-compose.stack.yml.

  ЖИВОЙ ДЕФЕКТ. Дефолтом стоял docker-compose.cppworker-bundled-with-agent.yml,
  а РЕЛИЗ идёт по docker-compose.stack.yml (release-all.ps1:104,
  deployments/release-manifest.json:6). Наборы сервисов разные: в bundled-файле
  ОТСУТСТВУЕТ imageworker (4 `image:` против 5), поэтому «проверено» относилось
  не к тому файлу, которым выпускают, и расхождение тега imageworker не ловилось
  никогда. Проверка, которая смотрит не на тот файл, опаснее отсутствующей: она
  даёт зелёный свет.

.EXAMPLE
  pwsh -File scripts/check-image-tags.ps1
  pwsh -File scripts/check-image-tags.ps1 -Verbose
  pwsh -File scripts/check-image-tags.ps1 -ComposeFile docker-compose.cppworker-bundled-with-agent.yml
#>
param(
    [string]$ComposeFile = "docker-compose.stack.yml",
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

# --- R486.3: манифест обязан нести идентичность КАЖДОГО образа --------------
#
# ЗАЧЕМ. До этой проверки манифест фиксировал только `image`+`imageId`, поэтому
# вопрос «а какой коммит внутри вот этого образа» он не отвечал: у webui и
# imageworker штамп отличается от балансера (их собрали позже, из другого
# коммита), и это НЕ рассинхрон — но и не было видно, что это так. Ровно класс
# «утверждение без доказательства»: тег записан, содержимое не зафиксировано.
#
# Контракты:
#   C1. у каждого образа в манифесте есть commit (7-40 hex) — свой, не общий;
#   C2. у каждого есть buildDate (ISO-8601) и imageId (sha256:…);
#   C3. у cppworker дополнительно llamaCommit, llamaDate, rpcProto;
#   C4. верхнеуровневый gitCommit есть и совпадает с commit записи балансера;
#   C5. записи РАЗНЫХ образов могут нести разные commit (сборка в разное время) —
#       но тогда это должно быть видно, то есть C1 обязателен для всех.
$manifestIssues = New-Object System.Collections.Generic.List[string]
if (Test-Path $manifestPath) {
    $mf = Get-Content $manifestPath -Raw | ConvertFrom-Json
    $balancerCommit = ""
    foreach ($img in $mf.images) {
        $label = [string]$img.image
        $commit = [string]$img.commit
        if ($commit -notmatch '^[0-9a-f]{7,40}$') {
            $manifestIssues.Add("C1: $label без per-image commit (нужен короткий или полный SHA сборки именно этого образа)")
        }
        if ([string]$img.buildDate -notmatch '^\d{4}-\d{2}-\d{2}T') {
            $manifestIssues.Add("C2: $label без buildDate (ISO-8601) — дата сборки образа не зафиксирована")
        }
        if ([string]$img.imageId -notmatch '^sha256:[0-9a-f]{64}$') {
            $manifestIssues.Add("C2b: $label без корректного imageId (sha256:…)")
        }
        if ($label -like '*cppworker*') {
            if ([string]$img.llamaCommit -notmatch '^[0-9a-f]{7,40}$') {
                $manifestIssues.Add("C3: $label без llamaCommit — неизвестно, какой llama.cpp слинкован")
            }
            if ([string]$img.llamaDate -notmatch '^\d{4}-\d{2}-\d{2}$') {
                $manifestIssues.Add("C3b: $label без llamaDate (ГГГГ-ММ-ДД)")
            }
            if ([string]$img.rpcProto -notmatch '^\d+\.\d+\.\d+$') {
                $manifestIssues.Add("C3c: $label без rpcProto (X.Y.Z из ggml-rpc.h)")
            }
        }
        if ($label -like '*balancer*') {
            $balancerCommit = $commit
        }
    }
    if ([string]$mf.gitCommit -notmatch '^[0-9a-f]{7,40}$') {
        $manifestIssues.Add("C4: верхнеуровневый gitCommit отсутствует или не SHA")
    } else {
        # C4b — «какая сборка считается выпуском».
        #
        # R493-арх (2026-11-12): правило было «gitCommit == коммит записи
        # БАЛАНСЕРА», и оно верно только для полного выпуска. Живой случай: выпуск
        # ТОЛЬКО воркера (обновление CUDA-архитектур) — gitCommit честно равен
        # коммиту собранного воркера, а балансер остался на прежнем образе, и
        # проверка падала на ровном месте трижды за день.
        #
        # Теперь: если манифест сообщает, ЧТО собиралось (releasedServices), сверяем
        # gitCommit с коммитами именно этих записей; иначе (старые манифесты) —
        # прежнее правило про балансер, а если балансера в записях нет — «совпадает
        # хотя бы с одним образом».
        $released = @()
        if ($null -ne $mf.PSObject.Properties['releasedServices'] -and $mf.releasedServices) {
            $released = @($mf.releasedServices | ForEach-Object { [string]$_ })
        }
        if ($released.Count -gt 0) {
            $releasedEntries = @($mf.images | Where-Object { $released -contains [string]$_.service })
            if ($releasedEntries.Count -eq 0) {
                $manifestIssues.Add("C4b: releasedServices=$($released -join ', ') не находит таких образов в манифесте")
            }
            foreach ($e in $releasedEntries) {
                if ([string]$e.commit -ne $mf.gitCommit) {
                    $manifestIssues.Add("C4b: gitCommit=$($mf.gitCommit) не совпадает с commit собранного образа $($e.image) = $($e.commit) — какая сборка считается выпуском?")
                }
            }
        } elseif ($balancerCommit) {
            if ($mf.gitCommit -ne $balancerCommit) {
                $manifestIssues.Add("C4b: gitCommit=$($mf.gitCommit) не совпадает с commit записи балансера $balancerCommit — какая сборка считается выпуском?")
            }
        } else {
            $anyMatch = @($mf.images | Where-Object { [string]$_.commit -eq $mf.gitCommit })
            if ($anyMatch.Count -eq 0) {
                $manifestIssues.Add("C4b: gitCommit=$($mf.gitCommit) не совпадает ни с одним образом манифеста — какая сборка считается выпуском?")
            }
        }
    }
    if ($manifestIssues.Count -eq 0) {
        Write-Host "  OK  идентичность зафиксирована у всех $($mf.images.Count) образов (per-image commit, buildDate, imageId; llama+rpc у cppworker)" -ForegroundColor DarkGray
    } else {
        foreach ($mi in $manifestIssues) { $problems.Add($mi) }
    }
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

# --- CUDA_ARCH: .env ↔ образ (R597) ---------------------------------------
#
# ЗАЧЕМ. `CUDA_ARCH` в deployments/.env обещает, под какие карты собран образ, но
# до этой проверки никто не сверял обещание с фактом. Живой случай (2026-11-19):
# в .env записано `CUDA_ARCH=75;86;120`, а образ собран только под 86 — на карте
# sm_75 или sm_120 он упал бы с «no kernel image is available for execution on the
# device», и искать причину пришлось бы в логах воркера.
#
# ИСТОЧНИК ФАКТА. В образе архива нет отдельной метки, поэтому берём описание OCI
# (`org.opencontainers.image.description`, Dockerfile.gpu печатает туда
# `CUDA_ARCH=<список>` — это build-arg, то есть ровно то, с чем собрали ядра).
# НАСТОЯЩЕЕ доказательство — `cuda_arch_built` в `/api/info` ЖИВОГО воркера
# (его отдаёт сам бинарь); здесь мы ловим расхождение ДО раскатки, когда воркера
# ещё нет.
#
# Если образа в локальном демоне нет (CI, свежий клон) — проверка пропускается с
# явной пометкой: «неизвестно» не выдаём за «совпало».
function Normalize-ArchList([string]$value) {
    if ([string]::IsNullOrWhiteSpace($value)) { return @() }
    return @(($value -replace 'sm_', '' -split '[;,]') |
        ForEach-Object { $_.Trim() } |
        Where-Object { $_ } |
        Sort-Object -Unique)
}

$cudaArchEnv = ''
# R675 (2026-11-21): читаем ТОТ ЖЕ файл, который уже найден выше ($envPath).
#
# ЖИВОЙ ДЕФЕКТ. Здесь стояло `$EnvFile` — параметр со значением по умолчанию "".
# При обычном запуске (без -EnvFile) `Join-Path … "..\deployments\\"` давал
# КАТАЛОГ, а `Join-Path (Get-Location) ""` — текущий каталог; `Test-Path` на
# каталоге истинен, поэтому Select-String получал папку, молча падал, и
# `$cudaArchEnv` оставался пустым. Следствие: сверка CUDA_ARCH «проходила
# успешно», ни разу не выполнившись, а в лог печаталось «CUDA_ARCH в  не задан»
# (имя файла тоже было пустым) — то есть ровно та проверка, ради которой всё
# написано (R597), не работала ни разу с момента появления.
if ($envPath -and (Test-Path $envPath -PathType Leaf)) {
    $m = Select-String -Path $envPath -Pattern '^\s*CUDA_ARCH\s*=\s*(.+)$' | Select-Object -First 1
    if ($m) { $cudaArchEnv = ($m.Matches[0].Groups[1].Value -replace '#.*$', '').Trim() }
}
if ($cudaArchEnv) {
    Write-Host ""
    Write-Host "CUDA_ARCH: $usedEnvName=$cudaArchEnv ↔ образ cppworker" -ForegroundColor Cyan

    # R675: offline-половина сверки — `.env` ↔ манифест выпуска (`cudaArch`).
    #
    # ЗАЧЕМ ОТДЕЛЬНО. Сверка с меткой образа ниже требует образа в локальном
    # демоне: в CI и на чистой машине её просто нет, проверка помечается
    # «ПРОПУСК» — и обещание `.env` о наборе архитектур остаётся непроверенным.
    # Манифест знает, под что СОБИРАЛИ (`cudaArch` пишет релиз), поэтому
    # расхождение `.env` ↔ манифест видно всегда, без docker. Это ровно дефект
    # R597: `.env` обещал 75;86;120, а собрано было только под 86.
    $manifestCudaArch = ''
    if ($null -ne $manifest -and $null -ne $manifest.PSObject.Properties['cudaArch']) {
        $manifestCudaArch = [string]$manifest.cudaArch
    }
    if ($manifestCudaArch) {
        $envArch = Normalize-ArchList $cudaArchEnv
        $mfArch = Normalize-ArchList $manifestCudaArch
        if (($envArch -join ';') -ne ($mfArch -join ';')) {
            $problems.Add("CUDA_ARCH: deployments/$usedEnvName обещает [$cudaArchEnv], а манифест $ManifestFile фиксирует сборку под [$manifestCudaArch] — на несовпавших архитектурах воркер упадёт с 'no kernel image is available'")
        } else {
            Write-Host "  OK  $usedEnvName=$($envArch -join ';') совпадает с манифестом (cudaArch=$manifestCudaArch)" -ForegroundColor DarkGray
        }
    } else {
        Write-Host "  ПРОПУСК  в $ManifestFile нет поля cudaArch — offline-сверка архитектур невозможна" -ForegroundColor DarkGray
    }
    $cppRepo = $effective.Keys | Where-Object { $_ -like '*cppworker' } | Select-Object -First 1
    if (-not $cppRepo) {
        Write-Host "  ПРОПУСК  образа cppworker нет в $ComposeFile" -ForegroundColor DarkGray
    } else {
        $cppRef = "${cppRepo}:$($effective[$cppRepo])"
        # Метки читаем через `{{json .Config.Labels}}`, а не `{{index …}}`: в
        # Windows PowerShell шаблон `{{index .Config.Labels "org…"}}` приходит в
        # docker искажённым и падает с «function "org" not defined» — проверено.
        # Тем же способом читает метки scripts/lib-release-manifest.ps1, так что
        # «как читать метку» остаётся в одной форме на два скрипта.
        $desc = ''
        $prevEap = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            $rawLabels = (& docker image inspect $cppRef --format '{{json .Config.Labels}}' 2>$null | Out-String)
        } finally {
            $ErrorActionPreference = $prevEap
        }
        if (-not [string]::IsNullOrWhiteSpace($rawLabels)) {
            try {
                $labelObj = $rawLabels | ConvertFrom-Json
                if ($labelObj -and $labelObj.PSObject.Properties['org.opencontainers.image.description']) {
                    $desc = [string]$labelObj.'org.opencontainers.image.description'
                }
            } catch {
                $desc = ''
            }
        }
        $desc = ([string]$desc).Trim()
        if (-not $desc -or $desc -eq '<no value>') {
            Write-Host "  ПРОПУСК  образа $cppRef нет в локальном демоне — сверка архива невозможна" -ForegroundColor DarkGray
        } else {
            $builtRaw = ''
            $bm = [regex]::Match($desc, 'CUDA_ARCH=([0-9;,]+)')
            if ($bm.Success) { $builtRaw = $bm.Groups[1].Value }
            $want = Normalize-ArchList $cudaArchEnv
            $built = Normalize-ArchList $builtRaw
            if ($built.Count -eq 0) {
                $problems.Add("${cppRef}: в метках образа нет CUDA_ARCH (описание '$desc') — какая архитектура собрана, неизвестно")
            } elseif ((($want -join ';') -ne ($built -join ';'))) {
                $missing = @($want | Where-Object { $built -notcontains $_ })
                $problems.Add("${cppRef} собран под [$($built -join ';')], а deployments/$usedEnvName обещает CUDA_ARCH=$cudaArchEnv" +
                    $(if ($missing.Count -gt 0) { " — не собрано под: $($missing -join ';'). На этих картах воркер упадёт с 'no kernel image is available'" } else { "" }))
            } else {
                Write-Host "  OK  образ $cppRef собран под $($built -join ';') — совпадает с .env" -ForegroundColor DarkGray
            }
        }
    }
} else {
    Write-Host ""
    Write-Host "CUDA_ARCH в deployments/$usedEnvName не задан — сверка архива с образом пропущена" -ForegroundColor DarkGray
}

# --- итог ------------------------------------------------------------------
Write-Host ""
if ($problems.Count -gt 0) {
    Write-Host "НАЙДЕНО ПРОБЛЕМ: $($problems.Count)" -ForegroundColor Red
    foreach ($p in $problems) { Write-Host "  - $p" -ForegroundColor Red }
    exit 1
}
Write-Host "OK: теги параметризованы, переменные ($($varSeen.Keys -join ', ')) есть в $usedEnvName (compose: $ComposeFile)" -ForegroundColor Green
exit 0
