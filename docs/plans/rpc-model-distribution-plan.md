# План распределения моделей по нескольким машинам (RPC)

## Дата: 2026-05-13
## Статус: Активный план

---

# ЧАСТЬ 1: Вариант C — Virtual Model Router

## 1.1 Обзор

Virtual Model Router создаёт абстракцию VirtualModel, которая маппится на несколько физических моделей (срезов/слайсов) на разных бэкендах. Каждый срез — это полная Ollama-модель, специализированная на определённом этапе обработки.

```
Client → Balancer
          ├─ VirtualModel: "llama-mega"
          │   ├─ Slice 0 (embed) → Backend-1 (Ollama)
          │   ├─ Slice 1 (middle) → Backend-2 (Ollama)
          │   └─ Slice 2 (output) → Backend-3 (Ollama)
          └─ Selects best slice → proxy to backend
```

## 1.2 Текущее состояние реализации

### Реализовано

| Файл | Компонент | Статус |
|------|-----------|--------|
| `internal/virtualmodel/virtual_model.go` | VirtualModel, SliceExecutionContext, ExecutePipeline | ✅ Sequential pipeline |
| `internal/virtualmodel/registry.go` | Registry (CRUD виртуальных моделей) | ✅ |
| `internal/virtualmodel/router.go` | Router (маршрутизация запросов) | ✅ Basic |
| `internal/virtualmodel/virtualmodel_test.go` | 31 тест (1009 строк) | ✅ ALL PASS |

### Архитектура выполнения (sequential)

1. Запрос приходит на VirtualModel
2. Срезы сортируются по `Ordinal`
3. Каждый срез выполняет HTTP POST `/api/generate` на свой backend
4. Output среза N-1 передаётся как input срезу N
5. Финальный ответ возвращается клиенту

### Типы конфигурации (`pkg/types/types.go:617-639`)

```go
type VirtualModelConfig struct {
    Name         string
    Description  string
    Slices       []ModelSliceConfig
    Coordination CoordinationConfig
}

type ModelSliceConfig struct {
    ID             string
    ModelName      string   // Ollama model name
    Ordinal        int
    TargetBackends []string
    FallbackMode   string   // "retry" | "skip" | "abort"
}

type CoordinationConfig struct {
    Mode         string // "sequential" | "parallel" | "tree"
    TimeoutMs    int
    SyncStrategy string // "http-callback" | "direct-response"
}
```

## 1.3 Что требуется для полной реализации

### Фаза C1: Parallel Pipeline Mode (P1)
**Цель:** Реализовать parallel и tree режимы координации (сейчас только sequential).

**Файл:** `internal/virtualmodel/pipeline_parallel.go`

**Механизм:**
- **Parallel:** Все срезы одного уровня выполняются одновременно (горутины + WaitGroup)
- **Tree:** Иерархическое разбиение: embedding параллельно с нескольких бэкендов, затем merge, затем middle layers

```go
type PipelineExecutor interface {
    Execute(ctx context.Context, vm *VirtualModel, input []byte) ([]byte, error)
}

type SequentialExecutor struct{}
type ParallelExecutor struct{}
type TreeExecutor struct{}
```

**Критерии приёмки:**
- [ ] Parallel: 3 среза выполняются одновременно, результат merge'ится
- [ ] Tree: embedding на 2 backend'ах параллельно → merge → middle layers
- [ ] Тест: `TestExecutePipeline_Parallel`, `TestExecutePipeline_Tree`

**Оценка:** 3–4 дня

---

### Фаза C2: Streaming через Pipeline (P1)
**Цель:** Поддержка streaming-запросов (`"stream": true`).

**Файл:** `internal/virtualmodel/streaming_pipeline.go`

**Проблема:** Сейчас `callSliceBackend` использует `io.ReadAll`, блокируя streaming.

