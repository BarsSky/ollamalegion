# ============================================================
# OllamaLegion — Удалённый деплоймент llama.cpp узла (Windows)
# ============================================================
# Копирует docker-compose.llama.cpp.yml, .env и конфигурацию
# на удалённую Windows-машину по WinRM/SSH и запускает контейнеры.
#
# Использование:
#   .\deploy-llama-remote.ps1 -ComputerName <host> -ModelsDir <path> -BalancerUrl <url> [-NodeName <name>] [-ApiToken <token>] [-GpuCount <n>] [-Variant <cpu|gpu>]
#
# Параметры:
#   -ComputerName  — имя/IP удалённой машины (обязательно)
#   -ModelsDir     — путь к GGUF-моделям на удалённой машине (обязательно)
#   -BalancerUrl   — URL балансера для регистрации (обязательно)
#   -NodeName      — имя узла (по умолчанию: llama-{ComputerName})
#   -ApiToken      — API токен безопасности (опционально)
#   -GpuCount      — количество GPU (по умолчанию: 1)
#   -Variant       — cpu или gpu (по умолчанию: gpu)
#   -Credential    — PSCredential для WinRM (опционально)
#   -UseSsh        — использовать SSH вместо WinRM
#
# Примеры:
#   # LAN с GPU (WinRM):
#   .\deploy-llama-remote.ps1 -ComputerName 192.0.2.50 -ModelsDir D:\models -BalancerUrl http://192.0.2.10:18081
#
#   # По SSH:
#   .\deploy-llama-remote.ps1 -ComputerName 192.0.2.50 -ModelsDir /data/models -BalancerUrl http://192.0.2.10:18081 -UseSsh -Credential (Get-Credential)
#
#   # CPU-only:
#   .\deploy-llama-remote.ps1 -ComputerName 192.0.2.60 -ModelsDir D:\models -BalancerUrl http://192.0.2.10:18081 -Variant cpu -GpuCount 0
# ============================================================

param(
    [Parameter(Mandatory=$true)]
    [string]$ComputerName,

    [Parameter(Mandatory=$true)]
    [string]$ModelsDir,

    [Parameter(Mandatory=$true)]
    [string]$BalancerUrl,

    [string]$NodeName = "",

    [string]$ApiToken = "",

    [int]$GpuCount = 1,

    [ValidateSet("gpu", "cpu")]
    [string]$Variant = "gpu",

    [System.Management.Automation.PSCredential]$Credential,

    [switch]$UseSsh
)

# --- Автоопределение имени узла ---
if ([string]::IsNullOrEmpty($NodeName)) {
    $NodeName = "llama-$ComputerName" -replace '\.', '-'
}

# --- Пути ---
$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ProjectDir = Split-Path -Parent $ScriptDir
$RemoteDir = if ($UseSsh) { "/opt/ollamalegion" } else { "C:\ollamalegion" }

# --- Выбор compose-файла ---
if ($Variant -eq "cpu") {
    $ComposeFile = "docker-compose.llama.cpu.yml"
    Write-Host ">>> Режим: CPU-only"
} else {
    $ComposeFile = "docker-compose.llama.cpp.yml"
    Write-Host ">>> Режим: GPU (count=$GpuCount)"
}

# --- Отображение конфигурации ---
Write-Host "============================================" -ForegroundColor Cyan
Write-Host " Деплоймент llama.cpp узла (Windows)" -ForegroundColor Cyan
Write-Host "============================================" -ForegroundColor Cyan
Write-Host " Хост:            $ComputerName"
Write-Host " Модели:          $ModelsDir"
Write-Host " Балансер:        $BalancerUrl"
Write-Host " Узел:            $NodeName"
Write-Host " GPU:             $GpuCount"
Write-Host " Compose:         $ComposeFile"
Write-Host " Удалённый путь:  $RemoteDir"
Write-Host " Транспорт:       $(if ($UseSsh) { 'SSH' } else { 'WinRM' })"
Write-Host "============================================"
Write-Host ""

# --- Функция выполнения команд на удалённой машине ---
function Invoke-RemoteCommand {
    param([string]$Command)
    if ($UseSsh) {
        $sshArgs = @($ComputerName, $Command)
        if ($Credential) {
            & ssh "$($Credential.UserName)@$ComputerName" $Command
        } else {
            & ssh $ComputerName $Command
        }
    } else {
        $params = @{
            ComputerName = $ComputerName
            ScriptBlock = [scriptblock]::Create($Command)
        }
        if ($Credential) { $params.Credential = $Credential }
        Invoke-Command @params
    }
}

# --- Функция копирования файлов ---
function Copy-RemoteFile {
    param([string]$Source, [string]$Destination)
    if ($UseSsh) {
        $userPrefix = if ($Credential) { "$($Credential.UserName)@" } else { "" }
        & scp $Source "${userPrefix}${ComputerName}:$Destination"
    } else {
        $sessionParams = @{ ComputerName = $ComputerName }
        if ($Credential) { $sessionParams.Credential = $Credential }
        $session = New-PSSession @sessionParams
        Copy-Item -Path $Source -Destination $Destination -ToSession $session -Force
        Remove-PSSession $session
    }
}

