Set-Location C:\Ollama\ollamalegion
$env:CUDA_ARCH = '86'
$log = 'C:\Ollama\ollamalegion\logs\build_v0513_20260804_191850.log'
function Log($msg) { Add-Content -Path $log -Value $msg -Encoding utf8 }
Log "=== Build started at $(Get-Date) ==="
try {
    & .\scripts\build-containers.ps1 -CppWorker -CudaArch '86' *>&1 | ForEach-Object { Log "$_" }
} catch {
    Log "ERROR: $($_.Exception.Message)"
    Log $_.ScriptStackTrace
}
Log "=== Build finished at $(Get-Date) ==="
