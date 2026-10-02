# verify_v61_stream.ps1 — прерывание СТРИМИНГОВОЙ генерации (как у Cline) и гейт reload.
$tok = 'changeme-bundled-with-agent-token'
$cw  = 'http://127.0.0.1:18092'
$lbapi = 'http://127.0.0.1:18080'
$backend = 'cppworker-gpu-bundled-agent'
$model = 'gemma-4-E4B-it-Q4_K_M'
$dir = 'C:\Ollama\ollamalegion\debug'

function WriteJson($path, $obj) {
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Depth 8 -Compress), (New-Object System.Text.UTF8Encoding($false)))
}
function StartStreamingGeneration {
    $body = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":true,"options":{"num_predict":1500,"num_ctx":32768},"messages":[{"role":"user","content":"Напиши очень подробное эссе о языке Go на 1200 слов."}]}'
    [System.IO.File]::WriteAllText("$dir\long_stream.json", $body, (New-Object System.Text.UTF8Encoding($false)))
    $j = Start-Job -ScriptBlock { param($f) curl.exe -s -N --max-time 600 -H 'Content-Type: application/json' --data-binary "@$f" 'http://127.0.0.1:18080/api/chat' } -ArgumentList "$dir\long_stream.json"
    Start-Sleep -Seconds 8
    return $j
}

"=== 1. СТРИМИНГОВАЯ генерация + unload force=true (сценарий Cline) ==="
$gen = StartStreamingGeneration
"in-flight до: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/info" | ConvertFrom-Json).models[0].activeQueries))"
$t0 = Get-Date
curl.exe -s -o NUL -w "HTTP=%{http_code} time=%{time_total}s`n" -X POST -H "X-API-Token: $tok" "$cw/api/models/unload?name=$model&force=true"
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s; моделей: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"
Remove-Job -Id $gen.Id -Force -ErrorAction SilentlyContinue

"`n=== 2. Прогрев + reload БЕЗ force при ctx 131072 (реальная смена параметров) ==="
$w = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":8,"num_ctx":32768},"messages":[{"role":"user","content":"привет"}]}'
[System.IO.File]::WriteAllText("$dir\warm61.json", $w, (New-Object System.Text.UTF8Encoding($false)))
curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary "@$dir\warm61.json" "$lbapi/api/chat" | Out-Null
$gen2 = StartStreamingGeneration
$rb = '{"name":"gemma-4-E4B-it-Q4_K_M","contextSize":131072}'
[System.IO.File]::WriteAllText("$dir\reload_big.json", $rb, (New-Object System.Text.UTF8Encoding($false)))
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s`n" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\reload_big.json" "$cw/api/models/reload?wait=false"
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s — ожидаем 409 за доли секунды"
Remove-Job -Id $gen2.Id -Force -ErrorAction SilentlyContinue

"`n=== 3. То же с force=true ==="
$gen3 = StartStreamingGeneration
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s`n" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\reload_big.json" "$cw/api/models/reload?wait=false&force=true" | Select-Object -First 1
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
Start-Sleep -Seconds 8
"моделей: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count)); ctx=$(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).models[0].context_size))"
Remove-Job -Id $gen3.Id -Force -ErrorAction SilentlyContinue
