<#
.SYNOPSIS
    Разбудить машину стенда по Wake-on-LAN и проверить, что она вернулась.

.DESCRIPTION
    Зачем это нужно. Машина стенда (вторая нода) уходит в сон по таймауту
    электропитания Windows — и вместе с ней уходит весь стенд: `ssh` и пробники
    получают «Connection timed out», а ARP-запись пропадает. Снаружи это
    неотличимо от «фаервол закрыл порт», поэтому первым делом смотрим ARP:
    пропала запись — машина спит или выключена, а не «порты закрыты».

    Признак сна, который виден без доступа к машине: перестаёт отвечать даже
    порт 5040 (встроенная служба Windows «Connected Devices Platform»), открытый
    на любой живой Windows. Отвечает 5040 — машина жива и дело в правилах;
    молчит и он — машина ушла.

    Скрипт бьёт magic-пакетом по широковещательным адресам (по умолчанию —
    широковещательный адрес своей подсети и 255.255.255.255) на порты 9 и 7,
    затем ждёт и, если задан -Target, проверяет порты.

    ВАЖНО: WoL работает, только если он разрешён в BIOS/UEFI и в свойствах
    сетевого адаптера («Разрешить этому устройству выводить компьютер из ждущего
    режима»). Если пакеты уходят, а машина не встаёт — дело в этих настройках, а
    не в скрипте. Надёжнее просто запретить сон на машине стенда:
        powercfg /change standby-timeout-ac 0
        powercfg /change hibernate-timeout-ac 0

.PARAMETER Mac
    MAC-адрес машины. Формат любой: fc-9d-05-48-34-07, fc:9d:05:48:34:07,
    fc9d05483407.

.PARAMETER Target
    IP машины для проверки портов после ожидания. Без него проверка пропускается
    (будить всё равно можно — broadcast не требует знать адрес).

.PARAMETER Broadcast
    Куда бить. По умолчанию: широковещательный адрес каждой своей IPv4-подсети
    плюс 255.255.255.255.

.PARAMETER Port
    Порты WoL. По умолчанию 9 и 7 (оба общеприняты).

.PARAMETER Repeat
    Сколько раз отправить пакет. По умолчанию 3 (потери по UDP никто не чинит).

.PARAMETER WaitSec
    Сколько ждать пробуждения перед проверкой. По умолчанию 30 (0 — не ждать).

.PARAMETER ProbePort
    Порты для проверки после ожидания. По умолчанию 5040,22 — «жива» и «есть SSH».

.EXAMPLE
    powershell -File scripts\wake-host.ps1 -Mac fc-9d-05-48-34-07 -Target 192.0.2.10

