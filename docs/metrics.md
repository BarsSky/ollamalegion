 метрик OllamaLegion

## Описание

Агент собирает **все возможные данные** с Ollama-сервера и хоста, передаёт балансеру для принятия решений и в WebUI для визуализации. Балансер дополнительно вычисляет proxy-метрики (точнее, чем агентские).

---

## 📊 Источники метрик

| Источник | Данные | Кто собирает |
|----------|--------|--------------|
| **NVML / nvidia-smi** | GPU utilization, VRAM, температура, частота, TDP | Агент (GPU mode) |
| **OS (/proc, gopsutil)** | CPU, RAM, Disk, Network | Агент (CPU+GPU mode) |
| **Ollama API** (`/api/tags`) | Список моделей, размеры, детали | Агент + fallback балансер |
| **Proxy counters** | ActiveRequests, RPS | **Балансер** (proxy-calculated) |
| **Predictor** | Тренды, прогнозы | **Балансер** |

---

## 1. GPU Metrics (`GPUMetrics`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `usagePercent` | float64 | % | NVML | ✅ | Загрузка GPU |
| `memoryTotal` | uint64 | MB | NVML | ✅ | Всего VRAM |
| `memoryUsed` | uint64 | MB | NVML | ✅ | Использовано VRAM |
| `memoryFree` | uint64 | MB | NVML | ✅ (вычисл.) | Свободно VRAM |
| `temperature` | int | °C | NVML | ✅ | Температура GPU |
| `powerUsage` | int | W | NVML | ✅ | Потребление энергии |
| `powerLimit` | int | W | NVML | ❌ | Лимит мощности (TDP) |
| `gpuClock` | int | MHz | NVML | ❌ | Частота GPU |
| `memClock` | int | MHz | NVML | ❌ | Частота памяти |

> **Примечание:** `powerLimit`, `gpuClock`, `memClock` собираются агентом, но **не отображаются в WebUI**.

---

## 2. System Metrics (`SystemMetrics`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `cpuUsagePercent` | float64 | % | gopsutil | ✅ | Общая загрузка CPU |
| `memoryTotal` | uint64 | MB | gopsutil | ✅ | Всего RAM |
| `memoryUsed` | uint64 | MB | gopsutil | ✅ | Использовано RAM |
| `memoryFree` | uint64 | MB | gopsutil | ✅ (вычисл.) | Свободно RAM |
| `diskTotal` | uint64 | MB | gopsutil | ❌ | Всего диска |
| `diskUsed` | uint64 | MB | gopsutil | ❌ | Использовано диска |
| `diskFree` | uint64 | MB | gopsutil | ❌ | Свободно диска |
| `networkRX` | uint64 | bytes | gopsutil | ❌ | Получено по сети (накопительно) |
| `networkTX` | uint64 | bytes | gopsutil | ❌ | Отправлено по сети (накопительно) |
| `networkRXRate` | float64 | bytes/s | gopsutil | ❌ | Скорость получения (вычисляется из дельты) |
| `networkTXRate` | float64 | bytes/s | gopsutil | ❌ | Скорость отправки (вычисляется из дельты) |

### 2.1 CPU Details (`SystemMetrics.CPU` / `CPUMetrics`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `usagePercent` | float64 | % | gopsutil | ✅ | Общая загрузка CPU |
| `usagePerCore` | []float64 | % | /proc/stat | ✅ | Загрузка по ядрам |
| `coreCount` | int | шт | /proc/cpuinfo | ✅ | Количество ядер |
| `threadCount` | int | шт | /proc/cpuinfo | ✅ | Количество потоков |
| `model` | string | — | /proc/cpuinfo | ✅ | Модель CPU |
| `loadAverage1` | float64 | — | /proc/loadavg | ✅ | Load avg 1 мин |
| `loadAverage5` | float64 | — | /proc/loadavg | ✅ | Load avg 5 мин |
| `loadAverage15` | float64 | — | /proc/loadavg | ✅ | Load avg 15 мин |
| `temperature` | int | °C | /sys/class/thermal | ✅ | Температура CPU |
| `throttled` | bool | — | /sys/devices/system/cpu | ✅ | CPU троттлинг |

