# Режим Virtual Router

Готовый к продакшену режим `virtual_router` (P.2) для балансера Ollama Legion.

## Что это

Режим `virtual_router` даёт простой pattern alias-on-pool: виртуальная модель
= алиас на пул бэкендов с одной и той же физической моделью. Балансер
выбирает бэкенд через Selector (round-robin / least-loaded / random)
и проксирует запрос. Пользователь видит одну модель (`virtual:my-gpt`),
а балансер распределяет нагрузку по всем бэкендам с этой моделью.

## Архитектура

```
Client
  ↓ POST /api/generate {"model": "virtual:my-gpt", ...}
Balancer (port 8080)
  ↓ OperatingMode=virtual_router + VirtualRouter + IsVirtualPathRequest + MatchesVirtualRequest
VirtualRouter.ServeHTTP
  ↓ parse body → lookup registry → Selector.Select()
  ↓ rewrite model in body: "virtual:my-gpt" → "llama-70b"
  ↓ add X-Original-Backend, X-Virtual-Model headers
  ↓ proxy to http://selected-backend:port/api/generate
Backend 1 / Backend 2 / Backend 3 (все с llama-70b)
  ↓ response
VirtualRouter (копирует headers + body)
  ↓ add X-Original-Backend в response
Client
```

## Режимы VirtualModel

VirtualModelConfig может быть в одном из двух режимов:

### Режим alias-on-pool (Phase 8 P.2 — этот документ)
```json
{
  "name": "virtual:my-gpt",
  "description": "Llama-70B distributed across 3 backends",
  "selection": "round_robin",
  "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
  "modelName": "llama-70b"
}
```

Использует VirtualRouter (Phase 8 P.2) — перехватчик в Proxy.ServeHTTP.

### Режим pipeline (legacy)
```json
{
  "name": "virtual:my-pipeline",
  "slices": [
    {"id": "emb", "modelName": "embedding-model", "ordinal": 0, "targetBackends": ["b1"]},
    {"id": "gen", "modelName": "llama-70b", "ordinal": 1, "targetBackends": ["b2", "b3"]}
  ],
  "coordination": {"mode": "sequential", "timeoutMs": 30000}
}
```

Использует `virtualmodel.Router` + `VirtualModel.ExecutePipeline` (legacy).

## Конфигурация

```json
{
  "balancing": {
    "operatingMode": "virtual_router",
    "virtualModels": {
      "enabled": true,
      "models": [
        {
          "name": "virtual:my-gpt",
          "description": "Llama-70B distributed",
          "selection": "round_robin",
          "backendPool": ["backend-1:11434", "backend-2:11434", "backend-3:11434"],
          "modelName": "llama-70b"
        },
        {
          "name": "virtual:fast-llama",
          "description": "Fast llama for short prompts",
          "selection": "least_loaded",
          "backendPool": ["backend-a:11434", "backend-b:11434"],
          "modelName": "llama-3-8b"
        }
      ]
    }
  }
}
```

Параметры `Selection`:

| Стратегия | Поведение |
|---|---|
| `round_robin` (по умолчанию) | Атомарный счётчик, mod по пулу. Равномерное распределение. |
| `least_loaded` | Выбирает бэкенд с максимальным FreeSlots (через LoadProvider). При равенстве — round-robin. |
| `random` | math/rand, для тестов и нагрузочного тестирования. |

`LoadProvider` (для `least_loaded`): callback, возвращающий `FreeSlots` для бэкенда.
Phase 8 P.2: заглушка fallback (stub) (= 1 для всех, по сути round-robin).
В будущем (Phase 9): подключение к реальному `Proxy.GetBackendMetrics()`.

## Поддерживаемые endpoints

| Путь | Формат | Примечания |
|---|---|---|
| `/api/generate` | Ollama | переписывается поле `model` |
| `/api/ollama/generate` | Ollama (алиас) | то же |
| `/api/chat` | Ollama | то же |
| `/api/ollama/chat` | Ollama (алиас) | то же |
| `/v1/chat/completions` | OpenAI | то же |
| `/v1/completions` | OpenAI legacy | то же |

Метод: только `POST`. GET (`/api/tags`, `/v1/models`) идёт в стандартный поток.

## Заголовки ответа

После прохождения через VirtualRouter, ответ содержит:

- `X-Original-Backend: <host:port>` — какой бэкенд обработал запрос.
- `X-Virtual-Model: <original_name>` — исходное имя виртуальной модели (для отладки).

Запрос содержит:

- `X-Original-Backend: <host:port>` — выбранный бэкенд.
- `X-Virtual-Model: <original_name>` — исходное имя (для бэкенда).
- `X-Backend-Selected: <host:port>` — дубликат X-Original-Backend.
- `X-Selection-Strategy: <round_robin|least_loaded|random>` — какая стратегия.

## Ответы с ошибками

| Статус | Тип ошибки | Когда |
|---|---|---|
| 400 | `json_parse_error` | Тело не JSON или обрезано |
| 400 | `wrong_virtual_mode` | Виртуальная модель в pipeline mode (не alias-on-pool) |
| 404 | `unknown_virtual_model` | Имя модели отсутствует в реестре |
| 502 | `backend_unreachable` | Отказ в соединении / таймаут |
| 503 | (нет типа ошибки) | virtual_router не активен (реестр отключён) |
| 503 | `no_healthy_backends` | Все бэкенды недоступны + нет кандидатов |
| 502 | (passthrough) | Бэкенд вернул 4xx/5xx (статус копируется) |

