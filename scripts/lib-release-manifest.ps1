#!/usr/bin/env pwsh
# lib-release-manifest.ps1 — R493/долг 15b (2026-11-11): ЕДИНСТВЕННЫЙ писатель
# deployments/release-manifest.json.
#
# ЗАЧЕМ. release-all.ps1 собирал манифест из полей {service, variable, image,
# imageTag, imageId, alias} — и на каждом выпуске терял per-image идентичность
# (commit/commitFull/commitDate/buildDate/author, у cppworker ещё
# llamaCommit/llamaDate/rpcProto) и блок standDeployed с доказательствами приёмки.
# Наблюдалось дважды за один выпуск 2026-11-11: сначала после сборки balancer+webui,
# затем после пересбора balancer. Идентичность приходилось восстанавливать руками
# из OCI-меток.
#
# При этом проверка уже существовала: check-image-tags.ps1 печатает
# «идентичность зафиксирована у всех N образов» и без этих полей падает. То есть
# проверка была, а писатель ей не подчинялся — классический рассинхрон «правило
# есть, но не в том месте».
#
# ПРАВИЛО: идентичность берётся из САМОГО ОБРАЗА (OCI-метки, их печёт
# scripts/build-identity.ps1), а не из .env и не из HEAD: образ мог быть собран
# раньше коммита, и тогда /api/version честно покажет его коммит. Если метки нет —
# манифест НЕ пишется (throw), чтобы неполный манифест не выглядел полным.
#
# Использование (обычно — из release-all.ps1):
#   . scripts/lib-release-manifest.ps1
#   Write-ReleaseManifest -ManifestPath ... -Tag ... -GitCommit ... -Images $imgs
# Самопроверка без Docker и без сети:
#   pwsh -File scripts/lib-release-manifest.ps1 -SelfTest
$ErrorActionPreference = 'Stop'

# Ключи OCI-меток, которые печёт build-identity.ps1 (R486).
$script:ManifestLabelKeys = @{
    CommitFull  = 'org.opencontainers.image.revision'
    BuildDate   = 'org.opencontainers.image.created'
    Author      = 'org.opencontainers.image.authors'
    Commit      = 'ru.ollamalegion.git.commit'
    RpcProto    = 'ru.ollamalegion.rpc.proto'
    LlamaCommit = 'ru.ollamalegion.llama.commit'
    LlamaDate   = 'ru.ollamalegion.llama.date'
}

# Get-ImageLabelsR493 — метки образа. $null, если образа нет в демоне.
function Get-ImageLabelsR493 {
    param([Parameter(Mandatory = $true)][string]$ImageRef)

    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $raw = & docker image inspect $ImageRef --format '{{json .Config.Labels}}' 2>$null
    } finally {
        $ErrorActionPreference = $prev
    }
    if ($LASTEXITCODE -ne 0 -or -not $raw) { return $null }
    try {
        return ($raw | Out-String | ConvertFrom-Json)
    } catch {
        return $null
    }
}

# Get-ImageIdentityR493 — идентичность сборки из OCI-меток образа.
# Возвращает $null, если образа нет; поля могут быть пустыми — их проверяет
# Assert-ManifestCompleteR493 (пустой commit/buildDate = манифест неполный).
function Get-ImageIdentityR493 {
    param([Parameter(Mandatory = $true)][string]$ImageRef)

    $labels = Get-ImageLabelsR493 -ImageRef $ImageRef
    if ($null -eq $labels) { return $null }

    $read = {
        param($key)
        if ($null -eq $labels.PSObject.Properties[$key]) { return '' }
        $v = $labels.$key
        if ($null -eq $v) { return '' }
        return [string]$v
    }

    return [ordered]@{
        commit      = & $read $script:ManifestLabelKeys.Commit
        commitFull  = & $read $script:ManifestLabelKeys.CommitFull
        commitDate  = ''
        buildDate   = & $read $script:ManifestLabelKeys.BuildDate
        author      = & $read $script:ManifestLabelKeys.Author
        llamaCommit = & $read $script:ManifestLabelKeys.LlamaCommit
        llamaDate   = & $read $script:ManifestLabelKeys.LlamaDate
        rpcProto    = & $read $script:ManifestLabelKeys.RpcProto
    }
}

