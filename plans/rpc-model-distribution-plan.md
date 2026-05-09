# План: Распределение одной модели по нескольким машинам (RPC)

## Дата: 2026-05-08
## Статус: Черновик для обсуждения

---

## 1. Контекст и мотивация

Текущая архитектура OllamaLegion предполагает, что **каждая модель целиком загружается на один бэкенд**. Балансировщик выбирает бэкенд, на котором модель уже загружена (Model Affinity) или может быть загружена (Warmup/Sync Load).

**Проблемы текущего подхода:**
- Большие модели (70B+, 405B) не помещаются на одну GPU
- Утилизация кластера неравномерна: одни GPU простаивают, другие перегружены
- Невозможно использовать совокупную VRAM нескольких машин для одной модели
- Выход из строя одной машины с уникальной моделью делает модель недоступной

**Цель:** Реализовать механизм, позволяющий распределять _одну_ модель по _нескольким_ машинам, используя RPC-коммуникацию между узлами.

---

## 2. Анализ архитектурных ограничений

| Ограничение | Описание |
|-------------|----------|
| **Ollama не поддерживает model parallelism** | Ollama загружает модель целиком на одну машину. Нет нативного API для шардирования. |
| **HTTP-based транспорт** | Взаимодействие между компонентами — HTTP/REST. Нет gRPC. |
| **Текущий proxy — агрегатор, не координатор** | Прокси передаёт запрос одному бэкенду. Нет логики split/merge. |
| **Отсутствие общего состояния между бэкендами** | KV cache, скрытые состояния (hidden states) не синхронизируются. |

---

## 3. Варианты реализации

### Вариант A: Model Replication Manager (Эволюционный)

**Описание:** Не true RPC-шардирование, а интеллектуальная репликация модели на multiple backends с единым управлением. Модель загружается на N машин, балансировщик распределяет запросы между ними.

```
Client → Balancer (select least-loaded replica) → Backend-1 (model loaded)
                                                → Backend-2 (model loaded)
                                                → Backend-N (model loaded)
```

**Что нужно сделать:**
- [ ] Добавить понятие **ModelInstanceGroup** — группа бэкендов, на которых должна быть загружена одна и та же модель
- [ ] Расширить Prewarm Controller: при загрузке модели на один бэкенд, инициировать загрузку на все бэкенды группы
- [ ] `minInstances`/`maxInstances` для модели — минимальное/максимальное количество реплик
- [ ] `ModelInstanceController` (частично реализован): поддерживать актуальное количество реплик
- [ ] `DispatchGroupAwareSelector`: при выборе бэкенда учитывать не только loadRatio, но и принадлежность к группе

**Конфигурация:**
```json
{
  "modelGroups": {
    "llama3.1:70b": {
      "minInstances": 2,
      "maxInstances": 4,
      "targetBackends": ["gpu-1", "gpu-2", "gpu-3"],
      "idleUnloadAfter": "15m"
    }
  }
}
```

**Плюсы:**
- ✅ Минимальные изменения в архитектуре
- ✅ Использует существующие механизмы (warmup, scoring, queue dispatch)
- ✅ Отказоустойчивость: при падении одной реплики — запросы идут к другой
- ✅ Постепенное внедрение, не ломает обратную совместимость

**Минусы:**
- ❌ Не решает проблему моделей, не помещающихся на одну GPU
- ❌ Требует больше VRAM суммарно (каждая реплика — полная копия)
- ❌ Нет синхронизации KV cache между репликами (сессии не переносятся)

**Оценка сложности:** 5–7 дней  
**Затраты:** Средние  
**Наибольший эффект:** Для кластеров с несколькими GPU одного типоразмера

---

### Вариант B: External RPC Coordinator (Интеграционный)

**Описание:** Создать отдельный микросервис **ModelCoordinator**, который управляет распределённым выполнением инференса. Каждый бэкенд запускает **ModelWorker** — легковесный процесс, который управляет "срезом" модели. Координатор принимает запросы от балансировщика, разбивает их на подзапросы (split), отправляет worker'ам, агрегирует ответы (merge).

```
Client → Balancer → ModelCoordinator → Worker-1 (слой 1-40)
                                       → Worker-2 (слой 41-80)
                                       → Worker-N (слой N-M)
                   Balancer ← ModelCoordinator (aggregated response)
```

