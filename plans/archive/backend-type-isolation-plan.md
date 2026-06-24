# План изоляции типов бэкендов (Ollama vs llama.cpp)

> **Дата:** 2026-05-19
> **Версия:** 1.0
> **Цель:** Полная изоляция и непересечение работы балансировщика с разными типами бэкендов (Ollama / llama.cpp), а также корректное отображение типов в WebUI.

---

## Сводка текущего состояния

| Компонент | Статус | Оценка |
|-----------|--------|--------|
| Система типов (`BackendType`, `BackendEngine`) | 🟢 100% | Enum'ы, ModeBackendTypes, ResolveEngine — реализованы |
| Фильтрация по типу для API (`backend_type_filter.go`) | 🟢 100% | filterBackendsByType, countBackendsByType — только для API |
| Диспетчеризация warmup по типу | 🟢 100% | warmupOllamaModel / warmupLlamaCppModel разделены |
| selectBackend — фильтрация по типу | 🔴 0% | **НЕТ фильтрации** |
| expandCandidates — фильтрация по типу | 🔴 0% | **НЕТ фильтрации** |
| WebUI — индикаторы типа в таблицах | 🟡 30% | Только sidebar badge, нет в таблицах/мониторе |
| WebUI — фильтры по типу | 🔴 0% | **Нет фильтров** |
| Тесты на изоляцию типов | 🔴 0% | **Нет тестов** |
| Валидация типа при CRUD | 🔴 0% | **Нет валидации** |

---

## Этап 1: Бэкенд — фильтрация в selectBackend (P0 🔴)

### Задача 1.1: Модификация `selectBackend()` — фильтрация по BackendType

**Файл:** `internal/balancer/backend_selector.go`

**Текущее состояние:** Метод `selectBackend(model string) string` обходит все бэкенды без учёта их типа.

**Необходимо:**
1. Добавить параметр `allowedTypes []types.BackendType` (или определять внутри из `p.config.OperatingMode` через `ModeBackendTypes`)
2. Перед вызовом `expandCandidates()` отфильтровать `p.backends` по `allowedTypes`
3. Все подметоды (`findBackendWithModel`, `findLessLoadedBackendAny`, `selectByResources`, etc.) должны принимать `allowedTypes` и фильтровать

**Подход A (рекомендуемый):** Изменить сигнатуру `selectBackend` и всех вызывающих:
```go
func (p *Proxy) selectBackend(model string, bt types.BackendType) string
```
— где `bt` — требуемый тип бэкенда (извлекается из запроса или определяется OperatingMode).

**Подход B (альтернативный):** Внутри `selectBackend` получить `allowedTypes` из `ModeBackendTypes[p.config.OperatingMode]` и фильтровать все итерации.

**Статус:** ⬜ Не начато

| Подзадача | Файл | Метод | Статус |
|-----------|------|-------|--------|
| 1.1.1 | `backend_selector.go` | `selectBackend()` | ⬜ |
| 1.1.2 | `backend_selector.go` | `selectBackendExcluding()` | ⬜ |
| 1.1.3 | `backend_selector.go` | `findBackendWithModel()` | ⬜ |
| 1.1.4 | `backend_selector.go` | `findBackendWithModelExcluding()` | ⬜ |
| 1.1.5 | `backend_selector.go` | `findLessLoadedBackendWithModel()` | ⬜ |
| 1.1.6 | `backend_selector.go` | `findLessLoadedBackendAny()` | ⬜ |
| 1.1.7 | `backend_selector.go` | `findFreeBackendForModelUnsafe()` | ⬜ |
| 1.1.8 | `backend_selector.go` | `selectByResources()` | ⬜ |
| 1.1.9 | `backend_selector.go` | `selectByResourcesExcluding()` | ⬜ |
| 1.1.10 | `backend_selector.go` | `selectFreeBackendAny()` | ⬜ |

### Задача 1.2: Модификация `expandCandidates()` — фильтрация по BackendType

**Файл:** `internal/balancer/candidate.go`

**Необходимо:**
1. Добавить параметр `allowedTypes []types.BackendType` в `expandCandidates()`
2. Фильтровать бэкенды по `allowedTypes` при формировании групп P1-P4
3. Убедиться, что llama.cpp-бэкенды не попадают в кандидаты для Ollama-запроса и наоборот

**Статус:** ⬜ Не начато

### Задача 1.3: Обновление всех вызовов `selectBackend`

