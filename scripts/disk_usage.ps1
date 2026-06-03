#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Сводка по дискам: docker system df + WSL-дистрибутивы + свободное место на дисках.

.EXAMPLE
    pwsh scripts/disk_usage.ps1
#>

[CmdletBinding()]
param()

Write-Host "=== Docker Disk Usage ==="
try { docker system df } catch { Write-Host "  docker недоступен: $($_.Exception.Message)" }

Write-Host "`n=== WSL Distributions ==="
try { wsl --list --verbose } catch { Write-Host "  wsl недоступен: $($_.Exception.Message)" }

Write-Host "`n=== Windows Drive Space ==="
try {
    Get-PSDrive -PSProvider FileSystem |
        Where-Object { $_.Name -notin @('A','B','D','E','F','G','H','I','J','K','L','M','N','O','P','Q','R','S','T','U','V','W','X','Y') } |
        Format-Table Name, @{n='Used(GB)';e={[math]::Round($_.Used/1GB,2)}}, @{n='Free(GB)';e={[math]::Round($_.Free/1GB,2)}} -AutoSize
}
catch {
    Write-Host "  ошибка получения дисков: $($_.Exception.Message)"
}