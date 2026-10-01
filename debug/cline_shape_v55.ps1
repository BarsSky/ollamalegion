# cline_shape_v55.ps1 — Cline-образный запрос (system-промпт Cline + 18 tools +
# plan-режим) ЧЕРЕЗ БАЛАНСЕР на 42/42 слоях: сколько занимает и что возвращает.
param([int]$ToolsCount = 18, [int]$NumPredict = 400)
$lb = 'http://127.0.0.1:18080'
$sess = "$env:USERPROFILE\.cline\data\sessions\1790846231847_sdk8m\1790846231847_sdk8m.messages.json"
$sys = (Get-Content $sess -Raw | ConvertFrom-Json).system_prompt
$desc = 'This tool performs a very detailed operation on the workspace. ' * 64
$tools = @()
for ($i = 1; $i -le $ToolsCount; $i++) {
    $tools += @{ type = 'function'; function = @{ name = "tool_$i"; description = "$desc (tool $i)"; parameters = @{ type = 'object'; properties = @{ path = @{ type = 'string' } }; required = @('path') } } }
}
$body = @{
    model    = 'gemma-4-E4B-it-Q4_K_M'
    stream   = $true
    options  = @{ num_predict = $NumPredict; num_ctx = 32768 }
    messages = @(@{ role = 'system'; content = $sys }, @{ role = 'user'; content = '<user_input mode="plan">опиши проект</user_input>' })
    tools    = $tools
} | ConvertTo-Json -Depth 12 -Compress
[System.IO.File]::WriteAllText('C:\Ollama\ollamalegion\debug\cline_shape.json', $body, (New-Object System.Text.UTF8Encoding($false)))

$t0 = Get-Date
$lines = & curl.exe -s -N --max-time 1800 -H 'Content-Type: application/json' --data-binary '@C:\Ollama\ollamalegion\debug\cline_shape.json' "$lb/api/chat"
$el = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
$fin = $null; $chunks = 0; $keepalives = 0
foreach ($l in $lines) {
    try { $o = $l | ConvertFrom-Json } catch { continue }
    $chunks++
    if ($o.keepalive) { $keepalives++; continue }
    if ($o.done) { $fin = $o }
}
"wall=${el}s  чанков=$chunks (keepalive=$keepalives)"
if ($fin) {
    $gd = [math]::Round($fin.eval_duration / 1e9, 2)
    "done_reason=$($fin.done_reason) eval_count=$($fin.eval_count) eval=${gd}s -> $([math]::Round($fin.eval_count/[math]::Max($gd,0.01),1)) tok/s | prompt_eval_count=$($fin.prompt_eval_count) за $([math]::Round($fin.prompt_eval_duration/1e9,2))s"
    if ($fin.message.tool_calls) { "tool_calls=$($fin.message.tool_calls.Count): $(($fin.message.tool_calls | ForEach-Object { $_.function.name }) -join ', ')" }
    if ($fin.message.content) { "content_len=$($fin.message.content.Length)" }
    if ($fin.error) { "ERROR: $($fin.error)" }
} else { 'финального чанка нет' }