# Get-CommitDateR493 — дата коммита из репозитория (у образа её в метках нет).
# Пусто, если коммит не найден: дату не выдумываем.
function Get-CommitDateR493 {
    param(
        [Parameter(Mandatory = $true)][string]$RepoRoot,
        [string]$Commit
    )
    if (-not $Commit) { return '' }
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $out = & git -C $RepoRoot show -s --format=%cI $Commit 2>$null
    } finally {
        $ErrorActionPreference = $prev
    }
    if ($LASTEXITCODE -ne 0 -or -not $out) { return '' }
    return ($out | Out-String).Trim()
}

# Assert-ManifestCompleteR493 — у каждого образа обязаны быть commit и buildDate.
# Это то, что проверяет check-image-tags.ps1; проверяем ДО записи, чтобы неполный
# манифест не появился на диске вовсе.
function Assert-ManifestCompleteR493 {
    param([Parameter(Mandatory = $true)][object[]]$Images)

    $problems = @()
    foreach ($img in $Images) {
        if (-not $img.service) { $problems += 'образ без service'; continue }
        if (-not $img.commit) { $problems += "$($img.service): нет commit в OCI-метках образа $($img.image)"; }
        if (-not $img.buildDate) { $problems += "$($img.service): нет buildDate в OCI-метках образа $($img.image)"; }
        if (-not $img.imageId) { $problems += "$($img.service): нет imageId (образ не найден в демоне?)"; }
    }
    if ($problems.Count -gt 0) {
        throw ("R493/15b: манифест выпуска неполный — записи нет.`n  - " + ($problems -join "`n  - ") +
            "`nСоберите образ через scripts/release-all.ps1 (он передаёт build-args идентичности), а не docker build напрямую.")
    }
}

# Read-PreviousManifestR493 — прошлый манифест (или $null). Нужен, чтобы сохранить
# note и standDeployed: это доказательства приёмки, а не вывод одного запуска.
function Read-PreviousManifestR493 {
    param([Parameter(Mandatory = $true)][string]$ManifestPath)
    if (-not (Test-Path $ManifestPath)) { return $null }
    try {
        return (Get-Content $ManifestPath -Raw | ConvertFrom-Json)
    } catch {
        Write-Host "[manifest] предупреждение: прошлый манифест не разобран ($($_.Exception.Message))" -ForegroundColor Yellow
        return $null
    }
}

# ConvertTo-JsonR493 — JSON с русским текстом как есть.
# ConvertTo-Json в PowerShell 5.1 экранирует всё не-ASCII в \uXXXX; манифест —
# читаемый документ (note и standDeployed с русскими доказательствами), поэтому
# экранирование раскрываем обратно.
function ConvertTo-JsonR493 {
    param([Parameter(Mandatory = $true)]$Value, [int]$Depth = 8)
    $json = $Value | ConvertTo-Json -Depth $Depth
    return [regex]::Replace($json, '\\u([0-9a-fA-F]{4})', {
            param($m)
            return [string][char][Convert]::ToInt32($m.Groups[1].Value, 16)
        })
}