---

## 3. Ollama Metrics (`OllamaMetrics`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `runningModels` | []RunningModel | — | Ollama API | ✅ | Запущенные модели |
| `availableModels` | []RunningModel | — | Ollama API | ❌ | Доступные модели |
| `activeRequests` | int | шт | **Proxy** | ✅ | Активные запросы |
| `totalRequests` | int64 | шт | **Proxy** | ❌ | Всего запросов |
| `avgResponseTime` | float64 | ms | — | ❌ | Среднее время ответа |
| `requestsPerSecond` | float64 | RPS | **Proxy** | ✅ | Запросов в секунду |
| `maxModels` | int | шт | — | ❌ | Лимит моделей |
| `maxConcurrentRequests` | int | шт | — | ❌ | Лимит запросов |
| `freeSlots` | int | шт | **Proxy** | ✅ | Свободные слоты (`MaxConcurrent - ActiveRequests`) |
| `availableSlots` | int | шт | **Proxy** | ❌ | Доступные слоты с учётом headroom (зарезервировано для будущего расширения) |

> **⚠️ Критически важно:** `activeRequests`, `totalRequests`, `requestsPerSecond`, `freeSlots` — вычисляются **балансером** (proxy-calculated), а не агентом. Агент отправляет `0` для этих полей, балансер переопределяет их точными значениями.

### 3.1 Running Model (`RunningModel`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `name` | string | — | Ollama API | ✅ | Название модели |
| `size` | uint64 | bytes | Ollama API | ❌ | Размер модели |
| `vramUsage` | uint64 | MB | эвристика | ✅ | Оценка VRAM |
| `ramUsage` | uint64 | MB | эвристика | ✅ | Оценка RAM (CPU) |
| `expiresAt` | time.Time | — | Ollama API | ✅ | Время выгрузки |
| `digest` | string | — | Ollama API | ✅ | Хеш модели |
| `loadCount` | int | шт | — | ❌ | Количество загрузок |
| `family` | string | — | Ollama API | ❌ | Семейство (llama, mistral...) |
| `format` | string | — | Ollama API | ❌ | Формат (gguf...) |
| `parameterSize` | string | — | Ollama API | ❌ | Размер параметров (7B, 70B...) |
| `quantization` | string | — | Ollama API | ❌ | Квантование (Q4_0...) |

> **Примечание:** Поля `family`, `format`, `parameterSize`, `quantization` собираются агентом из `/api/ps` → `details` и `/api/tags` fallback. Передаются в `BackendMetrics.Ollama.RunningModels` и доступны в WebUI.

---

## 4. Proxy-Calculated Metrics (Балансер)

| Поле | Тип | Единица | Алгоритм | Описание |
|------|-----|---------|----------|----------|
| `CalculatedRPS` | float64 | RPS | `len(RequestHistory) / 60.0` | Скользящее окно 60 секунд |
| `ActiveRequests` | int | шт | `ActiveReqs++ / --` | Точный HTTP счётчик |
| `FreeSlots` | int | шт | `MaxConcurrent - ActiveRequests` | Свободные слоты |
| `AvailableSlots` | int | шт | `FreeSlots - headroom` | Доступные слоты с учётом резерва (headroom) |
 
| `QueueStats.current_size` | int | шт | `len(queue)` | Текущая очередь |
| `QueueStats.processed_total` | int64 | шт | счётчик | Всего обработано |
| `QueueStats.dispatch_by_affinity` | int64 | шт | `atomic.AddInt64` | Запросов через Model Affinity |
| `QueueStats.dispatch_by_load` | int64 | шт | `atomic.AddInt64` | Запросов через Resource-Aware |
| `QueueStats.dispatch_by_config` | int64 | шт | `atomic.AddInt64` | Запросов через Weight/Config |

---

