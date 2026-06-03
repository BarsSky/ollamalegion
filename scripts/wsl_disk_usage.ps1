#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Подробная проверка WSL: VHDX-файлы Docker Desktop, размер overlay2/volumes внутри WSL.

.DESCRIPTION
    Использует каталог $env:LOCALAPPDATA\Docker\wsl для поиска VHDX.
    Если Docker Desktop установлен в другом месте — переопределите через -DockerWslPath.

.EXAMPLE
    pwsh scripts/wsl_disk_usage.ps1
    pwsh scripts/wsl_disk_usage.ps1 -DockerWslPath "D:\Docker\wsl"
#>

[CmdletBinding()]
param(
    [string]$DockerWslPath
)

if ([string]::IsNullOrWhiteSpace($DockerWslPath)) {
    $DockerWslPath = "$env:LOCALAPPDATA\Docker\wsl"
}

Write-Host "=== WSL Distributions ==="
try { wsl --list --verbose } catch { Write-Host "  wsl недоступен: $($_.Exception.Message)" }

Write-Host "`n=== Docker VHDX File Sizes ($DockerWslPath) ==="
if (Test-Path $DockerWslPath) {
    Get-ChildItem -Path $DockerWslPath -Filter *.vhdx -Recurse -ErrorAction SilentlyContinue | ForEach-Object {
        $sizeGB = [math]::Round($_.Length / 1GB, 2)
        Write-Host "  $($_.FullName) : $sizeGB GB"
    }
}
else {
    Write-Host "  Path not found: $DockerWslPath"
}

Write-Host "`n=== Disk Usage Inside docker-desktop WSL ==="
try { wsl -d docker-desktop df -h / 2>&1 | Select-Object -First 10 } catch { Write-Host "  $_" }

Write-Host "`n=== Disk Usage Inside docker-desktop-data WSL ==="
try { wsl -d docker-desktop-data df -h / 2>&1 | Select-Object -First 10 } catch { Write-Host "  $_" }

Write-Host "`n=== Docker Overlay2 Size (docker-desktop) ==="
try { wsl -d docker-desktop sh -c "du -sh /var/lib/docker/overlay2 2>/dev/null || echo 'not found'" 2>&1 | Select-Object -First 5 } catch { Write-Host "  $_" }

Write-Host "`n=== Docker Volumes Size ==="
try { wsl -d docker-desktop sh -c "du -sh /var/lib/docker/volumes 2>/dev/null || echo 'not found'" 2>&1 | Select-Object -First 5 } catch { Write-Host "  $_" }

Write-Host "`n=== Largest Overlay2 Layers (top 5) ==="
try { wsl -d docker-desktop sh -c "du -sh /var/lib/docker/overlay2/* 2>/dev/null | sort -rh | head -5 || echo 'none'" 2>&1 | Select-Object -First 10 } catch { Write-Host "  $_" }