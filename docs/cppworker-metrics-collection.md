# Метрики cppworker: poller vs agent

> Документ описывает **как именно** собираются live-метрики для бэкендов
> `BackendType=llama_cpp` (cppworker) в OllamaLegion. Целевая аудитория —
> разработчики, которые видят «всё в нулях» на вкладках Dashboard / Backends /
> Models / GGUF Models, и хотят понять, что включить и где смотреть.

---

## 1. Архитектура сбора метрик

В OllamaLegion **два независимых механизма** сбора метрик для cppworker-бэкенда.
Они дополняют друг друга: poller даёт **только** список моделей, agent — всё
остальное (GPU/CPU/RAM).

```
┌─────────────────────┐         ┌──────────────────────┐
│     cppworker       │  GET    │  balancer            │
│   :18092 (host)     │◀────────│  :18081              │
│  /api/models        │ 30s/2s  │                      │
│  /api/models/load/  │         │  ┌────────────────┐  │
│       progress      │         │  │ llamaCpp-      │  │
└─────────────────────┘         │  │ MetricsPoller  │  │
        ▲                       │  │ (in-process)   │  │
        │ GET                   │  └────────────────┘  │
        │ /api/models           │           │           │
        │ /metrics              │           ▼           │
┌─────────────────────┐         │  metricsMgr.llama    │
│   agent sidecar     │         │  Metrics[b.id] = {   │
│   :18032 (host)     │────────▶│    LoadedModels,     │
│   LlamaCollector    │ POST    │    LoadingModels,    │
│   (NVML, CPU, RAM)  │ /api/v1 │    BackendMetrics    │
└─────────────────────┘  backends│  }                   │
                                └──────────────────────┘
                                          │
                                          ▼
                                ┌──────────────────────┐
                                │  WebUI / REST API    │
                                │  Dashboard, Backends │
                                │  /api/v1/backends    │
                                └──────────────────────┘
```

**Ключевые точки:**

1. **Poller живёт внутри балансера** (`internal/balancer/llamacpp_metrics_poller.go`).
   Запускается автоматически при старте `cmd/balancer`. Опрашивает cppworker
   по HTTP. Не требует отдельного контейнера, не использует agent.

2. **Agent — отдельный контейнер** (`cmd/agent`), запускается рядом с cppworker
   в одной pod'е. Содержит NVML-binding для GPU-метрик, читает `/proc/stat` и
   `/proc/meminfo` для хоста. Регистрирует бэкенд через `POST /api/v1/backends`.

3. **Данные агрегируются** в `internal/balancer/cluster_state.go:60-148`:
   - `metricsMgr.metrics[id]` (от agent) → `BackendMetrics.GPU`, `System`, `Ollama`.
   - `metricsMgr.llamaMetrics[id]` (от poller) → `LlamaCppMetrics.LoadedModels/LoadingModels`.

---

## 2. Что собирает `llamaCppMetricsPoller`

**Файл:** `internal/balancer/llamacpp_metrics_poller.go` (read-only — **не трогать**).

Poller делает **только два HTTP-запроса** к каждому зарегистрированному
cppworker-бэкенду:

| Endpoint | Метод | Интервал | Что возвращает |
|---|---|---|---|
| `/api/models` | GET | 30 сек (idle) / 2 сек (при loading) | Массив `models[]` со state, VRAM, RAM, contextSize, gpuLayers, quantization |
| `/api/models/load/progress` | GET | (только при loading) | Массив `models[]` со state, elapsedMs, error |

**Адаптивный интервал:**
- Если в предыдущем poll'е наблюдались loading-модели → следующий poll через
  `fastInterval=2s` (см. `lastLoadingSeen`).
- Иначе → `interval=30s`, чтобы не нагружать cppworker.

**Что попадает в `metricsMgr.llamaMetrics[b.id]`:**
- `LoadedModels []LlamaCppModel` — все модели (любое state: loaded/loading/error).
  Фильтрация по `state==loaded` делается на стороне UI.
