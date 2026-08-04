Set-Location C:\Ollama\ollamalegion
$log = 'C:\Ollama\ollamalegion\logs\build_balancer_v0513_20260804_193408.log'
function Log($msg) { Add-Content -Path $log -Value $msg -Encoding utf8 }
Log "=== Balancer build started at $(Get-Date) ==="
try {
    & docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile . 2>&1 | ForEach-Object { Log "$_" }
} catch {
    Log "ERROR: $($_.Exception.Message)"
}
Log "=== Balancer build finished at $(Get-Date) ==="
