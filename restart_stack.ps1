Write-Host "=== Stopping balancer.exe ==="
Stop-Process -Name balancer -Force -ErrorAction SilentlyContinue
Start-Sleep -Seconds 2

Write-Host "=== Stopping agent container ==="
docker rm -f ollama-legion-agent-gpu 2>$null
Start-Sleep -Seconds 1

Write-Host "=== Removing state.json ==="
Remove-Item c:\Ollama\ollamalegion\data\state.json -ErrorAction SilentlyContinue

Write-Host "=== Building balancer.exe ==="
Set-Location c:\Ollama\ollamalegion
go build -o balancer.exe ./cmd/balancer
if ($LASTEXITCODE -ne 0) { Write-Host "BUILD FAILED"; exit 1 }
Write-Host "Build OK"

Write-Host "=== Starting balancer.exe ==="
Start-Process -FilePath 'c:\Ollama\ollamalegion\balancer.exe' -ArgumentList '-config', 'c:\Ollama\ollamalegion\config\config.json' -WindowStyle Hidden
Start-Sleep -Seconds 4

Write-Host "=== Checking health ==="
try {
  $h = Invoke-RestMethod http://localhost:18081/api/v1/health
  Write-Host "Health: $($h.status)"
} catch {
  Write-Host "Health check failed: $_"
  exit 1
}

Write-Host "=== Starting agent ==="
docker run -d --name ollama-legion-agent-gpu --restart unless-stopped -p 18032:18032 `
  -e BACKEND_TYPE=llama_cpp `
  -e AGENT_ID=llama-gpu-node-1 `
  -e BALANCER_URL=http://host.docker.internal:18081 `
  -e CPPWORKER_URL=http://host.docker.internal:18091 `
  -e OLLAMA_URL=http://host.docker.internal:11434 `
  -e COLLECT_INTERVAL=10 `
  -e HEARTBEAT_INTERVAL=15 `
  -e NODE_LABELS=zone=lan `
  ollama-legion/agent:latest

Write-Host "=== Waiting for registration ==="
Start-Sleep -Seconds 10

Write-Host "=== CLUSTER STATE ==="
$c = Invoke-RestMethod http://localhost:18081/api/v1/cluster
Write-Host "backendEngine: $($c.backendEngine)"
Write-Host "operatingMode: $($c.operatingMode)"
Write-Host "totalBackends: $($c.totalBackends)"
Write-Host "healthyBackends: $($c.healthyBackends)"
Write-Host "backendTypeCounts: $($c.backendTypeCounts | ConvertTo-Json -Compress)"
foreach($b in $c.backends){
  Write-Host "Backend: id=$($b.id) status=$($b.status) type=$($b.backendType)"
}