- `LoadingModels []LlamaCppModel` — модели в процессе загрузки (state=loading).
  Используется UI для отображения спиннера и elapsed-time «Загружается model-name 25s».

**Что poller НЕ собирает** (важно!):
- ❌ GPU metrics (usage%, VRAM used/total, temperature, power, clocks) — нет NVML.
- ❌ CPU/RAM хоста — poller работает на хосте балансера, не cppworker'а.
- ❌ `requests_per_second`, `avg_response_time` — cppworker не отдаёт.
- ❌ `total_queries`, `active_queries` (есть в `/api/models`, но не пробрасываются).

**NB:** для этих метрик нужен **agent sidecar** (см. §3).

---

## 3. Что собирает agent (Ollama agent + `LlamaCollector`)

**Файл:** `internal/agent/collector.go`, `internal/agent/llama_collector.go`.

Agent запускается **рядом с cppworker** (sidecar pattern — shared network namespace
или bridge-сеть). Конфигурируется через `BackendType`:

```go
// internal/agent/collector.go:81-89
if config.BackendType == types.BackendTypeLlamaCpp {
    cppURL := config.CppWorkerURL
    if cppURL == "" {
        cppURL = "http://localhost:18091"
    }
    a.llamaCollector = NewLlamaCollector(cppURL)
}
```

**`LlamaCollector` собирает:**

| Метрика | Источник | Попадает в |
|---|---|---|
| `gpu[].usage` (%) | NVML `nvmlDeviceGetUtilizationRates` (Linux) или `nvidia-smi` (Windows fallback) | `BackendMetrics.GPU.Usage` |
| `gpu[].memoryUsed` / `memoryTotal` (MB) | NVML `nvmlDeviceGetMemoryInfo` | `BackendMetrics.GPU.MemoryUsed/MemoryTotal` |
| `gpu[].temperature` (°C) | NVML `nvmlDeviceGetTemperature` | `BackendMetrics.GPU.Temperature` |
| `gpu[].powerDraw` (W) | NVML `nvmlDeviceGetPowerUsage` | `BackendMetrics.GPU.PowerDraw` |
| `gpu[].clocks.graphics/memory` (MHz) | NVML `nvmlDeviceGetClockInfo` | `BackendMetrics.GPU.Clocks*` |
| `cpuPercent` (%) | `/proc/stat` (Linux) / `wmic` (Windows) | `BackendMetrics.System.CPUPercent` |
| `memUsed` / `memTotal` (MB) | `/proc/meminfo` / `wmic` | `BackendMetrics.System.MemUsed/MemTotal` |
| `models[]` | `GET http://cppworker:18091/api/models` | `BackendMetrics.LlamaCpp.LoadedModels` (дублирует poller, но agent идёт первым) |

**Когда agent активен** (`BackendType=llama_cpp` и `hasAgent=true` в
`/api/v1/backends`) — в `data/state.json` видна запись:

```json
{
  "id": "cppworker-gpu-with-agent",
  "type": "llama_cpp",
  "hasAgent": true,
  "host": "cppworker-gpu",
  "port": 18092
}
```

`hasAgent=true` означает: `metricsMgr.metrics[id]` заполнен, UI/WebUI рисует
live-значения GPU/CPU/RAM.

---

## 4. Когда использовать sidecar vs standalone

| Сценарий | Рекомендация | Compose |
|---|---|---|
| Продакшен с GPU-нодой, нужен полный мониторинг | **Sidecar** — agent в той же pod | `docker-compose.cppworker-with-agent.yml` |
| Standalone демо / один хост без внешнего балансера | **Bundled sidecar** | `docker-compose.cppworker-with-agent.standalone.yml` |
| Уже развёрнут bundled (`docker-compose.cppworker-bundled.yml`) | **Ничего не менять** — bundled-стек работает, agent не нужен для инференса | `docker-compose.cppworker-bundled.yml` |
| Legacy cppworker без мониторинга (метрики = 0, но инференс работает) | **Добавить sidecar agent** | см. §6 Troubleshooting |
| CPU-only cppworker (без GPU) | **Опционально** — agent даст CPU/RAM, но NVML-метрики пустые | любой sidecar compose |