# Write-ReleaseManifest — запись манифеста: тег выпуска + образы с идентичностью
# + note/standDeployed из прошлого манифеста.
function Write-ReleaseManifest {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$Tag,
        [string]$GitCommit = '',
        [string]$CudaArch = '',
        [string]$ComposeFile = '',
        [string]$ComposeProfile = '',
        # Сервисы, собранные В ЭТОМ запуске. По ним вычисляется gitCommit выпуска:
        # check-image-tags.ps1 (C4b) требует, чтобы он совпадал с коммитом образа,
        # а не просто с HEAD (HEAD уезжает вперёд между сборкой и выпуском).
        [string[]]$ReleaseServices = @(),
        [Parameter(Mandatory = $true)][object[]]$Images,
        [string]$RepoRoot = '',
        # Перенести идентичность из прошлого манифеста (образ мог быть вычищен
        # prune'ом): тогда OCI-меток нет, а врать про «нет идентичности» нельзя.
        [switch]$NoIdentityRefresh,
        # Подмена источника идентичности — только для самопроверки без Docker.
        [scriptblock]$IdentityProvider = $null,
        # Подмена источника imageId — для самопроверки без Docker.
        [scriptblock]$ImageIdProvider = $null
    )

    if (-not $RepoRoot) { $RepoRoot = (Split-Path -Parent $PSScriptRoot) }

    $prev = Read-PreviousManifestR493 -ManifestPath $ManifestPath

    # 1. Идентичность каждого образа — из образа, а не из .env/HEAD.
    $out = @()
    foreach ($img in $Images) {
        $entry = [ordered]@{
            alias    = [string]$img.alias
            variable = [string]$img.variable
            service  = [string]$img.service
            imageTag = [string]$img.imageTag
            image    = [string]$img.image
            imageId  = [string]$img.imageId
        }
        if (-not $entry.imageId -and $entry.image) {
            if ($null -ne $ImageIdProvider) {
                $entry.imageId = [string](& $ImageIdProvider $entry.image)
            } else {
                $prevEap = $ErrorActionPreference
                $ErrorActionPreference = 'Continue'
                try {
                    $id = & docker image inspect $entry.image --format '{{.Id}}' 2>$null
                } finally {
                    $ErrorActionPreference = $prevEap
                }
                if ($LASTEXITCODE -eq 0 -and $id) { $entry.imageId = ([string]($id | Select-Object -First 1)).Trim() }
            }
        }
        if (-not $NoIdentityRefresh) {
            $ident = if ($null -ne $IdentityProvider) { & $IdentityProvider $entry.image } else { Get-ImageIdentityR493 -ImageRef $entry.image }
            if ($null -eq $ident) {
                $entry.commit = ''
                $entry.buildDate = ''
            } else {
                foreach ($k in $ident.Keys) { $entry[$k] = $ident[$k] }
                $entry.commitDate = Get-CommitDateR493 -RepoRoot $RepoRoot -Commit $entry.commit
            }
        } else {
            # Перенос из прошлого манифеста (образ мог быть вычищен prune'ом).
            $old = $null
            if ($null -ne $prev) {
                $old = @($prev.images) | Where-Object { $_.service -eq $entry.service } | Select-Object -First 1
            }
            foreach ($k in @('commit', 'commitFull', 'commitDate', 'buildDate', 'author', 'llamaCommit', 'llamaDate', 'rpcProto')) {
                $entry[$k] = if ($null -ne $old -and $null -ne $old.PSObject.Properties[$k]) { [string]$old.$k } else { '' }
            }
        }
        # Пустые необязательные поля не тащим в файл — но commit/buildDate обязательны.
        if (-not $entry.commitDate) { $entry.Remove('commitDate') }
        foreach ($k in @('commitFull', 'author', 'llamaCommit', 'llamaDate', 'rpcProto')) {
            if (-not $entry[$k]) { $entry.Remove($k) }
        }
        $out += [pscustomobject]$entry
    }

    Assert-ManifestCompleteR493 -Images $out

    # gitCommit выпуска — коммит, ЗАПЕЧЁННЫЙ в собранные образы, а не HEAD:
    # проверка C4b в check-image-tags.ps1 сравнивает его с commit записи образа и
    # падает на расхождении («какая сборка считается выпуском?»). Живой случай
    # 2026-11-11: манифест записан уже после следующего коммита, gitCommit=50ebd8fd
    # при балансере 0ed632cc — проверка поймала, и это правильно.
    $releaseCommit = $GitCommit
    if ($ReleaseServices -and $ReleaseServices.Count -gt 0) {
        $commits = @($out | Where-Object { $ReleaseServices -contains $_.service } |
            ForEach-Object { $_.commit } | Sort-Object -Unique)
        if ($commits.Count -gt 1) {
            throw ("R493/15b: в одном выпуске собраны образы из РАЗНЫХ коммитов (" + ($commits -join ', ') +
                ') — манифест не пишу: непонятно, какая сборка считается выпуском. Пересоберите образы из одного HEAD.')
        }
        if ($commits.Count -eq 1) { $releaseCommit = $commits[0] }
    }

    $manifest = [ordered]@{
        tag            = $Tag
        builtAt        = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
        gitCommit      = $releaseCommit
        cudaArch       = $CudaArch
        composeFile    = $ComposeFile
        composeProfile = $ComposeProfile
        images         = $out
    }
    # releasedServices — ЧТО собиралось в этом запуске. Нужно проверке C4b
    # (check-image-tags.ps1): при частичном выпуске (только воркер) gitCommit
    # равен коммиту собранного образа, а не балансера, и сверять его с балансером
    # нельзя. Без этого поля проверка падала на ровном месте при каждом выпуске
    # одного компонента (живой случай 2026-11-12).
    if ($ReleaseServices -and $ReleaseServices.Count -gt 0) {
        $manifest['releasedServices'] = @($ReleaseServices | Sort-Object)
    }
    # note/standDeployed — доказательства приёмки, они не выводятся из сборки.
    if ($null -ne $prev) {
        if ($null -ne $prev.PSObject.Properties['note'] -and $prev.note) { $manifest['note'] = [string]$prev.note }
        if ($null -ne $prev.PSObject.Properties['standDeployed'] -and $null -ne $prev.standDeployed) {
            $manifest['standDeployed'] = $prev.standDeployed
        }
    }

    $json = ConvertTo-JsonR493 -Value $manifest
    [System.IO.File]::WriteAllText($ManifestPath, $json + "`n", (New-Object System.Text.UTF8Encoding($false)))
    Write-Host "[manifest] записан: $ManifestPath (образов: $($out.Count), идентичность из OCI-меток)" -ForegroundColor Cyan
    return $manifest
}

