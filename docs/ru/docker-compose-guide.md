# Docker Compose: руководство по файлам развёртывания

В OllamaLegion используется **многофайловая** структура Docker Compose. Каждый файл отвечает за свой компонент или режим работы. Все файлы лежат в `deployments/`.

## Быстрая шпаргалка

| Файл | Что запускает | Когда использовать |
|------|---------------|-------------------|
| `docker-compose.yml` | Балансер + WebUI | **Основной старт** — ядро системы |
| `docker-compose.full.yml` | Балансер + WebUI + CppWorker (CPU/GPU/STUB) | **Полный стек с профилями** — всё на одном хосте |
| `docker-compose.agent.yml` | Агент (CPU или GPU) | На каждом узле с Ollama, где нужен мониторинг |
| `docker-compose.agent.gpu.yml` | Оверлей для агента (production GPU) | Поверх `agent.yml` для production GPU-узлов |
| `docker-compose.llama.cpp.yml` | CppWorker + Агент (llama.cpp, GPU) | Отдельный узел с llama.cpp вместо Ollama |
| `docker-compose.llama.cpu.yml` | CppWorker + Агент (llama.cpp, CPU-only) | То же самое, но без GPU |
| `docker-compose.cppworker.yml` | Только CppWorker (CPU/GPU/STUB) | Изолированный запуск cppworker с профилями |
| `docker-compose.cocoindex.yml` | CocoIndex (поисковый сервис) | Специфичный компонент |

---

## 1. Сборка Docker-образов

Каждый компонент собирается из своего Dockerfile в `docker/<компонент>/Dockerfile`. Сборка запускается **из корня репозитория** (`c:/Ollama/ollamalegion`).

### 1.1. Предварительные требования

- **Docker Desktop** / **Docker Engine** версии 24+
- **BuildKit** (включён по умолчанию в Docker Desktop 4.x+)
- **Go 1.24+** (только если собираете на хосте, для Docker не нужен)
- **NVIDIA Container Toolkit** — для GPU-таргетов: [инструкция](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html)

### 1.2. Образ Balancer (Балансировщик)

Собирается из `docker/balancer/Dockerfile`. Многостадийная сборка: Go-билдер компилирует бинарник, затем копирует его в минимальный `alpine`.

```powershell
# В корне репозитория (c:\Ollama\ollamalegion)
docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .
```

| Таргет | Описание | Размер образа |
|--------|----------|---------------|
| `default` | Балансировщик (единственный таргет) | ~25 МБ |

**Сборочные аргументы (--build-arg):**

| Аргумент | По умолчанию | Назначение |
|----------|-------------|------------|
| `CGO_ENABLED` | `0` | Отключённый CGO (статическая линковка) |
| `GOOS` | `linux` | Целевая ОС |
| `GOARCH` | `amd64` | Целевая архитектура |

**Что нужно для сборки:** только Go и исходники в корне. Зависимости (`go.sum`, `go.mod`) копируются в контейнер автоматически.

---

### 1.3. Образы CppWorker (llama.cpp worker)

Три независимых Dockerfile в `docker/cppworker/`. Каждый — для своего режима.

```powershell
# CPU — реальный llama.cpp, без CUDA (реальный инференс):
docker build -t ollama-legion/cppworker:cpu --target runtime -f docker/cppworker/Dockerfile.cpu .

# GPU — реальный llama.cpp + CUDA (multi-GPU инференс, ~10-15 минут):
docker build -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .

# GPU — ускоренная сборка только для вашего GPU (в 3-4 раза быстрее):
# RTX 4060/4070/4080/4090 (Ada Lovelace):
docker build --build-arg CUDA_ARCH="89" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
# RTX 3060/3070/3080/3090 (Ampere):
docker build --build-arg CUDA_ARCH="86" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .
# GTX 1660/RTX 2060 (Turing):
docker build --build-arg CUDA_ARCH="75" -t ollama-legion/cppworker:gpu --target runtime -f docker/cppworker/Dockerfile.gpu .

# STUB — заглушка без llama.cpp (только для CI/тестов):
docker build -t ollama-legion/cppworker:stub --target runtime -f docker/cppworker/Dockerfile.stub .
```

