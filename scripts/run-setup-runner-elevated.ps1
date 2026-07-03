# Перерегистрация runner'а с меткой ollamalegion-ci
# Запускать ТОЛЬКО из PowerShell от имени Администратора!
# (ПКМ на PowerShell → Запуск от имени администратора)

$logPath = "C:\Users\knaga\runner-setup.log"
$runnerDir = "C:\actions-runner"
$regToken = "AHOXUNLGHM6PON3MHLYEFJLKIITGK"
$runnerName = "skyworker-ci"
$labels = "self-hosted,windows,ollamalegion-ci"

$psArgs = @"
-NoProfile -ExecutionPolicy Bypass -Command "`$ErrorActionPreference='Stop'; Set-Location '$runnerDir'; try { Write-Host 'Stopping service...'; & .\svc.cmd stop 2>`$null } catch {}; Start-Sleep -Seconds 3; Write-Host 'Removing old registration...'; & .\config.cmd remove --unattended 2>`$null; Write-Host 'Registering runner...'; & .\config.cmd --url https://github.com/BarsSky/ollamalegion --token '$regToken' --name '$runnerName' --labels '$labels' --work _work --replace --unattended; if (`$LASTEXITCODE -ne 0) { Write-Host 'Registration failed!'; exit 1 }; Write-Host 'Starting service...'; & .\svc.cmd start; if (`$LASTEXITCODE -ne 0) { Write-Host 'Service start failed! Trying install...'; & .\svc.cmd install; & .\svc.cmd start }; Write-Host 'DONE!' } catch { Write-Host `"ERROR: `$_`"; exit 1 }" *>&1 | Out-File '$logPath'
"@

Write-Host "=== Runner '$runnerName' перерегистрация с labels='$labels' ==="
Write-Host "Будет запрошено подтверждение UAC (нажмите Да)."
Start-Process powershell -ArgumentList $psArgs -Verb RunAs -Wait -WindowStyle Normal

if (Test-Path $logPath) {
    Write-Host "`n=== ЛОГ ==="
    Get-Content $logPath
} else {
    Write-Host "`nЛог не найден."
}