**Файлы:** `internal/balancer/proxy.go`, `internal/balancer/slot_handler.go`, `internal/balancer/session_handler.go`, `internal/balancer/queue_manager.go`, `internal/balancer/ollama_router_admin.go`

**Необходимо:**
1. Во всех местах вызова `selectBackend(model)` передавать правильный `BackendType`
2. Определять `BackendType` из:
   - Запроса (если запрос идёт к Ollama API — `BackendTypeOllama`, если к llama.cpp — `BackendTypeLlamaCpp`)
   - `p.config.OperatingMode` через `ModeBackendTypes`
   - По умолчанию — `BackendTypeOllama` (обратная совместимость)

**Статус:** ⬜ Не начато

| Подзадача | Файл | Строки | Статус |
|-----------|------|--------|--------|
| 1.3.1 | `proxy.go` | `ServeHTTP()` → вызов `selectBackend` | ⬜ |
| 1.3.2 | `proxy.go` | `processRequest()` → вызов `selectBackend` | ⬜ |
| 1.3.3 | `slot_handler.go` | Вызов `selectBackend` | ⬜ |
| 1.3.4 | `session_handler.go` | `resolveSessionBackend()` | ⬜ |
| 1.3.5 | `queue_manager.go` | `SelectBackend()` | ⬜ |
| 1.3.6 | `ollama_router_admin.go` | `selectBackendByResources()` | ⬜ |

### Задача 1.4: Определение BackendType из входящего запроса

**Файл:** `internal/balancer/proxy.go` (новый метод)

**Необходимо:**
1. Создать метод `determineRequestBackendType(r *http.Request) types.BackendType`
2. Логика:
   - Если `r.URL.Path` — Ollama API endpoint (`/api/generate`, `/api/chat`, `/api/tags`, `/api/show`, etc.) → `BackendTypeOllama`
   - Если `r.URL.Path` — llama.cpp API endpoint (`/v1/chat/completions`, `/v1/completions`, etc.) → `BackendTypeLlamaCpp`
   - Иначе → из `ModeBackendTypes[config.OperatingMode]`
3. Интегрировать в `ServeHTTP()` перед вызовом `selectBackend`

**Статус:** ⬜ Не начато

---

## Этап 2: Бэкенд — защита proxyRequest (P1 🟡)

### Задача 2.1: Проверка совместимости запроса и бэкенда в `proxyRequest()`

**Файл:** `internal/balancer/proxy_request.go`

**Необходимо:**
1. В начале `proxyRequest()` добавить проверку:
   ```go
   if !types.IsModeCompatibleWithBackendType(p.config.OperatingMode, state.Backend.Type) {
       return fmt.Errorf("backend type %s not compatible with operating mode %s", state.Backend.Type, p.config.OperatingMode)
   }
   ```
2. Это предотвратит отправку запроса на несовместимый бэкенд даже если `selectBackend` по какой-то причине вернул неправильный

**Статус:** ⬜ Не начато

### Задача 2.2: Защита `getBackendPort()` от CppWorker→Ollama fallback

**Файл:** `internal/balancer/backend_state.go`

**Текущее поведение:** `getBackendPort()` возвращает `backend.OllamaPort` по умолчанию. Если у llama.cpp-бэкенда `CppWorkerPort == 0`, используется дефолтный 18091, но если `OllamaPort` тоже задан (например, остался от шаблона) — может быть путаница.

**Необходимо:**
1. Для `EngineLlamaCPP`: если `CppWorkerPort == 0`, использовать 18091, но явно логировать WARN
2. Не фоллбэчить на `OllamaPort` для llama.cpp-бэкендов никогда

**Статус:** ⬜ Не начато

---

## Этап 3: API — валидация типа бэкенда при CRUD (P1 🟡)

### Задача 3.1: Валидация при создании бэкенда

**Файл:** `internal/api/handlers.go` (handleAddBackend)

**Необходимо:**
1. При создании бэкенда проверять совместимость `BackendType` с текущим `OperatingMode`
2. Если `BackendType == llama_cpp`, но `OperatingMode == standard` и в кластере есть Ollama-бэкенды — WARN
3. Если `BackendType == ollama`, но `OperatingMode == virtual_router` — ERROR (только llama.cpp разрешён)

**Статус:** ⬜ Не начато

### Задача 3.2: Валидация при обновлении бэкенда

**Файл:** `internal/api/handlers.go` (handleUpdateBackend)

