# run_go_test.ps1 — запуск `go test` с диагностируемым логом (Windows/self-hosted).
#
# См. scripts/run_go_test.sh — та же идея: при падении показываем сводку
# FAIL/panic, а не только последние строки. `go test | Select-Object -Last N`
# прячет строку `FAIL <pkg>` для многосоставных путей вроде ./internal/....
#
# Использование:
#   scripts/run_go_test.ps1 -Label "internal/balancer" -Args @("-race","-tags","llama_stub","./internal/...")
param(
    [Parameter(Mandatory = $true)][string]$Label,
    [Parameter(Mandatory = $true)][string[]]$Args
)

$go = if ($env:GOEXE -and (Test-Path $env:GOEXE)) { $env:GOEXE } else { "go.exe" }
$out = [System.IO.Path]::GetTempFileName()

try {
    & $go test @Args > $out 2>&1
    $status = $LASTEXITCODE

    if ($status -eq 0) {
        Write-Host "=== ${Label}: OK ==="
        Get-Content $out -Tail 10
        exit 0
    }

    Write-Host "=== ${Label}: FAILED (exit $status) ==="
    Write-Host "--- упавшие пакеты и тесты ---"
    Get-Content $out | Select-String -Pattern '^(FAIL|--- FAIL:|panic:|# )' | Select-Object -First 100 | ForEach-Object { $_.Line }
    Write-Host "--- последние 60 строк вывода ---"
    Get-Content $out -Tail 60
    exit $status
} finally {
    Remove-Item -Force $out -ErrorAction SilentlyContinue
}
