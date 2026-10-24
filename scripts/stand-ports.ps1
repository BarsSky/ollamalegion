<#
.SYNOPSIS
    Порты стенда OllamaLegion: открыть в фаерволе Windows и/или продиагностировать чужие.

.DESCRIPTION
    Зачем это вообще нужно. Windows-фаервол по умолчанию ЗАПРЕЩАЕТ входящие и
    РАЗРЕШАЕТ исходящие. Из этого следует неочевидное:

      * агент на второй машине регистрируется в балансере САМ — это ИСХОДЯЩЕЕ
        соединение, и правила ему не нужны;
      * а вот всё, что идёт В ОБРАТНУЮ сторону — балансер к cppworker (18092),
        мастер раскладки к rpc-server (50052), Вы к WebUI (18083) — входящее
        на ту машину, и без правила оно молча дропается (таймаут, не «отказ»).

    Порты, публикуемые Docker Desktop, обычно получают правило автоматически, но
    полагаться на это нельзя: у iperf3 правила не будет вовсе (это не контейнер),
    а на стенде автоправила уже не появились.

    Скрипт запускается НА ТОЙ машине, порты которой нужно открыть. Для второй
    машины — сначала WinRM/SSH до неё (иначе её и не открыть, и не развернуть).

    Три действия:
      Open   (по умолчанию) — создать входящие правила, по одному на порт,
                              идемпотентно (уже есть — не трогаем без -Force);
      Remove                — удалить правила, созданные этим скриптом;
      Probe                 — проверить ЧУЖИЕ порты и отличить три состояния:
                              connected / refused (хост жив, никто не слушает) /
                              timeout (тишина: фаервол DROP или хост недоступен).

    Права администратора нужны для Open/Remove; Probe работает без них.

.PARAMETER Action
    Open | Remove | Probe. По умолчанию Open.

.PARAMETER Target
    Хост для -Action Probe (IP или имя). Обязателен для Probe.

.PARAMETER Ports
    Порты. По умолчанию — весь набор стенда (см. $standPorts в начале файла).

.PARAMETER Prefix
    Префикс имени правил и группа. По умолчанию OllamaLegion.

.PARAMETER RemoteAddress
    Откуда разрешены входящие. LocalSubnet — только своя подсеть (безопаснее,
    чем Any). Укажите Any, если машины в разных подсетях.

.PARAMETER IncludeUdp
    Добавить UDP-правила (нужно iperf3 в UDP-режиме).

.PARAMETER IncludeSsh
    Добавить порт 22 (sshd). Нужен ровно один раз — чтобы вообще попасть на машину
    и разложить ключ; дальше развёртывание идёт уже по SSH. В набор по умолчанию
    НЕ входит: открывать удалённый вход на каждой машине стенда молча — плохая идея.

.PARAMETER Force
    Пересоздать правила, даже если они уже есть (для Open).

.PARAMETER TimeoutMs
    Таймаут одной пробы для Probe. По умолчанию 1500 мс.

.EXAMPLE
    # Что сейчас происходит с портами второй машины (без прав администратора)
    powershell -File scripts\stand-ports.ps1 -Action Probe -Target 192.0.2.10

.EXAMPLE
    # Открыть порты стенда на ЭТОЙ машине (в консоли администратора)
    powershell -File scripts\stand-ports.ps1

.EXAMPLE
    # Показать, что было бы сделано, ничего не меняя
    powershell -File scripts\stand-ports.ps1 -WhatIf

.EXAMPLE
    # Убрать правила, созданные скриптом
    powershell -File scripts\stand-ports.ps1 -Action Remove
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [ValidateSet('Open', 'Remove', 'Probe')]
    [string]$Action = 'Open',
    [string]$Target = '',
    [int[]]$Ports = @(),
    [string]$Prefix = 'OllamaLegion',
    [string]$RemoteAddress = 'LocalSubnet',
    [switch]$IncludeUdp,
    [switch]$IncludeSsh,
    [switch]$Force,
    [int]$TimeoutMs = 1500
)

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }

