# scripts/upgrade-donor-node.ps1 — R118.8 (2026-10-14).
#
# ОБНОВЛЕНИЕ УЗЛА-ДОНОРА (удалённый cppworker с встроенным агентом) до другой
# сборки того же образа — БЕЗ потери конфигурации.
#
# ЗАЧЕМ. На узле-доноре (CYBORG, 192.0.2.10) крутится cppworker-node2 со
# встроенным агентом (AGENT_EMBEDDED=on): он регистрирует бэкенд в балансере и
# отдаёт свои слои по RPC. Сборка отставала (gpu-rpc86-r115d против r116c на
# мастере), а состав env/портов/томов у него «ручной» (не из compose-файла
# репозитория), поэтому обновление — это аккуратное пересоздание контейнера:
#   * конфигурация снимается с РАБОТАЮЩЕГО контейнера (docker inspect) и
#     переносится в env-файл на узле;
#   * старый контейнер НЕ удаляется, а переименовывается в <name>-<oldtag>-bak —
#     откат = остановить новый, вернуть имя старому, запустить;
#   * проверяется, что узел поднялся healthy, агент перерегистрировал бэкенд и
#     роль (глазами балансера), и что /app/rpc-server совпадает по хэшу с
#     мастером (иначе раскладка несовместима — правило «один коммит llama.cpp +
#     своя CUDA-арх»).
#
# Запуск (подставьте свои значения; в репозитории личных данных нет):
#   pwsh -File scripts/upgrade-donor-node.ps1 `
#     -SshTarget '<ssh-user>@<donor-host>' `
#     -KeyPath "$env:USERPROFILE\.ssh\<your-key>" `
#     -NewImage 'ollama-legion/cppworker:gpu-<tag>' `
#     -DryRun                      # сначала посмотреть, что будет сделано
#   pwsh -File scripts/upgrade-donor-node.ps1 ...   # без -DryRun — применить
#
# Проверка после обновления (живая, этим и подтверждается успех):
#   scripts/balancer-scenarios.ps1  →  все сценарии PASS, в логе мастера
#   «RPC: устройств в раскладке 2».

[CmdletBinding()]
param(
    # Обязательный параметр: SSH-цель донора задаётся оператором, в репозитории
    # не хранится (личные данные не коммитятся). Формат: user@host или
    # user@email@host для Windows-учёток с '@' в имени.
    [string]$SshTarget = '',
    [string]$KeyPath = "$env:USERPROFILE\.ssh\id_ed25519_donor",
    [string]$Container = 'cppworker-node2',
    [string]$NewImage,
    [string]$BackupSuffix = '-bak',
    [string]$EnvFileOnNode = 'C:\cppworker-node2-upgrade.env',
    # 50052 — порт УЗЛА РАСКЛАДКИ (rpc-server). Без его публикации приказ
    # балансера «подними узел» выполняется (процесс работает, слушает внутри
    # контейнера), но с балансера узел недоступен: running=true, listening=true,
    # reachable=false, и раскладка деградирует в локальную. Проверено живьём
    # 2026-10-17: контейнер без -p 50052 → «узел поднят, но с балансера
    # недоступен»; с -p 50052 → reachable=true.
    [string[]]$Ports = @('18034:18034', '18092:18092', '50052:50052'),
    [string[]]$Volumes = @('node2-cache:/app/.cache', '/run/desktop/mnt/host/c/ollama/ollamalegion/models:/app/models'),
    [string]$Runtime = 'nvidia',
    [int]$HealthyWaitSec = 90,
    [switch]$DryRun,
    # R168 (2026-10-17): перенести образ на узел (docker save -> scp -> docker load).
    # Без этого скрипт пересоздавал контейнер на СТАРОМ образе, если оператор забыл
    # скопировать сборку руками — и раскладка расходилась по версии llama.cpp.
    [switch]$TransferImage,
    # R171 (2026-10-17): дополнительные env-ключи для узла (заменяют одноимённые
    # из старого контейнера). Нужны, например, для режима внешнего узла раскладки:
    #   -ExtraEnv 'CPPWORKER_RPC_EXTERNAL_ADDR=192.0.2.10:50055'
    # Без них пришлось бы править env-файл руками на узле после каждого обновления.
    [string[]]$ExtraEnv = @(),
    # R173 (2026-10-18): пересоздать контейнер, даже если образ НЕ изменился.
    #
    # ЗАЧЕМ. Живой случай: после проверки исправления нужно было убрать
    # отладочный режим узла (CPPWORKER_RPC_SERVER_DEBUG) и вернуть автоперезапуск
    # по умолчанию — образ тот же, но настройки другие. Скрипт видел «образ уже
    # такой — обновлять нечего» и выходил, а ExtraEnv применить было негде.
    [switch]$Force,
    # R118.17: проверка ЧИСТОЙ логики выбора имени бэкапа без обращения к узлу
    # (узел может быть выключен, а логика обязана быть проверяемой).
    [switch]$SelfTest
)