> **Dispatch-метрики** отражают, через какой механизм балансировки был выбран бэкенд:
> 1. **Model Affinity** — модель уже загружена на бэкенде (этапы 1–3 в `selectBackend`)
> 2. **Resource-Aware** — выбор по свободным ресурсам (GPU, VRAM, CPU) + prediction bonus
> 3. **Config/Weight** — fallback-выбор по весам бэкенда (когда `UseEnhancedScoring=false`)

### 4.1 Dispatch-by-Group Metrics (Model Replication)

Дополнительные метрики, которые появляются при включённой репликации моделей (Variant A):

| Поле | Тип | Единица | Описание |
|------|-----|---------|----------|
| `dispatch_by_group` | int64 | шт | Запросов, направленных через group-based dispatch |
| `replication_group_count` | int | шт | Количество активных групп репликации |
| `replicas_actual` | int | шт | Фактическое количество реплик |
| `replicas_target` | int | шт | Целевое количество реплик (min/max) |
| `replication_reconcile_count` | int64 | шт | Количество выполненных reconcile-циклов |

Group-based dispatch работает поверх основных механизмов (Model Affinity, Resource-Aware). Если модель входит в группу репликации, балансировщик учитывает min/max реплики при выборе бэкенда.

---

## 5. Prediction Metrics (Балансер)

| Поле | Тип | Описание |
|------|-----|----------|
| `secondsToCritical` | float64 | Секунд до критического состояния |
| `criticalReason` | string | Причина: gpu_usage, vram, ram, concurrent_requests, models_capacity, none |
| `gpuUsageTrend` | float64 | Тренд загрузки GPU (%/мин) |
| `vramUsageTrend` | float64 | Тренд VRAM (%/мин) |
| `ramUsageTrend` | float64 | Тренд RAM (%/мин) |
| `freeSlotsTrend` | float64 | Тренд свободных слотов |
| `requestCapacity` | float64 | Загрузка бэкенда (0-100%) |

### 5.1 Использование предиктора в балансировке

**Filtering (5.1):** Бэкенды, у которых прогнозируется критическое состояние **в течение 5 минут** (`0 < secondsToCritical < 300`), **исключаются из пула** для новых запросов. Это касается всех путей выбора бэкенда:
- `selectByResources()` — Resource-Aware выбор
- `findBackendWithModel()` — Model Affinity
- `findBackendWithModelExcluding()` — Model Affinity с исключениями

**Scoring (5.2):** В `calculateScore()` добавляется `predictionBonus`:
- `secondsToCritical < 0` или `>= 600` → **+3.0** (бэкенд стабилен >10 мин)
- `300 <= secondsToCritical < 600` → **+1.5** (стабилен >5 мин)
- `0 < secondsToCritical < 120` → **-5.0** (критическое состояние <2 мин — штраф)

> **Примечание:** Поля `gpuUsageTrend`, `vramUsageTrend`, `ramUsageTrend`, `freeSlotsTrend` вычисляются и хранятся в `BackendState.Prediction`, но **непосредственно не используются** для принятия решения о маршрутизации. Они доступны через WebSocket/WebUI для визуального анализа.

---


## 6. Сводная таблица: Задумано vs Реализовано

| Компонент | Задумано | Реализовано | Gap |
|-----------|----------|-------------|-----|
| **Агент GPU** | Все поля NVML | ✅ Все 9 полей | Нет |
| **Агент CPU** | Per-core, load avg, temp, throttling | ✅ Все поля | Нет |
| **Агент Disk** | Total/Used/Free | ✅ | Нет |
| **Агент Network** | RX/TX | ✅ | Нет |
| **Агент Ollama** | Все поля моделей + details | ✅ family, format, parameterSize, quantization | **Нет** |
| **Агент modelSize** | Сбор size из `/api/tags` | ✅ Size uint64 в RunningModel, ModelSizes map | Нет |
| **Балансер RPS** | Скользящее окно 60с | ✅ | Нет |
| **Балансер Queue** | Stats API | ✅ | Нет |
| **Балансер Prediction** | Filtering + Scoring | ✅ | Нет |
| **Балансер Queue Dispatch** | Счётчики dispatch_by_affinity/load/config | ✅ Реализованы в backend_selector.go | Отображаются в monitor.html Dispatch Stats |
| **WebUI GPU** | usage, VRAM, temp, power | ✅ 7/9 основных | powerLimit, gpuClock, memClock — отображаются в gpuCard и backendsTable |
| **WebUI System** | CPU%, RAM, CPU details | ✅ CPU%, RAM, coreCount, loadAvg 1/5/15, model, temperature, throttling | Disk total/used/free, Network RX/TX — собираются агентом, не отображаются (planned) |
| **WebUI GPU Hidden** | powerLimit, gpuClock, memClock | ✅ Собираются агентом | ✅ Отображаются в gpuCard и backendsTable |
| **WebUI Ollama** | runningModels, activeRequests, RPS, freeSlots | ✅ 4/4 основных | family, format, parameterSize, quantization — собираются, доступны в tooltip |
| **WebUI Queue Dispatch** | dispatch_by_affinity/load/config | ✅ Отображаются в monitor.html | ✅ Счётчики реализованы в backend |