| Образ | Dockerfile | Описание | Размер | Базовый образ |
|-------|-----------|----------|--------|---------------|
| `cppworker:cpu` | `Dockerfile.cpu` | Реальный llama.cpp CPU-only | ~150 МБ | `alpine:latest` |
| `cppworker:gpu` | `Dockerfile.gpu` | Реальный llama.cpp + CUDA multi-GPU | ~350 МБ | `nvidia/cuda:12.2-runtime-ubuntu` |
| `cppworker:stub` | `Dockerfile.stub` | Заглушка (llama_stub, CGO_ENABLED=0) | ~100 МБ | `alpine:latest` |

> **Важно:** CPU-образ использует **реальный llama.cpp** (CGo-вызовы, без тега `llama_stub`). STUB-образ — только для CI и функциональных тестов, не для продакшена.

**Что включено в образ:**
- Python 3 + `huggingface_hub` (авто-загрузка GGUF из HuggingFace при старте)
- `wget`, `curl`, `ca-certificates`
- Entrypoint: `docker/cppworker/entrypoint.sh` (авто-загрузка моделей)

---

### 1.4. Образ Web UI

Собирается из `docker/webui/Dockerfile`. Статические файлы → `nginx:alpine`.

```powershell
docker build -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

| Таргет | Описание | Размер |
|--------|----------|--------|
| `default` | nginx + статика + entrypoint | ~65 МБ |

**Entrypoint:**
- Генерирует `nginx.conf` из шаблона с переменными `API_HOST`, `API_PORT`, `CPPWORKER_HOST`, `CPPWORKER_PORT`
- Генерирует `js/modules/config.js` с рантайм-параметрами (`WEBUI_CONFIG`)
- Запускает nginx на порту `${NGINX_PORT}` (по умолчанию 80)

**Проверка после сборки:**
```powershell
docker run --rm -p 18030:80 -e API_HOST=localhost -e API_PORT=18081 ollama-legion/webui:latest
# WebUI доступен: http://localhost:18030
```

---

### 1.5. Образ Agent (Агент мониторинга)

Собирается из `docker/agent/Dockerfile`. Запускается на узлах с Ollama или llama.cpp.

```powershell
# CPU-версия:
docker build -t ollama-legion/agent:latest --target agent -f docker/agent/Dockerfile .

# GPU-версия:
docker build -t ollama-legion/agent:latest-gpu --target agent-gpu -f docker/agent/Dockerfile .
```

| Таргет | Описание | Размер |
|--------|----------|--------|
| `agent` | CPU-only агент | ~30 МБ |
| `agent-gpu` | Агент с NVIDIA-утилитами для GPU-метрик | ~300 МБ |

---

### 1.6. Сборка всего стека одной командой

Используйте `deployments/docker-compose.full.yml` с профилями для сборки и запуска балансировщика + WebUI + CppWorker в одной сети:

```powershell
# В корне репозитория:

# CPU (по умолчанию, реальный llama.cpp):
docker compose -f deployments/docker-compose.full.yml --profile default build

# GPU (llama.cpp + CUDA):
docker compose -f deployments/docker-compose.full.yml --profile gpu build

# STUB (CI/тесты):
docker compose -f deployments/docker-compose.full.yml --profile stub build

# Сборка без кэша (чистая пересборка):
docker compose -f deployments/docker-compose.full.yml --profile default build --no-cache

# Сборка конкретного сервиса:
docker compose -f deployments/docker-compose.full.yml --profile default build cppworker-cpu
docker compose -f deployments/docker-compose.full.yml --profile gpu build cppworker-gpu
docker compose -f deployments/docker-compose.full.yml --profile default build loadbalancer
docker compose -f deployments/docker-compose.full.yml --profile default build webui
```

---

### 1.7. Быстрый старт одной командой (сборка + запуск)

```powershell
# Полный стек — CPU (реальный llama.cpp, по умолчанию):
docker compose -f deployments/docker-compose.full.yml up -d --build

