$c = Invoke-RestMethod http://localhost:18081/api/v1/cluster
Write-Host "backendEngine: $($c.backendEngine)"
Write-Host "operatingMode: $($c.operatingMode)"
Write-Host "totalBackends: $($c.totalBackends)"
Write-Host "healthyBackends: $($c.healthyBackends)"
Write-Host "backendTypeCounts: $($c.backendTypeCounts | ConvertTo-Json -Compress)"
foreach($b in $c.backends){
  Write-Host "Backend: id=$($b.id) status=$($b.status) type=$($b.backendType)"
}