---

## 7. WebUI Функционал

### 7.1 Dashboard
- **GPU Cluster**: карточки каждого бэкенда с GPU метриками (usage, VRAM, temp, power)
- **System Overview**: CPU%, RAM, active requests, RPS, free slots
- **Prediction Alerts**: предупреждения о бэкендах с `secondsToCritical < 300`
- **Real-time**: WebSocket обновления каждые 5 секунд

### 7.2 Бэкенды
- Таблица всех бэкендов с фильтрацией и поиском
- Столбцы: ID, статус, GPU%, VRAM, RAM, CPU%, RPS, active requests, free slots
- CRUD операции: добавление, редактирование, удаление бэкендов
- Подключение/отключение агентов

### 7.3 Модели
- Список запущенных моделей по бэкендам
- Детали: family, format, parameterSize, quantization (в tooltip)
- Поиск по названию модели

### 7.4 Сессии
- Таблица активных сессий с backend affinity
- Поиск по ID сессии

### 7.5 Очередь
- Текущий размер очереди, processed total
- Таймауты и конфигурация workers

### 7.6 Логи
- Журнал событий с фильтрацией по уровню (info, warning, error)
- Экспорт в файл

### 7.7 Настройки
- Просмотр текущей конфигурации балансировщика

---

## 8. Поток данных метрик

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│   Ollama API    │────▶│  Agent Collector │────▶│ Agent HTTP POST │
│  (/api/tags)    │     │  (GPU+System)    │     │ /api/v1/agents  │
└─────────────────┘     └──────────────────┘     │    /metrics     │
                                                 └────────┬────────┘
                                                          │
                              ┌───────────────────────────┘
                              ▼
                    ┌─────────────────┐
                    │  Load Balancer  │
                    │   Proxy + API   │
                    └────────┬────────┘
                             │
              ┌─────────────┼─────────────┐
              ▼             ▼             ▼
        ┌─────────┐   ┌─────────┐   ┌─────────┐
        │ Queue   │   │ Predictor│   │ WS/API  │
        │ Manager │   │          │   │ Handlers│
        └─────────┘   └─────────┘   └────┬────┘
                                         │
                              ┌─────────┴──────────┐
                              ▼                    ▼
                        ┌──────────┐         ┌──────────┐
                        │  Web UI  │         │ REST API │
                        │ Dashboard│         │  Client  │
                        └──────────┘         └──────────┘
```

---

## 9. Алгоритм принятия решения балансером

### 9.1 Выбор бэкенда (`selectBackend`)

```
Pre-step: Model Replication — если модель в группе репликации, выбор из реплик

1. Model Affinity (P1 — LOADED)
   → expandCandidates → бэкенд с моделью в памяти
   → loadRatio < triggerLoadThreshold (default 80%)

2. Model Warming (P2 — WARMING)
   → expandCandidates → бэкенд в процессе загрузки
   → Ожидание готовности (polling) с таймаутом ModelLoadTimeout

3. Sync Model Load (P3 — FREE)
   → Если SyncModelLoad.Enabled
   → Запуск warmupModel() на free бэкенде
   → Ожидание готовности с таймаутом