**Компоненты:**
- **ModelCoordinator** (новый микросервис, Go или Python):
  - REST/gRPC API для приёма запросов от балансировщика
  - Split логика: разбивает prompt/tokens на сегменты
  - Pipeline management: последовательный/параллельный вызов worker'ов
  - Merge логика: собирает output token'ов со всех worker'ов
  - KV cache management: координирует распределённую KV cache

- **ModelWorker** (новый компонент, запускается рядом с Ollama):
  - Управляет конкретным срезом модели (слои X-Y)
  - HTTP API для получения запросов от координатора
  - Использует Ollama API для инференса своего среза
  - Отправляет partial результаты координатору

- **Balancer integration**:
  - Новый тип бэкенда "distributed" с ссылкой на координатор
  - `selectDistributedBackend()` в `selectBackend()`
  - Мониторинг состояния координатора

**Плюсы:**
- ✅ Решает проблему моделей, не помещающихся на одну GPU
- ✅ Масштабируемость: можно добавлять worker'ы горизонтально
- ✅ Гибкость: координатор можно реализовать на любом стеке
- ✅ Независимость от Ollama version

**Минусы:**
- ❌ Чрезвычайно сложная реализация (split/merge трансформеров — нетривиальная задача)
- ❌ Латенси: сетевое взаимодействие между worker'ами добавляет задержки
- ❌ Ollama не поддерживает частичную загрузку модели → нужен кастомный бэкенд
- ❌ Фактически требуется написать распределённый inference engine
- ❌ Нужна синхронизация: каждый слой зависит от выхода предыдущего

**Оценка сложности:** 3–6 месяцев  
**Затраты:** Очень высокие  
**Наибольший эффект:** Для giant-моделей (70B+, 405B) на кластере маломощных GPU

---

### Вариант C: Virtual Model Router с Model Slice Mapping (Гибридный)

**Описание:** Не шардировать модель "по-настоящему", а создать абстракцию **VirtualModel**, которая маппится на несколько физических моделей на разных бэкендах. Каждая физическая модель — это "срез" или адаптированная версия (например, LoRA-адаптер, quantized split).

```
Client → Balancer
         ├─ VirtualModel: "llama-mega"
         │   ├─ Slice 1: "llama-mega-embed" → Backend-1 (Ollama)
         │   ├─ Slice 2: "llama-mega-layers" → Backend-2 (Ollama)
         │   └─ Slice 3: "llama-mega-output" → Backend-3 (Ollama)
         │
         └─ Selects best slice → proxy to that backend
```

**Конфигурация:**
```json
{
  "virtualModels": {
    "llama-mega": {
      "description": "Llama 3.1 405B distributed across 3 GPUs",
      "slices": [
        {
          "id": "embed",
          "modelName": "llama3.1:405b-layers-1-40",
          "targetBackends": ["gpu-1", "gpu-2"],
          "ordinal": 0
        },
        {
          "id": "middle",
          "modelName": "llama3.1:405b-layers-41-80",
          "targetBackends": ["gpu-3", "gpu-4"],
          "ordinal": 1
        },
        {
          "id": "output",
          "modelName": "llama3.1:405b-layers-81-120",
          "targetBackends": ["gpu-5", "gpu-6"],
          "ordinal": 2
        }
      ],
      "coordination": {
        "mode": "sequential",
        "timeoutMs": 30000,
        "syncStrategy": "http-callback"
      }
    }
  }
}
```

**Архитектура:**
```
internal/balancer/
├── virtual_model.go          # VirtualModel definition, state
├── virtual_model_registry.go # Registry of virtual models
├── virtual_model_router.go   # Route requests through slices
├── slice_coordinator.go      # Coordinate slice execution
├── slice_aggregator.go       # Merge responses from slices
└── rpc_client.go             # HTTP client for slice-to-slice communication
```

**Механизм работы:**
1. Запрос приходит к балансировщику на модель `llama-mega`
2. `VirtualModelRouter` определяет, что это VirtualModel
3. Создаётся `SliceExecutionContext` с ID запроса
4. Запрос проходит по slices последовательно (pipeline):
   - Slice 0 (embedding) → ответ → передаётся в Slice 1
   - Slice 1 (middle layers) → ответ → передаётся в Slice 2
   - Slice 2 (output) → финальный ответ → клиенту
