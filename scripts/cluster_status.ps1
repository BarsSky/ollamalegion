#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Краткая сводка состояния кластера OllamaLegion (бэкенды, типы, статусы).

.DESCRIPTION
    Использует Management API балансировщика. По умолчанию ходит на
    http://localhost:18081, переопределяется через -Admin или env OLLAMALEGION_ADMIN.

.EXAMPLE
    pwsh scripts/cluster_status.ps1
    pwsh scripts/cluster_status.ps1 -Admin http://balancer.local:18081
#>

[CmdletBinding()]
param(
    [string]$Admin = $env:OLLAMALEGION_ADMIN
)

if ([string]::IsNullOrWhiteSpace($Admin)) {
    $Admin = "http://localhost:18081"
}

try {
    $c = Invoke-RestMethod -Uri "$Admin/api/v1/cluster" -Method Get -TimeoutSec 10
    Write-Host "backendEngine:      $($c.backendEngine)"
    Write-Host "operatingMode:      $($c.operatingMode)"
    Write-Host "totalBackends:      $($c.totalBackends)"
    Write-Host "healthyBackends:    $($c.healthyBackends)"
    Write-Host "backendTypeCounts:  $($c.backendTypeCounts | ConvertTo-Json -Compress)"
    foreach ($b in $c.backends) {
        Write-Host "Backend: id=$($b.id) status=$($b.status) type=$($b.backendType)"
    }
}
catch {
    Write-Host "ERROR: $($_.Exception.Message)" -ForegroundColor Red
    exit 1
}