# $standPorts — порты стенда и зачем каждый. Комментарий рядом с портом важнее
# самого порта: через месяц никто не вспомнит, что 50052 — это rpc-server.
#
# КЛЮЧИ — СТРОКИ, и это не стилистика: у [ordered]-словаря в PowerShell 5.1
# индексатор Item[int] ПОЗИЦИОННЫЙ, поэтому $d[18092] ищет 18092-й элемент и
# возвращает пусто. Ищем через $d["$port"].
$standPorts = [ordered]@{
    '18079' = 'балансер: OpenAI-поверхность (/v1)'
    '18080' = 'балансер: основной LB-порт'
    '18081' = 'балансер: management API'
    '18083' = 'WebUI'
    '18092' = 'cppworker (llama.cpp)'
    '18093' = 'imageworker (sd.cpp)'
    '18094' = 'sd-server (внутренний порт imageworker)'
    '18033' = 'встроенный агент метрик imageworker'
    # 18032 и 18034 — агенты. На ОДНОЙ машине они не нужны: балансер ходит в них
    # по имени compose-сервиса внутри общей сети. Но на ОТДЕЛЬНОЙ машине
    # балансеру больше нечем дотянуться до агента (AGENT_PUBLIC_HOST = адрес
    # машины), поэтому узел обязан публиковать и открывать эти порты.
    '18032' = 'агент внешним контейнером (on another machine)'
    '18034' = 'встроенный агент метрик узла (AGENT_EMBEDDED_PORT)'
    '50052' = 'RPC-узел раскладки (llama.cpp rpc-server)'
    '5201'  = 'iperf3 — измерение сети между машинами (шаг 0 плана)'
}

# Get-PortPurpose — зачем порт. 22 живёт отдельно от $standPorts: он нужен не
# стенду, а нам, чтобы на машину вообще попасть.
function Get-PortPurpose {
    param([int]$Port, [string]$Fallback = '')
    if ($Port -eq 22) { return 'SSH — канал управления (sshd Windows)' }
    $p = "$($standPorts["$Port"])"
    if ($p) { return $p }
    return $Fallback
}

function Test-IsAdmin {
    try {
        $id = [Security.Principal.WindowsIdentity]::GetCurrent()
        return (New-Object Security.Principal.WindowsPrincipal($id)).IsInRole(
            [Security.Principal.WindowsBuiltInRole]::Administrator)
    } catch { return $false }
}