**Чеклист для sidecar:**

- [ ] `BackendType=llama_cpp` в `agent`-конфиге.
- [ ] `CppWorkerURL` указывает на **внутренний** адрес cppworker'а (например,
      `http://localhost:18091` в shared network namespace, или `http://cppworker-gpu:18091` в bridge).
- [ ] На GPU-хосте установлен `nvidia-container-toolkit` (для NVML).
- [ ] Agent регистрирует бэкенд через `POST /api/v1/backends` с правильным `id`
      и `host`/`port`, которые балансер может резолвить.
- [ ] Если используется bundled-стек — `CPPWORKER_REGISTER_DISABLE=true` на
      cppworker (issue 2026-06-25 про дубли).

---

## 5. Backward compatibility: legacy `BackendType=""`

До введения `BackendType` поле было пустым. Для legacy-бэкендов в `data/state.json`
(которые ещё не мигрировали) применяется нормализация:

```go
// internal/balancer/normalizeBackendType
if bt == "" {
    return types.BackendTypeOllama // legacy default
}
```

**Это означает:** если у вас в `data/state.json` бэкенд `cppworker-gpu` без
поля `type`, балансер считает его **Ollama-бэкендом**. Agent sidecar по-прежнему
работает (LlamaCollector выбирается по `BackendType` из **agent-конфига**, а не
из state.json), но в `data/state.json` будет видно `type="ollama"` и
`hasAgent=true` без `LlamaCpp`-блока.

**Миграция:** `POST /api/v1/backends/{id}` с `{"type": "llama_cpp"}` →
балансер перезапишет state.json с правильным `BackendType`.

**Проверка:** `curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | jq '.backends[] | {id, type, hasAgent}'`

---

## 6. Troubleshooting

### 6.1 «Метрики в нулях для cppworker backend»

**Шаг 1.** Проверить, зарегистрирован ли бэкенд с `BackendType=llama_cpp`:

```bash
curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/backends | jq '.backends[] | select(.id | contains("cppworker")) | {id, type, hasAgent}'
```

Если `type="ollama"` или пусто — это legacy-бэкенд, см. §5.

**Шаг 2.** Если `type="llama_cpp"` и `hasAgent=false`:

- Проверить, запущен ли agent sidecar: `docker ps | grep agent`.
- Проверить логи agent: `docker logs <agent-container> 2>&1 | grep -E "registered|error"`.
- Убедиться, что agent может достучаться до cppworker:
  `docker exec <agent-container> curl http://cppworker-gpu:18091/health`.

**Шаг 3.** Если `type="llama_cpp"` и `hasAgent=true`, но метрики всё равно 0:

- Проверить, что NVML доступен внутри agent-контейнера:
  `docker exec <agent-container> nvidia-smi`.
- Если `command not found` — образ собран без `nvidia-container-toolkit`.
  Пересобрать с `--gpus all` и `runtime: nvidia` в compose.
- На Windows: agent использует `nvidia-smi` через `exec.Command` (см.
  `internal/agent/nvml_windows.go`). Если `nvidia-smi` нет в PATH —
  метрики будут нулями.

**Шаг 4.** Проверить, что poller вообще работает:

```bash
# В логах балансера должны быть строки:
grep "llamaCppMetricsPoller" data/balancer.log
# "llamaCppMetricsPoller started" — OK
# "llamaCppMetricsPoller: updated llama.cpp metrics" — каждый 30s/2s
```

