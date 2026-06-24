# CppWorker ↔ Balancer ↔ OpenWebUI: маршрутизация и регистрация

**Дата:** 2026-06-07
**Среда:** Windows 11, RTX 3070 (sm_86), CUDA 12.2
**Симптомы:** OpenWebUI получает «Ollama Server Disconnected» после загрузки модели;
повторные обращения — пустой ответ; cppworker грузит модель в память, но не доступен из балансера.

---

## 1. Архитектура маршрутов (как должно быть)

```
┌──────────────────┐    18030     ┌──────────────┐    18081     ┌──────────────┐
│  Внешний WebUI   │ ──────────▶  │  Наш WebUI   │ ──────────▶  │   Balancer   │
│  (OpenWebUI)     │   (UI)      │   (nginx)    │   (API)      │              │
└──────────────────┘              └──────────────┘              └──────┬───────┘
                                                                       │
                                                          Ollama-прокси │
                                                                       │ 18080
                                                                       ▼
                                                            ┌──────────────┐
                                                            │  Backend     │  cppWorkerPort
                                                            │  selection   │ ──────────▶
                                                            └──────────────┘  18092 (gpu)
                                                                           18091 (cpu)
                                                                           11434 (ollama)
```

| Сервис                  | Порт           | Назначение                                |
|-------------------------|----------------|-------------------------------------------|
| Балансер (Ollama-прокси)| **18080**      | Ollama-совместимый endpoint               |
| Балансер (управляющий)  | **18081**      | Регистрация бэкендов, управление, WebUI   |
| WebUI (наш)             | **18030**      | UI состояния и настроек                   |
| Backend ollama          | **11434**      | Стандартный Ollama                        |
| Backend llama.cpp CPU   | **18091**      | llama.cpp CPU HTTP                        |
| Backend llama.cpp GPU   | **18092**      | llama.cpp GPU HTTP                        |

---

## 2. Корневые причины «Server Disconnected»

### 2.1. cppworker-gpu не зарегистрирован в балансере

**Где:** `deployments/docker-compose.cppworker.yml` (строки 88-142, env-секция `cppworker-gpu`).

**Проблема:** в env-секции `cppworker-gpu` **отсутствовали**:
- `BALANCER_URL` (каноническое имя)
- `CPPWORKER_BALANCER_URL` (читает Go-сторона `cmd/cppworker/balancer_register.go:62`)
- `CPPWORKER_ADVERTISED_PORT` (порт, под которым балансер будет стучаться на cppworker)
- `BALANCER_API_TOKEN` / `CPPWORKER_BALANCER_TOKEN`
- `CPPWORKER_HOST`, `CPPWORKER_BACKEND_ID` (идентификация в балансере)

**Эффект:** `register-with-balancer.sh` (строки 27-30) делал `exit 0` при пустом `BALANCER_URL`.
Go-регистрация тоже не стартовала (читает `CPPWORKER_BALANCER_URL`).

**Итог:** балансер не знал о существовании `cppworker-gpu` → OpenWebUI получал 5xx/disconnected.

### 2.2. Несоответствие default-портов cppworker между Go-кодом и shell-скриптом

**Где:**
- `cmd/cppworker/main.go:39` → `flag.Int("port", 18092, "18091 is legacy")` — HTTP-сервер слушает **18092**.
- `docker/cppworker/entrypoint.sh:99,106` → `${CPPWORKER_PORT:-18091}` — healthcheck на **18091** (если ENV не передан).
- `docker/cppworker/register-with-balancer.sh:13` → `${CPPWORKER_PORT:-18091}` — внутренний healthcheck скрипта на **18091**.

**Эффект (в теории):** при `compose`-запуске `CPPWORKER_PORT=18092` передаётся явно, healthcheck ОК.
Но в случае `docker run` без compose — healthcheck на 18091 никогда не проходил бы.

**Исправление:** дефолты во всех скриптах приведены к **18092** (соответствует Go-коду).

### 2.3. Cross-sync env: BALANCER_URL vs CPPWORKER_BALANCER_URL

**Где:** исторически shell-скрипт читал `BALANCER_URL`, а Go-код — `CPPWORKER_BALANCER_URL`.