4. Resource-Aware scoring (P4 — FALLBACK)
   → selectByResources()
   - GPU free * 0.30
   - VRAM free * 0.20
   - CPU free * 0.15
   - modelLoadedBonus, predictionBonus
   - Request penalty (active/max)
   - queueDepthPenalty, errorRatePenalty
   - Weight multiplier
   - Prediction-based filtering: secondsToCritical < 300 → исключаются
```

### 9.2 Лимиты ресурсов (`checkResourceLimits`)

| Ресурс | Лимит | Действие при превышении |
|--------|-------|------------------------|
| GPU Usage | > maxUsagePercent | Исключить из пула |
| VRAM Usage | > maxVramUsagePercent | Исключить из пула |
| CPU Usage | > maxUsagePercent | Исключить из пула |
| RAM Usage | > maxUsagePercent | Исключить из пула |
| Disk Free | < minFreeMB | Исключить из пула |
| Active Requests | >= MaxConcurrentReqs | Исключить из пула |
| Models Count | >= MaxModels | Исключить из пула |

### 9.3 Очередь (`QueueManager`)

```
- maxSize = config.Balancing.QueueMaxSize
- numWorkers = config.Balancing.QueueWorkers
- timeout = config.Balancing.QueueTimeout

Если бэкенд не выбран:
  1. Поставить в очередь
  2. Worker пытается найти бэкенд каждые 100ms
  3. При таймауте → 503 Service Unavailable