.EXAMPLE
    # Разбудить и сразу проверить порты узла стенда
    powershell -File scripts\wake-host.ps1 -Mac fc-9d-05-48-34-07 -Target 192.0.2.10 `
        -ProbePort 5040,22,18092,50052,5201
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$Mac,
    [string]$Target = '',
    [string[]]$Broadcast = @(),
    [int[]]$Port = @(9, 7),
    [int]$Repeat = 3,
    [int]$WaitSec = 30,
    [int[]]$ProbePort = @(5040, 22)
)

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }

# --- MAC: принимаем любой привычный формат ---
$hex = ($Mac -replace '[^0-9A-Fa-f]', '')
if ($hex.Length -ne 12) {
    Write-Host ("MAC '$Mac' не разобрать: после чистки {0} hex-символов, нужно 12" -f $hex.Length) -ForegroundColor Red
    exit 2
}
$macBytes = New-Object byte[] 6
for ($i = 0; $i -lt 6; $i++) { $macBytes[$i] = [Convert]::ToByte($hex.Substring($i * 2, 2), 16) }
$macNorm = (($macBytes | ForEach-Object { $_.ToString('x2') }) -join '-')

# --- Куда бить: своя подсеть + глобальный broadcast ---
# Host-биты адреса забиваем единицами. Так понятнее, чем арифметика через
# BitConverter, и не зависит от порядка байт.
function Get-BroadcastAddress {
    param([string]$Ip, [int]$PrefixLength)
    $bytes = [System.Net.IPAddress]::Parse($Ip).GetAddressBytes()
    $hostBits = 32 - $PrefixLength
    for ($b = 3; $b -ge 0 -and $hostBits -gt 0; $b--) {
        $take = [Math]::Min(8, $hostBits)
        $bytes[$b] = [byte]($bytes[$b] -bor (255 -shr (8 - $take)))
        $hostBits -= $take
    }
    return ($bytes -join '.')
}

if ($Broadcast.Count -eq 0) {
    $found = New-Object System.Collections.ArrayList
    foreach ($ip in (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue)) {
        if ($ip.IPAddress -like '127.*') { continue }
        if ($ip.PrefixLength -lt 8 -or $ip.PrefixLength -gt 30) { continue }
        $bc = Get-BroadcastAddress -Ip $ip.IPAddress -PrefixLength $ip.PrefixLength
        if (-not $found.Contains($bc)) { [void]$found.Add($bc) }
    }
    [void]$found.Add('255.255.255.255')
    $Broadcast = @($found)
}

# --- Magic-пакет: 6×FF + 16×MAC ---
$packet = New-Object byte[] 102
for ($i = 0; $i -lt 6; $i++) { $packet[$i] = 0xFF }
for ($i = 0; $i -lt 16; $i++) {
    for ($j = 0; $j -lt 6; $j++) { $packet[6 + $i * 6 + $j] = $macBytes[$j] }
}

Write-Host ("=== Wake-on-LAN: {0} → {1} (порты {2}) ===" -f $macNorm, ($Broadcast -join ', '), ($Port -join ',')) -ForegroundColor Cyan
$sent = 0
foreach ($dst in $Broadcast) {
    foreach ($p in $Port) {
        for ($n = 0; $n -lt $Repeat; $n++) {
            try {
                $udp = New-Object System.Net.Sockets.UdpClient
                $udp.EnableBroadcast = $true
                $udp.Connect($dst, $p)
                [void]$udp.Send($packet, $packet.Length)
                $udp.Close()
                $sent++
            } catch {
                Write-Host ("  {0}:{1} — ошибка: {2}" -f $dst, $p, $_.Exception.Message) -ForegroundColor Red
            }
        }
    }
}
Write-Host ("отправлено пакетов: {0}" -f $sent) -ForegroundColor Green

if ($WaitSec -gt 0) {
    Write-Host ("ждём {0} c ..." -f $WaitSec)
    Start-Sleep -Seconds $WaitSec
}

if (-not $Target) {
    Write-Host 'Проверка не запрошена (-Target не задан). Проверить вручную:' -ForegroundColor Yellow
    Write-Host ("  arp -a | findstr <ip>     # запись вернулась → машина встала")
    Write-Host ("  powershell -File scripts\stand-ports.ps1 -Action Probe -Target <ip> -IncludeSsh")
    exit 0
}

Write-Host ("--- проверка {0} ---" -f $Target) -ForegroundColor Cyan
$alive = 0
foreach ($p in $ProbePort) {
    $client = New-Object System.Net.Sockets.TcpClient
    $verdict = ''
    try {
        $ar = $client.BeginConnect($Target, $p, $null, $null)
        if ($ar.AsyncWaitHandle.WaitOne(1500, $false)) {
            try { $client.EndConnect($ar); $verdict = 'CONNECTED'; $alive++ }
            catch [System.Net.Sockets.SocketException] {
                if ($_.Exception.SocketErrorCode -eq 'ConnectionRefused') { $verdict = 'refused (жива, слушать некому)' }
                else { $verdict = 'socket: ' + $_.Exception.SocketErrorCode }
            }
        } else { $verdict = 'timeout' }
    } catch { $verdict = 'err: ' + $_.Exception.Message }
    finally { $client.Close() }
    $purpose = switch ($p) { 5040 { 'жива (Windows CDP)' } 22 { 'SSH (sshd)' } default { '' } }
    Write-Host ("  {0,-6} {1,-34} {2}" -f $p, $verdict, $purpose)
}

$arp = arp -a | Select-String ([regex]::Escape($Target))
if ($arp) { Write-Host ("ARP: {0}" -f $arp.Line.Trim()) } else { Write-Host 'ARP: записи нет — машина не отвечает на ARP' -ForegroundColor Yellow }

if ($alive -eq 0) {
    Write-Host 'Ни один порт не ответил: либо машина ещё не встала, либо WoL выключен в BIOS/адаптере,' -ForegroundColor Yellow
    Write-Host 'либо адрес не тот.' -ForegroundColor Yellow
} else {
    Write-Host 'Машина отвечает.' -ForegroundColor Green
}