Если poller не запускается — проверить, что `llamaCppRouter` инициализирован
(должен быть при `OperatingMode` ∈ {`standard`, `replication`, `rpc_coordinator`}).

### 6.2 «LoadedModels пуст, хотя модель загружена»

- Проверить `GET http://cppworker-host:18092/api/models` напрямую:
  должен вернуть JSON с `count > 0`.
- Если cppworker отдаёт модели, а poller показывает пусто — возможно,
  `cppworker` за NAT, и `host:port` в state.json недоступен с балансера.
- Проверить `BackendMetrics` в WebUI: Backends tab → "View metrics" →
  `loadedModels.length` должен совпадать с `count` из `/api/models`.

### 6.3 «Прогресс загрузки не обновляется (UI показывает 0%)»

`/api/models/load/progress` реализован в cppworker ≥ 2026-06-25. На старых
образах endpoint вернёт 404 — poller это игнорирует (см. код
`pollLoadingProgress`: «404 — endpoint может быть не реализован в старой
версии cppworker. Тихо игнорируем.»). Решение: пересобрать cppworker-образ
после 2026-06-25 или подождать завершения загрузки (poller всё равно
обновит `LoadedModels` после `/api/models` через 2 сек).

### 6.4 «После restart балансера метрики пропали»

Poller делает **немедленный первый poll** при старте (`loop()` строка 101),
но agent-метрики (`metricsMgr.metrics[id]`) обновляются только когда agent
отправит следующий heartbeat. Если agent настроен с большим
`heartbeat_interval` (default 30 сек) — после рестарта балансера будет
«дыра» в ~30 сек.

---

## 7. Связанные документы

- `docs/issues/2026-06-29-llamacpp-agent-sidecar.md` — полный постмортем
  про sidecar (env-переменные, compose-файлы, ограничения).
- `docs/issues/2026-06-29-big-model-20gb-fix.md` — auto_tune_nctx + 3-stage
  cascade (метрики VRAM критичны для выбора n_ctx).
- `docs/runbook-tools.md` — runbook для tools/tool_calls (если нужно дебажить
  inference-проблемы, метрики нагрузки тоже полезны).
- `docs/ru/docker-compose-guide.md` §4.1 — sidecar-стек: cppworker + agent.
- `internal/balancer/llamacpp_metrics_poller.go` — сам poller (read-only).
- `internal/agent/collector.go:81-89` — `BackendType=llama_cpp` switch.
- `pkg/types/backend_type.go` — константы `BackendTypeLlamaCpp` / `BackendTypeOllama`.

---

## 8. Сводная таблица: что откуда берётся

| Поле в WebUI / API | Источник | Endpoint / метод |
|---|---|---|
| Backend list (`/api/v1/backends`) | `data/state.json` | agent `POST /api/v1/backends` при регистрации |
| `BackendMetrics.GPU.Usage` (%) | **agent** | NVML (Linux) / `nvidia-smi` (Windows) |
| `BackendMetrics.GPU.MemoryUsed` | **agent** | NVML |
| `BackendMetrics.GPU.Temperature` | **agent** | NVML |
| `BackendMetrics.System.CPUPercent` | **agent** | `/proc/stat` / `wmic` |
| `BackendMetrics.System.MemUsed` | **agent** | `/proc/meminfo` / `wmic` |
| `LlamaCppMetrics.LoadedModels[].Name` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].State` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].ContextLength` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].NumGPULayers` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].Quantization` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].VRAMUsage` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadedModels[].RAMUsage` | **poller** | `GET /api/models` |
| `LlamaCppMetrics.LoadingModels[].ElapsedMs` | **poller** | `GET /api/models/load/progress` |
| `LlamaCppMetrics.LoadingModels[].Error` | **poller** | `GET /api/models/load/progress` |

**Главный вывод:** для **полных** live-метрик нужен **agent sidecar**. Poller
один не даст GPU/CPU/RAM — только список моделей и прогресс загрузки.