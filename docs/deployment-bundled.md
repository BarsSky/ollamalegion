# Bundled Stack: cppworker-gpu + balancer + webui

> **Один compose-файл, три сервиса, ноль ручной настройки регистрации.**
> CppWorker сам регистрируется в балансировщике при старте.

## Зачем нужен bundled-стек

В `docker-compose.full.yml` есть cppworker-cpu/gpu/stub, agent, balancer, webui — 6 сервисов
с кучей перемычек и профилей. Для типичной single-GPU-машины (RTX 3070/3080/4070 + 32-64 GB RAM)
это избыточно: нужен **только** cppworker-gpu + балансировщик + WebUI.

`docker-compose.cppworker-bundled.yml` — это **чистый минимальный стек** на 3 сервиса:

| Сервис | Порт (хост) | Назначение |
|--------|-------------|-----------|
| `loadbalancer` | 18080 (API), 18081 (admin) | OpenAI-совместимый прокси + admin API |
| `cppworker-gpu` | 18092 | llama.cpp C-bridge (real, не stub) |
| `webui` | 18083 | Дашборд для мониторинга и управления |

Клиенты (Cline, OpenWebUI, anything-llm) подключаются **только к балансировщику** на 18080.

## Ключевая фишка: auto-registration

В bundled-стеке **cppworker сам регистрирует себя** в балансировщике через новый
механизм `cmd/cppworker/balancer_register.go`:

1. CppWorker стартует
2. Видит ENV `CPPWORKER_BALANCER_URL=http://loadbalancer:18081`
3. Запускает background goroutine `autoRegisterWithBalancer`
4. Резолвит DNS `loadbalancer` в Docker-network
5. POST `{BALANCER_URL}/api/v1/backends` с JSON:
   ```json
   {
     "id": "cppworker-gpu-bundled",
     "name": "cppworker-gpu-bundled",
     "host": "cppworker-gpu",
     "cppWorkerPort": 18092,
     "backendType": "llama_cpp",
     "gpuMode": "gpu"
   }
   ```
6. С заголовком `X-API-Token: <CPPWORKER_BALANCER_TOKEN>`
7. Если регистрация успешна → лог `cppworker registered with balancer: id=cppworker-gpu-bundled, status=200`
8. Если 409 Conflict (уже зарегистрирован) → трактуем как OK
9. Если ошибка (балансировщик ещё не поднят) → retry через 15s (настраивается)
10. Периодический local /health check (heartbeat) — даёт лог только при сбое

### Что если балансировщик упал?

CppWorker **не падает**: он логирует `balancer host not resolvable, cppworker will work in
standalone mode` и продолжает принимать запросы напрямую на порт 18092. Это позволяет:

- Разрабатывать/тестировать cppworker без поднятого балансировщика
- Пережить restart балансировщика без перезапуска cppworker (auto-registration восстановится)

### Что если cppworker упал?

Docker `restart: unless-stopped` поднимет его. После старта auto-registration повторится.
Если балансировщик уже содержит ID `cppworker-gpu-bundled` — получим 409 Conflict и это
будет воспринято как успех (balancer знает про этот backend).

## Quick start

### 1. Подготовить .env

```bash
cd deployments
cp .env.bundled.example .env.bundled
# ОБЯЗАТЕЛЬНО смените CPPWORKER_API_TOKEN на свой!
```

### 2. Положить модель

```bash
mkdir -p ../models
cp /path/to/gemma-4-E4B-it-Q4_K_M.gguf ../models/
```

### 3. Запустить стек

**Windows PowerShell:**
```powershell
.\scripts\start-bundled.ps1           # запуск в фоне
.\scripts\start-bundled.ps1 -Rebuild  # с пересборкой образов
.\scripts\start-bundled.ps1 -Down     # остановить
.\scripts\start-bundled.ps1 -Logs     # логи
```

**Linux / Mac / WSL:**
```bash
./scripts/start-bundled.sh           # запуск в фоне
./scripts/start-bundled.sh rebuild   # с пересборкой
./scripts/start-bundled.sh down      # остановить
./scripts/start-bundled.sh logs      # логи
```

**Или напрямую через docker compose:**
```bash
cd deployments
docker compose -f docker-compose.cppworker-bundled.yml \
               --env-file .env.bundled \
               up -d --build
```

### 4. Проверить регистрацию

