# Создаём JSON в файле чтобы избежать проблем с quoting
$token = "changeme-bundled-full-token-min-32-chars-please"
$bigContent = "x" * 80000
$body = @{
    model = "Qwen3-Instruct-2507-q4km"
    messages = @(@{ role = "user"; content = $bigContent })
    max_tokens = 50
    stream = $false
} | ConvertTo-Json -Compress
$body | Out-File -Encoding utf8 -FilePath "C:\Ollama\ollamalegion\logs\big_prompt.json"
Write-Host "Body file size: $((Get-Item C:\Ollama\ollamalegion\logs\big_prompt.json).Length) bytes"
Write-Host "Calling curl..."
cmd /c "curl -s -i -X POST -H 'Authorization: Bearer $token' -H 'Content-Type: application/json' -d @C:\Ollama\ollamalegion\logs\big_prompt.json http://192.168.13.20:18092/v1/chat/completions 2>&1" > C:\Ollama\ollamalegion\logs\big_prompt_response.log
Write-Host "Response size: $((Get-Item C:\Ollama\ollamalegion\logs\big_prompt_response.log).Length) bytes"
