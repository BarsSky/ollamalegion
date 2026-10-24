# rpc-shard-load.ps1 — S1 (2026-10-13): живая проба раскладки слоёв на RPC-узел.
#
# ЧТО ДЕЛАЕТ
#   1. грузит модель НАПРЯМУЮ в cppworker (docker exec + curl), минуя балансер:
#      предфильтр балансера отвергает запрос, если параметры не совпадают с уже
#      загруженной моделью (409), а для пробы раскладки нужен свой набор;
#   2. показывает, как llama.cpp разложил слои: сколько устройств («using device»)
#      и сколько слоёв ушло на каждое (CUDA0 / RPC0);
#   3. меряет скорость: TTFT и tok/s по полям ответа (prompt_eval_duration,
#      eval_count, eval_duration);
#   4. печатает строку для протокола приёмки (§6: single / hybrid / network split).
#
# ПРЕДУСЛОВИЕ: в deployments/.env включены CPPWORKER_RPC_ENABLE=true,
# CPPWORKER_RPC_SERVERS=<host:port>, CPPWORKER_SPLIT_MODE=1, контейнер пересоздан.
#
# ПРИМЕР
#   pwsh scripts/rpc-shard-load.ps1 -Model 'Qwen3.8-27B-UD-Q4_K_M' -GpuLayers 56 -ContextSize 8192
#   pwsh scripts/rpc-shard-load.ps1 -Model 'Qwen3.8-27B-UD-Q4_K_M' -SkipLoad   # только замер

[CmdletBinding()]
param(
    [string]$Container = 'ol-stack-cppworker-gpu',
    [string]$Token = 'changeme-bundled-with-agent-token',
    [string]$Model = 'Qwen3.8-27B-UD-Q4_K_M',
    [int]$GpuLayers = 56,
    [int]$ContextSize = 8192,
    [string]$KvCacheType = 'q4_0',
    [string]$Prompt = 'What is 2+2? Answer with the number only.',
    [int]$MeasureTokens = 48,
    [switch]$SkipLoad,
    [int]$TimeoutSec = 1800
)

$ErrorActionPreference = 'Continue'

function Invoke-CppWorkerJson {
    param([string]$Path, [string]$Json, [int]$Timeout = 60)
    $local = Join-Path $env:TEMP ("rpcprobe-" + [guid]::NewGuid().ToString('N') + ".json")
    [System.IO.File]::WriteAllText($local, $Json, (New-Object System.Text.UTF8Encoding($false)))
    $remote = "/tmp/" + [System.IO.Path]::GetFileName($local)
    docker cp $local "${Container}:$remote" | Out-Null
    $out = docker exec $Container curl -s -m $Timeout -X POST "http://127.0.0.1:18092$Path" `
        -H 'Content-Type: application/json' -H "X-API-Token: $Token" -d "@$remote" 2>&1
    Remove-Item $local -ErrorAction SilentlyContinue
    return ($out -join "`n")
}

if (-not $SkipLoad) {
    Write-Host "=== Загрузка $Model (gpuLayers=$GpuLayers, ctx=$ContextSize, kv=$KvCacheType) ===" -ForegroundColor Cyan
    $body = "{`"name`":`"$Model`",`"gpuLayers`":$GpuLayers,`"contextSize`":$ContextSize,`"parallel`":1,`"kvCacheType`":`"$KvCacheType`"}"
    # wait=false + опрос прогресса: длинный wait=true держит curl открытым минутами,
    # и его ответ легко потерять (живой случай: модель загрузилась и разложилась,
    # а отчёт пробника был пустым).
    $null = Invoke-CppWorkerJson -Path '/api/models/load-with-params' -Json $body -Timeout 120
    Write-Host '  ждём окончания загрузки...' -ForegroundColor Gray
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    $state = ''
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 5
        $prog = Invoke-CppWorkerJson -Path ("/api/models/load/progress?model=" + [uri]::EscapeDataString($Model)) -Timeout 30
        try { $state = "$(($prog | ConvertFrom-Json).state)" } catch { $state = '' }
        if ($state -eq 'loaded') { break }
        if ($state -eq 'failed') { Write-Host ("  загрузка провалилась: " + $prog) -ForegroundColor Red; break }
    }
    Write-Host ("  состояние: " + $state) -ForegroundColor Gray
}

Write-Host ''
Write-Host '=== Как llama.cpp разложил слои ===' -ForegroundColor Cyan
$logs = docker logs $Container --since 40m 2>&1
$devLines = $logs | Select-String -Pattern 'using device'
$devLines | ForEach-Object { "  " + $_.Line.Trim() }
$assign = $logs | Select-String -Pattern 'assigned to device'
$perDevice = @{}
foreach ($a in $assign) {
    if ($a.Line -match 'assigned to device (\S+)') {
        $dev = $Matches[1]
        if (-not $perDevice.ContainsKey($dev)) { $perDevice[$dev] = 0 }
        $perDevice[$dev]++
    }
}
if ($perDevice.Count -gt 0) {
    Write-Host '  слоёв на устройство:' -ForegroundColor Gray
    $perDevice.GetEnumerator() | Sort-Object Name | ForEach-Object { "    {0,-8} {1}" -f $_.Key, $_.Value }
} else {
    Write-Host '  строк «assigned to device» нет — модель ещё грузится или это CPU-only' -ForegroundColor Yellow
}
$plan = $logs | Select-String -Pattern 'MemfitBudget: ёмкость RPC|tensor split calculated|RPC-складка|ёмкость RPC-узлов|applying partial offload|auto-adapting GPU layers' | Select-Object -Last 6
if ($plan) {
    Write-Host '  решения планировщиков:' -ForegroundColor Gray
    $plan | ForEach-Object { "    " + $_.Line.Trim().Substring(0, [Math]::Min(200, $_.Line.Trim().Length)) }
}

Write-Host ''
Write-Host '=== Замер скорости (non-stream) ===' -ForegroundColor Cyan
$chatBody = "{`"model`":`"$Model`",`"messages`":[{`"role`":`"user`",`"content`":`"$Prompt`"}],`"stream`":false,`"options`":{`"num_predict`":$MeasureTokens}}"
$sw = [Diagnostics.Stopwatch]::StartNew()
$resp = Invoke-CppWorkerJson -Path '/api/chat' -Json $chatBody -Timeout $TimeoutSec
$sw.Stop()
Write-Host ("  " + $resp.Substring(0, [Math]::Min(300, $resp.Length)))
try {
    $r = $resp | ConvertFrom-Json
    if ($r.done -eq $true) {
        $evalCount = [double]$r.eval_count
        $evalNs = [double]$r.eval_duration
        $promptNs = [double]$r.prompt_eval_duration
        $tps = if ($evalNs -gt 0) { [math]::Round($evalCount / ($evalNs / 1e9), 2) } else { 0 }
        $ttft = [math]::Round($promptNs / 1e9, 1)
        Write-Host ''
        Write-Host ("ИТОГ: TTFT={0}s  tok/s={1}  eval_count={2}  wall={3}s  devices={4}" -f `
            $ttft, $tps, $evalCount, [math]::Round($sw.Elapsed.TotalSeconds, 1), (($perDevice.GetEnumerator() | Where-Object { $_.Key -ne 'CPU' }) | Measure-Object).Count) `
            -ForegroundColor Green
    }
} catch {
    Write-Host "  не удалось разобрать ответ как JSON" -ForegroundColor Yellow
}