**Решение:**
1. Первый срез отправляет запрос с `stream: true`
2. Получает SSE-поток от Ollama
3. Каждый chunk перенаправляется следующему срезу как input
4. Финальный срез передаёт chunks клиенту через SSE

```go
func (vm *VirtualModel) ExecutePipelineStreaming(
    w http.ResponseWriter,
    input []byte,
    params map[string]string,
) error
```

**Критерии приёмки:**
- [ ] Streaming-запрос через 2 среза возвращает токены клиенту
- [ ] Задержка между токенами < 200ms
- [ ] При ошибке одного среза — graceful shutdown SSE с error chunk
- [ ] Тест: `TestExecutePipelineStreaming_TwoSlices`

**Оценка:** 4–5 дней  
**Зависит от:** Фазы C1 (parallel mode для parallel streaming)

---

### Фаза C3: Retry & Fallback (P1)
**Цель:** Улучшенная отказоустойчивость при ошибках срезов.

**Файл:** `internal/virtualmodel/resilience.go`

**Механизмы:**
- **Retry с backoff:** при ошибке HTTP запроса — повтор на том же backend'е
- **Backend failover:** при недоступности backend'а — retry на другом из `TargetBackends`
- **Skip mode:** если `FallbackMode = "skip"` и срез упал — пропустить, передать input следующему
- **Abort mode:** если `FallbackMode = "abort"` — вернуть ошибку клиенту

```go
type ResilienceConfig struct {
    MaxRetries      int
    RetryBackoff    time.Duration
    FailoverEnabled bool
    CircuitBreaker  CircuitBreakerConfig
}
```

**Критерии приёмки:**
- [ ] Backend падает → автоматический failover на другой backend из TargetBackends
- [ ] 3 retry с exponential backoff при 500-ошибке
- [ ] Circuit breaker: после 5 ошибок backend исключается на 30 секунд
- [ ] Тест: `TestResilience_Failover`, `TestResilience_CircuitBreaker`

**Оценка:** 3–4 дня

---

### Фаза C4: Balancer Integration (P2)
**Цель:** Глубокая интеграция VirtualModel Router в основной proxy flow.

**Файл:** `internal/balancer/proxy.go` — доработка ServeHTTP

**Текущее состояние:** Router вызывается отдельно, но не интегрирован в `ServeHTTP`.

**Нужно:**
```go
// В ServeHTTP, после определения modelName:
if p.virtualModelRouter != nil {
    result, handled, err := p.virtualModelRouter.Route(modelName, body, params)
    if handled && err == nil {
        // Отправляем result клиенту
        w.Header().Set("Content-Type", "application/json")
        w.Write(result)
        return
    }
    // Если handled && err != nil — вернуть ошибку
    // Если !handled — продолжаем обычный flow
}
```

**Критерии приёмки:**
- [ ] Запрос на VirtualModel обрабатывается через pipeline, а не как обычная модель
- [ ] Метрики VirtualModel отображаются в мониторе
- [ ] Session stickiness работает с VirtualModel (requestID через pipeline)

**Оценка:** 2–3 дня

---

### Фаза C5: WebUI & Monitoring (P2)
**Цель:** Визуализация VirtualModel pipeline в мониторе.

**Файлы:** `webui/js/modules/virtual-model.js`, `webui/virtual-models.html`

**Функционал:**
- Список виртуальных моделей с срезами
- Pipeline визуализация: клиент → slice 1 → slice 2 → slice 3
- Цветовая индикация статуса каждого среза
- Задержка (latency) по каждому срезу
- Активные запросы в pipeline

**Критерии приёмки:**
- [ ] WebUI отображает все VirtualModels из Registry
- [ ] Pipeline показывает текущий активный запрос
- [ ] При ошибке среза — красный индикатор с деталями

**Оценка:** 3–5 дней

---

### Фаза C6: Configuration & API (P2)
**Цель:** REST API для управления VirtualModels.