```

---

## 10. WebSocket формат

```json
{
  "id": "gpu-1",
  "timestamp": "2024-01-15T10:30:00Z",
  "status": "healthy",
  "hasAgent": true,
  "gpu": {
    "usagePercent": 45.5,
    "memoryTotal": 24576,
    "memoryUsed": 12000,
    "memoryFree": 12576,
    "temperature": 65,
    "powerUsage": 250
  },
  "system": {
    "cpuUsagePercent": 30.2,
    "memoryTotal": 65536,
    "memoryUsed": 20000,
    "memoryFree": 45536
  },
  "ollama": {
    "runningModels": [
      {
        "name": "llama3.1:70b",
        "vramUsage": 18000
      }
    ],
    "activeRequests": 3,
    "requestsPerSecond": 12.5,
    "freeSlots": 7
  },
  "prediction": {
    "secondsToCritical": 300,
    "criticalReason": "vram",
    "requestCapacity": 65.5
  }
}
```

---

## 11. Расширенный мониторинг Ollama

### 11.1 Флаги запуска Ollama (`OllamaRuntimeFlags`)

Агент собирает флаги запуска процесса Ollama из трёх источников (по приоритету):

| Приоритет | Источник | Как собирается |
|-----------|----------|----------------|
| 1 | Аргументы процесса | `ps aux` (Linux/macOS), PowerShell `Get-Process` (Windows) |
| 2 | Переменные окружения | `OLLAMA_NUM_GPU`, `OLLAMA_CONTEXT_LENGTH`, `OLLAMA_NUM_PARALLEL`, `OLLAMA_NUM_THREADS`, `OLLAMA_KV_CACHE_TYPE` |
| 3 | Значения по умолчанию | `numGpuLayers=-1` (auto), `contextLength=2048`, `numParallel=1`, `numThreads=0` (auto), `batchSize=512` |

**Поддерживаемые флаги:**

| Поле | Флаг CLI | Описание |
|------|----------|----------|
| `numGpuLayers` | `-ngl`, `--num-gpu-layers` | Слои на GPU |
| `contextLength` | `-c`, `--ctx-size` | Размер контекста |
| `numParallel` | `-np`, `--parallel` | Параллельных запросов |
| `numThreads` | `-t`, `--threads` | Потоков CPU |
| `batchSize` | `-b`, `--batch-size` | Размер батча |
| `gpuSplitMode` | `--split-mode` | Режим разделения GPU |
| `mainGpu` | `--main-gpu` | Основной GPU |
| `lowVram` | `--low-vram` | Режим Low VRAM |
| `f16kv` | `--no-kv-offload` (отключает) | FP16 для KV cache |
| `kvCacheQuant` | `--cache-type-k` | Квантование KV cache |
| `flashAttention` | `--flash-attn` | Flash Attention |

### 11.2 Контекст моделей (`ModelContextInfo`)

Для каждой загруженной модели агент собирает:

| Поле | Описание |
|------|----------|
| `contextLength` | Размер контекста (токенов) |
| `contextSource` | Источник: `modelfile` / `env` / `runtime` / `default` |
| `effectiveContext` | Фактический контекст (max из модели и флагов) |
| `contextMemoryMB` | Память контекста (batch overhead) |
| `kvCacheMemoryMB` | Память KV cache |
| `modelMemoryMB` | Память самой модели |
| `totalMemoryMB` | Общая память (модель + контекст) |
| `numLayers` | Количество слоёв |
| `hiddenSize` | Размер скрытого слоя |
| `precisionBits` | Точность KV (16 или 32 бита) |

**Приоритет определения контекста:**
1. `runtime` — флаг `-c`/`--ctx-size` (переопределяет всё)
2. `modelfile` — `num_ctx` из `/api/show` → `parameters`
3. `env` — `OLLAMA_CONTEXT_LENGTH`
4. `default` — 2048 токенов

**Формула памяти контекста:**
```
Context Memory = batch_size × effective_context × hidden_size × precision_bits / 8 / 1024²  (MB)
KV Cache Memory = num_layers × 2 (K+V) × hidden_size × effective_context × precision_bits / 8 / 1024²  (MB)
```

Архитектура и размер слоёв определяются из `model_info.general.architecture` от `/api/show`. Fallback-таблицы для `llama`, `qwen2`, `mistral`, `mixtral`, `phi`.

### 11.3 Ёмкость бэкенда (`BackendCapacity`)

Агент оценивает, какие модели можно загрузить на бэкенд:

| Поле | Описание |
|------|----------|
| `freeVram` | Свободно VRAM (MB) |
| `guaranteedVram` | Гарантированно свободно (90% от free) |
| `loadedModelVram` | VRAM уже загруженных моделей |
| `contextOverheadMB` | Суммарная память контекстов |
| `loadableModelCount` | Количество моделей, которые можно загрузить |
| `mode` | `gpu` или `cpu` |

**Алгоритм оценки загружаемости:**
1. `guaranteedVRAM = freeVRAM × 0.9` (10% запас)
2. Для каждой доступной модели из `/api/tags`:
   - `estimatedVRAM = modelSizeMB + contextMemory + kvCacheMemory`
   - `canLoad = estimatedVRAM ≤ guaranteedVRAM`
3. Сортировка по `estimatedVRAM`

Для CPU-агента аналогично по RAM, без GPU layers.

### 11.4 API Endpoints

Новые endpoints:

- `GET /api/v1/backends/{id}/capacity` — детальная ёмкость бэкенда с флагами, контекстами и доступными моделями
- `GET /api/v1/models/capacity` — глобальная сводка по всем бэкендам: `totalLoadable`, `backends[]` с `loadableModelCount`, `runtimeFlags`, `availableModels`

### 11.5 WebUI

Dashboard показывает:
- **Ollama Runtime** — компактные бейджи флагов: `GPU:47 | C:8192 | NP:4 | T:8 | B:512`
- **Context** — рядом с моделью: `Ctx: 8192 (modelfile)`, тултип с деталями памяти
- **Capacity Bar** — прогресс-бар VRAM: загружено / контексты / свободно / гарантировано
- **Available to Load** — список моделей с зелёными/красными индикаторами загружаемости
- **Loadable Count** — бейдж с общим количеством моделей, доступных для загрузки

### 11.6 CPU Mode

При `platformMode = "cpu"`:
- VRAM не собирается, используется RAM
- GPU layers игнорируются
- `canLoad` оценивается по `System.MemoryFree`
- Precision bits остаются 16 (F16KV) или 32

---

*Документ версия 1.1.0 | Обновлено: 2025-04-27*
