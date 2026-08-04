Set-Location C:\Ollama\ollamalegion
$env:DOCKER_BUILDKIT = '1'
$log = 'C:\Ollama\ollamalegion\logs\build_webui_v0513_20260804_192927.log'
function Log($msg) { Add-Content -Path $log -Value $msg -Encoding utf8 }
Log "=== WebUI build started at $(Get-Date) ==="
try {
    & docker compose -f deployments/docker-compose.yml build webui 2>&1 | ForEach-Object { Log "$_" }
} catch {
    Log "ERROR: $($_.Exception.Message)"
    Log $_PSItem.ScriptStackTrace
}
Log "=== WebUI build finished at $(Get-Date) ==="
