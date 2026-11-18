# live_tools_stream_check.ps1 — живая проверка tools-стрима после R527.
#
# ЧТО ПРОВЕРЯЕТ на РЕАЛЬНОМ запросе через балансер (18080):
#   1. стрим действительно потоковый: приходит >= 2 NDJSON-кадра с непустым
#      message.content (а не один финальный кадр);
#   2. финальный кадр — done:true с done_reason из {stop, length};
#   3. служебные маркеры/разметка вызова не утекли в content дельт;
#   4. в ответе есть хотя бы один content-байт (клиент не получил пустоту).
#
# ЗАПУСК:
#   pwsh scripts/live_tools_stream_check.ps1 -Model 'Qwen3-4B-Instruct-2507-Q4_K_M'

[CmdletBinding()]
param(
    [string]$Model = 'Qwen3-4B-Instruct-2507-Q4_K_M',
    [string]$Balancer = 'http://127.0.0.1:18080',
    [int]$MaxTokens = 160,
    [int]$TimeoutSec = 600
)

$ErrorActionPreference = 'Stop'

$tool = @{
    type     = 'function'
    function = @{
        name        = 'read_files'
        description = 'Read a file from disk'
        parameters  = @{
            type       = 'object'
            properties = @{
                path = @{ type = 'string'; description = 'file path' }
            }
            required   = @('path')
        }
    }
}

$payload = @{
    model    = $Model
    stream   = $true
    messages = @(
        @{ role = 'user'; content = 'Answer in one short sentence: what is 2+2? Do not use tools.' }
    )
    tools    = @($tool)
    options  = @{ num_predict = $MaxTokens; temperature = 0.2 }
}

$body = $payload | ConvertTo-Json -Depth 12 -Compress
$req = [System.Net.HttpWebRequest]::Create("$Balancer/api/chat")
$req.Method = 'POST'
$req.ContentType = 'application/json; charset=utf-8'
$req.Timeout = $TimeoutSec * 1000
$req.ReadWriteTimeout = $TimeoutSec * 1000
$req.ServicePoint.Expect100Continue = $false

$enc = [System.Text.Encoding]::UTF8
$bytes = $enc.GetBytes($body)
$req.ContentLength = $bytes.Length
$stream = $req.GetRequestStream()
$stream.Write($bytes, 0, $bytes.Length)
$stream.Close()

$resp = $req.GetResponse()
$reader = New-Object System.IO.StreamReader($resp.GetResponseStream(), $enc)
$frames = 0
$contentFrames = 0
$contentBytes = 0
$doneReason = $null
$leaked = @()
$markers = @('<think', '</think', '<|channel', '<channel|', '<end_of_turn>', '<|end|>', '<start_of_turn>', '[TOOL_CALLS]')

try {
    while (($line = $reader.ReadLine()) -ne $null) {
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        try { $frame = $line | ConvertFrom-Json } catch { continue }
        if ($frame.keepalive) { continue }
        $frames++
        if ($frame.done -eq $true) {
            $doneReason = $frame.done_reason
            if ($frame.error) { throw "финальный кадр с ошибкой: $($frame.error)" }
            continue
        }
        if ($frame.message -and $frame.message.content) {
            $contentFrames++
            $contentBytes += ([string]$frame.message.content).Length
            foreach ($m in $markers) {
                if (([string]$frame.message.content).Contains($m)) { $leaked += $m }
            }
        }
    }
} finally {
    $reader.Close()
    $resp.Close()
}

$ok = $true
if ($frames -lt 2) { "FAIL: кадров $frames (< 2) — стрим не потоковый"; $ok = $false }
if ($contentFrames -lt 2) { "FAIL: кадров с content $contentFrames (< 2) — ответ пришёл одним куском"; $ok = $false }
if ($contentBytes -lt 1) { "FAIL: клиент не получил ни одного байта content"; $ok = $false }
if (-not $doneReason) { "FAIL: нет финального кадра done:true"; $ok = $false }
if ($doneReason -and $doneReason -notin @('stop', 'length')) { "FAIL: done_reason=$doneReason"; $ok = $false }
if ($leaked.Count -gt 0) { "FAIL: в content-дельтах служебные маркеры: $($leaked -join ', ')"; $ok = $false }

"frames=$frames content_frames=$contentFrames content_bytes=$contentBytes done_reason=$doneReason"
if ($ok) { "OK: tools-стрим потоковый, разметка не утекла" } else { exit 1 }

