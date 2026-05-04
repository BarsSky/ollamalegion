# Развертывание Ollama Load Balancer

Руководство по развертыванию всех компонентов системы в различных средах.

## Содержание

1. [Docker Compose развертывание](#docker-compose-развертывание)
2. [Развертывание агента на GPU серверах](#развертывание-агента-на-gpu-серверах)
3. [Production развертывание](#production-развертывание)
4. [Масштабирование](#масштабирование)

---

## Docker Compose развертывание

### Быстрый старт

```bash
# Перейдите в директорию deployments
cd deployments

# Запуск балансировщика и Web UI
docker-compose up -d

# Просмотр логов
docker-compose logs -f loadbalancer

# Проверка статуса
docker-compose ps

# Остановка
docker-compose down
```

### Конфигурация docker-compose.yml

Файл [`deployments/docker-compose.yml`](../deployments/docker-compose.yml):

```yaml
services:
  loadbalancer:
    image: ollama-legion/balancer:latest
    build:
      context: ..
      dockerfile: docker/balancer/Dockerfile
    container_name: ollama-legion-balancer
    restart: unless-stopped
    
    ports:
      - "${LB_PORT:-18080}:18080"
      - "${LB_API_PORT:-18081}:18081"
    
    volumes:
      - ../config/config.example.json:/app/config.json:ro
    
    environment:
      - LB_HOST=${LB_HOST:-0.0.0.0}
      - LB_PORT=${LB_PORT:-18080}
      - LB_API_PORT=${LB_API_PORT:-18081}
      - LB_ALGORITHM=${LB_ALGORITHM:-resource-aware}
      - LB_MODEL_AFFINITY=${LB_MODEL_AFFINITY:-true}
      - LB_SESSION_STICKINESS=${LB_SESSION_STICKINESS:-true}
      - LB_HEALTH_CHECK_INTERVAL=${LB_HEALTH_CHECK_INTERVAL:-10}
      - LB_METRICS_INTERVAL=${LB_METRICS_INTERVAL:-5}
      - LB_REQUEST_TIMEOUT=${LB_REQUEST_TIMEOUT:-120}
      - LB_QUEUE_TIMEOUT=${LB_QUEUE_TIMEOUT:-300}
      - LB_QUEUE_MAX_SIZE=${LB_QUEUE_MAX_SIZE:-100}
      - LB_GPU_MAX_USAGE=${LB_GPU_MAX_USAGE:-90}
      - LB_GPU_MAX_VRAM=${LB_GPU_MAX_VRAM:-85}
      - LB_GPU_MAX_TEMP=${LB_GPU_MAX_TEMP:-85}
      - LB_CPU_MAX_USAGE=${LB_CPU_MAX_USAGE:-80}
      - LB_MEMORY_MAX_USAGE=${LB_MEMORY_MAX_USAGE:-85}
      - LB_DISK_MIN_FREE_MB=${LB_DISK_MIN_FREE_MB:-10240}
      - LB_LOG_LEVEL=${LB_LOG_LEVEL:-info}
      - LB_LOG_FORMAT=${LB_LOG_FORMAT:-json}
      
      # Бэкенды через переменные окружения
      - BACKEND_0_ID=${BACKEND_0_ID:-gpu-1}
      - BACKEND_0_NAME=${BACKEND_0_NAME:-GPU Server 1}
      - BACKEND_0_HOST=${BACKEND_0_HOST:-192.168.13.66}
      - BACKEND_0_PORT=${BACKEND_0_PORT:-11434}
      - BACKEND_0_AGENT_PORT=${BACKEND_0_AGENT_PORT:-18032}
      - BACKEND_0_WEIGHT=${BACKEND_0_WEIGHT:-1}
      - BACKEND_0_MAX_REQS=${BACKEND_0_MAX_REQS:-10}
    
    networks:
      - ollama-legion-net
    
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:18081/api/v1/health"]
      interval: 30s
      timeout: 60s
      retries: 3
      start_period: 60s

  webui:
    image: ollama-legion/webui:latest
    container_name: ollama-legion-webui
    restart: unless-stopped
    
    ports:
      - "${WEBUI_PORT:-18030}:80"
    
    volumes:
      - ../webui/dist:/usr/share/nginx/html:ro
      - ../webui/nginx.conf:/etc/nginx/conf.d/default.conf:ro
    
    networks:
      - ollama-legion-net

networks:
  ollama-legion-net:
    driver: bridge
    ipam:
      config:
        - subnet: 172.28.0.0/16
```

### Файл .env для Docker Compose

Создайте файл `.env` в директории `deployments/`:

```bash
# Load Balancer settings
LB_HOST=0.0.0.0
LB_PORT=18080
LB_API_PORT=18081

# Algorithm settings
LB_ALGORITHM=resource-aware
LB_MODEL_AFFINITY=true
LB_SESSION_STICKINESS=true

# Intervals (seconds)
LB_HEALTH_CHECK_INTERVAL=10
LB_METRICS_INTERVAL=5

# Timeouts (seconds)
LB_REQUEST_TIMEOUT=120
LB_QUEUE_TIMEOUT=300
LB_QUEUE_MAX_SIZE=100

# Resource limits
LB_GPU_MAX_USAGE=90
LB_GPU_MAX_VRAM=85
LB_GPU_MAX_TEMP=85
LB_CPU_MAX_USAGE=80
LB_MEMORY_MAX_USAGE=85
LB_DISK_MIN_FREE_MB=10240

# Logging
LB_LOG_LEVEL=info
LB_LOG_FORMAT=json

# Web UI
WEBUI_PORT=18030

# Backends
BACKEND_0_ID=gpu-1
BACKEND_0_NAME=GPU Server 1
BACKEND_0_HOST=192.168.13.66
BACKEND_0_PORT=11434
BACKEND_0_AGENT_PORT=18032
BACKEND_0_WEIGHT=1
BACKEND_0_MAX_REQS=10

BACKEND_1_ID=gpu-2
BACKEND_1_NAME=GPU Server 2
BACKEND_1_HOST=192.168.13.70
BACKEND_1_PORT=11434
BACKEND_1_AGENT_PORT=18032
BACKEND_1_WEIGHT=1
BACKEND_1_MAX_REQS=10
```

---

## Развертывание агента на GPU серверах

Агент должен запускаться **отдельно на каждом GPU сервере** с установленным Ollama.

> **Важно:** Агент и балансер **НЕ обязаны** находиться в одной Docker-сети или на одном хосте.
> Единственное требование — **взаимная IP-доступность по HTTP**:
> - Агент → Балансер: HTTP запросы на `BALANCER_URL`
> - Балансер → Агент/Ollama: HTTP запросы на публичный IP агента

### Способ 1: Скрипт автоматического развертывания (рекомендуется)

Используйте скрипт [`deploy-agent-docker.sh`](../scripts/deploy-agent-docker.sh):

```bash
# Перейдите в директорию скриптов
cd scripts

# Запуск с параметрами (укажите публичный IP балансера)
./deploy-agent-docker.sh \
  --balancer-url http://192.168.1.10:18081 \
  --agent-id gpu-1 \
  --public-host 192.168.1.20

# Или кратко:
./deploy-agent-docker.sh -b http://192.168.1.10:18081 -i gpu-1 -h 192.168.1.20
```

#### Параметры скрипта

| Параметр | Краткий | Описание | Обязательный |
|----------|---------|----------|--------------|
| `--balancer-url` | `-b` | URL балансировщика (публичный IP) | Да |
| `--agent-id` | `-i` | Идентификатор агента | Да |
| `--public-host` | `-h` | Публичный IP/hostname агента | Да |
| `--agent-port` | `-p` | Порт агента (по умолчанию: 18032) | Нет |
| `--ollama-url` | `-o` | URL Ollama (по умолчанию: http://localhost:11434) | Нет |
| `--nvml-enabled` | `-n` | Включить NVML (по умолчанию: true) | Нет |
| `--metrics-interval` | `-m` | Интервал метрик (по умолчанию: 5s) | Нет |
| `--help` | `-h` | Показать справку | Нет |

### Способ 2: Docker Compose для агента

Используйте [`docker-compose.agent.yml`](../deployments/docker-compose.agent.yml):

```bash
# На GPU сервере
cd deployments

# Создание .env файла
cat > .env << EOF
# Обязательные параметры
AGENT_ID=gpu-1
BALANCER_URL=http://192.168.1.10:18081
AGENT_PUBLIC_HOST=192.168.1.20

# Сетевые настройки
# Для Linux используйте IP хоста или network_mode: host
# Для Windows/macOS host.docker.internal работает из коробки
AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

# GPU настройки
NVML_ENABLED=true
METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

# Логирование
LOG_LEVEL=info
LOG_FORMAT=text
EOF

# Запуск агента
docker-compose -f docker-compose.agent.yml --env-file .env up -d

# Просмотр логов
docker-compose -f docker-compose.agent.yml logs -f agent

# Проверка статуса
docker-compose -f docker-compose.agent.yml ps
```

#### Сетевые режимы Docker

**A) Bridge (по умолчанию)** — агент в изолированной сети, порты проброшены:
```yaml
# Порты проброшены на хост, балансер обращается к AGENT_PUBLIC_HOST:AGENT_PORT
ports:
  - "18032:18032"
```

**B) Host network (Linux)** — агент использует сетевой стек хоста напрямую:
```yaml
services:
  agent:
    network_mode: host
    # При host mode не нужны ports и networks
    environment:
      - AGENT_PUBLIC_HOST=192.168.1.20  # IP хоста
```

**C) Docker Swarm overlay** — для кластеров в swarm-режиме:
```yaml
networks:
  ollama-legion-net:
    driver: overlay
    external: true
```

#### GPU проброс для NVML

Конфигурация [`docker-compose.agent.yml`](../deployments/docker-compose.agent.yml) включает проброс GPU через NVIDIA Container Toolkit:

```yaml
services:
  agent:
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
```

**Требования для GPU проброса:**
- NVIDIA Driver (версия 535+ для Linux, 528+ для Windows)
- NVIDIA Container Toolkit установлен на хосте
- Docker Compose v3.8+

**Проверка NVIDIA Container Toolkit:**
```bash
# Проверка доступности GPU в Docker
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi

# Если команда выше работает, GPU проброс настроен корректно
```

**Проверка работы NVML внутри контейнера:**
```bash
# Подключение к контейнеру агента
docker exec -it ollama-legion-agent bash

# Проверка доступности nvidia-smi
nvidia-smi

# Проверка метрик агента
curl http://localhost:18032/metrics
```

### Способ 3: Docker run

```bash
# С bridge-сетью (порты проброшены)
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://192.168.1.10:18081 \
  -e AGENT_PUBLIC_HOST=192.168.1.20 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://192.168.1.20:11434 \
  -e NVML_ENABLED=true \
  -e METRICS_INTERVAL=5s \
  -e HEARTBEAT_INTERVAL=3s \
  -p 18032:18032 \
  ollama-legion/agent:latest

# С host-сетью (Linux)
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  --network host \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://192.168.1.10:18081 \
  -e AGENT_PUBLIC_HOST=192.168.1.20 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://localhost:11434 \
  -e NVML_ENABLED=true \
  ollama-legion/agent:latest
```

### Способ 4: Бинарный файл как systemd сервис

#### Шаг 1: Сборка и копирование

```bash
# На машине для сборки
cd /path/to/ollama-loadbalancer
./scripts/build-agent.sh

# Копирование на GPU сервер
scp ./bin/agent user@gpu-server:/usr/local/bin/
```

#### Шаг 2: Создание systemd сервиса

Создайте файл `/etc/systemd/system/ollama-agent.service`:

```ini
[Unit]
Description=Ollama Load Balancer Agent
After=network.target ollama.service
Wants=ollama.service

[Service]
Type=simple
User=ollama
Group=ollama
Environment="AGENT_ID=gpu-1"
Environment="BALANCER_URL=http://<balancer-ip>:18081"
Environment="COLLECT_INTERVAL=5"
Environment="HEARTBEAT_INTERVAL=3"
Environment="METRICS_PORT=18032"
ExecStart=/usr/local/bin/agent
Restart=always
RestartSec=10
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

#### Шаг 3: Запуск сервиса

```bash
# Перезагрузка systemd
sudo systemctl daemon-reload

# Включение автозапуска
sudo systemctl enable ollama-agent

# Запуск
sudo systemctl start ollama-agent

# Проверка статуса
sudo systemctl status ollama-agent

# Просмотр логов
journalctl -u ollama-agent -f
```

---

## Production развертывание

### Архитектура production кластера

```
                    ┌─────────────────┐
                    │  Load Balancer  │
                    │   (HA Cluster)  │
                    └────────┬────────┘
                             │
           ┌─────────────────┼─────────────────┐
           │                 │                 │
    ┌──────▼──────┐   ┌──────▼──────┐   ┌──────▼──────┐
    │  GPU Node 1 │   │  GPU Node 2 │   │  GPU Node N │
    │   + Agent   │   │   + Agent   │   │   + Agent   │
    │   Ollama    │   │   Ollama    │   │   Ollama    │
    └─────────────┘   └─────────────┘   └─────────────┘
```

### Требования для production

| Компонент | Требование |
|-----------|------------|
| **Load Balancer** | 2+ ноды для HA, внешний load balancer (nginx/HAProxy) |
| **GPU Nodes** | NVIDIA GPU с драйверами, Docker + NVIDIA Container Toolkit |
| **Сеть** | Минимум 1 Gbps между компонентами |
| **Хранение** | SSD для логов и конфигураций |

### HA конфигурация для Load Balancer

```yaml
# docker-compose.prod.yml
version: '3.8'

services:
  loadbalancer-1:
    image: ollama-legion/balancer:latest
    container_name: ollama-legion-1
    restart: unless-stopped
    ports:
      - "18080:18080"
      - "18081:18081"
    volumes:
      - ./config.json:/app/config.json:ro
    environment:
      - LB_HOST=0.0.0.0
      - LB_PORT=18080
      - LB_API_PORT=18081
    networks:
      - ollama-legion-net
    healthcheck:
      test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:18081/api/v1/health"]
      interval: 10s
      timeout: 5s
      retries: 3

  loadbalancer-2:
    image: ollama-legion/balancer:latest
    container_name: ollama-legion-2
    restart: unless-stopped
    ports:
      - "18082:18080"
      - "18083:18081"
    volumes:
      - ./config.json:/app/config.json:ro
    environment:
      - LB_HOST=0.0.0.0
      - LB_PORT=18080
      - LB_API_PORT=18081
    networks:
      - ollama-legion-net
    healthcheck:
      test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:18081/api/v1/health"]
      interval: 10s
      timeout: 5s
      retries: 3

  nginx-lb:
    image: nginx:alpine
    container_name: nginx-lb
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./nginx-lb.conf:/etc/nginx/nginx.conf:ro
      - ./certs:/etc/nginx/certs:ro
    depends_on:
      - loadbalancer-1
      - loadbalancer-2
    networks:
      - ollama-legion-net

networks:
  ollama-legion-net:
    driver: bridge
```

### Конфигурация nginx для HA

```nginx
# nginx-lb.conf
events {
    worker_connections 1024;
}

http {
    upstream ollama_lb {
        least_conn;
        server loadbalancer-1:18080;
        server loadbalancer-2:18080;
    }

    upstream management_api {
        least_conn;
        server loadbalancer-1:18081;
        server loadbalancer-2:18081;
    }

    server {
        listen 80;
        server_name _;

        location / {
            proxy_pass http://ollama_lb;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection 'upgrade';
            proxy_set_header Host $host;
            proxy_set_header X-Real-IP $remote_addr;
        }

        location /api/ {
            proxy_pass http://management_api;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection 'upgrade';
            proxy_set_header Host $host;
        }

        location /ws/ {
            proxy_pass http://management_api;
            proxy_http_version 1.1;
            proxy_set_header Upgrade $http_upgrade;
            proxy_set_header Connection "Upgrade";
            proxy_set_header Host $host;
        }
    }
}
```

### Безопасность production развертывания

#### 1. TLS/SSL шифрование

При включённом TLS прокси доступен на `tlsPort` (8443), а HTTPS Management API — на `tlsPort+1` (8444).

```bash
# Генерация сертификатов
openssl req -x509 -nodes -days 365 -newkey rsa:4096 \
  -keyout certs/server.key \
  -out certs/server.crt \
  -subj "/CN=ollama-legion.example.com"
```

#### 2. Firewall правила

```bash
# На Load Balancer
sudo ufw allow 80/tcp    # HTTP
sudo ufw allow 443/tcp   # HTTPS
sudo ufw allow 22/tcp    # SSH

# На GPU серверах
sudo ufw allow from <lb-ip> to any port 18032
sudo ufw allow 11434/tcp # Ollama (локально)
```

#### 3. Аутентификация

```json
{
  "auth": {
    "enabled": true,
    "tokens": [
      "secure-master-token-here"
    ],
    "headerName": "X-API-Token"
  }
}
```

---

## Масштабирование

### Горизонтальное масштабирование

Добавление новых GPU серверов:

```bash
# 1. Добавьте новый бэкенд в конфигурацию
# config.json или через переменные окружения

BACKEND_2_ID=gpu-3
BACKEND_2_NAME=GPU Server 3
BACKEND_2_HOST=192.168.13.80
BACKEND_2_PORT=11434
BACKEND_2_AGENT_PORT=18032
BACKEND_2_WEIGHT=1
BACKEND_2_MAX_REQS=10

# 2. Перезапустите балансировщик
docker-compose restart loadbalancer

# 3. Разверните агент на новом сервере
./deploy-agent-docker.sh -b http://<lb-ip>:18081 -i gpu-3
```

### Добавление бэкенда через API

```bash
# Регистрация нового бэкенда
curl -X POST http://<lb-ip>:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -H "X-API-Token: your-token" \
  -d '{
    "id": "gpu-3",
    "name": "GPU Server 3",
    "host": "192.168.13.80",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10,
    "labels": ["nvidia", "a100"]
  }'
```

### Вертикальное масштабирование

Увеличение ресурсов существующих узлов:

```json
{
  "resources": {
    "gpu": {
      "maxUsagePercent": 95,
      "maxVramUsagePercent": 90,
      "maxTemperature": 90
    },
    "cpu": {
      "maxUsagePercent": 90
    },
    "memory": {
      "maxUsagePercent": 90
    }
  },
  "balancing": {
    "queueMaxSize": 500,
    "queueTimeout": 900
  }
}
```

### Мониторинг масштабирования

```bash
# Проверка статуса кластера
curl http://<lb-ip>:18081/api/v1/cluster | jq

# Проверка метрик
curl http://<lb-ip>:18081/api/v1/metrics | jq

# WebSocket для real-time мониторинга
wscat -c ws://<lb-ip>:18081/ws/metrics
```

---

## Проверка развертывания

### Чеклист

```bash
# 1. Проверка балансировщика
curl http://localhost:18081/api/v1/health
# Ожидаемый ответ: {"status": "healthy", ...}

# 2. Проверка списка бэкендов
curl http://localhost:18081/api/v1/backends
# Ожидаемый ответ: {"backends": [...], "total": N}

# 3. Проверка Web UI (Dashboard)
# Откройте http://localhost:18030 в браузере

# 4. Проверка Монитора (real-time визуализация кластера)
# Откройте http://localhost:18030/monitor.html в браузере
# Монитор показывает:
#   - Canvas-визуализацию потока запросов между бэкендами
#   - Статус бэкендов (Healthy / Busy / Critical)
#   - Очередь запросов в реальном времени
#   - Диагностические предупреждения

# 5. Проверка агента на GPU сервере
curl http://localhost:18032/metrics

# 6. Проверка WebSocket
wscat -c ws://localhost:18081/ws/metrics

# 7. Тестовый запрос к Ollama через балансировщик
curl -X POST http://localhost:18080/api/generate \
  -H "Content-Type: application/json" \
  -d '{"model": "llama3.1:8b", "prompt": "Hello!", "stream": false}'
```

### Диагностика проблем

См. раздел [Troubleshooting](troubleshooting.md) для решения часто встречающихся проблем.

---

## Следующие шаги

- [API документация](api.md) — Использование REST API и WebSocket
- [Troubleshooting](troubleshooting.md) — Решение проблем
- [Конфигурация](configuration.md) — Настройка всех компонентов
