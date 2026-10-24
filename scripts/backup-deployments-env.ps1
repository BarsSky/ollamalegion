# backup-deployments-env.ps1 — страховка от потери deployments/.env.
#
# ЗАЧЕМ. `deployments/.env` в git НЕ отслеживается (так задумано: у каждой машины
# свой путь к моделям и свой токен), и он же — единственный источник интерполяции
# compose (пути, теги образов, флаги балансера). Один раз он уже был потерян:
# файл обнулился, compose смонтировал пустой `../models`, и оба воркера видели
# 0 моделей — при этом сам стек выглядел «живым» (healthcheck зелёный). Понять,
# что пропала именно строка `MODELS_DIR`, удалось только по фактическому mount'у
# другого контейнера.
#
# ЧТО ДЕЛАЕТ.
#   1. Читает `.env` АБСОЛЮТНЫМ путём через [System.IO.File]::ReadAllText —
#      обычный `Get-Content` на hidden-файле из корня репозитория возвращает
#      $null, а запись такого «прочитанного» значения обнуляет файл (это и была
#      авария).
#   2. Отказывается что-либо делать, если файл пуст или потерял критические
#      ключи: пустой файл в бэкапе бесполезен, а молчаливое «всё ок» — вредно.
#   3. Кладёт копию в `deployments/.env.bak-<yyyyMMdd-HHmmss>` и чистит старые.
#
# ИСПОЛЬЗОВАНИЕ.
#   pwsh scripts/backup-deployments-env.ps1                 # бэкап + отчёт
#   pwsh scripts/backup-deployments-env.ps1 -Verify         # только проверка
#   pwsh scripts/backup-deployments-env.ps1 -KeepLast 20    # больше истории
#
# ВЫХОД. 0 — файл на месте и целый (бэкап сделан, если не -Verify);
#        1 — файл пуст/недоступен или потерял критические ключи.

[CmdletBinding()]
param(
    # Каталог deployments (по умолчанию — рядом с репозиторием скрипта).
    [string]$DeploymentsDir = (Join-Path (Split-Path -Parent $PSScriptRoot) 'deployments'),

    # Сколько последних бэкапов держать (0 = не чистить).
    [int]$KeepLast = 10,

    # Только проверить файл, ничего не писать.
    [switch]$Verify
)

$ErrorActionPreference = 'Stop'

# Критические ключи: без них стек либо не поднимется, либо поднимется «зелёным»,
# но с пустым каталогом моделей / чужим тегом образа. Ровно этот класс отказа и
# надо ловить до, а не после.
$criticalKeys = @(
    'MODELS_DIR',              # без него compose монтирует пустой ../models
    'CPPWORKER_API_TOKEN',     # без него клиенты получают 401
    'BALANCER_TAG',
    'WEBUI_TAG',
    'CPPWORKER_GPU_TAG',
    'CUDA_ARCH',
    'CPPWORKER_LLAMA_BUILD'
)

$envPath = Join-Path $DeploymentsDir '.env'
Write-Host "env-файл: $envPath"

if (-not (Test-Path -LiteralPath $envPath)) {
    Write-Host "ОШИБКА: файла нет. Восстановите его из .env.example + .env.bundled-with-agent.example." -ForegroundColor Red
    exit 1
}

# Абсолютный путь + [System.IO.File]: Get-Content и [IO.File]::ReadAllLines с
# относительным путём на hidden-файле дают $null (и именно так файл был обнулён).
$absolute = [System.IO.Path]::GetFullPath($envPath)
$text = [System.IO.File]::ReadAllText($absolute)

if ([string]::IsNullOrWhiteSpace($text)) {
    Write-Host "ОШИБКА: файл пуст (0 значимых байт). НЕ перезаписывайте его вслепую — сначала восстановите содержимое." -ForegroundColor Red
    exit 1
}

$lines = $text -split "`r?`n"
Write-Host ("строк: {0}, размер: {1} байт" -f $lines.Count, $text.Length)

# Разбор KEY=VALUE (последнее вхождение побеждает — как у compose).
$values = @{}
foreach ($line in $lines) {
    if ($line -match '^\s*#' -or $line -notmatch '=') { continue }
    $idx = $line.IndexOf('=')
    $key = $line.Substring(0, $idx).Trim()
    $val = $line.Substring($idx + 1).Trim()
    if ($key) { $values[$key] = $val }
}

$missing = @()
foreach ($key in $criticalKeys) {
    if (-not $values.ContainsKey($key)) {
        $missing += $key
    } elseif ([string]::IsNullOrWhiteSpace($values[$key])) {
        $missing += "$key (пустое значение)"
    }
}

Write-Host ''
Write-Host 'Критические ключи:' -ForegroundColor Cyan
foreach ($key in $criticalKeys) {
    if ($values.ContainsKey($key) -and -not [string]::IsNullOrWhiteSpace($values[$key])) {
        Write-Host ("  ok      {0}={1}" -f $key, $values[$key]) -ForegroundColor Green
    } else {
        Write-Host ("  ПОТЕРЯН {0}" -f $key) -ForegroundColor Red
    }
}

if ($missing.Count -gt 0) {
    Write-Host ''
    Write-Host ("ОШИБКА: потеряны ключи: {0}" -f ($missing -join ', ')) -ForegroundColor Red
    Write-Host 'Бэкап НЕ сделан: копия неполного .env создаёт ложную уверенность.' -ForegroundColor Red
    exit 1
}

if ($Verify) {
    Write-Host ''
    Write-Host 'Проверка пройдена (-Verify: файл не изменялся).' -ForegroundColor Green
    exit 0
}

$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$backupPath = Join-Path $DeploymentsDir (".env.bak-{0}" -f $stamp)

# Копируем побайтово из уже проверенного текста: источник заведомо не пуст.
[System.IO.File]::WriteAllText([System.IO.Path]::GetFullPath($backupPath), $text)
$backupSize = (Get-Item -LiteralPath $backupPath).Length
if ($backupSize -lt 1024) {
    Remove-Item -LiteralPath $backupPath -Force
    Write-Host ("ОШИБКА: бэкап получился подозрительно маленьким ({0} байт) — удалён." -f $backupSize) -ForegroundColor Red
    exit 1
}
Write-Host ''
Write-Host ("Бэкап: {0} ({1} байт)" -f (Split-Path -Leaf $backupPath), $backupSize) -ForegroundColor Green

if ($KeepLast -gt 0) {
    $old = Get-ChildItem -LiteralPath $DeploymentsDir -Filter '.env.bak-*' -File |
        Sort-Object LastWriteTime -Descending |
        Select-Object -Skip $KeepLast
    foreach ($f in $old) {
        Remove-Item -LiteralPath $f.FullName -Force
        Write-Host ("удалён старый бэкап: {0}" -f $f.Name) -ForegroundColor DarkGray
    }
}

$kept = Get-ChildItem -LiteralPath $DeploymentsDir -Filter '.env.bak-*' -File | Measure-Object
Write-Host ("бэкапов в каталоге: {0} (храним последние {1})" -f $kept.Count, $KeepLast)
exit 0