5. `SliceAggregator` собирает результаты
6. При ошибке одного slice — retry на другой бэкенд из `targetBackends`

**Плюсы:**
- ✅ Решает проблему больших моделей
- ✅ Использует стандартный Ollama на каждом узле
- ✅ Pipeline parallelism естественно вписывается в HTTP модель
- ✅ Отказоустойчивость: несколько бэкендов на slice
- ✅ Можно делать различные топологии (sequential, parallel, tree)

**Минусы:**
- ❌ Требуется "разрезать" модель на отдельные Ollama-модули (кастомный скрипт)
- ❌ Латенси: последовательный pipeline (O(N) сетевых вызовов)
- ❌ Сложность: нужно реализовать split/merge для разных типов запросов (generate, chat, embeddings)
- ❌ Синхронизация состояния между slices для streaming-запросов

**Оценка сложности:** 4–8 недель  
**Затраты:** Высокие  
**Наибольший эффект:** Для кластеров с сегментированными GPU (разные типы/объёмы VRAM)

---

### Вариант D: Distributed Inference через Custom Backend (Go-native workers)

**Описание:** Полностью кастомная реализация распределённого инференса на Go. Вместо использования Ollama как чёрного ящика, создаётся легковесный CGo-модуль (LLama.cpp binding), который работает как ModelSlice и может общаться с другими slice'ами по RPC (gRPC).

**Архитектура:**
```
                    ┌──────────────────────┐
                    │   Balancer            │
                    │   ┌────────────────┐  │
                    │   │ DistInference   │  │
                    │   │ Engine          │  │
                    │   └────┬─────┬──────┘  │
                    └────────┼─────┼─────────┘
                             │     │
              ┌──────────────┘     └──────────────┐
              ▼                                     ▼
    ┌──────────────────┐                 ┌──────────────────┐
    │ Worker-1 (Go)     │                 │ Worker-N (Go)     │
    │ + LLama.cpp       │◄─── gRPC ─────►│ + LLama.cpp       │
    │ Слои 1-40         │                 │ Слои 41-80        │
    │ KV cache shard    │                 │ KV cache shard    │
    └──────────────────┘                 └──────────────────┘
```

**Плюсы:**
- ✅ Максимальная производительность (Go + CGo + gRPC)
- ✅ Полный контроль над распределением слоёв
- ✅ Эффективная синхронизация KV cache
- ✅ Можно реализовать Tensor Parallelism (разделение матриц)

**Минусы:**
- ❌ Чрезвычайно сложно и рискованно
- ❌ LLama.cpp CGo binding — отдельная большая задача
- ❌ Ломает "прозрачность" Ollama
- ❌ Требует глубокого понимания архитектуры трансформеров
- ❌ Должен поддерживаться параллельно с основным Ollama-прокси

**Оценка сложности:** 6+ месяцев  
**Затраты:** Экстремально высокие  
**Наибольший эффект:** Продуктовое решение для high-load distributed inference

---

## 4. Рекомендуемый подход

### Short-term (1–2 недели): Вариант A — Model Replication Manager

Наиболее прагматичный вариант для текущей архитектуры. Реализует отказоустойчивую репликацию модели по нескольким бэкендам без изменения модели инференса.

### Medium-term (4–8 недель): Вариант C — Virtual Model Router

После стабилизации репликации, реализовать VirtualModel с pipeline parallelism. Это даст возможность распределять одну модель по машинам в cases, когда она не помещается на одну GPU.

### Long-term (6+ месяцев): Вариант D — Custom Distributed Backend

Только если проект перерастёт в полноценный distributed inference engine.

---

## 5. Детальный план реализации (Short-term: Вариант A)

### Фаза A1: ModelInstanceGroup (2 дня)

**Файлы:**
- `internal/balancer/model_instance_group.go` — новая структура
- `internal/balancer/proxy.go` — интеграция
- `pkg/types/types.go` — новые типы

**Что делаем:**
```go
// model_instance_group.go
type ModelInstanceGroup struct {
    ModelName      string   `json:"modelName"`
    MinInstances   int      `json:"minInstances"`
    MaxInstances   int      `json:"maxInstances"`
    TargetBackends []string `json:"targetBackends"` // если пусто — все healthy
    IdleUnloadAfter Duration `json:"idleUnloadAfter"`
    
    // Runtime state
    mu         sync.RWMutex
    instances  map[string]*InstanceState // backendID → state
}

type InstanceState struct {
    BackendID  string
    Status     ModelLoadStatus  // loading, loaded, unloading, idle
    LoadedAt   time.Time
    LastUsedAt time.Time
    UseCount   int64
}
```

