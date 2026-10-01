# verify_v53.ps1 — проверка после v52/v53:
#   1) загрузка ЧЕРЕЗ БАЛАНСЕР (профиль + KV-раскладка) → ожидаем 42/42 слоёв;
#   2) замер скорости генерации;
#   3) загрузка ПРЯМЫМ запросом к cppworker (ленивая загрузка) → ожидаем q4_0 в KV.
$tok = 'changeme-bundled-with-agent-token'
$cw = 'http://127.0.0.1:18092'
$lb = 'http://127.0.0.1:18080'
$model = 'gemma-4-E4B-it-Q4_K_M'

function ShowState([string]$tag) {
    $inf = (curl.exe -s -H "X-API-Token: $tok" "$cw/api/info" | Out-String | ConvertFrom-Json)
    $m = (curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | Out-String | ConvertFrom-Json).models[0]
    if ($null -eq $m) { "${tag}: модель не загружена"; return }
    "${tag}: gpuLayers=$($inf.models[0].gpuLayers)/$($m.n_layers) kv=$($m.kv_cache_type) ctx=$($m.context_size) per_seq=$($m.context_per_seq) parallel=$($m.parallel) vramUsed=$($inf.gpuDevices[0].vramUsedMB) vramFree=$($inf.gpuDevices[0].vramFreeMB)"
}

function Measure([string]$tag) {
    $b = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":true,"options":{"num_predict":200,"num_ctx":32768},"messages":[{"role":"user","content":"Напиши 60 слов о языке Go."}]}'
    [System.IO.File]::WriteAllText('C:\Ollama\ollamalegion\debug\v53_sp.json', $b, (New-Object System.Text.UTF8Encoding($false)))
    $t0 = Get-Date
    $lines = & curl.exe -s -N --max-time 600 -H 'Content-Type: application/json' --data-binary '@C:\Ollama\ollamalegion\debug\v53_sp.json' "$cw/api/chat"
    $el = [math]::Round(((Get-Date) - $t0).TotalSeconds, 1)
    $fin = $null
    foreach ($l in $lines) { try { $o = $l | ConvertFrom-Json } catch { continue }; if ($o.done) { $fin = $o } }
    if ($fin) {
        $gd = [math]::Round($fin.eval_duration / 1e9, 2)
        "${tag}: wall=${el}s eval_count=$($fin.eval_count) eval=${gd}s -> $([math]::Round($fin.eval_count / [math]::Max($gd,0.01),1)) tok/s | prefill $($fin.prompt_eval_count) tok in $([math]::Round($fin.prompt_eval_duration/1e9,2))s"
    } else { "${tag}: финальный чанк не получен (wall=${el}s)" }
}

"=== 1. Загрузка через БАЛАНСЕР (auto-load по профилю) ==="
curl.exe -s -X POST -H "X-API-Token: $tok" "$cw/api/models/unload?name=$model&force=true" | Out-Null
Start-Sleep -Seconds 4
$b1 = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":8,"num_ctx":32768},"messages":[{"role":"user","content":"привет"}]}'
[System.IO.File]::WriteAllText('C:\Ollama\ollamalegion\debug\v53_warm.json', $b1, (New-Object System.Text.UTF8Encoding($false)))
$t0 = Get-Date
curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary '@C:\Ollama\ollamalegion\debug\v53_warm.json' "$lb/api/chat" | Out-Null
"warm-up(wall через балансер, включая загрузку): $([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
ShowState '  состояние'
Measure '  скорость'

"=== 2. Прямой запрос к cppworker (ленивая загрузка, фикс v53) ==="
curl.exe -s -X POST -H "X-API-Token: $tok" "$cw/api/models/unload?name=$model&force=true" | Out-Null
Start-Sleep -Seconds 4
$t0 = Get-Date
curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary '@C:\Ollama\ollamalegion\debug\v53_warm.json' "$cw/api/chat" | Out-Null
"warm-up(wall, включая загрузку): $([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
ShowState '  состояние'
Measure '  скорость'
