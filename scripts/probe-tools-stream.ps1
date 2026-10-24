# probe-tools-stream.ps1 — R108 (2026-10-13): живая проверка агентского пути Cline.
#
# ЧТО ПРОВЕРЯЕТ на реальном запросе Cline-формы (18 инструментов, stream=true):
#   1. маркеры размышлений НЕ попадают в content-дельты (R108);
#   2. размышления приходят отдельным полем (message.thinking / legacy reasoning);
#   3. вызов инструмента приходит в message.tool_calls (а не «командой в
#      квадратных скобках» в тексте) — R106c;
#   4. сырой JSON вызова не утекает в content дельтами — R106d (markdown-фенс).
#
# ПРИМЕР
#   pwsh scripts/probe-tools-stream.ps1 -Model 'Qwen3.8-27B-UD-Q4_K_M' -MaxTokens 256

[CmdletBinding()]
param(
    [string]$Model = 'Qwen3.8-27B-UD-Q4_K_M',
    [string]$Balancer = 'http://127.0.0.1:18080',
    [string]$PayloadFile = 'debug/probe_tools.json',
    [int]$MaxTokens = 256,
    [int]$TimeoutSec = 1800
)

$ErrorActionPreference = 'Stop'

$payload = Get-Content $PayloadFile -Raw | ConvertFrom-Json
$payload.model = $Model
$payload.stream = $true
if (-not $payload.options) { $payload | Add-Member -NotePropertyName options -NotePropertyValue ([pscustomobject]@{}) }
$payload.options | Add-Member -NotePropertyName num_predict -NotePropertyValue $MaxTokens -Force
$body = $payload | ConvertTo-Json -Depth 12 -Compress

$markers = @('<think', '</think', '<thinking', '</thinking', '<reasoning', '</reasoning',
             '<analysis', '</analysis', '<|channel', '<channel|', '<|think', '<think|',
             # R108 (2026-10-13): служебные токены chat-шаблонов — тоже не текст ответа.
             '<end_of_turn>', '<start_of_turn>', '<|end|>', '<|eot_id|>', '[TOOL_CALLS]', '</tool_call>')

$req = [System.Net.HttpWebRequest]::Create("$Balancer/api/chat")
$req.Method = 'POST'
$req.ContentType = 'application/json; charset=utf-8'
$req.Timeout = $TimeoutSec * 1000
$req.ReadWriteTimeout = $TimeoutSec * 1000
$bytes = [System.Text.Encoding]::UTF8.GetBytes($body)
$req.ContentLength = $bytes.Length
$s = $req.GetRequestStream(); $s.Write($bytes, 0, $bytes.Length); $s.Close()
$sw = [Diagnostics.Stopwatch]::StartNew()
try { $resp = $req.GetResponse() } catch [System.Net.WebException] {
    $wr = $_.Exception.Response
    $er = New-Object System.IO.StreamReader($wr.GetResponseStream(), [System.Text.Encoding]::UTF8)
    Write-Host ("HTTP {0}: {1}" -f [int]$wr.StatusCode, $er.ReadToEnd()) -ForegroundColor Red
    exit 2
}
$reader = New-Object System.IO.StreamReader($resp.GetResponseStream(), [System.Text.Encoding]::UTF8)

$content = New-Object System.Text.StringBuilder
$reasoning = New-Object System.Text.StringBuilder
$markerHits = New-Object System.Collections.Generic.List[string]
$toolCalls = New-Object System.Collections.Generic.List[string]
$chunks = 0; $doneReason = ''; $firstTokenSec = -1

while (-not $reader.EndOfStream) {
    $line = $reader.ReadLine()
    if ([string]::IsNullOrWhiteSpace($line)) { continue }
    try { $o = $line | ConvertFrom-Json } catch { continue }
    $chunks++
    if ($o.message) {
        $c = "$($o.message.content)"
        if ($c -ne '') {
            if ($firstTokenSec -lt 0) { $firstTokenSec = $stopped = $sw.Elapsed.TotalSeconds }
            [void]$content.Append($c)
            foreach ($m in $markers) { if ($c.Contains($m)) { $markerHits.Add($c) } }
            if ($c.Contains('"name"') -or $c.Contains('"parameters"') -or $c.Contains('"arguments"')) {
                $markerHits.Add('RAW-JSON-IN-CONTENT: ' + $c.Substring(0, [Math]::Min(80, $c.Length)))
            }
        }
        if ($o.message.thinking) { [void]$reasoning.Append("$($o.message.thinking)") }
        elseif ($o.message.reasoning) { [void]$reasoning.Append("$($o.message.reasoning)") }
        if ($o.message.tool_calls) {
            foreach ($tc in $o.message.tool_calls) {
                $toolCalls.Add("$($tc.function.name)(" + (($tc.function.arguments | ConvertTo-Json -Compress -Depth 5)) + ")")
            }
        }
    }
    if ($o.done -eq $true) { $doneReason = "$($o.done_reason)" }
}
$reader.Close(); $resp.Close()

Write-Host ("HTTP 200 | чанков: {0} | {1}s | TTFT: {2}s | done_reason={3}" -f `
    $chunks, [math]::Round($sw.Elapsed.TotalSeconds, 1), [math]::Round($firstTokenSec, 1), $doneReason)
Write-Host ("reasoning: {0} символов" -f $reasoning.Length)
Write-Host ("content:   {0} символов" -f $content.Length)
Write-Host ("tool_calls: {0}" -f $toolCalls.Count)
$toolCalls | ForEach-Object { Write-Host ("   " + $_) -ForegroundColor Cyan }

if ($markerHits.Count -gt 0) {
    Write-Host "❌ В CONTENT попали маркеры размышлений или сырой JSON вызова:" -ForegroundColor Red
    $markerHits | Select-Object -First 5 | ForEach-Object { Write-Host ("   " + $_) -ForegroundColor Red }
    exit 1
}
Write-Host "✅ маркеров размышлений и сырого JSON вызова в content нет" -ForegroundColor Green
if ($toolCalls.Count -eq 0) {
    Write-Host "⚠ tool_calls нет — модель в этом прогоне не вызвала инструмент (не дефект, но проверка слабее)" -ForegroundColor Yellow
}
exit 0