**API:**
- `GET /api/v1/models/groups` — список групп
- `POST /api/v1/models/groups` — создать/обновить группу
- `DELETE /api/v1/models/groups/:name` — удалить группу

### Фаза A2: InstanceController (2 дня)

**Файлы:**
- `internal/balancer/model_instance_controller.go` — доработка существующего

**Логика:**
- Фоновый цикл (ticker 10s):
  1. Для каждой ModelInstanceGroup проверяем количество loaded instances
  2. Если loaded < minInstances → запускаем warmup на свободных бэкендах
  3. Если loaded > maxInstances → ищем кандидатов на unload (LRU)
  4. Обновляем `InstanceState` метрики

- Приоритет: сначала на бэкендах с уже загруженной моделью (reuse), потом на свободных
- Учёт VRAM: не загружать если не хватает памяти

### Фаза A3: Dispatch с учётом групп (1 день)

**Файлы:**
- `internal/balancer/backend_selector.go` — доработка `selectBackend()`
- `internal/balancer/candidate.go` — расширение `expandCandidates()`

**Логика:**
- При выборе бэкенда для модели, которая принадлежит группе:
  1. Сначала пытаемся найти нагруженный бэкенд из группы (loadRatio < threshold)
  2. Если все перегружены → запускаем warmup на новом бэкенде из группы
  3. Если группа заполнена (maxInstances) → выбираем наименее загруженный из группы
  4. Fallback: любой healthy бэкенд

### Фаза A4: Конфигурация и API (1 день)

**Конфигурация:**
```json
{
  "modelGroups": {
    "llama3.1:70b": {
      "minInstances": 2,
      "maxInstances": 4,
      "idleUnloadAfter": "15m",
      "targetBackends": ["gpu-1", "gpu-2", "gpu-3"]
    }
  }
}
```

**API endpoints:**
- `GET /api/v1/models/groups/:name` — статус группы
- `POST /api/v1/models/groups/:name/scale` — ручное масштабирование
- `GET /api/v1/models/groups/:name/instances` — список инстансов

### Фаза A5: Тестирование (1–2 дня)

- Unit-тесты для `ModelInstanceGroup`, `InstanceController`
- Интеграционный тест: 3 бэкенда, модель с minInstances=2
- Edge cases: падение бэкенда, добавление нового бэкенда, переполнение VRAM
- Load test: 2 модели, 5 бэкендов, min/max instances

---

## 6. Детальный план Medium-term (Вариант C — Virtual Model Router)

### Фаза C1: VirtualModel (1 неделя)

**Новые файлы:**
- `internal/balancer/virtual_model.go`
- `internal/balancer/virtual_model_registry.go`
- `internal/balancer/virtual_model_router.go`
- `internal/balancer/slice_coordinator.go`
- `internal/balancer/slice_aggregator.go`
- `internal/balancer/rpc_client.go`

**Типы:**
```go
// virtual_model.go
type VirtualModel struct {
    Name          string
    Slices        []ModelSlice
    Coordination  CoordinationConfig
    mu            sync.RWMutex
    activeJobs    map[string]*SliceExecutionContext
}

type ModelSlice struct {
    ID             string
    ModelName      string           // Ollama model name
    Ordinal        int              // Порядок в pipeline
    TargetBackends []string         // Куда можно направить
    FallbackMode   string           // retry | skip | abort
}

type CoordinationConfig struct {
    Mode          string   // "sequential" | "parallel" | "tree"
    TimeoutMs     int
    SyncStrategy  string   // "http-callback" | "direct-response"
}

type SliceExecutionContext struct {
    RequestID    string
    VirtualModel string
    CurrentSlice int
    InputData    []byte
    OutputQueue  chan SliceResult
    Error        error
    StartedAt    time.Time
    ctx          context.Context
    cancel       context.CancelFunc
}
```

### Фаза C2: Pipeline execution (1 неделя)

