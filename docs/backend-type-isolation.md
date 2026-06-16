# Изоляция типов бэкендов (Ollama vs llama.cpp)

> **Дата:** 2026-06-14  
> **Версия:** 1.2

## Архитектура типов

OllamaLegion поддерживает два типа бэкендов: **Ollama** (🦙) и **llama.cpp** (🦒). Каждый тип имеет свой API, порты инференса и методы взаимодействия.

### Типы бэкендов (`pkg/types/backend_type.go`)

| Константа | Значение | API-эндпоинты | Порт инференса |
|-----------|----------|---------------|----------------|
| `BackendTypeOllama` | `"ollama"` | `/api/generate`, `/api/chat`, `/api/tags` | `OllamaPort` (11434) |
| `BackendTypeLlamaCpp` | `"llama_cpp"` | `/v1/chat/completions`, `/v1/completions`, `/api/generate`, `/api/chat` (Ollama-compatible) | `CppWorkerPort` (18091) |

### Engine'ы (`BackendEngine`)

```
BackendEngineOllamaAPI = "ollama_api"   // Ollama REST API
BackendEngineLlamaCPP  = "llama_cpp"    // llama.cpp + cppworker
```

`ResolveEngine(type)` — разрешает engine из типа бэкенда (`ollama` → `ollama_api`, `llama_cpp` → `llama_cpp`, пустой → `ollama_api`).

## Совместимость с OperatingMode

| OperatingMode | Разрешённые типы |
|---------------|-----------------|
| `standard` | ollama, llama_cpp |
| `replication` | ollama, llama_cpp |
| `rpc_coordinator` | ollama, llama_cpp |
| `virtual_router` | llama_cpp |
| `distributed_inference` | llama_cpp |

- `IsModeCompatibleWithBackendType(mode, type)` проверяет совместимость.
- Пустой `Backend.Type` (legacy/не задан) нормализуется в `ollama` через `internal/balancer.normalizeBackendType`.

## Маршрутизация запросов

Определение требуемого типа бэкенда происходит в `determineRequestBackendType(r)`:

- `/v1/*` (OpenAI-совместимые) → всегда `llama_cpp`.
- `/api/*` в смешанных режимах (`standard`/`replication`/`rpc_coordinator`) → пустой `BackendType`; выбор конкретного бэкенда делает `selectBackend` на основе модели, affinity и ресурсов.
- `/api/*` в `virtual_router`/`distributed_inference` → всегда `llama_cpp`.

Для `/api/chat` и `/api/generate` в смешанных режимах используется основной flow `ServeHTTP` / `selectBackend`, а не перехват специализированными роутерами. Это позволяет в смешанном кластере направить Ollama-запрос на Ollama-бэкенд, а не на llama.cpp.

## Фильтрация в `selectBackend`/`expandCandidates`

Фильтрация кандидатов по типу бэкенда реализована в:

- `internal/balancer/backend_selector.go` — `selectBackend` принимает `allowedTypes []types.BackendType` и передаёт его во все вспомогательные методы (`expandCandidates`, `findBackendWithModel`, `selectByResources`, `selectFreeBackendAny`).
- `internal/balancer/candidate.go` — `expandCandidates` фильтрует `p.backends` через `isBackendAllowed(bt, allowedTypes)`, исключая бэкенды несовместимого типа из кандидатов.

`allowedTypes` формируется в `getDefaultAllowedTypes` на основе `OperatingMode` и запрошенного `BackendType`. В смешанных режимах (`standard`/`replication`/`rpc_coordinator`) это обычно оба типа; в `virtual_router`/`distributed_inference` — только `llama_cpp`.

## Поток запроса с фильтрацией

```
HTTP Request → ServeHTTP()
  ├─ determineRequestBackendType(r):
  │   /v1/*   → BackendTypeLlamaCpp
  │   /api/*  → "" (в standard/replication/rpc_coordinator) или BackendTypeLlamaCpp
  │   иначе   → из OperatingMode
  │
  ├─ selectBackend(model, bt) → фильтрация через allowedTypes:
  │   ├─ expandCandidates(model, allowedTypes)
  │   ├─ findBackendWithModel(model, allowedTypes)
  │   ├─ selectByResources(allowedTypes)
  │   └─ selectFreeBackendAny(allowedTypes)
  │
  └─ proxyRequest(w, r, backendID)
      └─ IsModeCompatibleWithBackendType(normalizeBackendType(state.Backend.Type), mode)
          └─ getBackendPort(backend) — возвращает правильный порт в зависимости от engine
```

## Валидация при CRUD

При добавлении/обновлении бэкенда через API (`handlers_backends.go`):