**Файлы:** `internal/api/handlers_virtual.go`

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `/api/v1/virtual-models` | GET | Список |
| `/api/v1/virtual-models` | POST | Создать |
| `/api/v1/virtual-models/:name` | GET | Статус |
| `/api/v1/virtual-models/:name` | DELETE | Удалить |
| `/api/v1/virtual-models/:name/infer` | POST | Inference |

**Оценка:** 2–3 дня

---

### Фаза C7: Тесты E2E (P3)
**Цель:** End-to-end тесты для полного pipeline.

**Файлы:** `tests/virtual_model_e2e_test.go`

**Сценарии:**
1. Создать VirtualModel с 2 срезами → inference → проверить результат
2. Упасть backend slice 1 → failover на slice 1 backup → inference успешен
3. Streaming через 3 среза → проверить chunks
4. Concurrent requests (10 parallel) → все успешны
5. Memory leak test: 1000 запросов → нет утечек

**Оценка:** 3–4 дня

---

## 1.4 Сводная таблица Варианта C

| Фаза | Задача | Приоритет | Срок | Зависимости | Статус |
|------|--------|-----------|------|-------------|--------|
| C1 | Parallel Pipeline Mode | P1 | 3–4 дня | — | ⬜ |
| C2 | Streaming через Pipeline | P1 | 4–5 дней | C1 | ⬜ |
| C3 | Retry & Fallback | P1 | 3–4 дня | — | ⬜ |
| C4 | Balancer Integration | P2 | 2–3 дня | — | ⬜ |
| C5 | WebUI & Monitoring | P2 | 3–5 дней | C4 | ⬜ |
| C6 | Configuration & API | P2 | 2–3 дня | — | ⬜ |
| C7 | E2E Tests | P3 | 3–4 дня | C1–C3 | ⬜ |

**Итого Вариант C:** 20–28 дней (~4–5 недель)

---

---

# ЧАСТЬ 2: Вариант B — External RPC Coordinator

## 2.1 Обзор

External RPC Coordinator — распределённый inference engine. Каждый worker обслуживает срез модели (набор слоёв трансформера). Координатор управляет pipeline, split/merge, KV cache.

```
Client → Balancer → ModelCoordinator
                      ├── Split Logic
                      ├── Pipeline Manager
                      ├── Merge Logic
                      └── KV Cache Manager
                            ↓
              Worker-1 (слои 1-40) ←→ Worker-2 (слои 41-80)
```

## 2.2 Текущее состояние реализации

### Реализовано (Skeleton)

| Файл | Компонент | Строки | Тесты | Статус |
|------|-----------|--------|-------|--------|
| `internal/rpccoordinator/coordinator.go` | ModelCoordinator, DistributedModel, LayerSlice, InferenceJob, pipeline execution | 560 | 25 | ✅ |
| `internal/rpccoordinator/worker_client.go` | WorkerClient (HTTP): health, load, unload, infer, metrics | 200 | — | ✅ |
| `internal/rpccoordinator/splitmerge.go` | Splitter, Merger, ParallelMergeContext | 250 | 20 | ✅ |
| `internal/rpccoordinator/kv_cache.go` | DistributedKVCache, KVCacheShard, LRU, TTL | 290 | 19 | ✅ |
| `internal/balancer/rpc_modules.go` | Интеграция: initRpcModules(), GetRpcCoordinator(), InferDistributed() | 150 | — | ✅ |
| `*_test.go` (3 файла) | 66 тестов, ALL PASS | 870 | 66 | ✅ |

### Интеграция в Balancer

```go
// internal/balancer/proxy.go
rpcCoordinator *rpccoordinator.ModelCoordinator

// internal/balancer/rpc_modules.go
if cfg.RpcCoordinator.Enabled {
    p.rpcCoordinator = rpccoordinator.NewModelCoordinator(cfg.RpcCoordinator)
}
```

### Конфигурация (`config/config.example.json`)