Endpoint'ы OpenAI используют формат `{"error":{"message","type"}}`; Ollama —
`{"error":"<message>"}`.

## Метрики

`VirtualRouter.GetMetrics().Snapshot()` возвращает:

```json
{
  "inferenceTotal": 1247,
  "inferenceErrors": 3,
  "streamPassThrough": 412,
  "selectionSkips": 0,
  "backendSelections": {
    "backend-1:11434": 416,
    "backend-2:11434": 415,
    "backend-3:11434": 416
  }
}
```

Phase 8 P.2: встроенные счётчики (counters). Phase 9: интеграция в endpoint
`/api/v1/metrics` и Prometheus exporter.

## Примеры

### Round-robin распределение

```bash
# 3 бэкенда с одинаковой llama-70b.
# Config: selection=round_robin, backendPool=["b1:11434","b2:11434","b3:11434"]

# 3 последовательных запроса → 3 разных бэкенда.
for i in 1 2 3; do
  curl -X POST http://balancer:8080/api/generate \
    -d '{"model":"virtual:my-gpt","prompt":"hello"}' \
    -D - 2>&1 | grep X-Original-Backend
done
# X-Original-Backend: b1:11434
# X-Original-Backend: b2:11434
# X-Original-Backend: b3:11434
```

### Least-loaded

```bash
# Config: selection=least_loaded, backendPool=["b1:11434","b2:11434","b3:11434"]
# (с LoadProvider, возвращающим FreeSlots=[2, 10, 5] соответственно)

# Все запросы пойдут на b2 (max FreeSlots=10).
```

### Сквозной проброс стриминга (streaming passthrough)

```bash
curl -X POST http://balancer:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model":"virtual:my-gpt",
    "messages":[{"role":"user","content":"Stream me"}],
    "stream":true
  }'
# SSE stream от выбранного бэкенда, заголовки X-Original-Backend
# показывают куда ушёл запрос.
```

### Failover (поведение legacy)

Если `selection=round_robin` и бэкенд недоступен — connection refused → 502.
Round-robin НЕ делает автоматический failover на следующий бэкенд.
Для failover используйте `least_loaded` с health checks (Phase 9) или
комбинируйте с `replication` mode (P.2 legacy alias).

## Миграция / fallback

Чтобы отключить virtual_router и вернуться к standard mode:

```json
{
  "balancing": {
    "operatingMode": "standard",
    "virtualModels": {
      "enabled": false
    }
  }
}
```

Перехваты в `Proxy.ServeHTTP` отсекаются проверкой `IsVirtualRouterMode`.
Конфиг по умолчанию (OperatingMode="" / "standard") → не срабатывает.

## Известные ограничения

- **No automatic failover**: round-robin / random / least-loaded не
  повторяют запрос на следующий бэкенд при ошибке. Бэкенд недоступен → 502.
  Phase 9: добавить логику retry-with-next-candidate.
- **LoadProvider stub**: `least_loaded` без настроенного `LoadProvider`
  использует fallback FreeSlots=1, что эквивалентно round-robin.
  Phase 9: подключение к реальным метрикам.
- **No model validation**: VirtualRouter не проверяет, что физическая
  модель действительно загружена на бэкенде до запроса. Если бэкенд
  вернёт 404 "model not found" — passthrough как ошибка бэкенда.
- **Режим pipeline не использует VirtualRouter**: только alias-on-pool
  mode (Phase 8 P.2). Режим pipeline (Slices + Coordination) →
  legacy путь `virtualmodel.Router`.

## Сравнение с P.1 (rpc_coordinator)

| Аспект | P.1 rpc_coordinator | P.2 virtual_router |
|---|---|---|
| Pattern | Pipeline parallelism (multi-slice) | Alias-on-pool (1 модель, N бэкендов) |
| Модель воркеров | Несколько воркеров, каждый обрабатывает slice | Все бэкенды имеют полную модель |
| Selection | Кандидаты на каждый slice | На каждый запрос из пула |
| Streaming | Да (полный SSE passthrough) | Да (passthrough) |
| Circuit breaker | На каждый воркер | Пока нет (Phase 9) |
| Auth | На каждый rpc path | Пока нет (Phase 9) |
| Сценарий применения | Модель больше 1 GPU | Высокая доступность / балансировка нагрузки |

## См. также

- `docs/archive/rounds/phase-8-p1-rpc-coordinator.md` — родственный документ P.1.
- `docs/plans/2026-q3-production-ready-plan.md` — общий план §P.2.
- `CHANGELOG.md` — история версий (раздел Phase 8 P.2).
- `internal/balancer/virtual_router.go` — реализация.
- `internal/virtualmodel/selector.go` — стратегии выбора.
- `internal/virtualmodel/registry.go` — существующий реестр (legacy pipeline mode).
- `pkg/types/rpc_variants.go` — типы VirtualModelConfig + SelectionStrategy.