**Эффект:** если пользователь задаёт только `BALANCER_URL`, Go auto-registration молча не работает.

**Исправление:** `entrypoint.sh` перед стартом cppworker выставляет
`CPPWORKER_BALANCER_URL=${CPPWORKER_BALANCER_URL:-${BALANCER_URL:-}}` (fallback).
То же — в `register-with-balancer.sh`.

### 2.4. `Dockerfile.gpu` собирает под все архитектуры (CUDA_ARCH=all)

**Где:** `docker/cppworker/Dockerfile.gpu:27,42-60`.

**Проблема:** дефолт `ARG CUDA_ARCH="all"` → `cmake` компилирует 8 SM-таргетов
(`50;61;70;75;80;86;89;90`). На хосте с RTX 3070 это **лишняя работа** (нужен только `86`).

**Эффект:** бинарь раздут, время сборки увеличено в 5-8 раз.

**Исправление (не сделано в этой итерации):** нужно явно передавать
`--build-arg CUDA_ARCH=86` при сборке.

---

## 3. Внесённые правки (файлы)

### 3.1. `docker/cppworker/Dockerfile.gpu`

Добавлен ENV-блок в runtime-стадию:

```dockerfile
ENV CPPWORKER_PORT=18092 \
    CPPWORKER_HOST=0.0.0.0 \
    CPPWORKER_BALANCER_URL="" \
    CPPWORKER_BALANCER_TOKEN="" \
    CPPWORKER_ADVERTISE_HOST="" \
    CPPWORKER_ADVERTISE_PORT=18092 \
    CPPWORKER_REGISTER_RETRY_INTERVAL=30s \
    CPPWORKER_REGISTER_HEARTBEAT=60s
```

Это страховка: даже если compose не передал `CPPWORKER_PORT`, внутри контейнера
по умолчанию будет 18092 (соответствует `flag.Int("port", 18092, ...)` в `main.go`).

### 3.2. `docker/cppworker/entrypoint.sh`

1. **Cross-sync env** перед стартом cppworker:
   ```sh
   export CPPWORKER_BALANCER_URL="${CPPWORKER_BALANCER_URL:-${BALANCER_URL:-}}"
   export CPPWORKER_BALANCER_TOKEN="${CPPWORKER_BALANCER_TOKEN:-${BALANCER_API_TOKEN:-}}"
   ```
2. **Дефолт порта** в healthcheck-е: `:-18092` (было `:-18091`).
3. **Условие запуска регистрации**: `if [ -n "${BALANCER_URL}" ] || [ -n "${CPPWORKER_BALANCER_URL}" ]`.
4. **Логирование** обоих имён URL в stdout для отладки.

### 3.3. `docker/cppworker/register-with-balancer.sh`

1. **Cross-sync env** в начале:
   ```sh
   BALANCER_URL="${BALANCER_URL:-${CPPWORKER_BALANCER_URL:-}}"
   BALANCER_API_TOKEN="${BALANCER_API_TOKEN:-${CPPWORKER_BALANCER_TOKEN:-}}"
   ```
2. **Default `CPPWORKER_PORT=18092`** (было 18091).
3. **Default `CPPWORKER_HOST=cppworker-gpu`**, `CPPWORKER_NAME="CppWorker GPU"`,
   `CPPWORKER_GPU_MODE=gpu`, `CPPWORKER_LABELS` включает `gpu,sm_86`.

### 3.4. `deployments/docker-compose.cppworker.yml`

В env-секцию `cppworker-gpu` добавлены:

```yaml
# === Auto-registration in balancer (port 18081) ===
- BALANCER_URL=${BALANCER_URL:-http://loadbalancer:18081}
- CPPWORKER_BALANCER_URL=${CPPWORKER_BALANCER_URL:-${BALANCER_URL:-http://loadbalancer:18081}}
- CPPWORKER_BALANCER_TOKEN=${CPPWORKER_BALANCER_TOKEN:-${BALANCER_API_TOKEN:-}}
- BALANCER_API_TOKEN=${BALANCER_API_TOKEN:-}
- CPPWORKER_ADVERTISED_PORT=${CPPWORKER_ADVERTISED_PORT:-18092}
- CPPWORKER_HOST=${CPPWORKER_HOST:-cppworker-gpu}
- CPPWORKER_BACKEND_ID=${CPPWORKER_BACKEND_ID:-cppworker-gpu}
- CPPWORKER_GPU_MODE=${CPPWORKER_GPU_MODE:-gpu}
```