# Полный стек — GPU (llama.cpp + CUDA):
$env:COMPOSE_PROFILES="gpu"
docker compose -f deployments/docker-compose.full.yml up -d --build

# Полный стек — STUB (только CI/тесты):
docker compose -f deployments/docker-compose.full.yml --profile stub up -d --build

# С авто-загрузкой модели из HuggingFace:
$env:HF_TOKEN="hf_xxx"
$env:HF_AUTO_DOWNLOAD_REPO="unsloth/gemma-4-E4B-it-GGUF"
$env:HF_AUTO_DOWNLOAD_FILE="gemma-4-E4B-it-Q4_K_M.gguf"
docker compose -f deployments/docker-compose.full.yml up -d --build
```

---

### 1.8. Типичные ошибки при сборке

| Ошибка | Причина | Решение |
|--------|---------|---------|
| `go: module not found` | `go.mod` или `go.sum` не скопиованы в контейнер | Убедитесь, что сборка запущена из корня репозитория |
| `unable to evaluate symlinks` | Симлинки или проблемы с путями в Windows | Используйте WSL2 backend (`docker context use wsl`) |
| `CUDA not found` | Нет NVIDIA Container Toolkit | Установите [`nvidia-container-toolkit`](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html) |
| `no space left on device` | Закончилось место для Docker-образов | `docker system prune -a` (осторожно: удалит все неиспользуемые образы!) |
| `SecretsUsedInArgOrEnv: ENV "HF_TOKEN"` | Предупреждение, не ошибка | Токен передаётся как ENV — нормально для dev-среды, в production используйте [Docker secrets](https://docs.docker.com/compose/use-secrets/) |
| `BuildKit not available` | Старая версия Docker | Обновите Docker до 24+, или `export DOCKER_BUILDKIT=0` для отключения |

---

### 1.9. Просмотр слоёв и размеров

```powershell
# Размер всех образов ollama-legion
docker images | Select-String "ollama-legion"

# История слоёв конкретного образа
docker history ollama-legion/cppworker:cpu

# Почистить билдер-кэш (освободить 10+ ГБ)
docker builder prune -a --force
```

---

## 2. Основной стек: балансер + WebUI

```bash
cd deployments
docker compose up -d
```

Запускает:
- **loadbalancer** — порты `18080` (прокси), `18081` (API)
- **webui** — порт `18030` (дашборд)

Бэкенды регистрируются позже — через WebUI, API или агентов.

---

## 3. Агент на узле с Ollama

Агент запускается **отдельно** на каждом сервере с Ollama. Он собирает метрики (GPU, VRAM, CPU) и отправляет балансеру.

### CPU-сервер (по умолчанию):
```bash
cp .env.llama.example .env
# отредактируйте .env: AGENT_ID, BALANCER_URL, AGENT_PUBLIC_HOST
docker compose -f docker-compose.agent.yml up -d
```

### GPU-сервер (с NVIDIA):
```bash
docker compose -f docker-compose.agent.yml --profile gpu up -d
```

### Production GPU (с host-сетью):
```bash
docker compose -f docker-compose.agent.yml -f docker-compose.agent.gpu.yml --profile gpu up -d
```

**Важно:** агент и балансер **не обязаны** быть в одной Docker-сети. Нужна только IP-доступность по HTTP:
- Агент → Балансер: `BALANCER_URL`
- Балансер → Агент: `AGENT_PUBLIC_HOST:18032`
- Балансер → Ollama: `AGENT_PUBLIC_HOST:11434`

---

## 4. Отдельный бекенд llama.cpp с агентом

Это **основной способ** поднять автономный узел инференса на llama.cpp (вместо Ollama). Запускается на любой машине с GPU или CPU.

### Шаг 1: Подготовка

```bash
cd deployments
cp .env.llama.example .env
```

Отредактируйте `.env`:
```ini
# Имя узла (станет AGENT_ID и именем контейнера)
NODE_NAME=llama-gpu-1