```bash
# Подождите ~30 сек пока healthcheck пройдёт, затем:
curl -H "X-API-Token: $CPPWORKER_API_TOKEN" http://localhost:18081/api/v1/backends | jq

# Должно быть:
# {
#   "backends": [
#     {
#       "id": "cppworker-gpu-bundled",
#       "name": "cppworker-gpu-bundled",
#       "host": "cppworker-gpu",
#       "cppWorkerPort": 18092,
#       "type": "llama_cpp",
#       "status": "healthy",
#       ...
#     }
#   ],
#   "total": 1
# }
```

### 5. Открыть WebUI

```
http://localhost:18083
```

Страница **"gguf Models"** → выбрать `cppworker-gpu-bundled` → **"Load"** → gemma-4 грузится в VRAM.

### 6. Подключить клиент (Cline/OpenWebUI)

- **Cline VSCode extension:** `Ollama Base URL` = `http://localhost:18080`
- **OpenWebUI:** `OLLAMA_API_BASE_URL` = `http://localhost:18080`

Все запросы идут через балансировщик. CppWorker получает их с заголовком `X-Cpp-Ctx`
(per-model profile `gemma-4` из `config.bundled.json` → 4096).

## ENV-переменные (всё в `.env.bundled`)

| Переменная | Дефолт | Описание |
|------------|--------|----------|
| `LB_PORT` | 18080 | OpenAI API порт на хосте |
| `LB_ADMIN_PORT` | 18081 | Admin API порт на хосте |
| `CPPWORKER_GPU_PORT` | 18092 | Прямой доступ к cppworker (обход балансировщика) |
| `WEBUI_PORT` | 18083 | WebUI порт на хосте |
| `CPPWORKER_API_TOKEN` | `changeme-bundled-strong-token-please-change` | API-токен (ДОЛЖЕН совпадать в balancer и cppworker) |
| `DEFAULT_CTX_SIZE` | 4096 | n_ctx по умолчанию |
| `DEFAULT_BATCH_SIZE` | 512 | batch size по умолчанию |
| `DEFAULT_GPU_LAYERS` | 30 | GPU offload (30 = partial для 8GB VRAM + 5GB модель) |
| `DEFAULT_FLASH_ATTN_TYPE` | -1 | Flash attention (-1=auto) |
| `DEFAULT_N_THREADS` | 0 | CPU threads (0 = auto) |
| `DEFAULT_USE_MMAP` | true | mmap для больших моделей |
| `DEFAULT_NUMA` | false | NUMA optimization |
| `CPPWORKER_RAM_FALLBACK_N_CTX` | true | Auto-reload модели с большим n_ctx через mmap/RAM при нехватке VRAM |
| `CPPWORKER_RAM_FALLBACK_GPU_LAYERS` | 0 | GPU-слоёв при RAM fallback: -1=текущее, 0=CPU-only, N=явное число |
| `CPPWORKER_RAM_FALLBACK_MAX_N_CTX` | 16384 | Максимальный n_ctx, до которого разрешён RAM fallback |
| `NVIDIA_VISIBLE_DEVICES` | all | CUDA devices (all / 0 / 0,1) |
| `LB_LOG_LEVEL` | info | Уровень логирования балансировщика |

## ENV-переменные cppworker для auto-registration (в compose-файле)

| Переменная | Значение | Описание |
|------------|----------|----------|
| `CPPWORKER_BALANCER_URL` | `http://loadbalancer:18081` | URL балансировщика (если пусто — auto-registration отключён) |
| `CPPWORKER_BALANCER_TOKEN` | `${CPPWORKER_API_TOKEN}` | X-API-Token для авторизации в балансировщике |
| `CPPWORKER_ADVERTISE_HOST` | `cppworker-gpu` | Имя контейнера в Docker-network |
| `CPPWORKER_ADVERTISE_PORT` | 18092 | Порт, на котором cppworker слушает внутри контейнера (всегда равен `CPPWORKER_PORT`) |
| `CPPWORKER_REGISTER_NAME` | `cppworker-gpu-bundled` | Уникальный ID бэкенда в балансировщике |
| `CPPWORKER_REGISTER_RETRY_INTERVAL` | 15s | Retry interval при недоступности балансировщика |
| `CPPWORKER_REGISTER_HEARTBEAT` | 60s | Период локального /health check (best-effort) |
| `CPPWORKER_REGISTER_GPU_MODE` | gpu | Режим бэкенда (auto/gpu/cpu) |

## Конфигурация per-model profiles

В `config.bundled.json` есть секция `modelManagement.profiles.gemma-4`:

```json
"gemma-4": {
  "numCtx": 4096,
  "batchSize": 256,
  "numGpuLayers": 30,
  "flashAttention": true,
  "useMmap": true
}
```

Этот профиль применяется балансировщиком при роутинге запросов к cppworker-gpu.
Приоритет: `body num_ctx > profile num_ctx > backend default num_ctx`.