**Необходимо:**
1. При смене типа бэкенда проверять совместимость с OperatingMode
2. Если бэкенд используется (ActiveReqs > 0) — запретить смену типа

**Статус:** ⬜ Не начато

---

## Этап 4: WebUI — индикаторы типа бэкенда (P1 🟡)

### Задача 4.1: Бейджи типа (🦙/🦒) в таблицах Dashboard

**Файл:** `webui/js/modules/renderers.js`

**Текущее состояние:** `backendsTable()` показывает `getBackendModeBadge(b)` (GPU/CPU/Cloud), но не тип бэкенда.

**Необходимо:**
1. Добавить бейдж типа бэкенда в колонку ID или отдельную колонку:
   - 🦙 `Ollama` (зелёный/синий)
   - 🦒 `llama.cpp` (оранжевый/янтарный)
2. CSS-классы: `.backend-type-ollama`, `.backend-type-llama_cpp`

**Статус:** ⬜ Не начато

| Подзадача | Файл | Функция | Статус |
|-----------|------|---------|--------|
| 4.1.1 | `renderers.js` | `backendsTable()` | ⬜ |
| 4.1.2 | `renderers.js` | `capacityCard()` | ⬜ |
| 4.1.3 | `renderers.js` | `backendsPage()` (management table) | ⬜ |
| 4.1.4 | CSS | Стили `.backend-type-*` | ⬜ |

### Задача 4.2: Бейджи типа в Monitor таблице

**Файл:** `webui/js/monitor/ui-renderer.js`

**Необходимо:**
1. В `renderBackendsTable()` добавить колонку/бейдж типа бэкенда
2. Данные уже доступны в `metrics.BackendType`

**Статус:** ⬜ Не начато

### Задача 4.3: Тип бэкенда в панели «Models in Memory» Monitor

**Файлы:** `webui/js/monitor/ui-renderer.js`, `webui/monitor.html`

**Необходимо:**
1. В `renderModelsInMemory()` показывать, на бэкенде какого типа загружена каждая модель
2. Формат: "🦙 backend-1" или "🦒 gpu-node-01"

**Статус:** ⬜ Не начато

### Задача 4.4: Тип бэкенда в expandable row (Backends page)

**Файл:** `webui/js/modules/renderers.js` (`renderBackendDetails`)

**Необходимо:**
1. В expandable row показывать тип бэкенда (🦙/🦒) и соответствующие параметры:
   - Для Ollama: `ollamaPort`, `OllamaConfig`
   - Для llama.cpp: `cppWorkerPort`, `CppWorkerConfig`

**Статус:** ⬜ Не начато

---

## Этап 5: WebUI — фильтры по типу (P2 🟡)

### Задача 5.1: Фильтры в Dashboard таблице бэкендов

**Файлы:** `webui/js/modules/renderers.js`, `webui/index.html`, `webui/js/app.js`

**Необходимо:**
1. Добавить кнопки-фильтры над таблицей бэкендов: «Все | 🦙 Ollama | 🦒 llama.cpp»
2. При клике фильтровать `backendsData` по `backend.BackendType` или `backend.Type`
3. Сохранять состояние фильтра в sessionStorage

**Статус:** ⬜ Не начато

### Задача 5.2: Фильтры в Backends management table

**Файлы:** `webui/js/modules/renderers.js`

**Необходимо:**
1. Аналогичные кнопки-фильтры над management таблицей
2. Фильтрация на клиентской стороне (все бэкенды уже загружены)

**Статус:** ⬜ Не начато

### Задача 5.3: Фильтры в Monitor таблице бэкендов

**Файлы:** `webui/js/monitor/ui-renderer.js`, `webui/monitor.html`

**Необходимо:**
1. Добавить кнопки-фильтры над таблицей бэкендов в мониторе
2. Совместимо с существующей фильтрацией по статусу

**Статус:** ⬜ Не начато

---

## Этап 6: Тесты (P1 🟡)

### Задача 6.1: Unit-тесты изоляции selectBackend

**Файл:** `tests/backend_type_isolation_test.go` (новый)

**Необходимо:**
1. Тест: `TestSelectBackend_FiltersByType_OllamaOnly`
   - Кластер: 2 Ollama + 2 llama.cpp бэкенда
   - Запрос с `BackendTypeOllama` → выбран только Ollama-бэкенд