$ErrorActionPreference = 'Stop'

# SSH-цель обязательна и не хранится в репозитории (личные данные не коммитятся).
if (-not $SelfTest -and -not $SshTarget) {
    throw "Укажите -SshTarget '<ssh-user>@<donor-host>' (в репозитории личных данных нет)."
}
if (-not $SelfTest -and $SshTarget -notmatch '@') {
    throw "Некорректный -SshTarget '$SshTarget': ожидается user@host (или user@email@host для Windows-учёток)."
}

# Переменные образа, которые НЕ надо переносить в env-файл: их задаёт базовый
# образ (CUDA), и повторное задание PATH/LD_LIBRARY_PATH может сломать запуск.
$imageOwnedEnv = @(
    'PATH', 'LD_LIBRARY_PATH', 'NVARCH', 'NVIDIA_REQUIRE_CUDA', 'NVIDIA_PRODUCT_NAME',
    'NV_CUDA_CUDART_VERSION', 'CUDA_VERSION', 'NV_CUDA_LIB_VERSION', 'NV_NVTX_VERSION',
    'NV_LIBNPP_VERSION', 'NV_LIBNPP_PACKAGE', 'NV_LIBCUSPARSE_VERSION',
    'NV_LIBCUBLAS_PACKAGE_NAME', 'NV_LIBCUBLAS_VERSION', 'NV_LIBCUBLAS_PACKAGE',
    'NV_LIBNCCL_PACKAGE_NAME', 'NV_LIBNCCL_PACKAGE_VERSION', 'NCCL_VERSION', 'NV_LIBNCCL_PACKAGE'
)

