$token = "changeme-bundled-full-token-min-32-chars-please"
$body = '{"name":"Qwen3-Instruct-2507-q4km","path":"/app/models/Qwen3-Instruct-2507-q4km.gguf"}'
[System.IO.File]::WriteAllText("$PSScriptRoot\load_qwen3.json", $body, (New-Object System.Text.UTF8Encoding $false))

$url = "http://192.168.13.20:18092/api/models/load?wait=true&waitTimeoutSec=300000"
$cmd = "curl -s -X POST -H 'Authorization: Bearer $token' -H 'Content-Type: application/json' -d @`"$PSScriptRoot\load_qwen3.json`" `"$url`""
Write-Host "Running: $cmd"
cmd /c $cmd 2>&1 | Out-File -Encoding utf8 -FilePath "$PSScriptRoot\load_qwen3_resp.log"
Get-Content "$PSScriptRoot\load_qwen3_resp.log" -Head 5