```json
"rpcCoordinator": {
  "enabled": false,
  "coordinatorURL": "http://localhost:18090",
  "workerPort": 18080,
  "timeout": "30s",
  "protocol": "http",
  "maxRetries": 3
}
```

## 2.3 Что требуется для полной реализации

### Фаза B1: Worker HTTP Server (P0) — Критический путь
**Цель:** Минимальный viable product для pipeline inference.

**Новые файлы:**
- `cmd/rpcworker/main.go` — точка входа
- `internal/rpcworker/server.go` — HTTP сервер
- `internal/rpcworker/inference.go` — логика inference среза
- `internal/rpcworker/model_manager.go` — загрузка/выгрузка

**API Worker'а:**
```
POST /rpc/infer       ← SliceInferRequest → SliceInferResponse
POST /rpc/load        ← {model_name, layers} → {status}
POST /rpc/unload      ← {model_name} → {status}
GET  /rpc/health      → {status, loaded_slices, gpu_info}
POST /rpc/kv_sync     ← KVCacheShard → {status}
GET  /rpc/kv_fetch    → {seq_len, key_tensor, value_tensor}
GET  /rpc/metrics     → {gpu_usage, vram_usage, active_requests}
```

**Проблема:** Ollama не поддерживает частичную загрузку модели.
**Решение (interim):** Использовать `OLLAMA_NUM_GPU_LAYERS` для ограничения слоёв. Каждый worker запускает полную модель, но с разным количеством слоёв в VRAM.

**Критерии приёмки:**
- [ ] Worker отвечает на `/rpc/health` за < 100ms
- [ ] `/rpc/load` загружает модель за < 30 секунд
- [ ] `/rpc/infer` обрабатывает запрос, возвращает валидный JSON
- [ ] Pipeline из 2 worker'ов проходит end-to-end тест
- [ ] При падении worker'а — pipeline возвращает ошибку после 3 retry

**Оценка:** 10–14 дней  
**Блокирует:** Фазы B2–B7

---

### Фаза B2: Management API (P1)
**Цель:** REST API для управления distributed моделями.

**Файлы:** `internal/api/handlers_rpc.go`

| Endpoint | Метод | Описание |
|----------|-------|----------|
| `/api/v1/rpc/workers` | GET/POST | Список / регистрация |
| `/api/v1/rpc/workers/:id` | DELETE | Удаление |
| `/api/v1/rpc/models` | GET/POST | Список / создание |
| `/api/v1/rpc/models/:name/load` | POST | Загрузка на worker'ы |
| `/api/v1/rpc/models/:name/infer` | POST | Inference |
| `/api/v1/rpc/jobs/:id` | GET/CANCEL | Статус / отмена |

**Оценка:** 3–5 дней  
**Зависит от:** B1

---

### Фаза B3: Worker Heartbeat & Auto-Discovery (P1)
**Цель:** Автоматическое обнаружение и мониторинг worker'ов.

**Файлы:** `internal/rpccoordinator/worker_registry.go`, `internal/rpcworker/heartbeat.go`

- Heartbeat каждые 30 секунд
- `lastSeen` трекинг, исключение при > 90 секунд
- Auto-discovery через UDP multicast (опционально)

**Оценка:** 2–3 дня  
**Зависит от:** B1, B2

---

### Фаза B4: Streaming Pipeline (P2)
**Цель:** Streaming-ответы через pipeline срезов.

**Файл:** `internal/rpccoordinator/streaming.go`

- Chunked transfer между срезами
- SSE от финального среза к клиенту

**Оценка:** 5–7 дней  
**Зависит от:** B1

---

### Фаза B5: gRPC Protocol (P2)
**Цель:** Binary protobuf вместо HTTP JSON.

**Файлы:** `pkg/protocol/rpc.proto`, `grpc_client.go`, `grpc_server.go`

- Bidirectional streaming для KV cache sync
- HTTP/2 multiplexing

**Оценка:** 5–7 дней  
**Зависит от:** B1

