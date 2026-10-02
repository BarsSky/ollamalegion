# verify_v61_final.ps1 — аккуратная проверка: СТРИМИНГОВАЯ генерация прерывается
# force-выгрузкой так же быстро, как отмена клиентом.
$tok = 'changeme-bundled-with-agent-token'
$cw  = 'http://127.0.0.1:18092'
$lbapi = 'http://127.0.0.1:18080'
$model = 'gemma-4-E4B-it-Q4_K_M'
$dir = 'C:\Ollama\ollamalegion\debug'

function WriteJson($path, $obj) {
    [System.IO.File]::WriteAllText($path, ($obj | ConvertTo-Json -Depth 8 -Compress), (New-Object System.Text.UTF8Encoding($false)))
}
function Models {
    return (curl.exe -s -H "X-API-Token: $tok" "$cw/api/models" | ConvertFrom-Json)
}
function ActiveQueries {
    $i = (curl.exe -s -H "X-API-Token: $tok" "$cw/api/info" | ConvertFrom-Json)
    if ($i.models -and $i.models.Count -gt 0) { return $i.models[0].activeQueries }
    return -1
}

"=== 0. Прогрев (ждём loaded) ==="
$w = '{"model":"gemma-4-E4B-it-Q4_K_M","stream":false,"options":{"num_predict":8,"num_ctx":32768},"messages":[{"role":"user","content":"привет"}]}'
WriteJson "$dir\warmf.json" @{ model = $model; stream = $false; options = @{ num_predict = 8; num_ctx = 32768 }; messages = @(@{ role = 'user'; content = 'привет' }) } | Out-Null
$t0 = Get-Date
curl.exe -s --max-time 900 -H 'Content-Type: application/json' --data-binary "@$dir\warmf.json" "$lbapi/api/chat" | Out-Null
$tries = 0
while ($tries -lt 60) {
    $m = Models
    if ($m.count -ge 1 -and $m.models[0].state -eq 'loaded') { break }
    Start-Sleep -Seconds 2; $tries++
}
"warm-up: $([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s; моделей=$($m.count) state=$($m.models[0].state) ctx=$($m.models[0].context_size) слоёв=$($m.models[0].gpu_layers)/$($m.models[0].n_layers)"

"`n=== 1. Стриминговая генерация (как у Cline) ==="
WriteJson "$dir\longf.json" @{ model = $model; stream = $true; options = @{ num_predict = 1500; num_ctx = 32768 }; messages = @(@{ role = 'user'; content = 'Напиши очень подробное эссе о языке Go на 1200 слов.' }) } | Out-Null
$gen = Start-Job -ScriptBlock { param($f) curl.exe -s -N --max-time 600 -H 'Content-Type: application/json' --data-binary "@$f" 'http://127.0.0.1:18080/api/chat' } -ArgumentList "$dir\longf.json"
$waited = 0
while ($waited -lt 40) {
    $aq = ActiveQueries
    if ($aq -ge 1) { break }
    Start-Sleep -Seconds 1; $waited++
}
"activeQueries через ${waited}s: $(ActiveQueries); job=$((Get-Job -Id $gen.Id).State)"
Start-Sleep -Seconds 5  # пусть реально генерирует

"`n=== 2. force-выгрузка во время стриминга ==="
$t0 = Get-Date
curl.exe -s -w "`nHTTP=%{http_code} time=%{time_total}s" -X POST -H "X-API-Token: $tok" "$cw/api/models/unload?name=$model&force=true"
"wall=$([math]::Round(((Get-Date)-$t0).TotalSeconds,1))s; моделей=$(Models | Select-Object -ExpandProperty count)"
Start-Sleep -Seconds 2
"job после выгрузки=$((Get-Job -Id $gen.Id).State)"
Remove-Job -Id $gen.Id -Force -ErrorAction SilentlyContinue

"`n=== лог ==="
docker logs ol-stack-cppworker-gpu --since 3m 2>&1 | Select-String -Pattern 'cancelled active generations|AbortWatcher|unloading model' | Select-Object -Last 6 | ForEach-Object { $_.Line.Substring(0,[Math]::Min(220,$_.Line.Length)) }
