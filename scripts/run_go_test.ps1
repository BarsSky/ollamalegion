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
    # Печатаем не только строки FAIL/--- FAIL:, но и следующие за ними
    # отступленные строки — это t.Errorf/t.Fatalf-сообщения и фреймы детектора
    # гонок. Без них по логу не понять, ЧТО упало.
    Write-Host "--- упавшие пакеты и тесты (с сообщениями об ошибках) ---"
    $lines = Get-Content $out
    for ($i = 0; $i -lt $lines.Count; $i++) {
        $line = $lines[$i]
        # R245 (2026-10-26): гонка НЕ печатает `--- FAIL:` — детектор выводит
        # `WARNING: DATA RACE` и стек, а тест падает позже строкой
        # `race detected during execution of test`. Без этого блока в логе CI
        # оставались только `FAIL <pkg>` и хвост чужих логов, и по прогону
        # 40326149098 нельзя было понять, ЧТО именно упало.
        if ($line -match '^WARNING: DATA RACE') {
            Write-Host $line
            $end = [Math]::Min($i + 45, $lines.Count - 1)
            for ($k = $i + 1; $k -le $end; $k++) { Write-Host $lines[$k] }
            $i = $end
            continue
        }
        if ($line -match '^(FAIL|--- FAIL:|panic:|# )') {
            Write-Host $line
            $j = $i + 1
            while ($j -lt $lines.Count -and $lines[$j] -match '^\s') {
                Write-Host $lines[$j]
                $j++
            }
        }
    }
    Write-Host "--- последние 60 строк вывода ---"
    Get-Content $out -Tail 60
    exit $status
} finally {
    Remove-Item -Force $out -ErrorAction SilentlyContinue
}