# --- Подготовка временных файлов ---
$TempDir = Join-Path $env:TEMP "ollamalegion-deploy-$(Get-Random)"
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    # --- Шаг 1: Создание директорий ---
    Write-Host ">>> Шаг 1/5: Создание директорий на $ComputerName ..."
    Invoke-RemoteCommand "mkdir -p $RemoteDir/config $RemoteDir/models 2>$null; New-Item -ItemType Directory -Path '$RemoteDir\config', '$RemoteDir\models' -Force -ErrorAction SilentlyContinue"

    # --- Шаг 2: Копирование compose-файла ---
    Write-Host ">>> Шаг 2/5: Копирование $ComposeFile ..."
    $composeSource = Join-Path $ProjectDir "deployments\$ComposeFile"
    Copy-RemoteFile $composeSource "$RemoteDir/docker-compose.yml"

    # --- Шаг 3: Подготовка и копирование cppworker.env ---
    Write-Host ">>> Шаг 3/5: Подготовка cppworker.env ..."
    $envSource = Join-Path $ProjectDir "config\cppworker.example.env"
    $envContent = Get-Content $envSource -Raw

    $envContent = $envContent -replace '(?m)^NODE_NAME=.*', "NODE_NAME=$NodeName"
    $envContent = $envContent -replace '(?m)^BALANCER_URL=.*', "BALANCER_URL=$BalancerUrl"
    $envContent = $envContent -replace '(?m)^LLAMA_MODELS_DIR=.*', "LLAMA_MODELS_DIR=/models"
    $envContent = $envContent -replace '(?m)^API_TOKEN=.*', "API_TOKEN=$ApiToken"

    if ($Variant -eq "cpu") {
        $envContent = $envContent -replace '(?m)^LLAMA_N_GPU_LAYERS=.*', "LLAMA_N_GPU_LAYERS=0"
        $envContent = $envContent -replace '(?m)^LLAMA_FLASH_ATTN=.*', "LLAMA_FLASH_ATTN=false"
    }

    $envTempPath = Join-Path $TempDir "cppworker.env"
    $envContent | Set-Content $envTempPath -NoNewline
    Copy-RemoteFile $envTempPath "$RemoteDir/config/cppworker.env"

    # --- Шаг 4: Подготовка и копирование .env ---
    Write-Host ">>> Шаг 4/5: Подготовка .env для docker compose ..."
    $dotEnvContent = @"
MODELS_DIR=$ModelsDir
BALANCER_URL=$BalancerUrl
NODE_NAME=$NodeName
GPU_COUNT=$GpuCount
API_TOKEN=$ApiToken
LLAMA_HTTP_PORT=18091
LLAMA_GRPC_PORT=19000
AGENT_PORT=18032
HEARTBEAT_INTERVAL=30
COLLECT_INTERVAL=15
LLAMA_N_GPU_LAYERS=$(if ($Variant -eq "cpu") { "0" } else { "-1" })
LLAMA_CTX_SIZE=$(if ($Variant -eq "cpu") { "4096" } else { "8192" })
LLAMA_BATCH_SIZE=$(if ($Variant -eq "cpu") { "256" } else { "512" })
LLAMA_FLASH_ATTN=$(if ($Variant -eq "cpu") { "false" } else { "true" })
LLAMA_IDLE_UNLOAD=30m
LLAMA_ENFORCE_REPLICATION=false
NODE_LABELS=$(if ($Variant -eq "cpu") { "zone=lan,cpu_only=true" } else { "zone=lan" })
"@

    $dotEnvTempPath = Join-Path $TempDir ".env"
    $dotEnvContent | Set-Content $dotEnvTempPath -NoNewline
    Copy-RemoteFile $dotEnvTempPath "$RemoteDir/.env"

    # --- Шаг 5: Запуск контейнеров ---
    Write-Host ">>> Шаг 5/5: Запуск контейнеров на $ComputerName ..."
    Invoke-RemoteCommand "cd $RemoteDir && docker compose up -d"

    Write-Host ""
    Write-Host ">>> Ожидание запуска (15 секунд) ..."
    Start-Sleep -Seconds 15

    Write-Host ""
    Write-Host ">>> Статус контейнеров:"
    Invoke-RemoteCommand "cd $RemoteDir && docker compose ps"

    Write-Host ""
    Write-Host "============================================" -ForegroundColor Green
    Write-Host " Деплоймент завершён!" -ForegroundColor Green
    Write-Host "============================================" -ForegroundColor Green
    Write-Host ""
    Write-Host "Проверка health:"
    Write-Host "  Invoke-RemoteCommand 'curl -s http://localhost:18091/api/v1/cppworker/health'"
    Write-Host ""
    Write-Host "Просмотр логов:"
    Write-Host "  Invoke-RemoteCommand 'cd $RemoteDir && docker compose logs -f'"
    Write-Host ""
    Write-Host "Остановка:"
    Write-Host "  Invoke-RemoteCommand 'cd $RemoteDir && docker compose down'"
    Write-Host ""
    Write-Host "Проверка регистрации на балансере:"
    Write-Host "  Invoke-RestMethod -Uri '$BalancerUrl/api/v1/cluster/backends' | ConvertTo-Json"
    Write-Host ""

} finally {
    # Очистка временных файлов
    Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}