- Тип валидируется: только `ollama` или `llama_cpp`.
- Проверяется совместимость с текущим `OperatingMode` через `IsModeCompatibleWithBackendType`.
- `Engine` выставляется через `ResolveEngine(type)`.
- Пустой `Backend.Type` считается legacy-Ollama при маршрутизации.

## WebUI

- **Dashboard:** бейджи 🦙/🦒 в таблице бэкендов и management page.
- **Monitor:** per-backend бейджи в таблице (`data-backend-type` атрибут).
- **Фильтры:** `BackendTypeFilter` (`backend-type-filter.js`) — кнопки-переключатели 🦙/🦒.
- **Режимы:** карточки режимов фильтруются по типу (`virtual_router`/`distributed_inference` только для `llama_cpp`).

## Варианты развёртывания

### Вариант A: Только Ollama (стандартный)

```bash
docker compose -f deployments/docker-compose.yml up -d
```

Бэкенды регистрируются через WebUI или API с типом `ollama`.

### Вариант B: Только llama.cpp

```bash
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.cppworker.yml up -d
```

Бэкенды регистрируются с `"type": "llama_cpp"` и `"cppWorkerPort": 18091`.

### Вариант C: Смешанный кластер (Ollama + llama.cpp)

```bash
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml \
  -f deployments/docker-compose.agent.yml \
  up -d
```

Бэкенды разных типов работают параллельно. Балансировщик направляет:

- `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings` → llama.cpp-бэкенды.
- `/api/generate`, `/api/chat` → выбор по `selectBackend` (Ollama или llama.cpp в зависимости от модели/ресурсов).
- `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull`, `/api/delete`, `/api/push` → обрабатываются соответствующими специализированными роутерами.

### Вариант D: Режимы OperatingMode

```bash
# Replication (ollama + llama_cpp)
docker compose -f deployments/docker-compose.yml up -d
# В WebUI: Settings → Operating Mode = replication

# Virtual Router (только llama.cpp)
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.cppworker.yml up -d
# В WebUI: Settings → Operating Mode = virtual_router
```

### Добавление бэкенда через API

```bash
# Ollama-бэкенд
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"ollama-gpu-1","host":"192.168.1.10","ollamaPort":11434,"type":"ollama"}'

# llama.cpp-бэкенд
curl -X POST http://localhost:18081/api/v1/backends \
  -H "Content-Type: application/json" \
  -d '{"id":"llamacpp-1","host":"192.168.1.20","cppWorkerPort":18091,"type":"llama_cpp"}'
```

**Примечание:** Бэкенды с `cppWorkerPort > 0` автоматически определяются как `llama_cpp`, даже если `type` не указан явно.

## Ключевые файлы

| Файл | Назначение |
|------|------------|
| `pkg/types/backend_type.go` | Типы, enum'ы, `ModeBackendTypes`, `ModeEngines`, `ResolveEngine` |
| `internal/balancer/router.go` | `routeRequest`, `determineRequestBackendType`, `isChatOrGenerateRequest` |
| `internal/balancer/proxy.go` | `ServeHTTP`, вызовы `selectBackend`, `queueRequest` |
| `internal/balancer/backend_selector.go` | `selectBackend`, `getAllowedTypesList`, `getDefaultAllowedTypes` |
| `internal/balancer/candidate.go` | `expandCandidates` с фильтрацией по `allowedTypes` |
| `internal/balancer/backend_type_filter.go` | `normalizeBackendType`, `filterBackendsByType` |
| `internal/balancer/proxy_request.go` | Проверка совместимости `IsModeCompatibleWithBackendType(normalizeBackendType(...), mode)` |
| `internal/balancer/backend_state.go` | `getBackendPort`, `resolveBackendEngine` |
| `internal/api/handlers_backends.go` | Валидация `BackendType` против `OperatingMode` при CRUD |

## Тесты

Ключевые тесты изоляции типов:

| Тест | Файл | Что проверяет |
|------|------|---------------|
| `TestSelectBackend_FiltersByType_OllamaOnly` | `tests/backend_type_isolation_test.go` | Явный `BackendTypeOllama` не выбирает llama.cpp |
| `TestSelectBackend_FiltersByType_LlamaCppOnly` | `tests/backend_type_isolation_test.go` | Явный `BackendTypeLlamaCpp` не выбирает Ollama |
| `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend` | `tests/backend_type_isolation_test.go` | Полный HTTP-flow `/api/generate` → Ollama |
| `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend` | `tests/backend_type_isolation_test.go` | Полный HTTP-flow `/v1/chat/completions` → llama.cpp |
| `TestOperatingMode_DispatchQueue_Standard` | `tests/operating_mode_dispatch_test.go` | `/api/chat` в standard mode проходит через основной flow |
| `TestOperatingMode_DispatchQueue_Replication` | `tests/operating_mode_dispatch_test.go` | `/api/chat` в replication mode проходит через основной flow |