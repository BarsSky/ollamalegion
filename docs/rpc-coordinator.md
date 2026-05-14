# External RPC Coordinator (Вариант B)

## Обзор

External RPC Coordinator — распределённый inference engine для моделей, не помещающихся на одну GPU. Координатор управляет worker'ами, каждый из которых обслуживает срез (slice) модели — набор слоёв трансформера.

## Архитектура

```
Client → Balancer → ModelCoordinator
                      ├── Split Logic
                      ├── Pipeline Manager (sequential/parallel/tree)
                      ├── Merge Logic
                      └── KV Cache Manager
                            ↓
              Worker-1 (слои 1-40)  ←→  Worker-2 (слои 41-80)
```

## Компоненты

### Core Coordinator (`internal/rpccoordinator/coordinator.go`)

- `ModelCoordinator` — основной движок распределённого inference
- `DistributedModel` — модель, разбитая на срезы по worker'ам
- `LayerSlice` — диапазон слоёв на конкретном worker'е
- `InferenceJob` — контекст выполнения запроса

### Worker Client (`internal/rpccoordinator/worker_client.go`)

- `WorkerClient` — HTTP клиент для взаимодействия с worker'ом
- API worker'а:
  - `POST /rpc/health` — health check
  - `POST /rpc/load` — загрузка среза модели
  - `POST /rpc/unload` — выгрузка среза
  - `POST /rpc/infer` — inference среза
  - `GET /rpc/metrics` — метрики worker'а

### Split/Merge Engine (`internal/rpccoordinator/splitmerge.go`)

- `Splitter` — разбиение prompt на сегменты
  - Strategies: `sentence`, `paragraph`, `token`, `equal`
- `Merger` — сборка output token'ов
  - Strategies: `concat`, `last`, `join_json`
- `ParallelMergeContext` — контекст параллельного merge'а

### KV Cache Manager (`internal/rpccoordinator/kv_cache.go`)

- `DistributedKVCache` — распределённый KV cache
- `KVCacheShard` — шард кеша для сессии
- LRU eviction, TTL cleanup, worker-to-worker sync

## Конфигурация

```json
{
  "balancing": {
    "rpcCoordinator": {
      "enabled": false,
      "coordinatorURL": "http://localhost:18090",
      "workerPort": 18080,
      "timeout": "30s",
      "protocol": "http",
      "maxRetries": 3
    }
  }
}
```

## Pipeline Execution

1. Координатор получает запрос от балансировщика
2. Сортирует срезы по ordinal (startLayer)
3. Последовательно вызывает worker'ов:
   - Slice N получает output от Slice N-1 как input
   - Каждый worker обрабатывает свои слои
4. Собирает статистику (latency, errors)
5. Возвращает финальный output клиенту

## Retry Logic

- При ошибке среза — retry с exponential backoff
- `maxRetries` из конфигурации (default: 3)
- Если все retries исчерпаны — возвращает ошибку

## Тесты

Файл: `internal/rpccoordinator/coordinator_test.go`

| Тест | Описание |
|------|----------|
| `TestNewModelCoordinator` | Создание координатора |
| `TestRegisterWorker_*` | Регистрация worker'ов |
| `TestRegisterDistributedModel_*` | Регистрация моделей |
| `TestInfer_*` | Inference pipeline |
| `TestInfer_PipelineTwoSlices` | Pipeline из 2 срезов |
| `TestInfer_WorkerFailure` | Failover при ошибке |
| `TestConcurrentInferAndCancel` | Race conditions |

## Статус реализации

| Компонент | Статус | Покрытие тестами |
|-----------|--------|------------------|
| Core Coordinator | ✅ Реализован | 25 тестов |
| Worker Client | ✅ Реализован | ✅ |
| Split/Merge | ✅ Реализован | partial |
| KV Cache | ✅ Реализован | partial |
| Balancer Integration | ✅ Реализован | ✅ |
| Configuration | ✅ Реализован | ✅ |

## Ограничения

- Требуется кастомный worker backend (Ollama не поддерживает частичную загрузку)
- Pipeline parallelism добавляет latency (N сетевых вызовов для N срезов)
- KV cache sync между слоями — опционален, добавляет overhead

## Roadmap

1. **Short-term:** Worker HTTP server (standalone или sidecar)
2. **Medium-term:** gRPC protocol для worker'ов
3. **Long-term:** Tensor parallelism (разбиение матриц внутри слоя)