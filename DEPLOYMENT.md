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
│                    Порт: 8080 (прокси)                          │
│                    Порт: 8081 (API)                             │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ HTTP/REST
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    GPU Серверы (Агенты)                         │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐             │
│  │  Agent 1    │  │  Agent 2    │  │  Agent N    │             │
│  │  GPU Server │  │  GPU Server │  │  GPU Server │             │
│  │  :9090      │  │  :9090      │  │  :9090      │             │
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
| **ОС** | Linux (Ubuntu 20.04+, Debian 11+, CentOS 8+) |
| **Docker** | 20.10+ с поддержкой NVIDIA Container Toolkit |
| **NVIDIA Driver** | 470.x или новее |
| **NVIDIA Container Toolkit** | Требуется для доступа к GPU метрикам |
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
```

---

## Развертывание агента через Docker

Это рекомендуемый способ развертывания.

### Шаг 1: Подготовка

Убедитесь, что Docker и NVIDIA Container Toolkit установлены:

```bash
# Установка Docker (Ubuntu/Debian)
curl -fsSL https://get.docker.com | sh

# Установка NVIDIA Container Toolkit
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit
sudo systemctl restart docker
```

### Шаг 2: Загрузка образа агента

```bash
# Загрузка готового образа (если доступен в реестре)
docker pull ollama-lb/agent:latest

# ИЛИ сборка локально
cd /path/to/ollama-loadbalancer
docker build -f docker/agent/Dockerfile -t ollama-lb/agent:latest .
```

### Шаг 3: Запуск контейнера

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<BALANCER_IP>:8081 \
  -e COLLECT_INTERVAL=5 \
  -e HEARTBEAT_INTERVAL=3 \
  -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro \
  -v /var/run/nvidia-top-level-device:/var/run/nvidia-top-level-device:ro \
  -v /proc:/host/proc:ro \
  -v /sys:/host/sys:ro \
  --network host \
  --gpus all \
  ollama-lb/agent:latest
```

**Параметры:**

| Параметр | Описание |
|----------|----------|
| `--name` | Имя контейнера |
| `--restart` | Автоматический перезапуск |
| `-e AGENT_ID` | Уникальный идентификатор агента |
| `-e BALANCER_URL` | URL балансировщика |
| `-v /usr/bin/nvidia-smi` | Доступ к nvidia-smi |
| `-v /proc` | Доступ к системным метрикам |
| `--network host` | Сетевой режим хоста |
| `--gpus all` | Доступ ко всем GPU |

### Шаг 4: Проверка

```bash
# Проверка статуса контейнера
docker ps | grep ollama-agent

# Просмотр логов
docker logs -f ollama-agent

# Проверка метрик агента
curl http://localhost:9090/metrics
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
Environment="BALANCER_URL=http://<BALANCER_IP>:8081"
Environment="COLLECT_INTERVAL=5"
Environment="HEARTBEAT_INTERVAL=3"
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
| `BALANCER_URL` | URL балансировщика | `http://192.168.1.100:8081` |

### Опциональные переменные

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `METRICS_PORT` | `9090` | Порт для локальных метрик |
| `COLLECT_INTERVAL` | `5` | Интервал сбора метрик (сек) |
| `HEARTBEAT_INTERVAL` | `3` | Интервал отправки heartbeat (сек) |
| `CONFIG_PATH` | - | Путь к файлу конфигурации |

### Пример .env файла

Создайте файл `.env` на GPU-сервере:

```bash
# Идентификатор агента (должен быть уникальным)
AGENT_ID=gpu-1

# URL балансировщика
BALANCER_URL=http://192.168.1.100:8081

# Интервалы (секунды)
COLLECT_INTERVAL=5
HEARTBEAT_INTERVAL=3

# Порт для метрик
METRICS_PORT=9090
```

Запуск с .env файлом:

```bash
# Docker
docker run --env-file .env ... ollama-lb/agent:latest

# Бинарный файл
set -a; source .env; set +a; /usr/local/bin/agent
```

---

## Регистрация агента через API

Агент автоматически регистрируется при первом подключении к балансировщику.

### Ручная регистрация через API

```bash
# Регистрация нового бэкенда
curl -X POST http://<BALANCER_IP>:8081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{
    "id": "gpu-1",
    "name": "GPU Server 1",
    "host": "192.168.13.66",
    "port": 11434,
    "agent_port": 9090,
    "weight": 1,
    "max_requests": 10
  }'
```

### Проверка зарегистрированных бэкендов

```bash
curl http://<BALANCER_IP>:8081/api/v1/backends
```

### Удаление бэкенда

```bash
curl -X DELETE http://<BALANCER_IP>:8081/api/v1/backends/gpu-1
```

---

## Проверка работоспособности

### 1. Проверка подключения агента

```bash
# Локальная проверка метрик
curl http://localhost:9090/metrics

# Проверка через API балансировщика
curl http://<BALANCER_IP>:8081/api/v1/cluster
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
curl -s http://<BALANCER_IP>:8081/api/v1/cluster | jq
```

Пример ответа:

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "totalBackends": 2,
  "healthyBackends": 2,
  "activeRequests": 0,
  "backends": [
    {
      "id": "gpu-1",
      "status": "healthy",
      "gpu": {
        "usagePercent": 0,
        "memoryTotal": 24576,
        "memoryUsed": 512,
        "temperature": 35
      }
    }
  ]
}
```

---

## Troubleshooting

### Агент не подключается к балансировщику

**Проблема:** Агент не может соединиться с балансировщиком

**Решение:**

```bash
# Проверка доступности балансировщика
curl -v http://<BALANCER_IP>:8081/api/v1/health

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

# Пересоздание контейнера с правильными правами
docker rm -f ollama-agent
docker run -d \
  --name ollama-agent \
  --gpus all \
  --network host \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<BALANCER_IP>:8081 \
  ollama-lb/agent:latest
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

---

## Быстрое развертывание

Используйте скрипт автоматического развертывания:

```bash
# На GPU сервере
cd /path/to/ollama-loadbalancer/scripts

# Запуск скрипта развертывания
./deploy-agent.sh gpu-1 http://192.168.1.100:8081
```

Скрипт автоматически:
1. Проверит зависимости (Docker, NVIDIA Toolkit)
2. Загрузит/соберет образ агента
3. Запустит контейнер с правильными параметрами

---

## Дополнительные ресурсы

- [README.md](README.md) - Общая информация о проекте
- [scripts/deploy-agent.sh](scripts/deploy-agent.sh) - Скрипт автоматического развертывания
- [docker-compose.yml](deployments/docker-compose.yml) - Конфигурация Docker Compose
