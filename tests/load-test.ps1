# Базовые метрики
Write-Host '=== BASELINE METRICS ===' -ForegroundColor Cyan
$clusterJson = curl.exe -s http://localhost:18081/api/v1/cluster
$cluster = $clusterJson | ConvertFrom-Json
$queueJson = curl.exe -s http://localhost:18081/api/v1/queue/stats
$queue = $queueJson | ConvertFrom-Json

Write-Host ('TotalBackends: ' + $cluster.totalBackends)
Write-Host ('HealthyBackends: ' + $cluster.healthyBackends)
Write-Host ('ActiveRequests: ' + $cluster.activeRequests)
Write-Host ('QueuedRequests: ' + $cluster.queuedRequests)
Write-Host ('RPS: ' + $cluster.rps)

foreach ($b in $cluster.backends) {
    Write-Host ('Backend: ' + $b.id + ' | CPU: ' + [math]::Round($b.system.cpuUsagePercent,1) + '% | ActiveReq: ' + $b.ollama.activeRequests + ' | FreeSlots: ' + $b.ollama.freeSlots)
}

Write-Host ''
Write-Host '=== STARTING LOAD TEST ===' -ForegroundColor Green
Write-Host 'Sending 8 parallel generate requests via balancer...'

$jobs = @()
for ($i = 1; $i -le 8; $i++) {
    $jobs += Start-Job -ScriptBlock {
        param($num)
        $body = '{"model":"nomic-embed-text:latest","prompt":"Load test request number ' + $num + '. Explain load balancing in distributed systems.","stream":false,"options":{"temperature":0.1,"num_predict":50}}'
        $start = Get-Date
        try {
            $resp = curl.exe -s -X POST http://localhost:18080/api/generate -H 'Content-Type: application/json' -d $body
            $elapsed = ((Get-Date) - $start).TotalSeconds
            $parsed = $resp | ConvertFrom-Json -ErrorAction SilentlyContinue
            return (@{
                num = $num
                status = 'ok'
                elapsed = [math]::Round($elapsed,2)
                model = $parsed.model
                eval_count = $parsed.eval_count
            } | ConvertTo-Json)
        } catch {
            return (@{
                num = $num
                status = 'error'
                error = $_.Exception.Message
            } | ConvertTo-Json)
        }
    } -ArgumentList $i
}

# Мониторинг во время нагрузки
Write-Host ''
Write-Host '=== MONITORING DURING LOAD ===' -ForegroundColor Yellow
for ($m = 1; $m -le 10; $m++) {
    Start-Sleep -Seconds 2
    $c = curl.exe -s http://localhost:18081/api/v1/cluster | ConvertFrom-Json
    Write-Host ('T+' + ($m*2) + 's | ActiveReq: ' + $c.activeRequests + ' | Queued: ' + $c.queuedRequests + ' | RPS: ' + [math]::Round($c.rps,2))
    foreach ($b in $c.backends) {
        Write-Host ('  -> ' + $b.id + ': CPU=' + [math]::Round($b.system.cpuUsagePercent,1) + '% ActiveReq=' + $b.ollama.activeRequests + ' FreeSlots=' + $b.ollama.freeSlots)
    }
}

# Ожидание завершения
Write-Host ''
Write-Host '=== WAITING FOR REQUESTS ===' -ForegroundColor Cyan
$results = $jobs | Wait-Job -Timeout 120 | Receive-Job
Remove-Job -State Completed -ErrorAction SilentlyContinue
Remove-Job -State Failed -ErrorAction SilentlyContinue

# Результаты
Write-Host ''
Write-Host '=== RESULTS ===' -ForegroundColor Green
foreach ($r in $results | ForEach-Object { $_ | ConvertFrom-Json }) {
    if ($r.status -eq 'ok') {
        Write-Host ('Request #' + $r.num + ': ' + $r.elapsed + 's | model=' + $r.model + ' eval_count=' + $r.eval_count)
    } else {
        Write-Host ('Request #' + $r.num + ': ERROR - ' + $r.error) -ForegroundColor Red
    }
}

$cluster2 = curl.exe -s http://localhost:18081/api/v1/cluster | ConvertFrom-Json
Write-Host ''
Write-Host '=== FINAL METRICS ===' -ForegroundColor Cyan
Write-Host ('TotalRequests: ' + $cluster2.totalRequests + ' (was ' + $cluster.totalRequests + ')')
Write-Host ('ActiveRequests: ' + $cluster2.activeRequests)
Write-Host ('QueuedRequests: ' + $cluster2.queuedRequests)
foreach ($b in $cluster2.backends) {
    Write-Host ('Backend ' + $b.id + ': TotalReq=' + $b.ollama.totalRequests + ' AvgResponse=' + [math]::Round($b.ollama.avgResponseTime,2) + 'ms')
}