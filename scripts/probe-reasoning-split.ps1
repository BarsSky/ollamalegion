# probe-reasoning-split.ps1 — R108 (2026-10-13): живая проверка того, что
# размышления отделены от ответа на ОБЕИХ поверхностях балансера.
#
# ЗАЧЕМ. Живой дефект: Cline через балансер (:18080, модель Qwen3.8-27B) получал
# ответ, перемешанный с маркерами размышлений (` thinking…`, `<|channel>thought…`),
# потому что cppworker решал «разделять ли reasoning и content» по ИМЕНИ модели.
# После R108 разбор безусловный и тего-зависимый, а нативный /api/chat отдаёт
# размышления под каноническим `message.thinking` (контракт §4.1).
#
# ЧТО ПРОВЕРЯЕТ (обе поверхности — «ollama api должно работать одинаково»):
#   1. Ollama  POST /api/chat            (stream=true)  → message.thinking
#   2. OpenAI  POST /v1/chat/completions (stream=true)  → delta.reasoning_content
# В обоих случаях: ни один маркер размышлений не должен попасть в content.
#
# ПРИМЕРЫ
#   pwsh scripts/probe-reasoning-split.ps1
#   pwsh scripts/probe-reasoning-split.ps1 -Model 'Qwen3.8-27B-UD-Q4_K_M' -Balancer http://127.0.0.1:18080
#   pwsh scripts/probe-reasoning-split.ps1 -NoThink   # проверить поведение без think-подсказки

[CmdletBinding()]
param(
    [string]$Model = 'Qwen3.8-27B-UD-Q4_K_M',
    [string]$Balancer = 'http://127.0.0.1:18080',
    [int]$MaxTokens = 512,
    [switch]$NoThink,
    [int]$TimeoutSec = 1800
)

$ErrorActionPreference = 'Stop'

# Маркеры, которыми модели открывают/закрывают блок размышлений
# (см. docs/BALANCER_CPPWORKER_API_CONTRACT.md §4.2 и thinkTagPairs в
# cmd/cppworker/reasoning_content.go). Любой из них в content — дефект.
#
# R108 (2026-10-13): добавлены служебные токены chat-шаблонов — они тоже не
# должны попадать в текст ответа (живой случай: «ок<end_of_turn> » доехало до
# клиента через балансер).
$markers = @(
    '<think', '</think', '<thinking', '</thinking',
    '<reasoning', '</reasoning', '<analysis', '</analysis',
    '<|channel', '<channel|', '<|think', '<think|',
    '<end_of_turn>', '<start_of_turn>', '<|end|>', '<|eot_id|>', '[TOOL_CALLS]', '</tool_call>'
)

$prompt = 'Реши по шагам и объясни: сколько будет 17 * 23? В конце дай ответ отдельной строкой.'

