# Ollama Load Balancer - Руководство по развертыванию

Это руководство содержит подробные инструкции по развертыванию агента Ollama Load Balancer на GPU-серверах.

## Содержание

1. [Архитектура](#архитектура)
2. [Требования](#требования)
3. [Развертывание агента через Docker (рекомендуемый способ)](#развертывание-агента-через-docker)
4. [Развертывание агента как бинарного файла](#развертывание-агента-как-бинарного-файла)
5. [Настройка переменных окружения](#настройка-переменных-окружения)
6. [Регистрация агента через API](#регистрация-агента-через-api)
7. [Проверка работоспособности](#проверка-работоспособности)
8. [Troubleshooting](#troubleshooting)

---

## Архитектура

```
┌─────────────────────────────────────────────────────────────────┐
│                    Балансировщик нагрузки                       │
│                    (Load Balancer)                              │
│                    Порт: 18080 (прокси)                          │
│                    Порт: 18081 (API + WebSocket)                 │
└─────────────────────────────────────────────────────────────────┘
                               │
                               │ HTTP/REST + WebSocket
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                    GPU Серверы (Агенты)                         │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐             │
│  │  Agent 1    │  │  Agent 2    │  │  Agent N    │             │
│  │  GPU Server │  │  GPU Server │  │  GPU Server │             │
│  │  :18032     │  │  :18032     │  │  :18032     │             │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘             │
│         │                │                │                     │
│         ▼                ▼                ▼                     │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐             │
│  │   Ollama    │  │   Ollama    │  │   Ollama    │             │
│  │   :11434    │  │   :11434    │  │   :11434    │             │
│  └─────────────┘  └─────────────┘  └─────────────┘             │
└─────────────────────────────────────────────────────────────────┘
```

**Важно:** Агент и балансировщик — это независимые компоненты:
- **Балансировщик** запускается на отдельном сервере/хосте
- **Агент** запускается на каждом GPU-сервере с Ollama

---

## Требования

### Для агента на GPU-сервере

| Требование | Описание |
|------------|----------|
| **ОС** | Linux (Ubuntu 20.04+, Debian 11+, CentOS 8+) или Windows Server |
| **Docker** | 20.10+ с поддержкой NVIDIA Container Toolkit |
| **NVIDIA Driver** | 470.x или новее |
| **NVIDIA Container Toolkit** | Требуется для доступа к GPU метрикам |
| **NVML Library** | NVIDIA Management Library для точных метрик GPU |
| **Ollama** | Установлен и запущен на стандартном порту 11434 |
| **Сеть** | Доступ к балансировщику по HTTP/HTTPS |
| **RAM** | Минимум 128 MB для агента |
| **CPU** | 1 ядро |

### Проверка требований

```bash
# Проверка версии Docker
docker --version

# Проверка NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# Проверка Ollama
curl http://localhost:11434/api/tags

# Проверка nvidia-smi
nvidia-smi

# Проверка NVML (для разработчиков)
ldconfig -p | grep nvml
```

---

## Развертывание агента через Docker Compose (рекомендуемый способ)

Это рекомендуемый способ развертывания с использованием Docker Compose.

### Шаг 1: Подготовка

Убедитесь, что Docker и NVIDIA Container Toolkit установлены:

```bash
# Установка Docker (Ubuntu/Debian)
curl -fsSL https://get.docker.com | sh

# Установка Docker Compose (если не установлен)
sudo curl -L "https://github.com/docker/compose/releases/latest/download/docker-compose-$(uname -s)-$(uname -m)" -o /usr/local/bin/docker-compose
sudo chmod +x /usr/local/bin/docker-compose

# Установка NVIDIA Container Toolkit
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit
sudo systemctl restart docker
```

### Шаг 2: Настройка конфигурации

```bash
# Перейдите в директорию с конфигурацией
cd /path/to/ollama-loadbalancer/deployments

# Скопируйте пример конфигурации
cp ../config/agent.example.env .env

# Отредактируйте .env файл
nano .env
```

Отредактируйте следующие параметры в `.env`:

```bash
AGENT_ID=gpu-1
BALANCER_URL=http://<BALANCER_IP>:18081
```

### Шаг 3: Запуск через Docker Compose

```bash
# Запуск агента
docker-compose -f docker-compose.agent.yml up -d

# Просмотр логов
docker-compose -f docker-compose.agent.yml logs -f

# Проверка статуса
docker-compose -f docker-compose.agent.yml ps
```

### Шаг 4: Проверка

```bash
# Проверка статуса контейнера
docker ps | grep ollama-legion-agent

# Проверка метрик агента
curl http://localhost:18032/metrics

# Проверка подключения к Ollama
curl http://localhost:11434/api/tags
```

---

## Развертывание через скрипт deploy-agent-docker.sh

Для быстрого развертывания используйте скрипт:

```bash
# Перейдите в директорию скриптов
cd /path/to/ollama-loadbalancer/scripts

# Запуск с параметрами
./deploy-agent-docker.sh --balancer-url http://<BALANCER_IP>:18081 --agent-id gpu-1

# Или кратко:
./deploy-agent-docker.sh -b http://<BALANCER_IP>:18081 -i gpu-1
```

**Параметры скрипта:**

| Параметр | Краткий | Описание | Обязательный |
|----------|---------|----------|--------------|
| `--balancer-url` | `-b` | URL балансировщика | Да |
| `--agent-id` | `-i` | Идентификатор агента | Да |
| `--agent-port` | `-p` | Порт агента (по умолчанию: 18032) | Нет |
| `--ollama-url` | `-o` | URL Ollama (по умолчанию: http://localhost:11434) | Нет |
| `--nvml-enabled` | `-n` | Включить NVML (по умолчанию: true) | Нет |
| `--metrics-interval` | `-m` | Интервал метрик (по умолчанию: 5s) | Нет |
| `--help` | `-h` | Показать справку | Нет |

### Примеры использования

```bash
# Базовый запуск
./deploy-agent-docker.sh -b http://192.168.1.100:18081 -i gpu-1

# С кастомными параметрами
./deploy-agent-docker.sh -b http://lb:18081 -i gpu-2 -p 18032 -m 10s

# С полным путем к compose файлу
./deploy-agent-docker.sh -b http://lb:18081 -i gpu-3 --compose-file docker-compose.agent.yml
```

---

## Развертывание агента через Docker (docker run)

Альтернативный способ запуска без Docker Compose.

### Шаг 1: Загрузка образа агента

```bash
# Загрузка готового образа (если доступен в реестре)
docker pull ollama-legion/agent:latest

# ИЛИ сборка локально
cd /path/to/ollama-loadbalancer
docker build -f docker/agent/Dockerfile -t ollama-legion/agent:latest .
```

### Шаг 2: Запуск контейнера

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<BALANCER_IP>:18081 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://localhost:11434 \
  -e NVML_ENABLED=true \
  -e METRICS_INTERVAL=5s \
  -e HEARTBEAT_INTERVAL=3s \
  -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro \
  -v /var/run/nvidia-top-level-device:/var/run/nvidia-top-level-device:ro \
  -v /proc:/host/proc:ro \
  -v /sys:/host/sys:ro \
  --network host \
  --gpus all \
  ollama-legion/agent:latest
```

**Параметры:**

| Параметр | Описание |
|----------|----------|
| `--name` | Имя контейнера |
| `--restart` | Автоматический перезапуск |
| `-e AGENT_ID` | Уникальный идентификатор агента |
| `-e BALANCER_URL` | URL балансировщика |
| `-e AGENT_PORT` | Порт для локальных метрик |
| `-e OLLAMA_URL` | URL локального Ollama |
| `-e NVML_ENABLED` | Включить NVML |
| `-e METRICS_INTERVAL` | Интервал отправки метрик |
| `-e HEARTBEAT_INTERVAL` | Интервал heartbeat |
| `-v /usr/bin/nvidia-smi` | Доступ к nvidia-smi |
| `-v /proc` | Доступ к системным метрикам |
| `--network host` | Сетевой режим хоста |
| `--gpus all` | Доступ ко всем GPU |

### Шаг 3: Проверка

```bash
# Проверка статуса контейнера
docker ps | grep ollama-agent

# Просмотр логов
docker logs -f ollama-agent

# Проверка метрик агента
curl http://localhost:18032/metrics
```

---

## Развертывание агента как бинарного файла

### Шаг 1: Сборка агента

```bash
# Перейдите в директорию проекта
cd /path/to/ollama-loadbalancer

# Запустите скрипт сборки
chmod +x scripts/build-agent.sh
./scripts/build-agent.sh

# Бинарный файл будет создан в ./bin/agent
```

### Шаг 2: Копирование на GPU-сервер

```bash
# Копирование бинарного файла
scp ./bin/agent user@gpu-server:/usr/local/bin/

# Или загрузите напрямую на сервере
cd /path/to/ollama-loadbalancer
GOOS=linux GOARCH=amd64 go build -o agent ./cmd/agent
sudo cp agent /usr/local/bin/
sudo chmod +x /usr/local/bin/agent
```

### Шаг 3: Создание systemd сервиса

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
Environment="BALANCER_URL=http://<BALANCER_IP>:18081"
Environment="COLLECT_INTERVAL=5"
Environment="HEARTBEAT_INTERVAL=3"
Environment="AGENT_PORT=18032"
ExecStart=/usr/local/bin/agent
Restart=always
RestartSec=10
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
```

### Шаг 4: Запуск сервиса

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

## Настройка переменных окружения

### Обязательные переменные

| Переменная | Описание | Пример |
|------------|----------|--------|
| `AGENT_ID` | Уникальный идентификатор агента | `gpu-1`, `server-a100` |
| `BALANCER_URL` | URL балансировщика | `http://192.168.1.100:18081` |

### Опциональные переменные агента

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `METRICS_PORT` | `18032` | Порт для локальных метрик |
| `COLLECT_INTERVAL` | `5` | Интервал сбора метрик (сек) |
| `HEARTBEAT_INTERVAL` | `3` | Интервал отправки heartbeat (сек) |
| `CONFIG_PATH` | - | Путь к файлу конфигурации |
| `NVML_ENABLED` | `true` | Включить NVML поддержку |

### Переменные окружения балансировщика

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `LB_HOST` | `0.0.0.0` | Хост для прослушивания |
| `LB_PORT` | `18080` | Порт прокси для Ollama API |
| `LB_API_PORT` | `18081` | Порт Management API |
| `LB_ALGORITHM` | `resource-aware` | Алгоритм балансировки |
| `LB_MODEL_AFFINITY` | `true` | Привязка к модели |
| `LB_SESSION_STICKINESS` | `true` | Привязка сессии |
| `LB_HEALTH_CHECK_INTERVAL` | `10` | Интервал health check (сек) |
| `LB_METRICS_INTERVAL` | `5` | Интервал метрик (сек) |
| `LB_REQUEST_TIMEOUT` | `120` | Таймаут запроса (сек) |
| `LB_QUEUE_TIMEOUT` | `300` | Таймаут очереди (сек) |
| `LB_QUEUE_MAX_SIZE` | `100` | Макс. размер очереди |
| `LB_GPU_MAX_USAGE` | `90` | Макс. загрузка GPU (%) |
| `LB_GPU_MAX_VRAM` | `85` | Макс. использование VRAM (%) |
| `LB_GPU_MAX_TEMP` | `85` | Макс. температура GPU (°C) |
| `LB_CPU_MAX_USAGE` | `80` | Макс. загрузка CPU (%) |
| `LB_MEMORY_MAX_USAGE` | `85` | Макс. использование RAM (%) |
| `LB_DISK_MIN_FREE_MB` | `10240` | Мин. свободно на диске (MB) |
| `LB_LOG_LEVEL` | `info` | Уровень логирования |
| `LB_LOG_FORMAT` | `json` | Формат логов |

### Пример .env файла

Создайте файл `.env` на GPU-сервере:

```bash
# Идентификатор агента (должен быть уникальным)
AGENT_ID=gpu-1

# URL балансировщика
BALANCER_URL=http://192.168.1.100:18081

# Интервалы (секунды)
COLLECT_INTERVAL=5
HEARTBEAT_INTERVAL=3

# Порт для метрик
METRICS_PORT=18032

# NVML настройки
NVML_ENABLED=true
```

Запуск с .env файлом:

```bash
# Docker
docker run --env-file .env ... ollama-legion/agent:latest

# Бинарный файл
set -a; source .env; set +a; /usr/local/bin/agent
```

---

## Регистрация агента через API

Агент автоматически регистрируется при первом подключении к балансировщику.

### Ручная регистрация через API

```bash
# Регистрация нового бэкенда
curl -X POST http://<BALANCER_IP>:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{
    "id": "gpu-1",
    "name": "GPU Server 1",
    "host": "192.168.13.66",
    "ollamaPort": 11434,
    "agentPort": 18032,
    "weight": 1,
    "maxConcurrentRequests": 10
  }'
```

### Проверка зарегистрированных бэкендов

```bash
curl http://<BALANCER_IP>:18081/api/v1/backends
```

### Удаление бэкенда

```bash
curl -X DELETE http://<BALANCER_IP>:18081/api/v1/backends/gpu-1
```

---

## Проверка работоспособности

### 1. Проверка подключения агента

```bash
# Локальная проверка метрик
curl http://localhost:18032/metrics

# Проверка через API балансировщика
curl http://<BALANCER_IP>:18081/api/v1/cluster
```

### 2. Проверка GPU метрик

```bash
# Внутри контейнера агента
docker exec ollama-agent nvidia-smi

# Или напрямую на хосте
nvidia-smi
```

### 3. Проверка логов

```bash
# Docker
docker logs ollama-agent

# Systemd
journalctl -u ollama-agent -f
```

### 4. Проверка состояния кластера

```bash
curl -s http://<BALANCER_IP>:18081/api/v1/cluster | jq
```

Пример ответа:

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "activeRequests": 0,
  "queuedRequests": 0,
  "backends": [
    {
      "id": "gpu-1",
      "status": "healthy",
      "gpu": {
        "usagePercent": 0,
        "memoryTotal": 24576,
        "memoryUsed": 512,
        "temperature": 35,
        "powerUsage": 50,
        "powerLimit": 450
      },
      "system": {
        "cpuUsagePercent": 15,
        "memoryTotal": 65536,
        "memoryUsed": 8000
      },
      "ollama": {
        "runningModels": [],
        "activeRequests": 0,
        "requestsPerSecond": 0
      }
    }
  ]
}
```

### 5. Проверка WebSocket подключения

```bash
# Установка wscat
npm install -g wscat

# Подключение к WebSocket
wscat -c ws://<BALANCER_IP>:18081/ws/metrics
```

---

## Troubleshooting

### Агент не подключается к балансировщику

**Проблема:** Агент не может соединиться с балансировщиком

**Решение:**

```bash
# Проверка доступности балансировщика
curl -v http://<BALANCER_IP>:18081/api/v1/health

# Проверка firewall
sudo ufw status
sudo iptables -L -n

# Проверка логов агента
docker logs ollama-agent
```

### Ошибки доступа к GPU

**Проблема:** Агент не может получить GPU метрики

**Решение:**

```bash
# Проверка NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# Проверка NVML
docker exec ollama-agent nvidia-smi

# Пересоздание контейнера с правильными правами
docker rm -f ollama-agent
docker run -d \
  --name ollama-agent \
  --gpus all \
  --network host \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<BALANCER_IP>:18081 \
  -e NVML_ENABLED=true \
  ollama-legion/agent:latest
```

### Высокая задержка heartbeat

**Проблема:** Балансировщик помечает агента как unhealthy

**Решение:**

1. Увеличьте интервал heartbeat:
```bash
-e HEARTBEAT_INTERVAL=10
```

2. Проверьте сетевую задержку:
```bash
ping <BALANCER_IP>
```

3. Проверьте нагрузку на сеть:
```bash
iftop -n
```

### Агент показывает неверные метрики

**Проблема:** Метрики GPU/CPU/RAM не соответствуют действительности

**Решение:**

```bash
# Проверка прав доступа к /proc
docker exec ollama-agent cat /host/proc/loadavg

# Проверка доступа к nvidia-smi
docker exec ollama-agent nvidia-smi

# Перезапуск агента
docker restart ollama-agent
```

### Очередь переполнена

**Проблема:** Запросы отклоняются из-за переполненной очереди

**Решение:**

1. Увеличьте размер очереди:
```bash
-e LB_QUEUE_MAX_SIZE=200
```

2. Увеличьте таймаут очереди:
```bash
-e LB_QUEUE_TIMEOUT=600
```

3. Добавьте больше бэкендов или увеличьте `maxConcurrentRequests`

### WebSocket не подключается

**Проблема:** Не удается подключиться к WebSocket для real-time метрик

**Решение:**

```bash
# Проверка доступности порта
telnet <BALANCER_IP> 18081

# Проверка CORS настроек
curl -v -X OPTIONS http://<BALANCER_IP>:18081/ws/metrics

# Проверка логов балансировщика
docker logs loadbalancer | grep -i websocket
```

---

## Быстрое развертывание

### Скрипт deploy-agent-docker.sh (рекомендуется)

Используйте скрипт для автоматического развертывания через Docker Compose:

```bash
# На GPU сервере
cd /path/to/ollama-loadbalancer/scripts

# Запуск с параметрами
./deploy-agent-docker.sh --balancer-url http://192.168.1.100:18081 --agent-id gpu-1

# Или кратко:
./deploy-agent-docker.sh -b http://192.168.1.100:18081 -i gpu-1
```

Скрипт автоматически:
1. Проверит зависимости (Docker, Docker Compose, NVIDIA Toolkit)
2. Проверит наличие docker-compose.agent.yml
3. Запустит контейнер с указанными параметрами
4. Покажет статус развертывания

### Скрипт deploy-agent.sh (альтернативный)

Для развертывания через docker run:

```bash
./deploy-agent.sh gpu-1 http://192.168.1.100:18081
```

---

## Дополнительные ресурсы

### Файлы конфигурации

- [`deployments/docker-compose.agent.yml`](deployments/docker-compose.agent.yml) - Docker Compose конфигурация для агента
- [`config/agent.example.env`](config/agent.example.env) - Пример файла окружения ��ля агента
- [`config/config.example.json`](config/config.example.json) - Пример конфигурации балансировщика

### Скрипты развертывания

- [`scripts/deploy-agent-docker.sh`](scripts/deploy-agent-docker.sh) - Скрипт развертывания через Docker Compose
- [`scripts/deploy-agent.sh`](scripts/deploy-agent.sh) - Скрипт развертывания через docker run
- [`scripts/build-agent.sh`](scripts/build-agent.sh) - Скрипт сборки агента

### Документация

- [`README.md`](README.md) - Общая информация о проекте
- [`CHANGELOG.md`](CHANGELOG.md) - История изменений проекта
- [`deployments/docker-compose.yml`](deployments/docker-compose.yml) - Конфигурация Docker Compose для балансировщика
