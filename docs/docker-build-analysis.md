# Анализ сборок Docker-образов OllamaLegion

Дата: 21.05.2026

## Итоговый статус всех образов

| Образ | Статус | Размер | Время сборки |
|-------|--------|--------|-------------|
| `ollama-legion/agent:cpu` | ✅ Успешно | 15.9 MB | ~8 мин |
| `ollama-legion/agent:gpu` | ✅ Успешно | 395 MB | ~11 мин |
| `ollama-legion/cppworker:cpu` | ✅ Успешно | 46.3 MB | ~12 мин |
| `ollama-legion/cppworker:gpu` | ✅ Успешно | 3.19 GB | ~17 мин |
| `ollama-legion/balancer:latest` | ✅ Успешно | 53.7 MB | ~4 мин |
| `ollama-legion/webui:latest` | ✅ Успешно | 101 MB | ~1 мин |

## Выявленные причины неуспешных сборок

### 1. Docker daemon OOM (out of memory) — КРИТИЧЕСКАЯ

**Симптом:** При отправке контекста сборки (~24 MB) Docker daemon падает с ошибкой:
```
fatal error: out of memory allocating heap arena map
runtime stack:
runtime.throw(...)
runtime.(*mheap).sysAlloc(...)
```

**Когда проявляется:** При параллельных сборках или при сборке тяжёлых образов (cppworker:gpu — 3.19 GB).

**Причина:** Docker Desktop работает в WSL2 с ограниченной памятью. При активных параллельных сборках заканчивается heap arena для runtime Go (самого Docker daemon).

**Решение:**
- Увеличить лимит памяти WSL2 (`.wslconfig`: `memory=8GB`)
- Избегать параллельных сборок на слабых машинах
- Использовать BuildKit с ограничением параллелизма

### 2. DNS timeout в Docker-сети — ЧАСТАЯ

**Симптом:** `go mod download` падает с ошибкой:
```
dial tcp: lookup proxy.golang.org on 192.168.65.7:53: read udp ... i/o timeout
```

**Когда проявляется:** При первом `go mod download` внутри Docker (особенно после перезапуска Docker Desktop).

**Причина:** Встроенный DNS Docker (192.168.65.7) иногда теряет связь с внешним DNS, особенно на Windows с WSL2.

**Решение:**
- Повторить сборку (обычно проходит со 2-3 попытки)
- Добавить `--dns 8.8.8.8` к `docker build`
- Использовать `--network=host` при сборке

### 3. Отсутствие базовых образов nvidia/cuda — ИСПРАВЛЕНО

**Симптом:** `pull access denied for nvidia/cuda` или образ не найден.

**Причина:** GPU-target'ы (`agent-gpu`, `cppworker-gpu`) требуют базовые образы:
- `nvidia/cuda:12.2.0-base-ubuntu22.04` (~341 MB)
- `nvidia/cuda:12.2.0-devel-ubuntu22.04` (~2.3 GB)
- `nvidia/cuda:12.2.0-runtime-ubuntu22.04`

**Решение:** Предварительно выкачать образы:
```powershell
docker pull nvidia/cuda:12.2.0-base-ubuntu22.04
docker pull nvidia/cuda:12.2.0-devel-ubuntu22.04
docker pull nvidia/cuda:12.2.0-runtime-ubuntu22.04
```

### 4. Временные сетевые сбои (apt, go proxy) — УМЕРЕННАЯ

**Симптом:** Ошибки загрузки пакетов apt или go модулей.

**Решение:** Повторить сборку. В 90% случаев проходит со второй попытки.

## Рекомендации по стабильной сборке

### Последовательная сборка (рекомендуется для слабых машин)

