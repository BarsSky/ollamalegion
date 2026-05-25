# Изоляция типов бэкендов (Ollama vs llama.cpp)

> **Дата:** 2026-05-19
> **Версия:** 1.0

## Архитектура типов

OllamaLegion поддерживает два типа бэкендов: **Ollama** (🦙) и **llama.cpp** (🦒). Каждый тип имеет свой API, порты инференса и методы взаимодействия.

### Типы бэкендов (`pkg/types/backend_type.go`)

| Константа | Значение | API-эндпоинты | Порт |
|-----------|----------|---------------|------|
| `BackendTypeOllama` | `"ollama"` | `/api/generate`, `/api/chat`, `/api/tags` | `OllamaPort` (11434) |
| `BackendTypeLlamaCpp` | `"llama_cpp"` | `/v1/chat/completions`, `/v1/completions` | `CppWorkerPort` (18091) |

### Engine'ы (`BackendEngine`)

```
BackendEngineOllamaAPI = "ollama_api"   // Ollama REST API
BackendEngineLlamaCPP  = "llama_cpp"    // llama.cpp + cppworker
```

`ResolveEngine(type)` — разрешает engine из типа бэкенда (ollama → ollama_api, llama_cpp → llama_cpp).

## Совместимость с OperatingMode

| OperatingMode | Разрешённые типы |
|---------------|-----------------|
| `standard` | ollama, llama_cpp |
| `replication` | ollama |
| `rpc_coordinator` | ollama |
| `virtual_router` | llama_cpp |
| `distributed_inference` | llama_cpp |

Функция `IsModeCompatibleWithBackendType(mode, type)` проверяет совместимость.

## Поток запроса с фильтрацией

```
HTTP Request → ServeHTTP()
  ├─ determineRequestBackendType(r):
  │   /api/*  → BackendTypeOllama
  │   /v1/*   → BackendTypeLlamaCpp
  │   иначе   → из OperatingMode
  │
  ├─ selectBackend(model, bt) → фильтрация всех методов:
  │   ├─ expandCandidates(model, allowedTypes)
  │   ├─ findBackendWithModel(model, allowedTypes)
  │   ├─ selectByResources(allowedTypes)
  │   └─ selectFreeBackendAny(allowedTypes)
  │
  └─ proxyRequest(w, r, backendID)
      └─ getBackendPort(backend) — возвращает правильный порт
          в зависимости от engine (ollama_port / cppworker_port)
```

## Валидация при CRUD

При добавлении/обновлении бэкенда через API (`handlers_backends.go`):
- Тип валидируется: только `ollama` или `llama_cpp`
- Проверяется совместимость с текущим `OperatingMode`
- `Engine` выставляется через `ResolveEngine(type)`

## WebUI

- **Dashboard:** бейджи 🦙/🦒 в таблице бэкендов и management page
- **Monitor:** per-backend бейджи в таблице (`data-backend-type` атрибут)
- **Фильтры:** `BackendTypeFilter` (`backend-type-filter.js`) — кнопки-переключатели 🦙/🦒
- **Режимы:** карточки режимов фильтруются по типу (replication только для ollama, virtual_router только для llama_cpp)

## Сборка Docker-образов

### Быстрая пересборка всех образов
```powershell
# PowerShell
.\scripts\build-containers.ps1

# или выборочно:
.\scripts\build-containers.ps1 -Balancer
.\scripts\build-containers.ps1 -WebUI
.\scripts\build-containers.ps1 -Agent
.\scripts\build-containers.ps1 -CppWorker
```

### Ручная сборка (Linux/macOS/PowerShell)

```bash
# Balancer (основной сервис)
docker build -t ollama-legion/balancer:latest -f docker/balancer/Dockerfile .

# WebUI (Dashboard + Monitor)
docker compose -f deployments/docker-compose.yml build webui

# Agent (GPU-агент для Ollama)
docker build -t ollama-legion/agent:latest -f docker/agent/Dockerfile .

# CppWorker (llama.cpp worker)
docker build -t ollama-legion/cppworker:latest -f docker/cppworker/Dockerfile .
```

## Варианты развёртывания

### Вариант A: Только Ollama (стандартный)
```bash
# docker-compose.yml уже содержит loadbalancer + webui
docker compose -f deployments/docker-compose.yml up -d
```
Бэкенды регистрируются через WebUI или API с типом `ollama`.

### Вариант B: Только llama.cpp
```bash
# Добавляем cppworker к стандартному стеку
docker compose -f deployments/docker-compose.yml -f deployments/docker-compose.cppworker.yml up -d
```
Бэкенды регистрируются с `"type": "llama_cpp"` и `"cppWorkerPort": 18091`.

### Вариант C: Смешанный кластер (Ollama + llama.cpp)
```bash
# Все сервисы вместе
docker compose \
  -f deployments/docker-compose.yml \
  -f deployments/docker-compose.cppworker.yml \
  -f deployments/docker-compose.agent.yml \
  up -d
```
Бэкенды разных типов работают параллельно. Балансировщик направляет:
- `/api/generate`, `/api/chat`, `/api/tags` → Ollama-бэкенды
- `/v1/chat/completions`, `/v1/completions` → llama.cpp-бэкенды

### Вариант D: Режимы OperatingMode
```bash
# Replication (только Ollama)
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
| `pkg/types/backend_type.go` | Типы, enum'ы, `ModeBackendTypes`, `ResolveEngine` |
| `internal/balancer/proxy.go` | `determineRequestBackendType()`, вызовы `selectBackend` |
| `internal/balancer/backend_selector.go` | Фильтрация бэкендов по `allowedTypes` |
| `internal/balancer/candidate.go` | `expandCandidates` с `allowedTypes` |
| `internal/balancer/backend_state.go` | `getBackendPort`, `resolveBackendEngine` |
| `internal/balancer/cluster_state.go` | Эвристика `CppWorkerPort>0` → `llama_cpp` |
| `internal/config/backend_type_validation.go` | `MigrateLegacyBackendTypes`, `normalizeBackendTypeWithHeuristic` |
| `internal/config/config.go` | `setDefaults` с эвристикой по `CppWorkerPort` |
| `internal/api/handlers_backends.go` | Валидация типа при CRUD |
| `webui/js/modules/backend-type-filter.js` | Клиентские фильтры и переключатели |
| `webui/js/modules/renderers.js` | Бейджи 🦙/🦒 в таблицах |
| `webui/js/monitor/ui-renderer.js` | Per-backend бейджи в Monitor |
| `scripts/build-containers.ps1` | Скрипт сборки всех Docker-образов |