# --- Probe ------------------------------------------------------------------
# Отличаем три состояния. Разница принципиальная:
#   refused — хост ОТВЕТИЛ, просто никто не слушает (порт закрыт приложением);
#   timeout — тишина: пакет дропнут фаерволом или хост недоступен.
# Без этого различия «порт не отвечает» уводит искать не там.
if ($Action -eq 'Probe') {
    if (-not $Target) { Write-Host 'для -Action Probe нужен -Target <хост>' -ForegroundColor Red; exit 2 }
    $probePorts = if ($Ports.Count -gt 0) { $Ports } else { @($standPorts.Keys | ForEach-Object { [int]$_ }) }
    if ($IncludeSsh -and ($probePorts -notcontains 22)) { $probePorts += 22 }
    Write-Host "=== Проба портов $Target (таймаут $TimeoutMs мс) ===" -ForegroundColor Cyan
    $summary = @{ connected = 0; refused = 0; timeout = 0; other = 0 }
    foreach ($p in $probePorts) {
        $client = New-Object System.Net.Sockets.TcpClient
        $verdict = ''
        try {
            $ar = $client.BeginConnect($Target, $p, $null, $null)
            if ($ar.AsyncWaitHandle.WaitOne($TimeoutMs, $false)) {
                try {
                    $client.EndConnect($ar)
                    $verdict = 'CONNECTED'; $summary.connected++
                } catch [System.Net.Sockets.SocketException] {
                    if ($_.Exception.SocketErrorCode -eq 'ConnectionRefused') {
                        $verdict = 'refused  — хост ответил, слушать некому'; $summary.refused++
                    } else {
                        $verdict = 'socket: ' + $_.Exception.SocketErrorCode; $summary.other++
                    }
                }
            } else {
                $verdict = 'timeout  — тишина: фаервол DROP или хост недоступен'; $summary.timeout++
            }
        } catch {
            $verdict = 'err: ' + $_.Exception.Message; $summary.other++
        } finally { $client.Close() }
        $purpose = Get-PortPurpose -Port $p
        Write-Host ("  {0,-6} {1,-52} {2}" -f $p, $verdict, $purpose)
    }
    Write-Host ("--- connected={0} refused={1} timeout={2} прочее={3} ---" -f `
        $summary.connected, $summary.refused, $summary.timeout, $summary.other) -ForegroundColor Cyan
    if ($summary.timeout -gt 0 -and $summary.connected -eq 0) {
        Write-Host 'Все порты в таймауте: похоже на DROP входящих. Проверьте правила:' -ForegroundColor Yellow
        Write-Host '  Get-NetFirewallRule -Group OllamaLegion | Format-Table DisplayName,Enabled,Direction,Action' -ForegroundColor Yellow
        Write-Host 'и что контейнеры на той машине вообще запущены: docker ps' -ForegroundColor Yellow
    }
    if ($summary.connected -gt 0) {
        Write-Host 'Хотя бы один порт отвечает — значит хост доступен, а фаервол не рубит всё подряд.' -ForegroundColor Green
    }
    exit 0
}

# --- Open/Remove ------------------------------------------------------------
$rulePorts = if ($Ports.Count -gt 0) { $Ports } else { @($standPorts.Keys | ForEach-Object { [int]$_ }) }
if ($IncludeSsh -and ($rulePorts -notcontains 22)) { $rulePorts += 22 }

if (-not (Test-IsAdmin)) {
    Write-Host "$Action требует прав администратора (правила фаервола)." -ForegroundColor Red
    Write-Host 'Запустите PowerShell от имени администратора:' -ForegroundColor Yellow
    Write-Host ("  powershell -File `"{0}`" -Action {1}" -f $PSCommandPath, $Action) -ForegroundColor Yellow
    if ($WhatIfPreference) {
        Write-Host '(сейчас -WhatIf: показываю план и выхожу)' -ForegroundColor Yellow
    } else {
        exit 3
    }
}

if ($Action -eq 'Open') {
    Write-Host "=== Открываю входящие порты стенда (группа $Prefix, откуда: $RemoteAddress) ===" -ForegroundColor Cyan
    foreach ($p in $rulePorts) {
        $name = "$Prefix-$p"
        $purpose = Get-PortPurpose -Port $p -Fallback 'порт стенда'
        $existing = Get-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue
        if ($existing -and -not $Force) {
            Write-Host ("  [{0}] уже есть — пропускаю ({1})" -f $p, $purpose) -ForegroundColor DarkGray
            continue
        }
        if ($existing -and $Force) {
            if ($PSCmdlet.ShouldProcess($name, 'удалить старое правило')) {
                Remove-NetFirewallRule -DisplayName $name -ErrorAction SilentlyContinue
            }
        }
        $params = @{
            DisplayName    = $name
            Group          = $Prefix
            Description    = "OllamaLegion: $purpose"
            Direction      = 'Inbound'
            Action         = 'Allow'
            Protocol       = 'TCP'
            LocalPort      = $p
            RemoteAddress  = $RemoteAddress
            Profile        = 'Any'
            Enabled        = 'True'
        }
        if ($PSCmdlet.ShouldProcess($name, "разрешить TCP $p ($purpose)")) {
            try {
                New-NetFirewallRule @params | Out-Null
                Write-Host ("  [{0}] открыт — {1}" -f $p, $purpose) -ForegroundColor Green
                if ($IncludeUdp -or $p -eq 5201) {
                    $udpName = "$Prefix-$p-udp"
                    if (-not (Get-NetFirewallRule -DisplayName $udpName -ErrorAction SilentlyContinue)) {
                        $params['DisplayName'] = $udpName
                        $params['Protocol'] = 'UDP'
                        New-NetFirewallRule @params | Out-Null
                        Write-Host ("  [{0}] открыт UDP (iperf3 -u)" -f $p) -ForegroundColor Green
                    }
                }
            } catch {
                Write-Host ("  [{0}] ОШИБКА: {1}" -f $p, $_.Exception.Message) -ForegroundColor Red
            }
        }
    }
    Write-Host '--- итог (правила группы) ---' -ForegroundColor Cyan
    Get-NetFirewallRule -Group $Prefix -ErrorAction SilentlyContinue |
        Select-Object DisplayName, Enabled, Direction, Action | Format-Table -AutoSize | Out-String | Write-Host
    Write-Host 'Проверить с ДРУГОЙ машины: powershell -File scripts\stand-ports.ps1 -Action Probe -Target <этот-хост>'
    Write-Host 'ВАЖНО: пока порты открыты, management API балансера (18081) виден в подсети — токен обязателен (auth.enabled=true).'
}

if ($Action -eq 'Remove') {
    $rules = Get-NetFirewallRule -Group $Prefix -ErrorAction SilentlyContinue
    if (-not $rules) { Write-Host "правил группы $Prefix нет — нечего удалять" -ForegroundColor Yellow; exit 0 }
    foreach ($r in $rules) {
        if ($PSCmdlet.ShouldProcess($r.DisplayName, 'удалить правило')) {
            Remove-NetFirewallRule -DisplayName $r.DisplayName -ErrorAction SilentlyContinue
            Write-Host ("  удалено: {0}" -f $r.DisplayName) -ForegroundColor Green
        }
    }
}
