<#
.SYNOPSIS
    Замер пропускной способности канала между машинами стенда — без внешних утилит.

.DESCRIPTION
    Зачем свой замер, если есть iperf3. Шаг 0 плана распределения требует цифру
    «сколько реально пропускает сеть» (в §3 заложено 112 МБ/с для 1 Гбит/с).
    На этом стенде iperf3 не годится: сборка под Windows — Cygwin, и на Windows
    11 сборки 26300 сервер принимает управляющее и потоковое соединения, а затем
    падает с «unable to send control message ... Bad file descriptor». Проверено
    в трёх вариантах запуска (WMI, живая SSH-сессия, с правилом «разрешить всё из
    подсети»), то есть это не фаервол и не способ запуска, а сама сборка.
    Поэтому канал меряется сокетами .NET: никакого Cygwin, обе стороны под нашим
    контролем, а числа — те же самые (байты/секунды по TCP).

    Замеряются оба направления, потому что они разные по смыслу: prefill и веса
    идут на узел (Upload), а токены возвращаются (Download). В §3 считаются обе
    величины, и потолок у них общий — канал.

    Роли:
      Server — слушает порт и обслуживает подключения по очереди;
      Client — подключается, гоняет трафик и печатает результат.

    Протокол намеренно примитивный: клиент шлёт один байт 'D' (сервер отдаёт)
    или 'U' (сервер принимает), дальше идёт поток. Сервер закрывает соединение
    сам — клиент читает до конца потока и по этому моменту считает время.

.PARAMETER Mode
    Server или Client.

.PARAMETER Port
    Порт. По умолчанию 5201 (он уже открыт правилом OllamaLegion-5201).

.PARAMETER Target
    Адрес сервера (только для Client).

.PARAMETER Megabytes
    Объём одного замера. По умолчанию 500 МБ (~4.5 с на 1 Гбит/с).

.PARAMETER Direction
    Download (сервер отдаёт), Upload (сервер принимает) или Both.
    По умолчанию Both — так видно, симметричен ли канал.

.PARAMETER LogPath
    Файл журнала сервера. По умолчанию — рядом со скриптом, nettest.log.

.PARAMETER BufferKB
    Размер буфера чтения/записи. По умолчанию 256 КБ (крупный буфер убирает
    влияние размера окна на коротких замерах).

.EXAMPLE
    # на машине-сервере (запускать так, чтобы процесс пережил закрытие SSH):
    powershell -File scripts\measure-network.ps1 -Mode Server

.EXAMPLE
    # на клиенте:
    powershell -File scripts\measure-network.ps1 -Mode Client -Target 192.0.2.10
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Server', 'Client')]
    [string]$Mode,
    [int]$Port = 5201,
    [string]$Target = '',
    [int]$Megabytes = 500,
    [ValidateSet('Download', 'Upload', 'Both')]
    [string]$Direction = 'Both',
    [string]$LogPath = '',
    [int]$BufferKB = 256,
    [int]$ConnectTimeoutSec = 10
)

$ErrorActionPreference = 'Continue'
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch { }

$MB = 1MB
$bufSize = $BufferKB * 1KB
$total = [int64]$Megabytes * $MB

# Write-Line — и в консоль, и в журнал: сервер часто запускают отцепленным, и
# консоли у него нет вовсе, поэтому журнал — единственный способ увидеть итог.
function Write-Line {
    param([string]$Text)
    Write-Output $Text
    if ($script:logPath) {
        try { Add-Content -LiteralPath $script:logPath -Value $Text -Encoding ascii } catch { }
    }
}

# Format-Rate — одна строка результата в машинночитаемом виде.
function Format-Rate {
    param([string]$Dir, [int64]$Bytes, [double]$Seconds)
    if ($Seconds -le 0) { $Seconds = 0.000001 }
    $mbps = $Bytes / $MB / $Seconds
    $mbits = $mbps * 8
    return ("RESULT dir={0} bytes={1} seconds={2:N3} MBps={3:N1} Mbps={4:N0}" -f $Dir, $Bytes, $Seconds, $mbps, $mbits)
}

if (-not $LogPath) { $LogPath = Join-Path $PSScriptRoot 'nettest.log' }
$script:logPath = if ($Mode -eq 'Server') { $LogPath } else { '' }