function Invoke-Node {
    param([string]$Command, [switch]$Quiet)
    if ($DryRun) {
        Write-Host "  [dry-run] ssh $SshTarget `"$Command`"" -ForegroundColor DarkGray
        return ''
    }
    if ($Quiet) {
        return (ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget $Command 2>&1)
    }
    return (ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget $Command)
}

# Get-FreeBackupName — R118.17: имя для бэкапа, которого на узле ещё нет.
#
# ЗАЧЕМ. Прежняя версия всегда брала «<container>-bak». Второй прогон скрипта
# падал с «Error when allocating new name: Conflict. The container name
# "/cppworker-node2-bak" is already in use» — причём ПОСЛЕ `docker stop`,
# поэтому узел оставался остановленным, и разбираться приходилось руками
# (живой случай 2026-10-15). Теперь имя подбирается с меткой времени, а старый
# бэкап не перетирается.
function Get-FreeBackupName {
    param(
        [string]$Container,
        [string]$Suffix,
        [string[]]$Existing
    )
    $base = "$Container$Suffix"
    if ($Existing -notcontains $base) { return $base }
    $stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
    $candidate = "$base-$stamp"
    $i = 2
    while ($Existing -contains $candidate) {
        $candidate = "$base-$stamp-$i"
        $i++
    }
    return $candidate
}

# Get-NodeContainerNames — имена контейнеров на узле (пустой список, если узел
# недоступен: тогда выбирается базовое имя, как раньше).
function Get-NodeContainerNames {
    $raw = Invoke-Node "docker ps -a --format {{.Names}}" -Quiet
    if (-not $raw) { return @() }
    return @($raw | ForEach-Object { "$_".Trim() } | Where-Object { $_ })
}

if ($SelfTest) {
    Write-Host '=== SelfTest: выбор имени бэкапа (без обращения к узлу) ===' -ForegroundColor Cyan
    $cases = @(
        @{ name = 'имя свободно → базовое';      existing = @('other');            want = 'cppworker-node2-bak' },
        @{ name = 'базовое занято → с меткой';   existing = @('cppworker-node2-bak'); want = 'cppworker-node2-bak-' },
        @{ name = 'пусто на узле → базовое';     existing = @();                   want = 'cppworker-node2-bak' }
    )
    $fail = 0
    foreach ($c in $cases) {
        $got = Get-FreeBackupName -Container 'cppworker-node2' -Suffix '-bak' -Existing $c.existing
        $ok = if ($c.want -eq 'cppworker-node2-bak-') { $got -like 'cppworker-node2-bak-*' -and $got -ne 'cppworker-node2-bak' }
              else { $got -eq $c.want }
        Write-Host ("  [{0}] {1}: got={2}" -f $(if ($ok) { 'PASS' } else { 'FAIL' }), $c.name, $got)
        if (-not $ok) { $fail++ }
    }
    # Повторный вызов при занятом базовом и занятом «с меткой» — обязан дать новое имя.
    $first = Get-FreeBackupName -Container 'c' -Suffix '-bak' -Existing @('c-bak')
    $second = Get-FreeBackupName -Container 'c' -Suffix '-bak' -Existing @('c-bak', $first)
    $unique = ($second -ne $first)
    Write-Host ("  [{0}] повторный вызов даёт другое имя: {1} / {2}" -f $(if ($unique) { 'PASS' } else { 'FAIL' }), $first, $second)
    if (-not $unique) { $fail++ }
    # R159 (2026-10-17): порт узла раскладки ОБЯЗАН быть в дефолтном списке.
    # Контрпример: узел, поднятый по приказу балансера в контейнере без
    # -p 50052, слушает только внутри — балансер видит running=true и честно
    # пишет «недоступен», а раскладка уходит в локальную. Это свойство ЗАПУСКА
    # контейнера, из процесса его не исправить, поэтому проверяем дефолт.
    $rpcPort = $Ports -contains '50052:50052'
    Write-Host ("  [{0}] дефолтные порты содержат 50052:50052: {1}" -f $(if ($rpcPort) { 'PASS' } else { 'FAIL' }), ($Ports -join ','))
    if (-not $rpcPort) { $fail++ }
    if ($fail -gt 0) { Write-Host "SelfTest: FAIL ($fail)" -ForegroundColor Red; exit 1 }
    Write-Host 'SelfTest: PASS' -ForegroundColor Green
    exit 0
}


if (-not $NewImage) {
    throw 'укажите -NewImage <образ> (для проверки логики без узла: -SelfTest)'
}

Write-Host "=== Обновление узла $SshTarget / $Container → $NewImage ===" -ForegroundColor Cyan

# --- 1. Снимаем конфигурацию работающего контейнера -------------------------
# В -DryRun узел не дёргаем: план печатается без обращения к нему (раньше
# dry-run всё равно ходил по ssh и падал, если узел выключен).
Write-Host '1. Конфигурация текущего контейнера'
$oldImage = '<не читаем в dry-run>'
if ($DryRun) {
    Write-Host '   [dry-run] docker inspect на узле не выполняется' -ForegroundColor DarkGray
} else {
    $raw = (ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget "docker inspect $Container") | Out-String
    if (-not $raw.Trim()) { throw "не удалось прочитать docker inspect $Container на узле" }
    $cur = ($raw | ConvertFrom-Json)[0]
    $oldImage = $cur.Config.Image
    if ($oldImage -eq $NewImage -and -not $Force) { Write-Host "   образ уже $NewImage — обновлять нечего (пересоздать с новыми настройками: -Force)" -ForegroundColor Yellow; return }
}
$appEnv = @()
if (-not $DryRun) {
    $appEnv = $cur.Config.Env | Where-Object {
        $name = ($_ -split '=', 2)[0]
        $imageOwnedEnv -notcontains $name
    }
}
Write-Host "   было : $oldImage"
Write-Host "   станет: $NewImage (переносим $($appEnv.Count) переменных окружения)"

# --- 2. Готовим env-файл ----------------------------------------------------
$envBody = @(
    "# Сгенерировано scripts/upgrade-donor-node.ps1 $(Get-Date -Format 'yyyy-MM-dd HH:mm')",
    "# Узел: $SshTarget, контейнер: $Container, образ: $NewImage"
) + $appEnv
# R171 (2026-10-17): операторские дополнения к env узла. Ключ заменяется целиком:
# иначе в env-файле окажутся и старое значение из контейнера, и новое — и какое
# из них победит, зависит от порядка строк (это ровно тот класс «непонятно, что
# применилось», с которым уже воевали).
foreach ($pair in $ExtraEnv) {
    if ([string]::IsNullOrWhiteSpace($pair) -or ($pair -notmatch '=')) { continue }
    $key = ($pair -split '=', 2)[0].Trim()
    if ([string]::IsNullOrWhiteSpace($key)) { continue }
    $envBody = @($envBody | Where-Object { (($_ -split '=', 2)[0]).Trim() -ne $key })
    $envBody += $pair.Trim()
    Write-Host "   доп. env: $key=" + (($pair -split '=', 2)[1])
}

# R168 (2026-10-17): КОММИТ llama.cpp в env-файле берём из НОВОГО образа, а не из
# старого контейнера.
#
# ЗАЧЕМ. env-файл собирается с работающего контейнера, поэтому после обновления
# образа в нём оставался прежний CPPWORKER_LLAMA_BUILD. Узел поднимался с новым
# бинарником и старым значением в env — и сверка «что собрано vs что заявлено»
# (R167) честно писала mismatch, а version_mismatch мешал раскладке. Значение у
# нового образа уже есть — печётся как CPPWORKER_LLAMA_BUILD_BAKED.
if (-not $DryRun -and $NewImage) {
    $imgEnv = (docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' $NewImage 2>$null) -join "`n"
    $baked = ''
    foreach ($line in ($imgEnv -split "`n")) {
        if ($line -match '^CPPWORKER_LLAMA_BUILD_BAKED=(.+)$') { $baked = $Matches[1].Trim() }
    }
    if ($baked) {
        $envBody = @($envBody | Where-Object { $_ -notmatch '^CPPWORKER_LLAMA_BUILD=' })
        $envBody += "CPPWORKER_LLAMA_BUILD=$baked"
        Write-Host "   llama.cpp из образа: $baked (env-файл синхронизирован)"
    } else {
        Write-Host "   ВНИМАНИЕ: в образе нет CPPWORKER_LLAMA_BUILD_BAKED (старая сборка) — env не трогаю" -ForegroundColor Yellow
    }
}
$tmp = Join-Path $env:TEMP ("upgrade-donor-node-{0}.env" -f (Get-Date -Format 'HHmmss'))
$envBody | Set-Content -Path $tmp -Encoding ASCII

Write-Host "2. env-файл для узла: $EnvFileOnNode"
if (-not $DryRun) {
    $envBody | ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget "more > $EnvFileOnNode"
    Write-Host "   перенесён ($($appEnv.Count) строк)"
} else {
    Write-Host "   [dry-run] перенос $($appEnv.Count) строк через stdin" -ForegroundColor DarkGray
}

# --- 2b. Перенос образа на узел (R168, 2026-10-17) --------------------------
#
# ЗАЧЕМ. Скрипт пересоздаёт контейнер на узле, но САМ образ туда не переносил:
# оператор копировал его руками (docker save -> scp -> docker load), и это
# забывалось — узел поднимался на старой сборке, а раскладка потом расходилась по
# версии llama.cpp. Путь по сети «docker save | ssh docker load» НЕ используется:
# бинарный поток через ssh на Windows портится (проверено), поэтому только
# save в файл -> scp -> load.
if ($TransferImage) {
    Write-Host "2b. Перенос образа $NewImage на узел"
    if (-not $DryRun) {
        $present = (Invoke-Node "docker images -q $NewImage" -Quiet) -join ''
        if ($present.Trim()) {
            Write-Host "   образ уже есть на узле — перенос не нужен"
        } else {
            $local = (docker images -q $NewImage) -join ''
            if (-not $local.Trim()) {
                throw "образ $NewImage не найден локально: сначала соберите его (scripts/build-containers.ps1)"
            }
            $tar = Join-Path $env:TEMP ("donor-image-{0}.tar" -f (Get-Date -Format 'HHmmss'))
            Write-Host "   docker save -> $tar"
            docker save -o $tar $NewImage
            if ($LASTEXITCODE -ne 0) { throw "docker save failed ($LASTEXITCODE)" }
            $sizeGb = [math]::Round((Get-Item $tar).Length / 1GB, 2)
            Write-Host "   scp -> узел ($sizeGb ГБ)"
            scp -i $KeyPath -o StrictHostKeyChecking=no $tar "${SshTarget}:C:/donor-image.tar"
            if ($LASTEXITCODE -ne 0) { throw "scp failed ($LASTEXITCODE)" }
            Write-Host "   docker load на узле"
            $loadOut = (Invoke-Node "docker load -i C:\donor-image.tar" -Quiet) -join "`n"
            Write-Host "   $($loadOut.Trim())"
            Invoke-Node "del /q C:\donor-image.tar" -Quiet | Out-Null
            Remove-Item $tar -Force -ErrorAction SilentlyContinue
        }
    } else {
        Write-Host "   [dry-run] docker save -> scp -> docker load $NewImage" -ForegroundColor DarkGray
    }
}

# --- 3. Старый контейнер — в бэкап, новый — с нового образа ----------------
#
# R118.17: имя бэкапа выбирается свободным (Get-FreeBackupName), а остановка и
# переименование обёрнуты в try/catch: если переименование всё-таки не удалось,
# исходный контейнер возвращается в работу, а не остаётся остановленным.
$backupName = Get-FreeBackupName -Container $Container -Suffix $BackupSuffix -Existing (Get-NodeContainerNames)
Write-Host "3. Пересоздание: $Container → $backupName, затем новый $Container"
try {
    Invoke-Node "docker stop $Container" | Out-Null
    Invoke-Node "docker rename $Container $backupName" | Out-Null
} catch {
    Write-Warning "шаг 3 упал ($($_.Exception.Message)) — возвращаю контейнер $Container в работу"
    Invoke-Node "docker start $Container" | Out-Null
    throw
}

$portArgs = ($Ports | ForEach-Object { "-p $_" }) -join ' '
$volArgs = ($Volumes | ForEach-Object { "-v $_" }) -join ' '
$runCmd = "docker run -d --name $Container --restart unless-stopped --runtime $Runtime " +
          "$portArgs $volArgs --env-file $EnvFileOnNode $NewImage"
Invoke-Node $runCmd

if ($DryRun) {
    Write-Host "`n[dry-run] Откат был бы: docker stop $Container && docker rm $Container && docker rename $backupName $Container && docker start $Container" -ForegroundColor DarkGray
    Write-Host '[dry-run] Готово (ничего не изменено).' -ForegroundColor Yellow
    return
}

# --- 4. Ждём healthy --------------------------------------------------------
Write-Host "4. Ожидание healthy (до $HealthyWaitSec c)"
$healthy = $false
$deadline = (Get-Date).AddSeconds($HealthyWaitSec)
while ((Get-Date) -lt $deadline) {
    Start-Sleep -Seconds 5
    $status = (ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget "docker inspect $Container --format {{.State.Health.Status}}") -join ''
    Write-Host "   статус: $status"
    if ($status.Trim() -eq 'healthy') { $healthy = $true; break }
}
if (-not $healthy) {
    Write-Warning "узел не стал healthy за $HealthyWaitSec c — откат: docker stop $Container; docker rm $Container; docker rename $backupName $Container; docker start $Container"
    exit 1
}

# --- 5. Совместимость раскладки: хэш rpc-server ----------------------------
Write-Host '5. Хэш /app/rpc-server (правило: один коммит llama.cpp на всех узлах раскладки)'
$nodeHash = (ssh -i $KeyPath -o StrictHostKeyChecking=no $SshTarget "docker exec $Container sha256sum /app/rpc-server") -join ''
Write-Host "   узел: $($nodeHash.Trim())"
Write-Host '   Сравните с мастером: docker exec <мастер> sha256sum /app/rpc-server'
Write-Host "   ВАЖНО (R173): хэш НЕ обязан совпадать — в бинарь вшита метаинформация" 
Write-Host "   сборки (коммит/дата/компилятор), поэтому две сборки одного и того же"
Write-Host "   llama.cpp дают разные хэши. Совместимость раскладки определяется"
Write-Host "   версией llama.cpp: сверяйте /api/info -> llama_build у узла и мастера"
Write-Host "   (R167: llama_build_mismatch) плюс версию протокола в логе узла"
Write-Host "   (Starting RPC server v...). Хэш здесь — только для сравнения сборок."

Write-Host "`nГотово. Проверка: scripts/balancer-scenarios.ps1 (все сценарии PASS)." -ForegroundColor Green
Write-Host "Откат: ssh $SshTarget `"docker stop $Container && docker rm $Container && docker rename $backupName $Container && docker start $Container`""