Подробнее: [docs/cppworker-model-params.md](cppworker-model-params.md) (Шаг 5 плана).

## Структура файлов

```
ollamalegion/
├── deployments/
│   ├── docker-compose.cppworker-bundled.yml   # ← этот стек
│   ├── .env.bundled.example                    # шаблон .env
│   └── .env.bundled                            # ваш реальный .env
├── config/
│   ├── config.bundled.json                     # конфиг балансировщика для стека
│   └── config.json                             # (скопирован из bundled, если ещё нет)
├── scripts/
│   ├── start-bundled.sh                        # запуск (Linux/Mac/WSL)
│   └── start-bundled.ps1                       # запуск (Windows)
└── cmd/cppworker/
    └── balancer_register.go                    # auto-registration логика
```

## Диагностика

### Проверить, что cppworker зарегистрировался

```bash
# 1. Логи cppworker
docker logs ol-bundled-cppworker-gpu 2>&1 | grep -E "registration|registered|balancer"

# Ожидаемые строки:
# balancer DNS resolved balancerURL=http://loadbalancer:18081 addresses=[172.x.x.x]
# starting balancer auto-registration backendID=cppworker-gpu-bundled ...
# cppworker registered with balancer id=cppworker-gpu-bundled status=200

# 2. Через admin API балансировщика
TOKEN=$(grep CPPWORKER_API_TOKEN deployments/.env.bundled | cut -d= -f2)
curl -H "X-API-Token: $TOKEN" http://localhost:18081/api/v1/backends | jq '.backends[] | {id, host, type, status}'
```

### Если cppworker НЕ зарегистрировался

```bash
# Проверить, что балансировщик healthy
docker ps --format "table {{.Names}}\t{{.Status}}" | grep ol-bundled

# Проверить DNS внутри cppworker
docker exec ol-bundled-cppworker-gpu nslookup loadbalancer
# Должен вернуть IP из сети ol-bundled-net

# Ручная попытка регистрации (для отладки)
docker exec ol-bundled-cppworker-gpu \
  curl -X POST http://loadbalancer:18081/api/v1/backends \
       -H "X-API-Token: $TOKEN" \
       -H "Content-Type: application/json" \
        -d '{"id":"manual-test","name":"manual","host":"cppworker-gpu","cppWorkerPort":18092,"backendType":"llama_cpp","gpuMode":"gpu"}'
```

### Если модель не загружается в VRAM

```bash
# Проверить, что в WebUI нажали Load
# Или запросить вручную:
TOKEN=$(grep CPPWORKER_API_TOKEN deployments/.env.bundled | cut -d= -f2)
curl -X POST http://localhost:18092/api/models/load \
     -H "Content-Type: application/json" \
     -d '{"name":"gemma-4","gpuLayers":30,"contextSize":4096,"batchSize":256}'

# Проверить GPU
nvidia-smi

# Проверить, что cuda images работают
docker exec ol-bundled-cppworker-gpu nvidia-smi
```

### Перезапуск без потери регистрации

`restart: unless-stopped` в compose сохраняет состояние. Если нужно пересобрать
cppworker-gpu (например, после изменения кода):

```bash
# Windows
.\scripts\start-bundled.ps1 -Rebuild

# Linux
./scripts/start-bundled.sh rebuild
```

CppWorker перезапустится, и через ~15s auto-registration повторится. Если балансировщик
уже содержит ID — получит 409 (трактуется как OK) и продолжит работать.

## Сравнение с `docker-compose.full.yml`

| | full.yml | cppworker-bundled.yml |
|---|----------|----------------------|
| Сервисов | 6 (cpu, gpu, stub, agent, balancer, webui) | 3 (balancer, gpu, webui) |
| Профили | cpu / gpu / stub / agent-gpu / default | нет (всё стартует) |
| Конфигурация | config.json (большой, общий) | config.bundled.json (минимальный, `backends: []`) |
| Регистрация | вручную через env `BACKENDS` или в config.json | автоматически при старте cppworker |
| Когда использовать | продакшн-кластер, несколько нод, agent с GPU-метриками | single-GPU рабочая станция, dev/test, мини-стенд |
| Размер compose | ~250 строк | ~150 строк |

## RAM fallback для большого контекста (n_ctx)

По умолчанию в bundled-стеке включён **RAM fallback** для запросов с большим `num_ctx`:

- Модель загружается с `DEFAULT_CTX_SIZE=4096` (безопасно для 8GB VRAM).
- Если клиент (OpenWebUI / Cline) отправляет запрос с `options.num_ctx=16384`,
  C-bridge вернёт `ErrNCtxNeedsReload`.