**Sequential pipeline:**
1. Balancer получает запрос на VirtualModel
2. Создаёт `SliceExecutionContext`
3. Последовательно вызывает slice'ы:
   - Slice 0: отправляет входные данные на Backend-1 → получает промежуточный результат
   - Slice 1: передаёт результат Slice 0 на Backend-2 → получает output
   - ... и т.д.
4. Финальный возвращается клиенту

**Поддержка streaming:**
- Каждый slice может быть streaming
- Use chunked transfer между slice'ами
- Heartbeat для длинных операций

### Фаза C3: Отказоустойчивость (3 дня)

- Retry логика: при ошибке slice → пробуем другой бэкенд из `TargetBackends`
- Timeout: если slice не отвечает → abort с ошибкой
- Fallback: если VirtualModel недоступен → return 503 с предложением single-node fallback
- Graceful degradation: если один slice упал, остальные продолжают

### Фаза C4: Мониторинг (2 дня)

- Метрики по slice-ам: latency, throughput, error rate
- WebUI: визуализация pipeline с этапами
- Dashboard: статус VirtualModel, количество активных execution context'ов

---

## 7. Сводная таблица

| Вариант | Сложность | Время | Решает проблему | Отказоуст. | Streaming | Совместимость |
|---------|-----------|-------|-----------------|-------------|-----------|---------------|
| **A** Model Replication | 🟢 Средняя | 1–2 нед | ❌ (не помещается) | ✅ Да | ✅ Да | ✅ Полная |
| **B** External Coordinator | 🔴 Очень высокая | 3–6 мес | ✅ Полностью | ✅ Да | ❌ Сложно | ❌ Новый сервис |
| **C** Virtual Model Router | 🟡 Высокая | 4–8 нед | ✅ Частично | ✅ Да | 🟡 Частично | 🟡 Гибрид |
| **D** Custom Backend | 🔴 Экстремальная | 6+ мес | ✅ Полностью | ✅ Да | ✅ Да | ❌ Ломает |

---

## 8. Рекомендация

**Начать с Варианта A (Model Replication Manager)** как наиболее прагматичного:

1. Даёт немедленную ценность: отказоустойчивость, равномерная загрузка
2. Использует существующую архитектуру на 90%
3. Не ломает обратную совместимость
4. Создаёт фундамент для VirtualModel (концепция групп/инстансов)

**После завершения A — перейти к Варианту C (Virtual Model Router)**:
1. Решает проблему больших моделей
2. Pipeline parallelism — естественное расширение ModelInstanceGroup
3. Можно тестировать в изоляции, не затрагивая обычные модели
4. Streaming support критичен для UX

**Варианты B и D** рекомендую отложить, так как они требуют глубоких изменений в инфраструктуре инференса и выходят за рамки "балансировщика нагрузки".

---

## 9. Риски и их mitigation

| Риск | Вероятность | Влияние | Mitigation |
|------|-------------|---------|------------|
| Ollama изменит API | Низкая | Среднее | Инкапсулировать вызовы в adapter |
| VRAM не хватает для реплик | Средняя | Высокое | Предварительная проверка capacity |
| Network latency между slice'ами | Высокая | Среднее | Для long-running задач latency менее критична |
| Сложность отладки pipeline | Средняя | Высокое | Детальное логирование каждого slice |
| Несовместимость streaming с pipeline | Высокая | Высокое | Для streaming пока использовать single-node |

---

## 10. Приложение: Требования к ModelCoordinator API (для Варианта B)

Если в будущем будет принято решение реализовать Вариант B:

```protobuf
service ModelCoordinator {
    rpc Infer (InferRequest) returns (InferResponse);
    rpc InferStream (InferRequest) returns (stream Chunk);
    rpc GetStatus (StatusRequest) returns (StatusResponse);
    rpc LoadModel (LoadRequest) returns (LoadResponse);
    rpc UnloadModel (UnloadRequest) returns (UnloadResponse);
}

message InferRequest {
    string virtual_model = 1;
    bytes prompt = 2;
    map<string, string> params = 3;
    string session_id = 4;
}

message InferResponse {
    string request_id = 1;
    bytes output = 2;
    repeated SliceStats slice_stats = 3;
    int64 total_ms = 4;
}

message SliceStats {
    string slice_id = 1;
    string backend_id = 2;
    int64 latency_ms = 3;
    bool success = 4;
}
```