function Invoke-StreamProbe {
    param(
        [string]$Name,
        [string]$Uri,
        [string]$Body,
        [scriptblock]$ExtractDelta
    )

    Write-Host ''
    Write-Host "=== $Name ===" -ForegroundColor Cyan
    Write-Host "POST $Uri"

    # PowerShell 5.1 (Windows PowerShell) — HttpClient недоступен без Add-Type,
    # поэтому используем HttpWebRequest: он умеет и POST по частям, и чтение
    # ответа потоком (нужно, чтобы видеть NDJSON/SSE по мере генерации).
    $req = [System.Net.HttpWebRequest]::Create($Uri)
    $req.Method = 'POST'
    $req.ContentType = 'application/json'
    $req.Accept = 'application/json'
    $req.Timeout = $TimeoutSec * 1000
    $req.ReadWriteTimeout = $TimeoutSec * 1000
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($Body)
    $req.ContentLength = $bytes.Length
    try {
        $stream = $req.GetRequestStream()
        $stream.Write($bytes, 0, $bytes.Length)
        $stream.Close()
        $resp = $req.GetResponse()
    } catch [System.Net.WebException] {
        # Не-2xx — это тоже результат проверки (например, 409 стенда «модель
        # загружена с другими параметрами»): показываем тело, а не только код.
        $webResp = $_.Exception.Response
        $code = if ($webResp) { [int]$webResp.StatusCode } else { 0 }
        $bodyText = ''
        if ($webResp) {
            $er = New-Object System.IO.StreamReader($webResp.GetResponseStream(), [System.Text.Encoding]::UTF8)
            $bodyText = $er.ReadToEnd(); $er.Close()
        }
        Write-Host ("HTTP {0} — запрос отклонён" -f $code) -ForegroundColor Red
        Write-Host ("   " + ($bodyText -replace "`r?`n", ' ')) -ForegroundColor DarkYellow
        return [pscustomobject]@{
            Surface          = $Name
            HttpStatus       = $code
            Chunks           = 0
            ReasoningChars   = 0
            ContentChars     = 0
            MarkersInContent = 0
            ElapsedSec       = 0
            Error            = $_.Exception.Message
        }
    } catch {
        throw "запрос к $Uri не удался: $($_.Exception.Message)"
    }
    $status = [int]$resp.StatusCode
    $reader = New-Object System.IO.StreamReader($resp.GetResponseStream(), [System.Text.Encoding]::UTF8)

    $content = New-Object System.Text.StringBuilder
    $reasoning = New-Object System.Text.StringBuilder
    $contentWithMarkers = New-Object System.Collections.Generic.List[string]
    $chunks = 0
    $doneReason = ''
    $doneContent = $null
    $t0 = Get-Date

    while (-not $reader.EndOfStream) {
        $line = $reader.ReadLine()
        if ([string]::IsNullOrWhiteSpace($line)) { continue }
        if ($line.StartsWith('data:')) { $line = $line.Substring(5).Trim() }
        if ($line -eq '[DONE]') { continue }
        try { $obj = $line | ConvertFrom-Json } catch { continue }
        $chunks++

        $delta = & $ExtractDelta $obj
        if ($null -ne $delta.Content -and $delta.Content -ne '') {
            [void]$content.Append($delta.Content)
            foreach ($m in $markers) {
                if ($delta.Content.Contains($m)) { $contentWithMarkers.Add($delta.Content) }
            }
        }
        if ($null -ne $delta.Reasoning -and $delta.Reasoning -ne '') {
            [void]$reasoning.Append($delta.Reasoning)
        }
        if ($obj.done -eq $true) {
            $doneReason = "$($obj.done_reason)"
            # Нативный /api/chat кладёт финальный ответ в message.content — он тоже
            # не должен содержать размышлений.
            if ($obj.message -and $obj.message.content) { $doneContent = "$($obj.message.content)" }
        }
    }
    $reader.Close(); $resp.Close()
    $elapsed = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)

    $contentText = $content.ToString()
    $reasoningText = $reasoning.ToString()

    Write-Host ("HTTP {0} | чанков: {1} | {2}s | done_reason={3}" -f $status, $chunks, $elapsed, $doneReason)
    Write-Host ("reasoning: {0} символов" -f $reasoningText.Length)
    Write-Host ("content:   {0} символов" -f $contentText.Length)

    $fail = $false
    if ($contentWithMarkers.Count -gt 0) {
        $fail = $true
        Write-Host "❌ МАРКЕРЫ В CONTENT (R108-дефект вернулся):" -ForegroundColor Red
        $contentWithMarkers | Select-Object -First 5 | ForEach-Object { Write-Host ("   " + $_) -ForegroundColor Red }
    }
    if ($null -ne $doneContent -and $doneContent -ne '') {
        foreach ($m in $markers) {
            if ($doneContent.Contains($m)) {
                $fail = $true
                Write-Host "❌ МАРКЕР В done-чанке message.content" -ForegroundColor Red
            }
        }
    }
    if (-not $fail) {
        Write-Host "✅ маркеров размышлений в content нет" -ForegroundColor Green
    }
    if ($reasoningText.Length -eq 0) {
        Write-Host "⚠ reasoning пуст — модель в этом прогоне не размышляла (не дефект, но проверка слабее)" -ForegroundColor Yellow
    } else {
        Write-Host ("   начало reasoning: " + ($reasoningText.Substring(0, [Math]::Min(160, $reasoningText.Length)) -replace "`n", ' '))
    }
    Write-Host ("   ответ: " + ($contentText.Substring(0, [Math]::Min(240, $contentText.Length)) -replace "`n", ' '))

    return [pscustomobject]@{
        Surface        = $Name
        HttpStatus     = $status
        Chunks         = $chunks
        ReasoningChars = $reasoningText.Length
        ContentChars   = $contentText.Length
        MarkersInContent = $contentWithMarkers.Count
        ElapsedSec     = $elapsed
    }
}

$thinkField = if ($NoThink) { '' } else { ',"think":true' }

# --- 1. Ollama-поверхность: /api/chat (NDJSON) ---
$ollamaBody = @"
{"model":"$Model","messages":[{"role":"user","content":"$prompt"}],"stream":true,"options":{"num_predict":$MaxTokens}$thinkField}
"@
$r1 = Invoke-StreamProbe -Name 'Ollama /api/chat (NDJSON)' -Uri "$Balancer/api/chat" -Body $ollamaBody -ExtractDelta {
    param($obj)
    # message.thinking — канон §4.1; message.reasoning — legacy-алиас.
    $reason = $null
    if ($obj.message) {
        if ($obj.message.thinking) { $reason = "$($obj.message.thinking)" }
        elseif ($obj.message.reasoning) { $reason = "$($obj.message.reasoning)" }
    }
    [pscustomobject]@{ Content = $(if ($obj.message) { "$($obj.message.content)" } else { $null }); Reasoning = $reason }
}

# --- 2. OpenAI-поверхность: /v1/chat/completions (SSE) ---
$openaiBody = @"
{"model":"$Model","messages":[{"role":"user","content":"$prompt"}],"stream":true,"max_tokens":$MaxTokens}
"@
$r2 = Invoke-StreamProbe -Name 'OpenAI /v1/chat/completions (SSE)' -Uri "$Balancer/v1/chat/completions" -Body $openaiBody -ExtractDelta {
    param($obj)
    $c = $null; $r = $null
    if ($obj.choices -and $obj.choices.Count -gt 0) {
        $d = $obj.choices[0].delta
        if ($d) {
            if ($d.content) { $c = "$($d.content)" }
            if ($d.reasoning_content) { $r = "$($d.reasoning_content)" }
        }
    }
    [pscustomobject]@{ Content = $c; Reasoning = $r }
}

Write-Host ''
Write-Host '=== ИТОГ ===' -ForegroundColor Cyan
@($r1, $r2) | Format-Table -AutoSize
$bad = @($r1, $r2) | Where-Object { $_.MarkersInContent -gt 0 }
if ($bad) {
    Write-Host 'РЕЗУЛЬТАТ: дефект воспроизведён на одной из поверхностей' -ForegroundColor Red
    exit 1
}
$errored = @($r1, $r2) | Where-Object { $_.PSObject.Properties['Error'] -and $_.Error }
if ($errored) {
    Write-Host 'РЕЗУЛЬТАТ: одна из поверхностей отклонила запрос (см. код выше)' -ForegroundColor Yellow
    exit 2
}
Write-Host 'РЕЗУЛЬТАТ: размышления отделены от ответа на обеих поверхностях' -ForegroundColor Green
exit 0