# --- Server ------------------------------------------------------------------
if ($Mode -eq 'Server') {
    $listener = New-Object System.Net.Sockets.TcpListener([System.Net.IPAddress]::Any, $Port)
    $listener.Start()
    Write-Line ("{0} server listening on 0.0.0.0:{1} (log: {2})" -f (Get-Date -Format 'HH:mm:ss'), $Port, $LogPath)

    while ($true) {
        $client = $listener.AcceptTcpClient()
        $peer = ''
        try {
            $peer = $client.Client.RemoteEndPoint.ToString()
            $client.NoDelay = $true
            $client.SendBufferSize = 1MB
            $client.ReceiveBufferSize = 1MB
            $stream = $client.GetStream()

            $cmd = $stream.ReadByte()
            if ($cmd -lt 0) { continue }
            $ch = [char]$cmd

            if ($ch -eq 'D') {
                # Сервер отдаёт поток. Сколько именно — говорит КЛИЕНТ (8 байт
                # длины сразу после команды): иначе объём отдачи задавался бы
                # настройкой сервера, и «-Megabytes 300» на клиенте получало бы
                # 500 МБ по умолчанию сервера — замер сравнивал бы разные вещи.
                $lb = New-Object byte[] 8
                $got = 0
                while ($got -lt 8) {
                    $n = $stream.Read($lb, $got, 8 - $got)
                    if ($n -le 0) { break }
                    $got += $n
                }
                if ($got -lt 8) { continue }
                $want = [System.BitConverter]::ToInt64($lb, 0)

                # Буфер заполняем случайными данными: так исключаем любое сжатие
                # на пути (иначе замер завышается).
                $chunk = New-Object byte[] $bufSize
                (New-Object System.Random 12345).NextBytes($chunk)
                $sw = [System.Diagnostics.Stopwatch]::StartNew()
                $sent = [int64]0
                while ($sent -lt $want) {
                    $n = [Math]::Min([int64]$chunk.Length, $want - $sent)
                    $stream.Write($chunk, 0, [int]$n)
                    $sent += $n
                }
                $stream.Flush()
                $sw.Stop()
                Write-Line ("{0} {1} {2}" -f (Get-Date -Format 'HH:mm:ss'), $peer, (Format-Rate -Dir 'send' -Bytes $sent -Seconds $sw.Elapsed.TotalSeconds))
            }
            elseif ($ch -eq 'U') {
                # Сервер принимает поток до закрытия клиентом.
                $buf = New-Object byte[] $bufSize
                $sw = [System.Diagnostics.Stopwatch]::StartNew()
                $got = [int64]0
                while ($true) {
                    $n = $stream.Read($buf, 0, $buf.Length)
                    if ($n -le 0) { break }
                    $got += $n
                }
                $sw.Stop()
                Write-Line ("{0} {1} {2}" -f (Get-Date -Format 'HH:mm:ss'), $peer, (Format-Rate -Dir 'recv' -Bytes $got -Seconds $sw.Elapsed.TotalSeconds))
            }
            else {
                Write-Line ("{0} {1} неизвестная команда '{2}'" -f (Get-Date -Format 'HH:mm:ss'), $peer, $ch)
            }
        } catch {
            Write-Line ("{0} {1} ошибка: {2}" -f (Get-Date -Format 'HH:mm:ss'), $peer, $_.Exception.Message)
        } finally {
            try { $client.Close() } catch { }
        }
    }
}

# --- Client ------------------------------------------------------------------
if (-not $Target) { Write-Host 'для -Mode Client нужен -Target <адрес>' -ForegroundColor Red; exit 2 }

function Invoke-Direction {
    param([char]$Cmd, [string]$Name)

    $client = New-Object System.Net.Sockets.TcpClient
    try {
        $iar = $client.BeginConnect($Target, $Port, $null, $null)
        if (-not $iar.AsyncWaitHandle.WaitOne($ConnectTimeoutSec * 1000, $false)) {
            throw ("не удалось подключиться к {0}:{1} за {2} с" -f $Target, $Port, $ConnectTimeoutSec)
        }
        $client.EndConnect($iar)
        $client.NoDelay = $true
        $client.SendBufferSize = 1MB
        $client.ReceiveBufferSize = 1MB
        $stream = $client.GetStream()
        $stream.WriteByte([byte][char]$Cmd)
        if ($Cmd -eq 'D') {
            # Сколько просить — решает клиент, см. комментарий в серверной ветке.
            $lb = [System.BitConverter]::GetBytes([int64]$total)
            $stream.Write($lb, 0, $lb.Length)
        }
        $stream.Flush()

        $buf = New-Object byte[] $bufSize
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $bytes = [int64]0

        if ($Cmd -eq 'D') {
            # Читаем до закрытия сервером: конец потока = конец замера.
            while ($true) {
                $n = $stream.Read($buf, 0, $buf.Length)
                if ($n -le 0) { break }
                $bytes += $n
            }
            $sw.Stop()
        } else {
            $chunk = New-Object byte[] $bufSize
            (New-Object System.Random 54321).NextBytes($chunk)
            while ($bytes -lt $total) {
                $n = [Math]::Min([int64]$chunk.Length, $total - $bytes)
                $stream.Write($chunk, 0, [int]$n)
                $bytes += $n
            }
            $stream.Flush()
            $sw.Stop()
            try { $client.Client.Shutdown([System.Net.Sockets.SocketShutdown]::Send) } catch { }
        }

        $line = Format-Rate -Dir $Name -Bytes $bytes -Seconds $sw.Elapsed.TotalSeconds
        # Возвращаем строку, а печатает её вызывающий: если печатать здесь, вывод
        # функции придётся гасить ([void](...)) вместе с результатом — на этом
        # первая версия и потеряла обе строки.
        return $line
    } finally {
        try { $client.Close() } catch { }
    }
}

Write-Output ("=== замер канала {0}:{1} ({2} МБ на направление) ===" -f $Target, $Port, $Megabytes)
if ($Direction -in @('Download', 'Both')) { Write-Output (Invoke-Direction -Cmd 'D' -Name 'download') }
if ($Direction -in @('Upload', 'Both')) { Write-Output (Invoke-Direction -Cmd 'U' -Name 'upload') }

# --- Что это значит для §3 ---------------------------------------------------
# Слой на границе раскладки передаёт H×bytes на токен (8 КБ при H=4096 и f16),
# а префилл 512 токенов — 512 таких порций. Считаем прямо от измеренной полосы.
Write-Output ''
Write-Output 'Ориентир §3 плана: 112 МБ/с (1 Гбит/с полезной нагрузки).'
Write-Output 'При 8 КБ на токен это 0.073 мс на токен и 37 мс на префилл 512 токенов.'