- CppWorker автоматически выгружает модель и перезагружает её с `n_ctx=16384`,
  форсируя `use_mmap=true` и используя `CPPWORKER_RAM_FALLBACK_GPU_LAYERS`.
- По умолчанию `CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0`: модель полностью уходит
  в RAM (CPU-only), освобождая всю VRAM под KV-cache. Это медленнее, чем GPU,
  но позволяет обработать большой контекст, когда видеопамяти недостаточно.
- После успешной перезагрузки запрос повторяется автоматически; клиент получает
  обычный ответ.

Настройка под ваше железо:

```env
# Включить fallback (по умолчанию true)
CPPWORKER_RAM_FALLBACK_N_CTX=true

# Оставить часть слоёв на GPU для скорости:
#   -1 = использовать DEFAULT_GPU_LAYERS
#    0 = CPU-only (медленно, но надёжно для 8GB VRAM)
#   20 = частичный offload (если модель маленькая или VRAM > 8GB)
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0

# Потолок n_ctx (защита от запросов с num_ctx=1000000)
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=16384
```

Если fallback не срабатывает, проверьте логи:

```bash
docker logs ol-bundled-cppworker-gpu 2>&1 | grep -i "RAM fallback"
```

Ожидаемые строки:

```text
RAM fallback: reloading model with larger n_ctx ...
RAM fallback: model unloaded ...
RAM fallback: model reloaded successfully ...
```

## FAQ

**Q: Я не хочу, чтобы cppworker регистрировался — что делать?**
A: Установите `CPPWORKER_BALANCER_URL=` (пусто) в `.env.bundled`. CppWorker запустится
в standalone-режиме, лог: `balancer auto-registration disabled`.

**Q: Как добавить несколько cppworker (multi-GPU)?**
A: Запустите несколько копий `cppworker-gpu` с разными `CPPWORKER_ADVERTISE_HOST`
и `CPPWORKER_REGISTER_NAME` (например, `cppworker-gpu-0`, `cppworker-gpu-1`) и
разными `NVIDIA_VISIBLE_DEVICES=0`, `NVIDIA_VISIBLE_DEVICES=1`. Каждый
зарегистрируется отдельно.

**Q: Что если CPPWORKER_API_TOKEN в .env не совпадает с auth.tokens в config.json?**
A: Скрипт `start-bundled.sh/ps1` подставляет токен автоматически при первом запуске.
Если меняете токен в `.env.bundled` — пересоздайте `config.json` или обновите
`auth.tokens[].name` вручную.

**Q: Почему `runtime: nvidia` а не `deploy.resources.reservations.devices`?**
A: `runtime: nvidia` — это старый синтаксис NVIDIA Container Runtime. Работает
с nvidia-docker2. Для нового синтаксиса (Docker 19.03+ с nvidia-container-toolkit)
можно заменить на `deploy.resources.reservations.devices`, но `runtime: nvidia`
более портабелен и совместим с docker-compose v1/v2.

**Q: Можно ли использовать bundled-стек для multi-host кластера?**
A: Нет, для multi-host используйте `docker-compose.full.yml` или `docker-compose.llama.cpp.yml`.
Bundled — это single-host решение.

**Q: Как сделать auto-registration по IP хоста, а не по DNS-имени `loadbalancer`?**
A: По умолчанию cppworker регистрируется через DNS-имя `loadbalancer` внутри compose-сети.
Если балансировщик запущен на другом хосте или Docker DNS не работает, задайте в `.env.bundled`:
```env
BALANCER_URL=http://<IP_хоста>:18081
# Опционально: явный хост/IP, по которому балансировщик будет обращаться к cppworker.
# Если не задано, используется DNS-имя контейнера `cppworker-gpu`.
CPPWORKER_ADVERTISE_HOST=<IP_или_имя_хоста>
```
При пустом `BALANCER_URL` cppworker отключает auto-registration и работает в standalone-режиме,
ожидая прямые запросы или ручную регистрацию через admin API балансировщика.

**Q: Почему `CPPWORKER_ADVERTISE_PORT`/`CPPWORKER_PORT` зафиксированы на 18092?**
A: В bundled-стеке внутренний порт cppworker един и совпадает с host-портом (`18092:18092`).
Это исключает путаницу с маршрутизацией между балансировщиком и cppworker.
`CPPWORKER_HOST=0.0.0.0` используется только для прослушивания внутри контейнера;
`CPPWORKER_ADVERTISE_HOST` — то, что балансировщик использует для обратных вызовов.