---

### Фаза B6: Load Balancing между срезами (P2)
**Цель:** Выбор наименее загруженного worker'а для среза.

**Файл:** `internal/rpccoordinator/selector.go`

- При нескольких TargetBackends — выбор по load ratio
- Failover на secondary при отказе primary

**Оценка:** 1–2 дня  
**Зависит от:** B1, B3

---

### Фаза B7: Prometheus Metrics + WebUI (P3)
**Цель:** Наблюдаемость за distributed inference.

**Метрики:**
```
rpc_inference_duration_ms — histogram
rpc_slice_latency_ms — histogram по worker'ам
rpc_worker_health — gauge
rpc_kv_cache_size_bytes — gauge
rpc_active_jobs — gauge
```

**WebUI:** Pipeline визуализация, цветовая индикация, timeline.

**Оценка:** 3–5 дней  
**Зависит от:** B1, B4

---

### Фаза B8: Tensor Parallelism (P3)
**Цель:** Разбиение матриц внутри слоя между worker'ами (vs pipeline).

**Разница:**
- **Pipeline** (сейчас): каждый worker свои слои, latency O(N)
- **Tensor parallelism**: матрицы разбиваются, все работают параллельно, latency O(1)

**Требует:** Custom inference engine (llama.cpp bindings)

**Оценка:** 20–30 дней  
**Зависит от:** Custom backend

---

## 2.4 Сводная таблица Варианта B

| Фаза | Задача | Приоритет | Срок | Зависимости | Статус |
|------|--------|-----------|------|-------------|--------|
| B1 | Worker HTTP Server | **P0** | 10–14 дней | — | ⬜ |
| B2 | Management API | **P1** | 3–5 дней | B1 | ⬜ |
| B3 | Heartbeat & Discovery | **P1** | 2–3 дня | B1, B2 | ⬜ |
| B4 | Streaming Pipeline | **P2** | 5–7 дней | B1 | ⬜ |
| B5 | gRPC Protocol | **P2** | 5–7 дней | B1 | ⬜ |
| B6 | Load Balancing срезов | **P2** | 1–2 дня | B1, B3 | ⬜ |
| B7 | Metrics + WebUI | **P3** | 3–5 дней | B1, B4 | ⬜ |
| B8 | Tensor Parallelism | **P3** | 20–30 дней | Custom backend | ⬜ |

**Итого Вариант B:** 49–63 дня (~10–12 недель)

---

# ЧАСТЬ 3: Сравнение и рекомендации

## Сводная таблица

| Критерий | Вариант C (Virtual Model) | Вариант B (RPC Coordinator) |
|----------|---------------------------|----------------------------|
| **Сложность** | 🟡 Средняя | 🔴 Высокая |
| **Время** | 4–5 недель | 10–12 недель |
| **Требует custom backend** | ❌ Нет (использует Ollama) | ⚠️ Частично |
| **Streaming** | 🟡 Medium | 🔴 Сложно |
| **Отказоустойчивость** | ✅ Retry + failover | ✅ Retry + failover |
| **Масштабируемость** | 🟡 До ~5 срезов | ✅ До N worker'ов |
| **Память** | Каждый срез = полная модель | Каждый worker = часть модели |
| **Latency** | O(N) сетевых вызовов | O(N) + overhead KV sync |

## Рекомендация

**Short-term (1–2 месяца):** Вариант C — Virtual Model Router
- Быстрее реализуется, использует стандартный Ollama
- Pipeline parallelism естественно вписывается в HTTP
- Можно тестировать в изоляции

**Medium-term (3–6 месяцев):** Вариант B — External RPC Coordinator
- Требуется worker backend (P0 — критический)
- После стабилизации worker'а — добавить streaming, gRPC
- Tensor parallelism — только при наличии ресурсов

**Long-term (6+ месяцев):** Tensor Parallelism
- Требует custom inference engine
- Максимальная производительность для giant-моделей