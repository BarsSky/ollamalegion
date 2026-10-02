# verify_v61.ps1 — проверка управления моделью во время генерации (R83/v60).
$tok = 'changeme-bundled-with-agent-token'
$lb  = 'http://127.0.0.1:18081'
$lbapi = 'http://127.0.0.1:18080'
$cw  = 'http://127.0.0.1:18092'
$backend = 'cppworker-gpu-bundled-agent'
$model = 'gemma-4-E4B-it-Q4_K_M'
$dir = 'C:\Ollama\ollamalegion\debug'

function WriteJson($path, $obj) {
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Depth 8 -Compress), (New-Object System.Text.UTF8Encoding($false)))
}
function StartLongGeneration {
    $body = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":1200,"num_ctx":32768},"messages":[{"role":"user","content":"Напиши очень подробное эссе о языке Go."}]}'
    [System.IO.File]::WriteAllText("$dir\long_gen.json", $body, (New-Object System.Text.UTF8Encoding($false)))
    $j = Start-Job -ScriptBlock { param($f) curl.exe -s --max-time 600 -H 'Content-Type: application/json' --data-binary "@$f" 'http://127.0.0.1:18080/api/chat' } -ArgumentList "$dir\long_gen.json"
    Start-Sleep -Seconds 6
    return $j
}

"=== прогрев модели ==="
$w = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":8,"num_ctx":32768},"messages":[{"role":"user","content":"привет"}]}'
[System.IO.File]::WriteAllText("$dir\warm61.json", $w, (New-Object System.Text.UTF8Encoding($false)))
$t0 = Get-Date
curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary "@$dir\warm61.json" "$lbapi/api/chat" | Out-Null
"warm-up: $([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s; моделей: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"

"`n=== 1. Выгрузка во время генерации: без force (как было) ==="
$gen = StartLongGeneration
WriteJson "$dir\op_unload.json" @{ operation = 'unload'; modelName = $model }
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\op_unload.json" "$lb/api/v1/backends/$backend/models" | Out-String
"генерация: $((Get-Job -Id $gen.Id).State)"

"`n=== 2. То же с force=true (что теперь делает WebUI после подтверждения) ==="
WriteJson "$dir\op_unload_force.json" @{ operation = 'unload'; modelName = $model; force = $true }
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\op_unload_force.json" "$lb/api/v1/backends/$backend/models" | Out-String
Start-Sleep -Seconds 2
"моделей после force-unload: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"
Remove-Job -Id $gen.Id -Force | Out-Null

"`n=== 3. Reload во время генерации: без force (раньше висело вечно) ==="
$warmAgain = Start-Job -ScriptBlock { param($f) curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary "@$f" 'http://127.0.0.1:18080/api/chat' } -ArgumentList "$dir\warm61.json"
Wait-Job -Id $warmAgain.Id -Timeout 300 | Out-Null; Remove-Job -Id $warmAgain.Id -Force | Out-Null
$gen2 = StartLongGeneration
$rb = '{"name":"gemma-4-E4B-it-Q4_K_M","contextSize":65536}'
[System.IO.File]::WriteAllText("$dir\reload61.json", $rb, (New-Object System.Text.UTF8Encoding($false)))
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\reload61.json" "$cw/api/models/reload?wait=false" | Out-String
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s (ожидаем 409 за доли секунды, а не ожидание генерации)"

"`n=== 4. Reload во время генерации: force=true ==="
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\reload61.json" "$cw/api/models/reload?wait=false&force=true" | Out-String
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
Start-Sleep -Seconds 5
"моделей после force-reload: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"
Remove-Job -Id $gen2.Id -Force | Out-Null
"`n=== лог: отмены и гейты ==="
docker logs ol-stack-cppworker-gpu --since 5m 2>&1 | Select-String -Pattern 'cancelled active generations|reload refused|force=true: ждём|не дренировались' | Select-Object -Last 6 | ForEach-Object { $_.Line.Substring(0,[Math]::Min(220,$_.Line.Length)) }