```powershell
$env:DOCKER_BUILDKIT=0

# Шаг 1: Базовые образы
docker pull nvidia/cuda:12.2.0-base-ubuntu22.04
docker pull nvidia/cuda:12.2.0-devel-ubuntu22.04
docker pull nvidia/cuda:12.2.0-runtime-ubuntu22.04

# Шаг 2: CPU образы (самые лёгкие)
docker build --pull=false --no-cache -t ollama-legion/agent:cpu --target agent-cpu -f docker/agent/Dockerfile .
docker build --pull=false --no-cache -t ollama-legion/cppworker:cpu --target cppworker-cpu -f docker/cppworker/Dockerfile .

# Шаг 3: Основные сервисы
docker build --pull=false --no-cache -t ollama-legion/balancer:latest --target production -f docker/balancer/Dockerfile .

# Шаг 4: GPU образы (самые тяжёлые)
docker build --pull=false --no-cache -t ollama-legion/agent:gpu --target agent-gpu -f docker/agent/Dockerfile .
docker build --pull=false --no-cache -t ollama-legion/cppworker:gpu --target cppworker-gpu -f docker/cppworker/Dockerfile .

# Шаг 5: WebUI
docker build --pull=false --no-cache -t ollama-legion/webui:latest -f docker/webui/Dockerfile .
```

### Параллельная сборка через docker compose (только core-сервисы)

```powershell
$env:DOCKER_BUILDKIT=0
docker compose -f deployments/docker-compose.yml build --no-cache
```

### Успешные команды сборки (проверено 21.05.2026, Windows 11 + Docker Desktop + WSL2)

**Важно:** На Windows с Docker Desktop BuildKit вызывает `context canceled` при сборке multi-stage образов с большим контекстом. Используйте **legacy builder** (`$env:DOCKER_BUILDKIT=0`).

#### CppWorker для llama.cpp (CPU и GPU)

```powershell
# Отключаем BuildKit (обязательно на Windows!)
$env:DOCKER_BUILDKIT=0

# CPU-образ (Alpine, CGO_ENABLED=0, stub llama.cpp) — 46 MB
docker build -t ollama-legion/cppworker:latest --target cppworker-cpu -f docker/cppworker/Dockerfile .

# GPU-образ (CUDA 12.2, CGO_ENABLED=1, реальный llama.cpp) — 3.2 GB
docker build -t ollama-legion/cppworker:latest-gpu --target cppworker-gpu -f docker/cppworker/Dockerfile .
```

#### Когда какой вариант собирать

| Вариант | Команда | Цель | Когда использовать |
|---------|---------|------|--------------------|
| **CPU-only** | `--target cppworker-cpu` | Alpine, stub llama.cpp (нет CGo) | Разработка, тестирование WebUI/API без GPU, CI/CD |
| **GPU (CUDA)** | `--target cppworker-gpu` | Ubuntu 22.04 + CUDA 12.2, реальный llama.cpp + libllama.a | Production с NVIDIA GPU, инференс GGUF моделей |
| **CPU (prod)** | `--target cppworker-cpu -tags llama_cpp` | Alpine, CGO_ENABLED=1 + libllama.a без CUDA | Production CPU-only инференс (требует libllama.a без `GGML_CUDA`) |

**Особенности GPU-сборки:**
- Stage `builder-gpu` использует `nvidia/cuda:12.2.0-devel-ubuntu22.04` (~2.3 GB)
- `libllama.a` компилируется из `c/llama.cpp/` с `GGML_CUDA=1`
- CppWorker линкуется через CGo с `libllama.a`
- Финальный образ: `nvidia/cuda:12.2.0-runtime-ubuntu22.04` (~3.2 GB)
- Время сборки GPU-образа: ~17 минут (первая сборка), ~2 минуты (с кэшем)

**Особенности CPU-сборки:**
- Stage `builder-cpu` использует `golang:1.24-alpine`, `CGO_ENABLED=0`, тег `llama_stub`
- Не требует CUDA toolkit, собирается на любой машине
- Время сборки: ~4 минуты (первая), ~30 секунд (с кэшем)

#### Почему `$env:DOCKER_BUILDKIT=0` на Windows

BuildKit в Docker Desktop на Windows отправляет контекст сборки асинхронно и может отменять передачу при обнаружении большого количества файлов (даже если они в `.dockerignore`). Legacy builder отправляет контекст синхронно и надёжнее работает с большими проектами.

### Финальный статус

Все 6 образов успешно собраны. Проблемы носят характер:
1. **Инфраструктурные** (OOM, DNS) — требуют настройки Docker/WSL2, а не изменения кода
2. **Зависимостей** (nvidia/cuda) — решено предварительным pull'ом
3. **Сетевые** — решаются повторными попытками