2. Тест: `TestSelectBackend_FiltersByType_LlamaCppOnly`
   - Кластер: 2 Ollama + 2 llama.cpp бэкенда
   - Запрос с `BackendTypeLlamaCpp` → выбран только llama.cpp-бэкенд
3. Тест: `TestSelectBackend_MixedCluster_NoCrossContamination`
   - 100 итераций Ollama-запросов → ни разу не выбран llama.cpp-бэкенд
4. Тест: `TestExpandCandidates_FiltersByType`
5. Тест: `TestFindBackendWithModel_FiltersByType`

**Статус:** ⬜ Не начато

### Задача 6.2: Интеграционные тесты

**Файл:** `tests/backend_type_isolation_test.go`

**Необходимо:**
1. Тест: `TestServeHTTP_OllamaRequest_RoutedToOllamaBackend`
2. Тест: `TestServeHTTP_LlamaCppRequest_RoutedToLlamaCppBackend`
3. Тест: `TestOperatingModeChange_ExcludesIncompatibleBackends`

**Статус:** ⬜ Не начато

### Задача 6.3: Тесты WebUI отображения типов

**Файл:** `webui/tests/` (новый)

**Необходимо:**
1. Тест: отображение бейджей 🦙/🦒 в таблицах
2. Тест: фильтры по типу работают

**Статус:** ⬜ Не начато

---

## Этап 7: Документация (P3)

### Задача 7.1: Документ по изоляции типов

**Файл:** `docs/backend-type-isolation.md` (новый)

**Содержание:**
1. Архитектура типов бэкендов
2. Как работает изоляция
3. OperatingMode и разрешённые типы
4. WebUI индикаторы
5. Диаграмма потока запроса с фильтрацией по типу

**Статус:** ⬜ Не начато

---

## Общая статистика

| Этап | Задач | Приоритет | Оценка времени | Статус |
|------|-------|-----------|----------------|--------|
| 1. Бэкенд — selectBackend фильтрация | 4 | 🔴 P0 | 6-8 часов | ⬜ |
| 2. Бэкенд — защита proxyRequest | 2 | 🟡 P1 | 1-2 часа | ⬜ |
| 3. API — валидация CRUD | 2 | 🟡 P1 | 2-3 часа | ⬜ |
| 4. WebUI — индикаторы | 4 | 🟡 P1 | 3-4 часа | ⬜ |
| 5. WebUI — фильтры | 3 | 🟡 P2 | 2-3 часа | ⬜ |
| 6. Тесты | 3 | 🟡 P1 | 3-4 часа | ⬜ |
| 7. Документация | 1 | 🟢 P3 | 1-2 часа | ⬜ |
| **Итого** | **19** | | **18-26 часов** | |

---

## Ключевые файлы для изменений

| Файл | Этапы | Назначение |
|------|-------|------------|
| `internal/balancer/backend_selector.go` | 1 | Основная логика выбора бэкенда — добавить фильтрацию по типу |
| `internal/balancer/candidate.go` | 1 | expandCandidates — фильтрация по типу |
| `internal/balancer/proxy.go` | 1, 2 | Вызовы selectBackend, determineRequestBackendType |
| `internal/balancer/proxy_request.go` | 2 | Проверка совместимости в proxyRequest |
| `internal/balancer/backend_state.go` | 2 | Защита getBackendPort |
| `internal/balancer/slot_handler.go` | 1 | Вызов selectBackend |
| `internal/balancer/session_handler.go` | 1 | resolveSessionBackend |
| `internal/balancer/queue_manager.go` | 1 | SelectBackend |
| `internal/balancer/ollama_router_admin.go` | 1 | selectBackendByResources |
| `internal/api/handlers.go` | 3 | Валидация при CRUD |
| `webui/js/modules/renderers.js` | 4, 5 | Индикаторы + фильтры в таблицах |
| `webui/js/monitor/ui-renderer.js` | 4, 5 | Индикаторы + фильтры в Monitor |
| `webui/js/app.js` | 4, 5 | Обработчики фильтров |
| `webui/monitor.html` | 4, 5 | HTML-шаблоны |
| `webui/index.html` | 4, 5 | HTML-шаблоны |
| `webui/css/custom.css` | 4 | Стили бейджей |
| `tests/backend_type_isolation_test.go` | 6 | Новые тесты |

---

## История изменений

| Дата | Версия | Изменения |
|------|--------|-----------|
| 2026-05-19 | 1.0 | Первая версия — полный план изоляции типов бэкендов |