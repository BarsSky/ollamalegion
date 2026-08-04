Set-Location C:\Ollama\ollamalegion
$env:DOCKER_BUILDKIT = '1'
$env:CUDA_ARCH = '86'
$log = 'C:\Ollama\ollamalegion\logs\build_cors_v0513_20260804_213312.log'
function Log($msg) { Add-Content -Path $log -Value $msg -Encoding utf8 }
Log "=== Build started at $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') ==="
try {
    & docker build --build-arg CUDA_ARCH=86 -t ollama-legion/cppworker:gpu-86 -f docker/cppworker/Dockerfile.gpu.86 --target runtime . 2>&1 | ForEach-Object { Log "$_" }
    Log "=== Build exit code: $LASTEXITCODE ==="
} catch {
    Log "ERROR: $($_.Exception.Message)"
}
Log "=== Build finished at $(Get-Date -Format 'yyyy-MM-dd HH:mm:ss') ==="
