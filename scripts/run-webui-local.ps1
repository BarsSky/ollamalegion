<#
.SYNOPSIS
    Запускает WebUI локально через nginx с proxy к API балансировщика.
.DESCRIPTION
    Генерирует nginx.conf и config.js из шаблонов и запускает nginx.
    Требует установленного nginx в PATH.
.PARAMETER ApiHost
    Хост API балансировщика (по умолчанию localhost)
.PARAMETER ApiPort
    Порт API балансировщика (по умолчанию 18081)
.PARAMETER NginxPort
    Порт WebUI (по умолчанию 18030)
#>
param(
    [string]$ApiHost = "localhost",
    [int]$ApiPort = 18081,
    [int]$NginxPort = 18030
)

$ErrorActionPreference = "Stop"

$Root = Resolve-Path "$PSScriptRoot\.."
$Webui = "$Root\webui"
$NginxConf = "$Webui\nginx.local.conf"

# Проверка наличия nginx
$Nginx = Get-Command nginx -ErrorAction SilentlyContinue
if (-not $Nginx) {
    Write-Error "nginx не найден в PATH. Установите nginx: https://nginx.org/en/download.html"
    exit 1
}

# Генерация локального nginx.conf
Write-Host "[run-webui-local] Generating nginx config for port $NginxPort -> API $ApiHost`:$ApiPort"

$NginxTemplate = @"
server {
    listen $NginxPort;
    server_name localhost;
    root $Webui;
    index index.html;

    gzip on;
    gzip_types text/plain text/css application/javascript application/json;

    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header X-Content-Type-Options "nosniff" always;

    location /api/ {
        proxy_pass http://$ApiHost`:$ApiPort/api/;
        proxy_http_version 1.1;
        proxy_set_header Host `$host;
        proxy_connect_timeout 30s;
        proxy_send_timeout 30s;
        proxy_read_timeout 30s;
    }

    location /ws/ {
        proxy_pass http://$ApiHost`:$ApiPort/ws/;
        proxy_http_version 1.1;
        proxy_set_header Upgrade `$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_read_timeout 86400s;
    }

    location ~* \.(js|css|png|jpg|jpeg|gif|ico|svg|woff|woff2|ttf|eot)$ {
        expires 1y;
        add_header Cache-Control "public, immutable";
    }

    location / {
        try_files `$uri `$uri/ /index.html;
    }

    location /health {
        access_log off;
        return 200 "healthy\n";
        add_header Content-Type text/plain;
    }
}
"@

$NginxTemplate | Set-Content -Path $NginxConf -Encoding UTF8

# Генерация config.js для локального запуска
$ConfigJs = @"
window.WEBUI_CONFIG = window.WEBUI_CONFIG || {};
Object.assign(window.WEBUI_CONFIG, {
    API_BASE: '',
    WS_URL: null,
    API_TOKEN: '',
    REFRESH_INTERVAL: 5000,
    MAX_RECONNECT_ATTEMPTS: 10,
    RECONNECT_INTERVAL_BASE: 3000
});
"@

$ConfigJs | Set-Content -Path "$Webui\js\modules\config.js" -Encoding UTF8

Write-Host "[run-webui-local] Starting nginx on http://localhost:$NginxPort"
Write-Host "[run-webui-local] Press Ctrl+C to stop"

# Запуск nginx с кастомным конфигом
nginx -c $NginxConf -p $Webui

Write-Host "[run-webui-local] nginx stopped"