Аналогичные правки **для `cppworker-cpu`** (с `CPPWORKER_PORT=18091` и `cppworker-cpu` в качестве host/id) — **не внесены в этой итерации, нужно сделать вручную по аналогии**.

---

## 4. Что осталось сделать (TODO)

### 4.1. Сборка под архитектуру текущей GPU

Для RTX 3070 (sm_86) собирать **только под 86**, а не под все архитектуры.

**Команда:**
```bash
# Очистить предыдущие слои (опционально, для чистоты)
docker builder prune -af

# Сборка с явной архитектурой
CUDA_ARCH=86 docker build \
  --build-arg CUDA_ARCH=86 \
  -t ollama-legion/cppworker:gpu-86 \
  -f docker/cppworker/Dockerfile.gpu \
  --target runtime .
```

**Альтернатива — собирать с тегом `arch_all` (multi-arch fallback):**
```bash
CUDA_ARCH=all docker build \
  --build-arg CUDA_ARCH=all \
  -t ollama-legion/cppworker:gpu-arch_all \
  -f docker/cppworker/Dockerfile.gpu \
  --target runtime .
```

**Ожидаемое ускорение:** 5-8x (8 SM → 1 SM).

### 4.2. Запуск через compose

```bash
# С учётом профиля GPU
docker compose -f deployments/docker-compose.cppworker.yml --profile gpu build
docker compose -f deployments/docker-compose.cppworker.yml --profile gpu up -d

# Просмотр логов
docker compose -f deployments/docker-compose.cppworker.yml --profile gpu logs -f cppworker-gpu
```

### 4.3. Проверить регистрацию в балансере

```bash
# 1. Должен быть зарегистрирован cppworker-gpu с портом 18092
curl -s http://127.0.0.1:18081/api/v1/backends | python -m json.tool

# Ожидаемый элемент:
# {
#   "id": "cppworker-gpu",
#   "name": "CppWorker GPU",
#   "host": "cppworker-gpu",
#   "cppWorkerPort": 18092,
#   "backendType": "llama_cpp",
#   "gpuMode": "gpu",
#   ...
# }
```

### 4.4. Локальный smoke-test (curl)

```bash
# 1. Health cppworker
curl -sf http://127.0.0.1:18092/health
# → {"status":"ok","version":"..."}

# 2. Список моделей
curl -s http://127.0.0.1:18092/api/tags | python -m json.tool

# 3. Загрузить модель
curl -s -X POST http://127.0.0.1:18092/api/models/load \
  -H 'Content-Type: application/json' \
  -d '{"name":"<model_name>","contextSize":4096,"gpuLayers":-1}' | python -m json.tool

# 4. Синхронный чат
curl -s -X POST http://127.0.0.1:18092/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"model":"<model_name>","messages":[{"role":"user","content":"hi"}],"stream":false}' | python -m json.tool

# 5. Стрим-чат (NDJSON, как для OpenWebUI)
curl -N -X POST http://127.0.0.1:18092/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"model":"<model_name>","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 6. Ollama-формат (как стучится OpenWebUI через прокси 18080)
curl -N -X POST http://127.0.0.1:18080/api/chat \
  -H 'Content-Type: application/json' \
  -d '{"model":"<model_name>","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 7. WebUI должен отобразить backend
open http://127.0.0.1:18030
```

### 4.5. Подключение OpenWebUI (внешний клиент)

В настройках OpenWebUI указать:
- **API Base URL:** `http://<host>:18080` (Ollama-прокси балансера)
- **API Key:** (пусто, если балансер не требует) или токен из `BALANCER_API_TOKEN`

Проверить:
```bash
# Из OpenWebUI
curl -s http://<host>:18080/api/tags
# → должен вернуть список моделей cppworker
```

