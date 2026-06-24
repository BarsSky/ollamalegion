# Backend Type Isolation (Ollama vs llama.cpp)

> **Версия:** 1.3 (2026-06-22)  
> **Связанные документы:** [`api.md`](api.md), [`audit-2026-06.md`](audit-2026-06.md), [`../.clinerules`](../.clinerules) §6  
> **Тесты:** `tests/backend_type_isolation_test.go` (15 тестов), `tests/operating_mode_dispatch_test.go`

## Содержание

1. [Зачем изоляция типов](#1-зачем-изоляция-типов)
2. [Типы бэкендов и Engine'ы](#2-типы-бэкендов-и-engines)
3. [Совместимость с OperatingMode](#3-совместимость-с-operatingmode)
4. [Маршрутизация запросов](#4-маршрутизация-запросов)
5. [Фильтрация в selectBackend/expandCandidates](#5-фильтрация-в-selectbackendexpandcandidates)
6. [Поток запроса с фильтрацией](#6-поток-запроса-с-фильтрацией)
7. [Валидация при CRUD](#7-валидация-при-crud)
8. [WebUI: бейджи и фильтры](#8-webui-бейджи-и-фильтры)
9. [Варианты развёртывания](#9-варианты-развёртывания)
10. [Ключевые файлы и тесты](#10-ключевые-файлы-и-тесты)

---

## 1. Зачем изоляция типов

OllamaLegion поддерживает два **разных** типа бэкендов:

| Тип | API | Порт | Иконка |
|---|---|---|---|
| **Ollama** | `/api/generate`, `/api/chat`, `/api/tags` и т.д. | 11434 | 🦙 |
| **llama.cpp** (через CppWorker) | `/api/generate`, `/api/chat` (Ollama-compat), `/v1/*` (OpenAI-compat) | 18091 | 🦒 |

Без изоляции клиентский `/api/generate` мог бы случайно попасть на `llama_cpp` бэкенд, а `/v1/chat/completions` — на Ollama. Это приводит к:
- Потере features (OpenAI tools в Ollama).
- Несовместимым ответам (разные форматы streaming).
- Лишним latency (двойная трансляция).

**Изоляция решает эту проблему на 3 уровнях:** фильтрация в `selectBackend`/`expandCandidates`, защита в `proxyRequest`, валидация в CRUD.

---

## 2. Типы бэкендов и Engine'ы

### 2.1 `BackendType`

Файл: `pkg/types/backend_type.go`

| Константа | Строковое значение | API |
|---|---|---|
| `BackendTypeOllama` | `"ollama"` | `/api/*` Ollama REST |
| `BackendTypeLlamaCpp` | `"llama_cpp"` | `/v1/*` OpenAI + Ollama-compat |

Пустой `Backend.Type` (legacy) нормализуется в `"ollama"` через `internal/balancer.normalizeBackendType` (обратная совместимость).

### 2.2 `BackendEngine`

| Engine | Описание |
|---|---|
| `BackendEngineOllamaAPI` | Ollama REST API (`ollama_api`) |
| `BackendEngineLlamaCPP` | llama.cpp + cppworker (`llama_cpp`) |

`ResolveEngine(type)` маппит `BackendType → BackendEngine`.

### 2.3 Порт бэкенда

`internal/balancer/backend_state.go:getBackendPort(backend)`:
- Для `BackendEngineLlamaCPP`: возвращает `backend.CppWorkerPort` (default 18091), **без fallback** на `OllamaPort`.
- Для `BackendEngineOllamaAPI`: возвращает `backend.OllamaPort` (default 11434).

---

## 3. Совместимость с OperatingMode

`pkg/types.ModeBackendTypes` и `IsModeCompatibleWithBackendType(mode, bt)`:

| OperatingMode | Допустимые типы |
|---|---|
| `standard` | `ollama`, `llama_cpp` |
| `replication` | `ollama`, `llama_cpp` |
| `rpc_coordinator` | `ollama`, `llama_cpp` |
| `virtual_router` | `llama_cpp` (только) |
| `distributed_inference` | `llama_cpp` (только) |

При попытке добавить Ollama-бэкенд в `virtual_router` — API возвращает 400 (см. §7).

---

## 4. Маршрутизация запросов

### 4.1 `determineRequestBackendType(r)` — `internal/balancer/proxy.go`

| Путь в URL | Тип |
|---|---|
| `/v1/*` (OpenAI-совместимые) | `BackendTypeLlamaCpp` (всегда) |
| `/api/*` в `standard`/`replication`/`rpc_coordinator` | `""` (пустой — `selectBackend` решает по модели/affinity) |
| `/api/*` в `virtual_router`/`distributed_inference` | `BackendTypeLlamaCpp` |
| Прочие | Из `ModeBackendTypes[OperatingMode]` |

### 4.2 Главный flow для `/api/chat` и `/api/generate`

В смешанных режимах (`standard`/`replication`/`rpc_coordinator`) `/api/chat` и `/api/generate` проходят через **основной flow** `ServeHTTP` / `selectBackend`, **а не** перехватываются специализированными роутерами (`llamacpp_router.go` / `ollama_router.go`). Это позволяет:
- В смешанном кластере направить Ollama-запрос на Ollama-бэкенд (а не на llama.cpp).
- В pure-llama_cpp режиме — на llama.cpp.

---

## 5. Фильтрация в `selectBackend`/`expandCandidates`

### 5.1 Сигнатура

```go
func (p *Proxy) selectBackend(model string, bt types.BackendType) string
func (p *Proxy) expandCandidates(modelName string, allowedTypes []types.BackendType) CandidateGroups
```

Оба метода принимают `allowedTypes` и фильтруют кандидатов через `isBackendTypeAllowed`:

```go
func isBackendTypeAllowed(bt types.BackendType, allowedTypes []types.BackendType) bool {
    if len(allowedTypes) == 0 { return true }  // обратная совместимость
    for _, at := range allowedTypes {
        if bt == at { return true }
    }
    return false
}
```

### 5.2 Вспомогательные методы (все принимают `allowedTypes`)

- `findBackendWithModel(modelName, allowedTypes)`
- `findBackendWithModelExcluding(modelName, exclude, allowedTypes)`
- `selectByResources(allowedTypes)`
- `selectByResourcesExcluding(exclude, allowedTypes)`
- `selectFreeBackendAny(allowedTypes)`
- `findFreeBackendForModelUnsafe(model, allowedTypes)`
- `findLessLoadedBackendWithModel(...)`
- `findLessLoadedBackendAny(...)`

### 5.3 Формирование `allowedTypes`

`getAllowedTypesList(bt)` + `getDefaultAllowedTypes()`:
- Из `ModeBackendTypes[p.config.OperatingMode]` (список допустимых типов).
- Если `bt != ""` — добавляется явно запрошенный тип.

---

## 6. Поток запроса с фильтрацией

```
HTTP Request → ServeHTTP()
  ├─ determineRequestBackendType(r):
  │   /v1/*   → BackendTypeLlamaCpp
  │   /api/*  → "" (в standard/replication/rpc_coordinator) или BackendTypeLlamaCpp
  │   иначе   → из OperatingMode
  │
  ├─ selectBackend(model, bt) → фильтрация через allowedTypes:
  │   ├─ expandCandidates(model, allowedTypes)       ← candidate.go: фильтр P1-P4
  │   ├─ findBackendWithModel(model, allowedTypes)    ← affinity check
  │   ├─ selectByResources(allowedTypes)              ← resource-aware
  │   └─ selectFreeBackendAny(allowedTypes)           ← fallback
  │
  └─ proxyRequest(w, r, backendID)
      └─ IsModeCompatibleWithBackendType(normalizeBackendType(state.Backend.Type), mode)
          └─ getBackendPort(backend) — возвращает правильный порт в зависимости от engine
```

**Гарантия:** после `selectBackend` кандидат **никогда** не будет противоположного типа, т.к. фильтрация в `expandCandidates` отсекает их до scoring/selection.

---

## 7. Валидация при CRUD

`internal/api/handlers_backends.go`:

### 7.1 `handleAddBackend`

```go
// Проверка совместимости типа с OperatingMode
if !types.IsModeCompatibleWithBackendType(bt, p.config.Balancing.OperatingMode) {
    return 400 Bad Request
}
```

Пример: попытка добавить `type: "ollama"` в режиме `virtual_router` → 400.

### 7.2 `handleUpdateBackend`

- Запрет смены типа, если `ActiveReqs > 0`.
- Повторная проверка совместимости с OperatingMode.

### 7.3 `Engine` вычисляется автоматически

```go
backend.Engine = types.ResolveEngine(backend.Type)
```

---

## 8. WebUI: бейджи и фильтры

### 8.1 Бейджи 🦙 / 🦒 (реализовано)

- `webui/js/modules/utils.js:getBackendTypeBadge(backend)` — рендерит бейдж.
- Применяется в: `backendsTable` (Dashboard), `backendsPage` (Backends management), `modelsGrid` (Models).
- `webui/js/monitor/backend-type-badges.js` — Monitor (`renderBackendsTable`, `renderModelsInMemory`).

### 8.2 Фильтры по типу (в backlog, R-2)

Пока не реализованы в WebUI. После R-2 будут:
- Dashboard: кнопки «Все | 🦙 Ollama | 🦒 llama.cpp» над таблицей бэкендов.
- Backends management: аналогичные кнопки.
- Monitor: переключатель типа (каркас в `ui-renderer.js:renderBackendTypeSwitcher` уже есть).

### 8.3 `BackendTypeFilter` + `toggleAgentsTab()`

`webui/js/modules/backend-type-filter.js`:
- При `llama_cpp` режиме скрывает вкладку Agents.
- В Settings скрывает секции, специфичные для Ollama (`.settings-section-ollama`).

---

## 9. Варианты развёртывания

### 9.1 Только Ollama (стандартный)

```bash
docker compose -f deployments/docker-compose.yml up -d
```

Бэкенды регистрируются через WebUI или API с типом `ollama`.

### 9.2 Только llama.cpp

```bash
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.cppworker.yml up -d
```

Бэкенды регистрируются с `"type": "llama_cpp"` и `"cppWorkerPort": 18091`.

### 9.3 Смешанный кластер (Ollama + llama.cpp)

```bash
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml \
  -f deployments/docker-compose.agent.yml \
  up -d
```

Бэкенды разных типов работают параллельно. Балансировщик:
- `/v1/*` → llama.cpp-бэкенды.
- `/api/generate`, `/api/chat` → выбор по `selectBackend` (Ollama или llama.cpp).
- `/api/tags`, `/api/show`, `/api/copy`, `/api/create`, `/api/pull`, `/api/delete`, `/api/push` → специализированные роутеры.

### 9.4 Режимы OperatingMode

```bash
# Replication (ollama + llama_cpp)
# В WebUI: Settings → Operating Mode = replication

# Virtual Router (только llama.cpp)
# В WebUI: Settings → Operating Mode = virtual_router
```

### 9.5 Добавление бэкенда через API

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

**Авто-детект:** бэкенды с `cppWorkerPort > 0` автоматически определяются как `llama_cpp`, даже если `type` не задан явно.

---

## 10. Ключевые файлы и тесты

### 10.1 Код

| Файл | Назначение |
|---|---|
| `pkg/types/backend_type.go` | `BackendType`, `BackendEngine`, `ModeBackendTypes`, `IsModeCompatibleWithBackendType`, `ResolveEngine` |
| `internal/balancer/router.go` | `routeRequest`, `determineRequestBackendType`, `isChatOrGenerateRequest` |
| `internal/balancer/proxy.go` | `ServeHTTP`, вызовы `selectBackend`, `queueRequest` |
| `internal/balancer/backend_selector.go` | `selectBackend`, `getAllowedTypesList`, `getDefaultAllowedTypes` |
| `internal/balancer/candidate.go` | `expandCandidates(modelName, allowedTypes)` |
| `internal/balancer/backend_type_filter.go` | `normalizeBackendType`, `filterBackendsByType` |
| `internal/balancer/proxy_request.go` | `IsModeCompatibleWithBackendType` защита |
| `internal/balancer/backend_state.go` | `getBackendPort`, `resolveBackendEngine` |
| `internal/api/handlers_backends.go` | CRUD-валидация типа |
| `internal/api/routes.go` | Регистрация `/api/v1/backends` |

### 10.2 Тесты

| Тест | Файл | Что проверяет |
|---|---|---|
| `TestSelectBackend_FiltersByType_OllamaOnly` | `tests/backend_type_isolation_test.go` | Явный `BackendTypeOllama` не выбирает llama.cpp |
| `TestSelectBackend_FiltersByType_LlamaCppOnly` | `tests/backend_type_isolation_test.go` | Явный `BackendTypeLlamaCpp` не выбирает Ollama |
| `TestSelectBackend_MixedCluster_NoCrossContamination` | `tests/backend_type_isolation_test.go` | 100 итераций Ollama-запросов не выбирают llama.cpp |
| `TestExpandCandidates_FiltersByType` | `tests/backend_type_isolation_test.go` | expandCandidates фильтрует по типу |
| `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend` | `tests/backend_type_isolation_test.go` | Полный HTTP-flow `/api/generate` → Ollama |
| `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend` | `tests/backend_type_isolation_test.go` | Полный HTTP-flow `/v1/chat/completions` → llama.cpp |
| `TestOperatingMode_DispatchQueue_Standard` | `tests/operating_mode_dispatch_test.go` | `/api/chat` в standard mode → main flow |
| `TestOperatingMode_DispatchQueue_Replication` | `tests/operating_mode_dispatch_test.go` | `/api/chat` в replication mode → main flow |
| `TestDetermineRequestBackendTypeForTest` | `tests/backend_type_init_test.go` | Логика `determineRequestBackendType` |

### 10.3 Запуск тестов

```bash
go test ./tests -run TestBackendType -count=1 -v
go test ./tests -run TestOperatingMode -count=1 -v