# ---------------------------------------------------------------------------
# Самопроверка: без Docker, без сети. Проверяет ровно те контракты, из-за которых
# манифест терял данные (долг 15b).
# ---------------------------------------------------------------------------
function Invoke-ManifestSelfTestR493 {
    $failures = 0
    function Check($name, [scriptblock]$body) {
        try {
            & $body
            Write-Host "  OK  $name" -ForegroundColor Green
        } catch {
            Write-Host "  FAIL $name — $($_.Exception.Message)" -ForegroundColor Red
            $script:stFailures++
        }
    }

    $tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("manifest-selftest-" + [guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmp -Force | Out-Null
    $path = Join-Path $tmp 'release-manifest.json'

    # Прошлый манифест с доказательствами и идентичностью.
    $prev = [ordered]@{
        tag            = 'r-prev'
        builtAt        = '2026-11-10T00:00:00Z'
        gitCommit      = 'aaaa1111'
        note           = 'заметка прошлого выпуска — должна остаться'
        standDeployed  = [ordered]@{ verifiedAt = '2026-11-10T01:00:00Z'; proof = @('доказательство 1', 'доказательство 2') }
        images         = @(
            [ordered]@{ service = 'balancer'; image = 'img:prev'; imageTag = 'prev'; imageId = 'sha256:1'; commit = 'aaaa1111'; buildDate = '2026-11-10T00:00:00Z' }
        )
    }
    (ConvertTo-JsonR493 -Value $prev) | Set-Content -Path $path -Encoding UTF8

    # Подменные источники: самопроверка не должна требовать Docker.
    $fakeIdent = {
        param($image)
        if ($image -eq 'img:new') {
            return [ordered]@{
                commit = 'bbbb2222'; commitFull = 'bbbb222233334444555566667777888899990000'
                commitDate = ''; buildDate = '2026-11-11T00:00:00Z'
                author = 'Selftest <selftest@example>'; llamaCommit = ''; llamaDate = ''; rpcProto = ''
            }
        }
        return $null
    }
    $fakeId = { param($image) return 'sha256:2' }

    Check 'note и standDeployed прошлого манифеста сохраняются' {
        $m = Write-ReleaseManifest -ManifestPath $path -Tag 'r-new' -GitCommit 'bbbb2222' `
            -ComposeFile 'docker-compose.stack.yml' -ComposeProfile 'full' `
            -Images @([pscustomobject]@{ service = 'balancer'; variable = 'BALANCER_TAG'; image = 'img:new'; imageTag = 'new' }) `
            -IdentityProvider $fakeIdent -ImageIdProvider $fakeId | Out-Null
        $re = Get-Content $path -Raw | ConvertFrom-Json
        if (-not $re.note) { throw 'note пропал' }
        if ($re.note -notmatch 'должна остаться') { throw "note искажён: $($re.note)" }
        if ($null -eq $re.standDeployed) { throw 'standDeployed пропал' }
        if (@($re.standDeployed.proof).Count -ne 2) { throw 'доказательства standDeployed потеряны' }
    }

    Check 'идентичность попадает в файл (commit, commitFull, buildDate, author, imageId)' {
        $re = Get-Content $path -Raw | ConvertFrom-Json
        $img = @($re.images)[0]
        if ($img.commit -ne 'bbbb2222') { throw "commit не записан: '$($img.commit)'" }
        if ($img.commitFull -ne 'bbbb222233334444555566667777888899990000') { throw "commitFull не записан: '$($img.commitFull)'" }
        if ($img.buildDate -ne '2026-11-11T00:00:00Z') { throw "buildDate не записан: '$($img.buildDate)'" }
        if ($img.author -notmatch 'Selftest') { throw "author не записан: '$($img.author)'" }
        if ($img.imageId -ne 'sha256:2') { throw "imageId не записан: '$($img.imageId)'" }
    }

    Check 'русский текст не превращается в \uXXXX' {
        $raw = Get-Content $path -Raw
        if ($raw -match '\\u04') { throw 'кириллица закодирована в \u-последовательности' }
        if ($raw -notmatch 'заметка') { throw 'русский текст не найден в файле как есть' }
    }

    Check 'неполная идентичность НЕ пишется (throw), файл не тронут' {
        $before = Get-Content $path -Raw
        $emptyIdent = { param($image) return $null }
        $threw = $false
        try {
            Write-ReleaseManifest -ManifestPath $path -Tag 'r-bad' -GitCommit 'cccc3333' `
                -Images @([pscustomobject]@{ service = 'balancer'; variable = 'BALANCER_TAG'; image = 'img:bad'; imageTag = 'bad' }) `
                -IdentityProvider $emptyIdent -ImageIdProvider $fakeId | Out-Null
        } catch { $threw = $true }
        if (-not $threw) { throw 'манифест без идентичности был записан — проверка не работает' }
        $after = Get-Content $path -Raw
        if ($after -ne $before) { throw 'файл изменён несмотря на отказ' }
    }

    Check 'gitCommit выпуска = коммит собранных образов (правило C4b), а не HEAD' {
        $ident2 = {
            param($image)
            $map = @{
                'img:a' = [ordered]@{ commit = 'aaaa1111'; commitFull = 'aaaa'; commitDate = ''; buildDate = '2026-11-11T00:00:00Z'; author = 'x'; llamaCommit = ''; llamaDate = ''; rpcProto = '' }
                'img:b' = [ordered]@{ commit = 'bbbb2222'; commitFull = 'bbbb'; commitDate = ''; buildDate = '2026-11-11T00:00:00Z'; author = 'x'; llamaCommit = ''; llamaDate = ''; rpcProto = '' }
            }
            if ($map.ContainsKey($image)) { return $map[$image] }
            return $null
        }
        $imgs = @(
            [pscustomobject]@{ service = 'svc-a'; variable = 'A_TAG'; image = 'img:a'; imageTag = 'a' },
            [pscustomobject]@{ service = 'svc-b'; variable = 'B_TAG'; image = 'img:b'; imageTag = 'b' }
        )
        # Собраны оба, коммиты разные → отказ.
        $threw = $false
        try {
            Write-ReleaseManifest -ManifestPath $path -Tag 'r-mixed' -GitCommit 'ffff9999' `
                -Images $imgs -ReleaseServices @('svc-a', 'svc-b') `
                -IdentityProvider $ident2 -ImageIdProvider $fakeId | Out-Null
        } catch { $threw = $true }
        if (-not $threw) { throw 'смесь коммитов в одном выпуске не отвергнута' }

        # Собран только svc-a → gitCommit выпуска = aaaa1111, не HEAD ffff9999.
        $m = Write-ReleaseManifest -ManifestPath $path -Tag 'r-one' -GitCommit 'ffff9999' `
            -Images $imgs -ReleaseServices @('svc-a') `
            -IdentityProvider $ident2 -ImageIdProvider $fakeId
        if ($m.gitCommit -ne 'aaaa1111') { throw "gitCommit выпуска должен быть коммитом образа, получено '$($m.gitCommit)'" }
        $re = Get-Content $path -Raw | ConvertFrom-Json
        if ($re.gitCommit -ne 'aaaa1111') { throw "в файле gitCommit='$($re.gitCommit)'" }
        if (@($re.images).Count -ne 2) { throw 'перенесённые образы потерялись' }
    }

    Check 'NoIdentityRefresh переносит идентичность прошлого манифеста' {
        # svc-a есть в прошлом манифесте (aaaa1111); образ 'img:gone' в демоне
        # отсутствует, OCI-меток нет — идентичность обязана переехать из прошлого
        # манифеста, а не превратиться в «нет данных».
        Write-ReleaseManifest -ManifestPath $path -Tag 'r-carry' -GitCommit 'bbbb2222' `
            -Images @([pscustomobject]@{ service = 'svc-a'; variable = 'A_TAG'; image = 'img:gone'; imageTag = 'gone' }) `
            -IdentityProvider { param($i) return $null } -ImageIdProvider $fakeId -NoIdentityRefresh | Out-Null
        $re = Get-Content $path -Raw | ConvertFrom-Json
        $img = @($re.images) | Where-Object { $_.service -eq 'svc-a' } | Select-Object -First 1
        if ($img.commit -ne 'aaaa1111') { throw "идентичность не перенесена: '$($img.commit)'" }
    }

    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue

    if ($script:stFailures -gt 0) {
        Write-Host ''
        Write-Host "ИТОГ: FAIL ($($script:stFailures))" -ForegroundColor Red
        exit 1
    }
    Write-Host ''
    Write-Host 'ИТОГ: PASS (самопроверка писателя манифеста)' -ForegroundColor Green
    exit 0
}

if ($MyInvocation.InvocationName -ne '.') {
    $argList = $args
    if ($argList -contains '-SelfTest') {
        $script:stFailures = 0
        Invoke-ManifestSelfTestR493
    } else {
        Write-Host 'lib-release-manifest.ps1: этот файл подключается (. scripts/lib-release-manifest.ps1).'
        Write-Host 'Самопроверка: pwsh -File scripts/lib-release-manifest.ps1 -SelfTest'
    }
}