---

## 5. Точки проверки для OpenWebUI

| Проверка                                | Ожидаемо                                              |
|-----------------------------------------|-------------------------------------------------------|
| `GET /health` на 18092                  | `{"status":"ok"}`                                     |
| `GET /api/tags` на 18092                | Список моделей cppworker (если есть .gguf)            |
| `GET /api/tags` на 18080 (через balancer)| Тот же список                                         |
| `POST /api/chat` stream на 18092        | NDJSON-поток токенов                                  |
| `POST /api/chat` stream на 18080        | NDJSON-поток через балансер                           |
| `GET /api/v1/backends` на 18081         | backend `cppworker-gpu` с `cppWorkerPort=18092`       |
| `GET /api/v1/health` на 18081           | `{"status":"ok"}`                                     |
| `GET /api/v1/backends/cppworker-gpu/health` (proxy) | `{"status":"ok"}`                     |
| WebUI на 18030                          | UI видит `cppworker-gpu` со статусом online           |

---

## 6. Известные ограничения и риски

1. **CUDA 12.2 не поддерживает sm_120** (Blackwell, RTX 50xx). Для новых GPU нужно
   поднять базовый образ до `nvidia/cuda:12.8.0-devel-ubuntu22.04` или новее.
2. **Правки для `cppworker-cpu`** (18091) — **не внесены** в этой итерации.
   Если используется CPU-режим, нужно добавить аналогичный env-блок.
3. **Локальный запуск без CUDA** для smoke-теста маршрутов — потребует сборки
   CPU-варианта (`Dockerfile.cpu` или GGML_CUDA=OFF). Это можно сделать отдельно.
4. **BuildKit cache mounts** не добавлены в `Dockerfile.gpu`. Для production-сборки
   рекомендуется добавить `--mount=type=cache,target=/go/pkg/mod` и
   `target=/root/.cache/go-build` для ускорения повторных сборок.
5. **Heartbeat cppworker** использует `http://<host>:<port>/health` для self-check.
   `resolveSelfHost` подменяет `0.0.0.0` на `127.0.0.1`. Это работает, но
   если в compose cppworker-gpu стартует раньше loadbalancer, healthcheck balancer
   может дать 404 на `/api/v1/health` — нужно проверить.

---

## 7. Контрольный чек-лист после всех правок

- [ ] Пересобрать cppworker-gpu с `CUDA_ARCH=86` (для RTX 3070).
- [ ] Поднять `loadbalancer` + `cppworker-gpu` через compose.
- [ ] `curl http://127.0.0.1:18081/api/v1/backends` — backend `cppworker-gpu` рисутствует.
- [ ] `curl http://127.0.0.1:18092/health` — `{"status":"ok"}`.
- [ ] `curl http://127.0.0.1:18080/api/tags` — список моделей.
- [ ] `curl -N -X POST http://127.0.0.1:18080/api/chat -d '{...,"stream":true}'` — стрим идёт.
- [ ] WebUI на 18030 показывает `cppworker-gpu` online.
- [ ] Внешний OpenWebUI подключается к `http://<host>:18080` и отправляет запросы
      без «Ollama Server Disconnected».
- [ ] Повторный запрос после загрузки модели отдаёт содержимое (не пусто).

---

## 8. Файлы под правку вручную (TODO)

1. **`deployments/docker-compose.cppworker.yml` → `cppworker-cpu`:** добавить
   аналогичный env-блок с `CPPWORKER_PORT=18091`, `CPPWORKER_HOST=cppworker-cpu`,
   `CPPWORKER_BACKEND_ID=cppworker-cpu`, `CPPWORKER_GPU_MODE=cpu`.

2. **`docker/cppworker/Dockerfile.gpu` (опционально):** добавить BuildKit cache mounts
   для ускорения пересборки.

3. **`cmd/cppworker/main.go` (опционально, не критично):** если `*port` равен default 18092
   и `os.Getenv("CPPWORKER_PORT") != ""` — применить значение из env к `*port`. Сейчас
   `flag.Parse()` затирает значение, но default совпадает с GPU-портом, так что в compose
   всё работает корректно.
