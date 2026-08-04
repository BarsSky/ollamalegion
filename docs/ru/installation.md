# Установка и сборка Ollama Load Balancer

Подробное руководство по установке и сборке всех компонентов системы.

## Содержание

1. [Требования к системе](#требования-к-системе)
2. [Установка через Docker (рекомендуемый способ)](#установка-через-docker)
3. [Локальная установка (из исходников)](#локальная-установка-из-исходников)
4. [Установка с NVML поддержкой](#установка-с-nvml-поддержкой)
5. [Сборка с флагами](#сборка-с-флагами)

---

## Требования к системе

### Для балансировщика (Load Balancer)

| Требование | Минимальные | Рекомендуемые |
|------------|-------------|---------------|
| **ОС** | Linux/Windows/macOS | Linux (Ubuntu 20.04+, Debian 11+) |
| **CPU** | 2 cores | 4 cores |
| **RAM** | 512 MB | 1 GB |
| **Disk** | 100 MB | 500 MB |
| **Network** | 100 Mbps | 1 Gbps |
| **Go** | 1.21+ | 1.21+ |

### Для агента (на каждом GPU сервере)

| Требование | Минимальные | Рекомендуемые |
|------------|-------------|---------------|
| **ОС** | Linux/Windows | Linux (Ubuntu 20.04+, Debian 11+) |
| **CPU** | 1 core | 2 cores |
| **RAM** | 128 MB | 256 MB |
| **Disk** | 50 MB | 100 MB |
| **Docker** | 20.10+ | 24.0+ с NVIDIA Container Toolkit |
| **NVIDIA Driver** | 470.x+ | 535.x+ |
| **Ollama** | Любая версия | Последняя стабильная |

### Для Web UI

| Требование | Минимальные | Рекомендуемые |
|------------|-------------|---------------|
| **CPU** | 1 core | 2 cores |
| **RAM** | 256 MB | 512 MB |
| **Disk** | 100 MB | 200 MB |

### Проверка требований

```bash
# Проверка версии Go
go version

# Проверка Docker
docker --version
docker-compose --version

# Проверка NVIDIA драйверов
nvidia-smi

# Проверка NVIDIA Container Toolkit
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# Проверка Ollama
curl http://localhost:11434/api/tags
```

---

## Установка через Docker

Это рекомендуемый способ развертывания.

### Шаг 1: Клонирование репозитория

```bash
git clone https://github.com/BarsSky/ollamalegion.git
cd ollamalegion
```

### Шаг 2: Подготовка конфигурации

```bash
# Скопируйте пример конфигурации балансировщика
cp config/config.example.json config/config.json

# Отредактируйте конфигурацию под ваши нужды
nano config/config.json
```

### Шаг 3: Запуск через Docker Compose

```bash
# Запуск балансировщика и Web UI
docker-compose up -d

# Просмотр логов
docker-compose logs -f loadbalancer

# Проверка статуса
docker-compose ps
```

### Шаг 4: Проверка работоспособности

```bash
# Проверка health endpoint
curl http://localhost:18081/api/v1/health

# Проверка Web UI (Dashboard)
# Откройте http://localhost:18030 в браузере

# Проверка Монитора (real-time визуализация)
# Откройте http://localhost:18030/monitor.html в браузере
# Монитор доступен по тому же адресу и порту, что и Web UI Dashboard
```

### Шаг 5: Развертывание агента на GPU серверах

Агент должен запускаться **отдельно на каждом GPU сервере** с Ollama.

```bash
# На каждом GPU сервере выполните:
cd /path/to/ollama-loadbalancer/scripts

# Используйте скрипт автоматического развертывания
./deploy-agent-docker.sh \
  --balancer-url http://<balancer-ip>:18081 \
  --agent-id gpu-1

# Или вручную через docker run
docker run -d \
  --name ollama-agent \
  --restart unless-stopped \
  -e AGENT_ID=gpu-1 \
  -e BALANCER_URL=http://<balancer-ip>:18081 \
  -v /usr/bin/nvidia-smi:/usr/bin/nvidia-smi:ro \
  -v /proc:/host/proc:ro \
  -v /sys:/host/sys:ro \
  --network host \
  ollama-legion/agent:latest
```

---

## Локальная установка (из исходников)

### Шаг 1: Установка Go

```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y golang-go

# Проверка версии
go version  # должна быть 1.21 или выше
```

### Шаг 2: Клонирование репозитория

```bash
git clone https://github.com/your-org/ollama-loadbalancer.git
cd ollama-loadbalancer
```

### Шаг 3: Установка зависимостей

```bash
go mod download
```

### Шаг 4: Сборка балансировщика

```bash
# Использование скрипта сборки (рекомендуется)
./scripts/build-balancer.sh

# Или вручную
go build -o bin/balancer ./cmd/balancer

# Проверка сборки
./bin/balancer --help
```

### Шаг 5: Сборка агента

```bash
# Использование скрипта сборки (рекомендуется)
./scripts/build-agent.sh

# Или вручную
go build -o bin/agent ./cmd/agent

# Проверка сборки
./bin/agent --help
```

### Шаг 6: Запуск балансировщика

```bash
# Создание конфигурационного файла
mkdir -p config
cp config/config.example.json config/config.json

# Запуск с конфигурацией
./bin/balancer -config config/config.json

# Или через переменные окружения
export LB_HOST=0.0.0.0
export LB_PORT=18080
export LB_API_PORT=18081
export BACKEND_0_ID=gpu-1
export BACKEND_0_HOST=192.0.2.66
./bin/balancer
```

### Шаг 7: Запуск агента

```bash
# На каждом GPU сервере
export AGENT_ID=gpu-1
export BALANCER_URL=http://<balancer-ip>:18081
export NVML_ENABLED=true
./bin/agent
```

### Шаг 8: Сборка и запуск Web UI

```bash
# Web UI собирается из docker/webui/Dockerfile
# Запуск в составе основного docker-compose.yml:
cd deployments
docker-compose up -d

# Или напрямую через nginx (статика должна быть в webui/dist/)
sudo apt-get install -y nginx
sudo cp webui/nginx.conf /etc/nginx/sites-available/ollama-legion
sudo ln -s /etc/nginx/sites-available/ollama-legion /etc/nginx/sites-enabled/
sudo systemctl restart nginx
```

---

## Установка с NVML поддержкой

NVML (NVIDIA Management Library) обеспечивает точные метрики GPU.

### Шаг 1: Установка NVIDIA драйверов

```bash
# Ubuntu/Debian
sudo apt-get update
sudo apt-get install -y nvidia-driver-535

# Перезагрузка после установки драйверов
sudo reboot
```

### Шаг 2: Установка NVML библиотеки

```bash
# NVML обычно устанавливается вместе с драйверами
# Проверка наличия библиотеки
ldconfig -p | grep nvml

# Для разработки могут потребоваться заголовочные файлы
sudo apt-get install -y nvidia-cuda-toolkit
```

### Шаг 3: Установка NVIDIA Container Toolkit

```bash
# Добавление репозитория
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | \
  sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit.gpg

distribution=$(. /etc/os-release;echo $ID$VERSION_ID)
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list

sudo apt-get update
sudo apt-get install -y nvidia-container-toolkit

# Настройка Docker
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

### Шаг 4: Проверка NVML

```bash
# Проверка внутри контейнера
docker run --rm --gpus all nvidia/cuda:11.0-base nvidia-smi

# Проверка go-nvml
go get github.com/NVIDIA/go-nvml/pkg/nvml
```

### Шаг 5: Сборка с NVML

```bash
# Для Unix систем с NVML
go build -tags nvml -o bin/agent ./cmd/agent

# Для Windows
go build -tags nvml -o bin/agent.exe ./cmd/agent

# Для систем без NVML (используется stub)
go build -o bin/agent ./cmd/agent
```

### Шаг 6: Конфигурация агента с NVML

```bash
# В docker-compose.agent.yml или .env файле
NVML_ENABLED=true

# При запуске бинарного файла
export NVML_ENABLED=true
./bin/agent
```

---

## Сборка с флагами

### Флаги сборки для агента

| Флаг | Описание | Платформа |
|------|----------|-----------|
| `-tags nvml` | Включить NVML поддержку | Linux/Windows |
| `-tags static` | Статическая линковка | Linux |
| `-ldflags "-s -w"` | Уменьшить размер бинарника | Все |

### Примеры сборки

#### Балансировщик (Linux)

```bash
# Базовая сборка
go build -o bin/balancer ./cmd/balancer

# Оптимизированная сборка
go build -ldflags "-s -w" -o bin/balancer ./cmd/balancer

# Для конкретной архитектуры
GOOS=linux GOARCH=amd64 go build -o bin/balancer ./cmd/balancer
```

#### Агент (Linux с NVML)

```bash
# С NVML поддержкой
CGO_ENABLED=1 go build -tags nvml -o bin/agent ./cmd/agent

# Статическая сборка с NVML
CGO_ENABLED=1 go build -tags "nvml static" -ldflags "-s -w" -o bin/agent ./cmd/agent

# Кросс-компиляция
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags nvml -o bin/agent ./cmd/agent
```

#### Агент (Windows с NVML)

```bash
# Для Windows
GOOS=windows GOARCH=amd64 CGO_ENABLED=1 go build -tags nvml -o bin/agent.exe ./cmd/agent
```

#### Агент (без NVML)

```bash
# Для систем без GPU или для тестирования
CGO_ENABLED=0 go build -o bin/agent ./cmd/agent
```

### Скрипты сборки

Проект включает готовые скрипты сборки:

```bash
# Сборка балансировщика
chmod +x scripts/build-balancer.sh
./scripts/build-balancer.sh

# Сборка агента (Linux)
chmod +x scripts/build-agent.sh
./scripts/build-agent.sh

# Сборка агента (Windows)
chmod +x scripts/build-agent.bat
./scripts/build-agent.bat
```

---

## Docker образы

### Сборка образов

```bash
# Балансировщик
docker build -f docker/balancer/Dockerfile -t ollama-legion/balancer:latest .

# Агент
docker build -f docker/agent/Dockerfile -t ollama-legion/agent:latest .

# Web UI
docker build -f docker/webui/Dockerfile -t ollama-legion/webui:latest .
```

### Мульти-архитектурные образы

```bash
# Установка buildx
docker buildx create --use

# Сборка для нескольких архитектур
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -f docker/balancer/Dockerfile \
  -t ollama-legion/balancer:latest \
  --push \
  .
```

---

## Проверка установки

### Чеклист проверки

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

# 5. Проверка агента на GPU сервере
curl http://localhost:18032/metrics

# 6. Проверка WebSocket подключения
wscat -c ws://localhost:18081/ws/metrics
```

### Диагностика проблем

См. раздел [Troubleshooting](troubleshooting.md) для решения часто встречающихся проблем.

---

## Следующие шаги

- [Конфигурация системы](configuration.md) — Настройка всех компонентов
- [Развертывание](deployment.md) — Docker Compose и production deployment
- [API документация](api.md) — Использование REST API и WebSocket
