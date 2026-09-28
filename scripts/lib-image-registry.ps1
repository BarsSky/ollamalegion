#!/usr/bin/env pwsh
# lib-image-registry.ps1 — R83 (2026-09-28): локальный репозиторий образов.
#
# ЗАЧЕМ. Стенды бывают двух видов: у разработчика образы локальные
# (`ollama-legion/balancer:v23`), а на объекте — из своего registry
# (`local-docker-hub:5000/ollama-legion/balancer:v23`). Раньше имя репозитория
# было вписано в compose-файл и в каждый скрипт сборки, поэтому переезд на
# локальный registry требовал правки в нескольких местах и легко расходился.
#
# Теперь префикс задаётся ОДНОЙ переменной в deployments/.env:
#   IMAGE_REGISTRY=                      → local (по умолчанию, стандартный путь)
#   IMAGE_REGISTRY=local-docker-hub:5000 → local-docker-hub:5000/ollama-legion/...
#
# Compose подставляет её сам (`${IMAGE_REGISTRY:-}`), а скрипты сборки берут
# готовый префикс отсюда, чтобы собранный образ назывался ТАК ЖЕ, как его ищет
# compose. Иначе `up -d` пошёл бы в registry за образом, который только что собран
# локально под другим именем.
#
# Подключается так:
#     . (Join-Path $PSScriptRoot 'lib-image-registry.ps1')
#     $prefix = Get-ImageRegistryPrefix -EnvDir 'deployments'

# Get-ImageRegistryPrefix — префикс репозитория для имён образов.
#
# Возвращает "" (локальный режим) либо "<registry>/" СО слэшем на конце —
# вызывающий код просто делает "${prefix}ollama-legion/balancer:$tag".
#
# Пустое значение переменной, её отсутствие и значения-заглушки ("local",
# "docker.io", "index.docker.io") трактуются как «стандартный путь»: иначе
# `docker.io/ollama-legion/...` ушёл бы в Docker Hub за несуществующим образом.
function Get-ImageRegistryPrefix {
    param(
        [Parameter(Mandatory = $true)][string]$EnvDir,
        [string]$VariableName = 'IMAGE_REGISTRY'
    )

    $value = $null

    # 1. Сначала deployments/.env — ОДИН источник истины, тот же, что читает
    #    `docker compose` при интерполяции ${IMAGE_REGISTRY:-}.
    $envPath = Join-Path $EnvDir '.env'
    if (Test-Path $envPath) {
        $match = Select-String -Path $envPath -Pattern "^\s*$VariableName=(.*)$" | Select-Object -First 1
        if ($match) {
            $value = $match.Matches[0].Groups[1].Value
        }
    }

    # 2. Fallback на окружение процесса — удобно для разовых сборок
    #    (`$env:IMAGE_REGISTRY = 'reg:5000'; .\scripts\build-containers.ps1`).
    if ([string]::IsNullOrWhiteSpace($value)) {
        $value = [Environment]::GetEnvironmentVariable($VariableName)
    }

    return Format-ImageRegistryPrefix -Registry $value
}

# Format-ImageRegistryPrefix — нормализация значения в префикс со слэшем.
# Отдельная функция, чтобы её можно было покрыть тестом без файловой системы.
function Format-ImageRegistryPrefix {
    param([string]$Registry)

    if ([string]::IsNullOrWhiteSpace($Registry)) { return '' }

    $clean = $Registry.Trim().Trim('/')

    # Значения-заглушки означают «стандартный путь», а не реальный registry.
    $local = @('local', 'docker.io', 'index.docker.io', 'registry-1.docker.io')
    if ($local -contains $clean.ToLower()) { return '' }

    return "$clean/"
}

# Resolve-ImageRef — полное имя образа с учётом репозитория.
# Пример: Resolve-ImageRef -Prefix 'reg:5000/' -Name 'ollama-legion/balancer' -Tag 'v23'
#         → reg:5000/ollama-legion/balancer:v23
function Resolve-ImageRef {
    param(
        [string]$Prefix,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Tag
    )

    return "$Prefix${Name}:$Tag"
}
