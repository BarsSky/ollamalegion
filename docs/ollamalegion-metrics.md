# Полный справочник метрик OllamaLegion

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
| `networkRX` | uint64 | bytes | gopsutil | ❌ | Получено по сети |
| `networkTX` | uint64 | bytes | gopsutil | ❌ | Отправлено по сети |

### 2.1 CPU Details (`SystemMetrics.CPU` / `CPUMetrics`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `usagePercent` | float64 | % | gopsutil | ✅ (дублирует) | Общая загрузка CPU |
| `usagePerCore` | []float64 | % | — | ❌ | Загрузка по ядрам |
| `coreCount` | int | шт | — | ❌ | Количество ядер |
| `threadCount` | int | шт | — | ❌ | Количество потоков |
| `model` | string | — | — | ❌ | Модель CPU |
| `loadAverage1` | float64 | — | — | ❌ | Load avg 1 мин |
| `loadAverage5` | float64 | — | ❌ | Load avg 5 мин |
| `loadAverage15` | float64 | — | ❌ | Load avg 15 мин |
| `temperature` | int | °C | — | ❌ | Температура CPU |
| `throttled` | bool | — | — | ❌ | CPU троттлинг |

> **⚠️ Важно:** Поля `usagePerCore`, `coreCount`, `threadCount`, `model`, `loadAverage*`, `temperature`, `throttled` определены в struct, но агент **не собирает** их (возвращает 0/пусто). Только `usagePercent` заполнен. Это **известный gap** — требует расширения collector.

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
| `freeSlots` | int | шт | **Proxy** | ✅ | Свободные слоты |

> **⚠️ Критически важно:** `activeRequests`, `totalRequests`, `requestsPerSecond`, `freeSlots` — вычисляются **балансером** (proxy-calculated), а не агентом. Агент отправляет `0` для этих полей, балансер переопределяет их точными значениями.

### 3.1 Running Model (`RunningModel`)

| Поле | Тип | Единица | Источник | Показ в WebUI | Описание |
|------|-----|---------|----------|---------------|----------|
| `name` | string | — | Ollama API | ✅ | Название модели |
| `size` | uint64 | bytes | Ollama API | ❌ | Размер модели |
| `vramUsage` | uint64 | MB | эвристика | ✅ | Оценка VRAM |
| `ramUsage` | uint64 | MB | эвристика | ❌ | Оценка RAM (CPU) |
| `expiresAt` | time.Time | — | Ollama API | ❌ | Время выгрузки |
| `digest` | string | — | Ollama API | ❌ | Хеш модели |
| `loadCount` | int | шт | — | ❌ | Количество загрузок |
| `family` | string | — | Ollama API | ❌ | Семейство (llama, mistral...) |
| `format` | string | — | Ollama API | ❌ | Формат (gguf...) |
| `parameterSize` | string | — | Ollama API | ❌ | Размер параметров (7B, 70B...) |
| `quantization` | string | — | Ollama API | ❌ | Квантование (Q4_0...) |

> **Примечание:** Поля `family`, `format`, `parameterSize`, `quantization` заполняются балансером через `fetchModelsFromOllama()` fallback, если агент не прислал. Агент (collector.go) **не парсит** `details` из `/api/tags`.

---

## 4. Proxy-Calculated Metrics (Балансер)

| Поле | Тип | Единица | Алгоритм | Описание |
|------|-----|---------|----------|----------|
| `CalculatedRPS` | float64 | RPS | `len(RequestHistory) / 60.0` | Скользящее окно 60 секунд |
| `ActiveRequests` | int | шт | `ActiveReqs++ / --` | Точный HTTP счётчик |
| `FreeSlots` | int | шт | `MaxConcurrent - ActiveRequests` | Свободные слоты |
| `QueueStats.current_size` | int | шт | `len(queue)` | Текущая очередь |
| `QueueStats.processed_total` | int64 | шт | счётчик | Всего обработано |

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
| **Агент CPU** | Per-core, load avg, temp | ⚠️ Только aggregate % | `coreCount`, `usagePerCore`, `loadAverage*`, `temperature`, `throttled` |
| **Агент Disk** | Total/Used/Free | ✅ | Нет |
| **Агент Network** | RX/TX | ✅ | Нет |
| **Агент Ollama** | Все поля моделей | ⚠️ Без `details` | `family`, `format`, `parameterSize`, `quantization` |
| **Балансер RPS** | Скользящее окно 60с | ✅ | Нет |
| **Балансер Queue** | Stats API | ✅ | Нет |
| **Балансер Prediction** | Filtering + Scoring | ✅ | Нет |
| **WebUI GPU** | Все поля | ⚠️ Без powerLimit, clocks | 3 поля не показаны |
| **WebUI System** | Все поля | ⚠️ Только CPU%, RAM | Disk, Network, CPU details не показаны |
| **WebUI Ollama** | Все поля | ⚠️ Без availableModels, details | `family`, `format`, `parameterSize`, `quantization` |

---

## 7. Рекомендации по заполнению gaps

### 7.1 Агент: CPU детализация
Добавить в `internal/agent/collector.go`:
```go
// Сбор per-core CPU stats через gopsutil/cpu.Percent(percpu=true)
// Сбор load average через gopsutil/load.Avg()
// Сбор CPU info через gopsutil/cpu.Info() (model, cores, threads)
```

### 7.2 Агент: Ollama model details
Добавить парсинг `details` из `/api/tags` ответа:
```go
for _, tag := range tagsResp.Models {
    model.Family = tag.Details.Family
    model.Format = tag.Details.Format
    model.ParameterSize = tag.Details.ParameterSize
    model.Quantization = tag.Details.Quantization
}
```

### 7.3 WebUI: Дополнительные поля
Добавить отображение:
- GPU: `powerLimit`, `gpuClock`, `memClock`
- System: `diskUsed`/`diskFree`, `networkRX`/`networkTX`
- CPU: `coreCount`, `loadAverage1`
- Models: `family`, `parameterSize`, `quantization`

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
1. Session Stickiness → тот же бэкенд, если здоров
2. Model Affinity → бэкенд с загруженной моделью
3. Resource-Aware scoring:
   - GPU free * 0.35
   - VRAM free * 0.25  
   - CPU free * 0.20
   - Request penalty (active/max)
   - Model capacity score
   - Prediction bonus/penalty (5.2)
4. Weight multiplier
5. Prediction-based filtering (5.1): бэкенды с secondsToCritical < 300 исключаются
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

*Документ версия 1.0.0 | Обновлено: 2025-04-25*