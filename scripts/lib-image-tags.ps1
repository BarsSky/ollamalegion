<#
  lib-image-tags.ps1 — R83 (2026-09-25): общие операции с тегами образов.

  ЗАЧЕМ. До R83 каждый скрипт выпуска обновлял версию по-своему:
  rebuild_balancer и rebuild_webui переписывали строку `image:` в compose
  регуляркой, rebuild_cppworker писал переменную в .env, а agent собирался через
  `docker compose --build` с тегом latest. Четыре реализации одной задачи — это
  четыре способа разойтись.

  Теперь compose НЕ правится вообще: теги всех четырёх образов читаются из
  deployments/.env (интерполяция `${VAR:-default}`), а запись значения и
  «последнего» алиаса делает этот файл — один раз и одинаково для всех.

  Подключается так:
      . (Join-Path $PSScriptRoot 'lib-image-tags.ps1')
#>

# Set-ImageTagVar — записать значение тега в deployments/.env (и, если ключ там
# есть, в .env.bundled-with-agent — держим файлы синхронными).
#
# ВАЖНО: строка `VAR=` должна уже существовать в .env: `-replace '^VAR=.*'` по
# отсутствующей строке — молчаливый no-op, и выпуск «не доедет» (compose возьмёт
# default из compose-файла). Поэтому отсутствие ключа — это throw, а не warning.
function Set-ImageTagVar {
    param(
        [Parameter(Mandatory = $true)][string]$VariableName,
        [Parameter(Mandatory = $true)][string]$Value,
        [Parameter(Mandatory = $true)][string]$EnvDir
    )

    $envPath = Join-Path $EnvDir ".env"
    if (-not (Test-Path $envPath)) {
        throw "нет файла $envPath — некуда записать $VariableName"
    }

    $content = Get-Content $envPath
    if (-not ($content | Where-Object { $_ -match "^$VariableName=" })) {
        throw "в $envPath нет строки '$VariableName='. Добавьте её (см. комментарий R83 в .env) — иначе тег не запишется и compose возьмёт default."
    }
    $content = $content -replace "^$VariableName=.*", "$VariableName=$Value"
    Set-Content -Path $envPath -Value $content

    $containerEnvPath = Join-Path $EnvDir ".env.bundled-with-agent"
    if (Test-Path $containerEnvPath) {
        $content2 = Get-Content $containerEnvPath
        if ($content2 | Where-Object { $_ -match "^$VariableName=" }) {
            $content2 = $content2 -replace "^$VariableName=.*", "$VariableName=$Value"
            Set-Content -Path $containerEnvPath -Value $content2
        }
    }

    Write-Host "[tags] $VariableName=$Value записан в deployments/.env" -ForegroundColor Cyan
}

# Add-ImageAlias — повесить «последний» тег-алиас на собранный образ.
#
# Провал алиаса НЕ должен валить выпуск: версия зафиксирована основным тегом и
# манифестом, а алиас — удобство для dev-конфигов, которые ссылаются на latest.
#
# ВАЖНО про cppworker: в репозитории ollama-legion/cppworker лежат
# взаимоисключающие варианты (cpu / stub / gpu-86 / gpu-arch_all / gpu-llamacpp),
# а `latest` — ОДИН тег на репозиторий, его перезапишет последняя сборка. Поэтому
# для cppworker алиас вариантный (latest-gpu-<arch>), иначе `latest` может начать
# указывать на CPU- или arch_all-сборку.
function Add-ImageAlias {
    param(
        [Parameter(Mandatory = $true)][string]$ImageRef,
        [Parameter(Mandatory = $true)][string]$AliasRef
    )

    docker tag $ImageRef $AliasRef
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "не удалось повесить алиас $AliasRef (выпуск продолжается: версия зафиксирована тегом $ImageRef)"
        return
    }
    Write-Host "[tags] alias $AliasRef" -ForegroundColor DarkGray
}

# Get-ImageID — id локального образа (для манифеста выпуска).
# RepoDigests для локально собранных образов пуст, поэтому берём .Id.
function Get-ImageID {
    param([Parameter(Mandatory = $true)][string]$ImageRef)
    $id = docker inspect --format '{{.Id}}' $ImageRef 2>$null | Select-Object -First 1
    if (-not $id) { return "" }
    return [string]$id
}
