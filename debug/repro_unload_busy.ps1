# repro_unload_busy.ps1 — воспроизведение: управление моделью из WebUI во время генерации.
# Проверяет три пути ровно так, как их зовут WebUI/балансер:
#   A) app.js  : POST /api/v1/backends/{id}/models {operation:unload}
#   B) тот же путь с force:true
#   C) прямой cppworker: POST /api/models/unload?name=..&force=true
$tok = 'changeme-bundled-with-agent-token'
$lb  = 'http://127.0.0.1:18081'
$cw  = 'http://127.0.0.1:18092'
$backend = 'cppworker-gpu-bundled-agent'
$model = 'gemma-4-E4B-it-Q4_K_M'
$dir = 'C:\Ollama\ollamalegion\debug'

function WriteJson($path, $obj) {
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Depth 8 -Compress), (New-Object System.Text.UTF8Encoding($false)))
}

function StartLongGeneration {
    $body = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":1200,"num_ctx":32768},"messages":[{"role":"user","content":"Напиши очень подробное эссе о языке Go: история, синтаксис, конкурентность, экосистема."}]}'
    [System.IO.File]::WriteAllText("$dir\long_gen.json", $body, (New-Object System.Text.UTF8Encoding($false)))
    $j = Start-Job -ScriptBlock {
        param($f)
        curl.exe -s --max-time 600 -H 'Content-Type: application/json' --data-binary "@$f" 'http://127.0.0.1:18080/api/chat'
    } -ArgumentList "$dir\long_gen.json"
    Start-Sleep -Seconds 6
    return $j
}

function ShowActive {
    $a = curl.exe -s -H "X-API-Token: $tok" "$cw/api/infer/active" | Out-String
    $n = ([regex]::Matches($a, '"model"')).Count
    "активных генераций (по полю model): $n"
}

"===== A) app.js-путь: unload БЕЗ force во время генерации ====="
$gen = StartLongGeneration
ShowActive
WriteJson "$dir\op_unload.json" @{ operation = 'unload'; modelName = $model }
$t0 = Get-Date
$r = curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\op_unload.json" "$lb/api/v1/backends/$backend/models" | Out-String
$r
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s | генерация после A: $((Get-Job -Id $gen.Id).State)"
Remove-Job -Id $gen.Id -Force | Out-Null
Start-Sleep -Seconds 2

"`n===== B) тот же путь с force=true ====="
$gen2 = StartLongGeneration
ShowActive
WriteJson "$dir\op_unload_force.json" @{ operation = 'unload'; modelName = $model; force = $true }
$t0 = Get-Date
$r2 = curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" -H 'Content-Type: application/json' --data-binary "@$dir\op_unload_force.json" "$lb/api/v1/backends/$backend/models" | Out-String
$r2
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
Start-Sleep -Seconds 3
"модели загружено после B: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"
Remove-Job -Id $gen2.Id -Force | Out-Null

"`n===== C) прямой cppworker с ?force=true ====="
$gen3 = StartLongGeneration
ShowActive
$t0 = Get-Date
$r3 = curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" "$cw/api/models/unload?name=$model&force=true" | Out-String
$r3
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s"
Start-Sleep -Seconds 3
"модели загружено после C: $(((curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json).count))"
Remove-Job -Id $gen3.Id -Force | Out-Null