# URL балансировщика (обязательно!)
BALANCER_URL=http://192.168.1.10:18081

# Директория с GGUF-моделями
MODELS_DIR=/data/models

# Параметры llama.cpp
LLAMA_N_GPU_LAYERS=-1    # -1 = все слои на GPU, 0 = CPU-only
LLAMA_CTX_SIZE=8192
LLAMA_BATCH_SIZE=512
LLAMA_FLASH_ATTN=true

# Порты (опционально)
LLAMA_HTTP_PORT=18091
LLAMA_GRPC_PORT=19000
AGENT_PORT=18032
```

### Шаг 2: Запуск (GPU)

```bash
docker compose -f docker-compose.llama.cpp.yml up -d
```

Требуется **NVIDIA Container Toolkit**: [инструкция по установке](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html).

### Шаг 2a: Запуск (CPU-only)

```bash
docker compose -f docker-compose.llama.cpu.yml up -d
```

### Шаг 3: Проверка

```bash
# Проверить, что CppWorker отвечает
curl http://localhost:18091/api/v1/cppworker/health

# Проверить, что агент зарегистрировался на балансере
curl http://<BALANCER_IP>:18081/api/v1/backends
```

После успешного запуска узел автоматически зарегистрируется на балансере. В WebUI на дашборде появится новый бэкенд с типом `🦒 llama.cpp`.

### Что именно запускается

```
llama-net (bridge):
├── cppworker (18091 — REST API, 19000 — gRPC)
│   └── /models → GGUF-модели (read-only)
└── agent (18032 — heartbeat + метрики)
    └── BACKEND_TYPE=llama_cpp
    └── CPPWORKER_URL=http://cppworker:18091
    └── BALANCER_URL=... (регистрация)
```

Агент внутри этого стека использует `BACKEND_TYPE=llama_cpp`, поэтому балансер понимает, что это llama.cpp-узел, и направляет запросы через `CPPWORKER_URL`, а не через Ollama API.

---

## 5. Сводка: режимы запуска для разных сценариев

### Сценарий A: «Всё локально на одной машине с Ollama»
```bash
# Терминал 1 — балансер + WebUI
docker compose up -d

# Терминал 2 — агент для локальной Ollama
docker compose -f docker-compose.agent.yml up -d
```

### Сценарий B: «Балансер на сервере, бекенды на GPU-машинах с Ollama»
```bash
# На сервере-балансере:
docker compose up -d

# На каждой GPU-машине:
docker compose -f docker-compose.agent.yml --profile gpu up -d
```

### Сценарий C: «Балансер + llama.cpp узел (без Ollama)»
```bash
# На сервере-балансере:
docker compose up -d

# На GPU-машине с llama.cpp:
docker compose -f docker-compose.llama.cpp.yml up -d
```

### Сценарий D: «Смешанный кластер (Ollama + llama.cpp)»
```bash
# Балансер:
docker compose up -d

# GPU-машина 1 с Ollama:
docker compose -f docker-compose.agent.yml --profile gpu up -d

# GPU-машина 2 с llama.cpp:
docker compose -f docker-compose.llama.cpp.yml up -d

# CPU-машина 3 с llama.cpp:
docker compose -f docker-compose.llama.cpu.yml up -d
```

Балансер автоматически определит тип каждого бэкенда (Ollama vs llama.cpp) и будет маршрутизировать запросы правильно. В сайдбаре WebUI появится маркер текущего движка.

---

## 6. Как понять, какой движок используется сейчас

Сразу после подключения к балансеру, в левом сайдбаре WebUI под логотипом отображается **маркер типа движка**:

| Маркер | Значение |
|--------|----------|
| 🦙 **Ollama API** | Все бэкенды — Ollama |
| 🦒 **llama.cpp** | Все бэкенды — llama.cpp |
| 🔌 **Автоопределение** | Смешанный кластер или движок не задан |

При наведении на маркер показывается tooltip с детализацией: количество бэкендов каждого типа.