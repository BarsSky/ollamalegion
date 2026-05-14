# Развертывание агента Ollama Load Balancer

Подробное руководство по развертыванию агента сбора метрик на серверах с Ollama. Агент поддерживает два режима работы: **GPU** (с NVIDIA GPU и NVML) и **CPU** (без GPU, только системные метрики).

> **Кроссплатформенность:** Примеры команд приведены для **bash** (Linux/macOS) и **PowerShell** (Windows). Выберите вариант для вашей ОС.

---

## Содержание

1. [Требования к системе](#требования-к-системе)
2. [Режимы работы агента](#режимы-работы-агента)
3. [Развертывание в режиме GPU](#развертывание-в-режиме-gpu)
4. [Развертывание в режиме CPU](#развертывание-в-режиме-cpu)
5. [Переменные окружения](#переменные-окружения)
6. [Примеры конфигураций](#примеры-конфигураций)
7. [Проверка работоспособности](#проверка-работоспособности)
8. [Генерация Docker контейнеров](#генерация-docker-контейнеров)
9. [Troubleshooting](#troubleshooting)

---

## Требования к системе

### Общие требования (для любого режима)

| Требование | Минимальные | Рекомендуемые |
|------------|-------------|---------------|
| **ОС** | Linux (Ubuntu 20.04+, Debian 11+, CentOS 8+) | Ubuntu 22.04 LTS, Debian 12 |
| **CPU** | 1 ядро | 2 ядра |
| **RAM** | 128 MB | 256 MB |
| **Диск** | 50 MB | 100 MB |
| **Docker** | 20.10+ | 24.0+ |
| **Ollama** | Установлен и запущен на порту `11434` | Последняя стабильная версия |
| **Сеть** | Доступ к балансировщику по HTTP/HTTPS | 1 Gbps |

### Дополнительные требования для GPU-режима

| Требование | Минимальные | Рекомендуемые |
|------------|-------------|---------------|
| **NVIDIA GPU** | Compute Capability 5.0+ | Compute Capability 7.0+ (Turing/Ampere) |
| **NVIDIA Driver** | 470.x | 535.x или новее |
| **NVIDIA Container Toolkit** | 1.12+ | 1.14+ |
| **CUDA** | 11.0+ | 12.0+ |

> **Важно:** Без NVIDIA Container Toolkit GPU-проброс в Docker не работает. Агент автоматически перейдёт в CPU-режим, но точные GPU-метрики будут недоступны.

### Windows: Docker Desktop + WSL2 + GPU

Для запуска GPU-контейнеров на Windows через Docker Desktop:

1. **Docker Desktop** → Settings → General → ✅ Use the WSL 2 based engine
2. **Docker Desktop** → Settings → Resources → WSL Integration → ✅ Enable integration with my default WSL distro
3. **Docker Desktop** → Settings → Resources → WSL Integration → ✅ Enable NVIDIA Container Toolkit (появится если установлен драйвер NVIDIA с WSL2 поддержкой)
4. **NVIDIA Driver** для WSL2: https://developer.nvidia.com/cuda/wsl (установите на Windows хост, НЕ внутри WSL)

Проверка GPU в Docker Desktop:
```powershell
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

Если образ `nvidia/cuda:12.2.0-base-ubuntu22.04` не найден локально, Docker сделает pull автоматически — это может занять несколько минут в первый раз.

---

## Режимы работы агента

Агент определяет режим работы через переменную `GPU_MODE`:

| Режим | Описание | Когда использовать |
|-------|----------|-------------------|
| **`auto`** | Автоматически определяет наличие GPU и NVML | По умолчанию; рекомендуется для смешанных кластеров |
| **`gpu`** | Принудительно GPU-режим, требует NVML | Серверы с NVIDIA GPU, где точные GPU-метрики критичны |
| **`cpu`** | Принудительно CPU-режим, GPU метрики отключены | Серверы без GPU, виртуальные машины, CPU-only inference |

> **Совет:** Для production кластеров с разнородным оборудованием используйте `auto`. Для стабильности в CI/CD или на чисто CPU-серверах явно указывайте `cpu`.

---

## Развертывание в режиме GPU

### Шаг 1: Проверка требований

```bash
# Проверка Docker
docker --version

# Проверка NVIDIA Driver
nvidia-smi

# Проверка NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi

# Проверка Ollama
curl http://localhost:11434/api/tags
```

Если команда `docker run --rm --gpus all ...` завершается с ошибкой — см. раздел [Установка NVIDIA Container Toolkit](#установка-nvidia-container-toolkit) ниже.

### Шаг 2: Создание .env файла

**Linux/macOS (bash):**
```bash
# Из корня репозитория
cp config/agent.example.env deployments/.env

# Отредактируйте deployments/.env — замените placeholder-значения
code deployments/.env
```

**Windows (PowerShell):**
```powershell
# Из корня репозитория
Copy-Item "config\agent.example.env" "deployments\.env"

# Отредактируйте deployments/.env — замените placeholder-значения
notepad "deployments\.env"
```

Ключевые переменные для GPU:
```env
AGENT_ID=gpu-1
BALANCER_URL=http://192.168.1.10:18081
AGENT_PUBLIC_HOST=192.168.1.20
GPU_MODE=gpu
NVML_ENABLED=true
ENABLE_NVML=true
OLLAMA_URL=http://host.docker.internal:11434
```

### Шаг 3: Запуск через Docker Compose (GPU)

Используйте **override-файл** `docker-compose.agent.gpu.yml` — он автоматически подключает NVIDIA Container Toolkit и CUDA runtime:

**Linux/macOS (bash):**
```bash
cd deployments

# Запуск с GPU пробросом
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --env-file .env up -d --build

# Просмотр логов
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml logs -f

# Проверка статуса
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml ps
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

# Запуск с GPU пробросом
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --env-file .env up -d --build

# Просмотр логов
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml logs -f

# Проверка статуса
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml ps
```

> **Windows + GPU:** Docker Desktop должен использовать WSL2 backend с включенной опцией "Use the WSL 2 based engine" и "Enable NVIDIA Container Toolkit" в Settings > Resources > WSL Integration.

> **Почему два файла?** Базовый `docker-compose.agent.yml` содержит общую конфигурацию. Override-файл `docker-compose.agent.gpu.yml` добавляет только GPU-специфичные настройки (`deploy.resources.reservations.devices` и `BASE_IMAGE=nvidia/cuda...`). Это позволяет запускать один и тот же базовый файл на CPU и GPU без модификаций.

### Шаг 4: Альтернатива — скрипт автоматизации (рекомендуется)

**Linux/macOS (bash):**
```bash
# Автоопределение GPU и запуск с правильными параметрами
bash scripts/deploy-agent-docker.sh --env-file deployments/.env

# Принудительно GPU (если автоопределение не сработало)
bash scripts/deploy-agent-docker.sh --gpu --env-file deployments/.env

# Только перезапуск без пересборки
bash scripts/deploy-agent-docker.sh --no-build --env-file deployments/.env
```

**Windows (PowerShell):**
```powershell
# Автоопределение GPU и запуск с правильными параметрами
.\scripts\deploy-agent-docker.ps1 -EnvFile "deployments\.env"

# Принудительно GPU
.\scripts\deploy-agent-docker.ps1 -Gpu -EnvFile "deployments\.env"

# Только перезапуск без пересборки
.\scripts\deploy-agent-docker.ps1 -NoBuild -EnvFile "deployments\.env"

# С pull базовых образов
.\scripts\deploy-agent-docker.ps1 -Pull -Build -EnvFile "deployments\.env"
```

Скрипт автоматически:
- Определяет наличие GPU через `nvidia-smi`
- Проверяет NVIDIA Container Toolkit
- Выбирает правильный compose-файл (CPU или GPU)
- Валидирует обязательные переменные в `.env`

### Шаг 5: Альтернатива — docker run

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  --gpus all \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://192.168.1.10:18081 \
  -e AGENT_PUBLIC_HOST=192.168.1.20 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://192.168.1.20:11434 \
  -e GPU_MODE=gpu \
  -e NVML_ENABLED=true \
  -e COLLECT_INTERVAL=5 \
  -e HEARTBEAT_INTERVAL=3s \
  -p 18032:18032 \
  ollama-legion/agent:latest
```

### Шаг 6: Альтернатива — systemd сервис

См. [DEPLOYMENT.md](../DEPLOYMENT.md) → раздел "Развертывание агента как бинарного файла" для инструкций по сборке и созданию systemd unit.

---

## Развертывание в режиме CPU

### Шаг 1: Проверка требований

```bash
# Проверка Docker
docker --version

# Проверка Ollama
curl http://localhost:11434/api/tags
```

### Шаг 2: Создание .env файла

**Linux/macOS (bash):**
```bash
# Из корня репозитория
cp config/agent.example.env deployments/.env

# Отредактируйте deployments/.env
code deployments/.env
```

**Windows (PowerShell):**
```powershell
Copy-Item "config\agent.example.env" "deployments\.env"
notepad "deployments\.env"
```

Ключевые переменные для CPU:
```env
AGENT_ID=cpu-1
BALANCER_URL=http://192.168.1.10:18081
AGENT_PUBLIC_HOST=192.168.1.30
GPU_MODE=cpu
NVML_ENABLED=false
ENABLE_NVML=false
OLLAMA_URL=http://host.docker.internal:11434
```

### Шаг 3: Запуск через Docker Compose (CPU)

Базовый файл `docker-compose.agent.yml` не содержит GPU-специфичных настроек — запускайте без override:

**Linux/macOS (bash):**
```bash
cd deployments

docker compose -f docker-compose.agent.yml --env-file .env up -d --build

# Просмотр логов
docker compose -f docker-compose.agent.yml logs -f
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

docker compose -f docker-compose.agent.yml --env-file .env up -d --build

# Просмотр логов
docker compose -f docker-compose.agent.yml logs -f
```

> **Важно:** Для CPU-режима не используйте `docker-compose.agent.gpu.yml` — он попытается инициализировать NVIDIA Container Toolkit и выдаст ошибку `nvidia-container-cli: driver not loaded` на машине без GPU.

### Шаг 4: Альтернатива — скрипт автоматизации

**Linux/macOS (bash):**
```bash
# Автоопределение (на CPU-сервере запустится в CPU-режиме)
bash scripts/deploy-agent-docker.sh --env-file deployments/.env

# Принудительно CPU
bash scripts/deploy-agent-docker.sh --cpu --env-file deployments/.env
```

**Windows (PowerShell):**
```powershell
# Автоопределение (на CPU-сервере запустится в CPU-режиме)
.\scripts\deploy-agent-docker.ps1 -EnvFile "deployments\.env"

# Принудительно CPU
.\scripts\deploy-agent-docker.ps1 -Cpu -EnvFile "deployments\.env"
```

### Шаг 5: Альтернатива — docker run

```bash
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=cpu-1 \
  -e BALANCER_URL=http://192.168.1.10:18081 \
  -e AGENT_PUBLIC_HOST=192.168.1.30 \
  -e AGENT_PORT=18032 \
  -e OLLAMA_URL=http://192.168.1.30:11434 \
  -e GPU_MODE=cpu \
  -e NVML_ENABLED=false \
  -e COLLECT_INTERVAL=5 \
  -e HEARTBEAT_INTERVAL=3s \
  -p 18032:18032 \
  ollama-legion/agent:latest
```

### Шаг 6: Альтернатива — systemd сервис

Для CPU-серверов сборка агента упрощается — NVML не требуется:

```bash
# Сборка без NVML
CGO_ENABLED=0 go build -o bin/agent ./cmd/agent

# Копирование и создание systemd unit
# ... (см. DEPLOYMENT.md)
```

---

## Переменные окружения

### Обязательные

| Переменная | Описание | Пример |
|------------|----------|--------|
| `AGENT_ID` | Уникальный идентификатор агента в кластере | `gpu-1`, `cpu-1`, `node-01` |
| `BALANCER_URL` | URL балансировщика для регистрации и heartbeat | `http://192.168.1.10:18081` |

### Сетевые настройки

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `AGENT_PORT` | `18032` | Порт для локальных метрик агента |
| `AGENT_PUBLIC_HOST` | `localhost` | Публичный IP/hostname, доступный балансировщику |
| `OLLAMA_URL` | `http://localhost:11434` | URL локального Ollama для сбора метрик моделей |

### Режим работы

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `GPU_MODE` | `auto` | `auto` / `gpu` / `cpu` — режим сбора метрик |
| `NVML_ENABLED` | `false` | `true` — включить NVML (только для GPU режима) |

> **Совместимость:** Если `GPU_MODE=cpu`, переменная `NVML_ENABLED` игнорируется. Если `GPU_MODE=gpu` и `NVML_ENABLED=false`, агент попытается использовать NVML, но при ошибке инициализации не упадёт — перейдёт к fallback-методам.

### Интервалы и таймауты

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `COLLECT_INTERVAL` | `5` | Интервал сбора метрик (секунд). Также принимается `METRICS_INTERVAL` (устар., для совместимости) |
| `HEARTBEAT_INTERVAL` | `3` | Интервал heartbeat сигналов балансировщику (секунд) |

### Логирование

| Переменная | По умолчанию | Описание |
|------------|--------------|----------|
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `text` | `text` или `json` (json рекомендуется для production) |

---

## Примеры конфигураций

### GPU сервер (production)

```bash
AGENT_ID=gpu-server-prod-01
BALANCER_URL=http://192.168.1.100:18081
AGENT_PUBLIC_HOST=192.168.1.101

GPU_MODE=gpu
NVML_ENABLED=true

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=json
```

### CPU сервер (production)

```bash
AGENT_ID=cpu-server-prod-01
BALANCER_URL=http://192.168.1.100:18081
AGENT_PUBLIC_HOST=192.168.1.102

GPU_MODE=cpu
NVML_ENABLED=false

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=json
```

### Смешанный кластер (auto режим)

```bash
AGENT_ID=auto-node-01
BALANCER_URL=http://lb.example.com:18081
AGENT_PUBLIC_HOST=node-01.example.com

GPU_MODE=auto
NVML_ENABLED=false  # auto сам определит, включать ли NVML

AGENT_PORT=18032
OLLAMA_URL=http://host.docker.internal:11434

METRICS_INTERVAL=5s
HEARTBEAT_INTERVAL=3s

LOG_LEVEL=info
LOG_FORMAT=text
```

---

## Проверка работоспособности

### 1. Локальная проверка метрик агента

```bash
curl http://localhost:18032/metrics
```

Ожидаемый ответ для **GPU** режима:

```json
{
  "agent_id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": {
    "usagePercent": 15,
    "memoryTotal": 24576,
    "memoryUsed": 4096,
    "temperature": 45,
    "powerUsage": 120,
    "powerLimit": 450
  },
  "system": {
    "cpuUsagePercent": 25,
    "memoryTotal": 65536,
    "memoryUsed": 8192
  },
  "ollama": {
    "runningModels": ["llama3.1:8b"],
    "activeRequests": 2,
    "requestsPerSecond": 0.5
  }
}
```

Ожидаемый ответ для **CPU** режима:

```json
{
  "agent_id": "cpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "gpu": null,
  "system": {
    "cpuUsagePercent": 35,
    "memoryTotal": 32768,
    "memoryUsed": 12288
  },
  "ollama": {
    "runningModels": ["llama3.1:8b"],
    "activeRequests": 1,
    "requestsPerSecond": 0.2
  }
}
```

### 2. Проверка через API балансировщика

```bash
# Статус кластера (должен показывать зарегистрированного агента)
curl http://<balancer-ip>:18081/api/v1/cluster

# Список бэкендов
curl http://<balancer-ip>:18081/api/v1/backends
```

### 3. Проверка логов агента

```bash
# Docker
docker logs -f ollama-agent

# Docker Compose
docker-compose -f docker-compose.agent.yml logs -f
```

В логах для GPU-режима должны присутствовать строки:

```
INFO  nvml initialized successfully
INFO  gpu metrics collector started
```

В логах для CPU-режима:

```
INFO  running in CPU mode, gpu metrics disabled
INFO  system metrics collector started
```

### 4. Проверка GPU проброса (только для GPU)

```bash
# Внутри контейнера агента
docker exec ollama-agent nvidia-smi

# Или напрямую на хосте
nvidia-smi
```

---

## Генерация Docker контейнеров

### Сборка образа агента

**Linux/macOS (bash):**
```bash
# CPU сборка (быстрее, меньше размер)
docker build -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=false \
  --build-arg BASE_IMAGE=ubuntu:22.04 \
  -t ollama-legion/agent:cpu-latest .

# GPU сборка (с NVML + CUDA runtime)
docker build -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=true \
  --build-arg BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04 \
  -t ollama-legion/agent:gpu-latest .
```

**Windows (PowerShell):**
```powershell
# CPU сборка
docker build -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=false `
  --build-arg BASE_IMAGE=ubuntu:22.04 `
  -t ollama-legion/agent:cpu-latest .

# GPU сборка
docker build -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=true `
  --build-arg BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04 `
  -t ollama-legion/agent:gpu-latest .
```

### Сборка через Docker Compose

**Linux/macOS (bash):**
```bash
cd deployments

# CPU — базовый файл
docker compose -f docker-compose.agent.yml build

# GPU — с override
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml build
```

**Windows (PowerShell):**
```powershell
Set-Location deployments

# CPU — базовый файл
docker compose -f docker-compose.agent.yml build

# GPU — с override
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml build
```

### Push в registry

```bash
# Тегируем и пушим
docker tag ollama-legion/agent:latest registry.example.com/ollama-legion/agent:latest
docker push registry.example.com/ollama-legion/agent:latest
```

### Генерация multi-arch образов (amd64 + arm64)

**Linux/macOS (bash):**
```bash
# Создание builder (один раз)
docker buildx create --name ollama-legion-builder --use

# Сборка и пуш multi-arch
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f docker/agent/Dockerfile \
  --build-arg ENABLE_NVML=false \
  -t registry.example.com/ollama-legion/agent:latest \
  --push .
```

**Windows (PowerShell):**
```powershell
# Создание builder (один раз)
docker buildx create --name ollama-legion-builder --use

# Сборка и пуш multi-arch
docker buildx build `
  --platform linux/amd64,linux/arm64 `
  -f docker\agent\Dockerfile `
  --build-arg ENABLE_NVML=false `
  -t registry.example.com/ollama-legion/agent:latest `
  --push .
```

---

## Troubleshooting

### Ошибка: base name (${BASE_IMAGE}) should not be blank

**Причина:** Docker BuildKit не видит `BASE_IMAGE` как build-arg при сборке через docker-compose.

**Решение:** Образец уже исправлен в `docker/agent/Dockerfile` — `ARG BASE_IMAGE` объявлен после `FROM`. Убедитесь, что используете `docker compose` (v2) вместо `docker-compose` (v1):

```bash
# Правильно (v2)
docker compose -f docker-compose.agent.yml build

# Неправильно (v1, может иметь проблемы с build args)
docker-compose -f docker-compose.agent.yml build
```

### Ошибка: nvidia-container-cli: driver not loaded

**Причина:** На CPU-сервере использован `docker-compose.agent.gpu.yml` с `deploy.resources.reservations.devices`.

**Решение:** Для CPU-серверов не используйте GPU override:

```bash
# Правильно (CPU)
docker compose -f docker-compose.agent.yml up -d

# Неправильно (CPU с GPU override)
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml up -d
```

### Агент не может инициализировать NVML (GPU режим)

**Признаки:** Логи содержат ошибку `nvml initialization failed`, GPU метрики отсутствуют.

**Решения:**

1. Проверьте NVIDIA Container Toolkit:
   ```bash
   docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
   ```

2. Убедитесь, что запущены оба compose-файла:
   ```bash
   docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml up -d --build
   ```

3. Проверьте переменные в `.env`:
   ```env
   ENABLE_NVML=true
   BASE_IMAGE=nvidia/cuda:12.2.0-runtime-ubuntu22.04
   ```

4. Если GPU проброс невозможен (например, облачный VPS без GPU), переключитесь в CPU режим:
   ```bash
   GPU_MODE=cpu
   NVML_ENABLED=false
   ENABLE_NVML=false
   ```

5. Пересоздайте контейнер:
   ```bash
   docker compose -f docker-compose.agent.yml down
   docker compose -f docker-compose.agent.yml up -d --build
   ```

### Агент не подключается к балансировщику

**Признаки:** Логи содержат ошибки соединения с `BALANCER_URL`.

**Решения:**

1. Проверьте доступность балансировщика:
   ```bash
   curl -v http://<balancer-ip>:18081/api/v1/health
   ```

2. Убедитесь, что `BALANCER_URL` содержит **публичный IP** балансировщика, а не `localhost` или Docker service name (если агент и балансировщик на разных хостах).

3. Проверьте firewall:
   ```bash
   sudo ufw status
   sudo iptables -L -n | grep 18081
   ```

### Балансировщик не видит метрики агента

**Признаки:** Агент зарегистрирован, но метрики пустые или устаревшие.

**Решения:**

1. Проверьте `AGENT_PUBLIC_HOST` — балансировщик обращается к агенту по этому адресу:
   ```bash
   curl http://<AGENT_PUBLIC_HOST>:18032/metrics
   ```

2. Убедитесь, что порт `AGENT_PORT` проброшен и доступен с хоста балансировщика.

3. Проверьте сетевой режим Docker:
   - `bridge` — требуется `ports:` и `AGENT_PUBLIC_HOST` = IP хоста
   - `host` — требуется `network_mode: host`, публичный IP сервера

### CPU сервер показывает нулевые или пустые GPU метрики

**Причина:** Переменная `GPU_MODE` не установлена в `cpu`, агент пытается собрать GPU метрики, которых нет.

**Решение:** Явно укажите:

```bash
GPU_MODE=cpu
NVML_ENABLED=false
```

После изменения `.env` пересоздайте контейнер:

```bash
docker-compose -f docker-compose.agent.yml down
docker-compose -f docker-compose.agent.yml up -d
```

---

## Установка NVIDIA Container Toolkit

Если `docker run --rm --gpus all ...` не работает:

### Ubuntu / Debian

```bash
# Добавление репозитория
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
  sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg

distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list

# Установка
sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit

# Настройка Docker runtime
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker

# Проверка
docker run --rm --gpus all nvidia/cuda:12.2.0-base-ubuntu22.04 nvidia-smi
```

### CentOS / RHEL / Fedora

```bash
# Добавление репозитория
curl -s -L https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo | \
  sudo tee /etc/yum.repos.d/nvidia-container-toolkit.repo

# Установка
sudo yum install -y nvidia-container-toolkit

# Настройка Docker runtime
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

---

## Дополнительные ресурсы

- [DEPLOYMENT.md](../DEPLOYMENT.md) — Общее руководство по развертыванию всех компонентов
- [docs/deployment.md](deployment.md) — Docker Compose, production deployment, масштабирование
- [docs/configuration.md](configuration.md) — Настройка балансировщика и агента
- [docs/troubleshooting.md](troubleshooting.md) — Общее руководство по решению проблем