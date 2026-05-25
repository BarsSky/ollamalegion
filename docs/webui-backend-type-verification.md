# WebUI Backend Type Verification — Проверка разделения Ollama / llama.cpp

> **Дата:** 2026-05-21
> **Цель:** Поэтапная проверка корректности отображения разделения бэкендов (ollama 🦙 / llama.cpp 🦒) в WebUI
> **Основание:** docs/backend-type-isolation.md, docs/webui-gap-analysis.md

## Легенда статусов

| Символ | Значение |
|--------|----------|
| ✅ | Пройдено — корректно |
| ⚠️ | Частично — есть недочёты |
| ❌ | Не пройдено — требуется исправление |
| 🔍 | В процессе проверки |
| ⏳ | Ожидает проверки |

---

## Этап 1: Проверка фильтра типов бэкендов (`BackendTypeFilter`)

**Файл:** `webui/js/modules/backend-type-filter.js`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 1.1 | Инициализация `initBackendTypeFilter` | Вызывается при загрузке, тип из localStorage → ClusterState → API config → fallback `ollama` | ✅ | Инициализация в app.js через `syncFromClusterState()`, приоритеты в `getCurrentType()` корректны |
| 1.2 | Приоритеты `getCurrentType()` | localStorage → `__lastClusterState.effectiveBackendType` → `__lastServerConfig` → `'ollama'` | ✅ | Приоритет localStorage → ClusterState → API config → fallback ollama. Асинхронная загрузка через `_fetchConfigAsync()` |
| 1.3 | Синхронизация `syncFromClusterState()` | Приоритет: `effectiveBackendType` → `backendEngine`, защита `_savingInProgress` | ✅ | Защита от гонки через `_savingInProgress`, приоритет effectiveBackendType → backendEngine, fallback на API config |
| 1.4 | `updateUI()` — видимость вкладок | GGUF видна при `llama_cpp`, Agents скрыта при `llama_cpp` (агенты только у Ollama) | ❌ | Вкладка GGUF корректно скрывается/показывается. **Вкладка Agents НЕ скрывается для llama_cpp** — нет вызова `toggleAgentsTab()` в `updateUI()` |
| 1.5 | `setCurrentType()` — payload на сервер | `{ backendEngine, operatingMode }` → `Api.updateConfig()` | ✅ | Корректно: `backendEngine: 'llama_cpp'` / `'ollama_api'` + текущий `operatingMode` |
| 1.6 | Совместимость с OperatingMode | Блокировка несовместимых режимов в Settings UI | ✅ | `toggleModeCards()` правильно скрывает replication/rpc для llama_cpp и virtual_router/distributed для ollama, авто-переключает на standard |

---

## Этап 2: Dashboard (`index.html` → вкладка Dashboard)

**Файлы:** `webui/js/modules/renderers.js` (`dashboard`, `runtimeCluster`, `gpuCluster`, `capacitySection`, `backendsTable`, `availableModels`)

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 2.1 | `runtimeCluster()` — Ollama Runtime флаги | Только для `ollama` бэкендов, скрыты для `llama_cpp` | ❌ | `runtimeCluster()` **не фильтрует по типу бэкенда** — показывает runtime-флаги Ollama (`numGpuLayers`, `contextLength`, `numParallel`) для ВСЕХ бэкендов, включая llama_cpp. Флаги берутся из `backend.ollama?.runtimeFlags` — для llama_cpp будет пустой объект, отобразится `default` |
| 2.2 | `gpuCluster()` / `gpuCard()` | GPU карточки для обоих типов, нет ошибок при отсутствии NVML у llama_cpp | ✅ | Фильтрует по mode (gpu/cpu/cloud), не зависит от типа бэкенда напрямую. CPU-only покажет CPU карточку корректно |
| 2.3 | `capacitySection()` / `capacityCard()` | Корректный VRAM/RAM per backend, CPU-only llama_cpp без VRAM | ✅ | Использует `backend.ollama?.backendCapacity` с fallback на пустой объект, корректно для обоих типов |
| 2.4 | `backendsTable()` — бейджи типа | 🦙/🦒 через `getBackendTypeBadge()`, правильный порт (OllamaPort / CppWorkerPort) | ⚠️ | Бейджи типа ✅ через `getBackendTypeBadge(b)`. **Порт в backendsTable не отображается вообще**. В backendsPage: `b.ollamaPort \|\| 11434` — для llama_cpp бэкенда без ollamaPort покажет некорректный fallback 11434 вместо cppWorkerPort |
| 2.5 | Метрические карточки (4 шт.) | Фильтрация по `allowedTypes`, не суммируются метрики разных типов без разбора | ✅ | `dashboard()` считает метрики по всем переданным бэкендам — фильтрация происходит на уровне `filterBackendsForUI()` в app.js |
| 2.6 | `availableModels()` | Модели фильтруются по типу бэкенда | ✅ | Модели берутся из `b.ollama?.backendCapacity?.availableModels`, фильтрация на уровне app.js через `filterBackendsForUI` |

---

## Этап 3: Backends Page

**Файлы:** `webui/js/modules/renderers.js` (`backendsPage`), `webui/js/app.js` (CRUD handlers), `webui/index.html`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 3.1 | CRUD форма — поле типа (`#backendType`) | Выбор `ollama` / `llama_cpp`, заполнение при редактировании | ✅ | `<select id="formBackendType">` с опциями ollama/llama_cpp, синхронизируется с BackendTypeFilter при открытии формы |
| 3.2 | Поле порта | `ollamaPort` для ollama, `cppWorkerPort` для llama_cpp | ⚠️ | Поля есть: `#formBackendOllamaPort` (ollama-field) и `#formBackendCppWorkerPort` (llama-field). Видимость переключается через `toggleBackendFormFields()`. **НО** в таблице backendsPage порт всегда `b.ollamaPort \|\| 11434`, без учёта cppWorkerPort |
| 3.3 | Таблица — колонка типа | Бейджи 🦙/🦒, не hardcoded | ✅ | `getBackendTypeBadge(b)` в обоих таблицах (backendsTable и backendsPage), динамически из `Utils.getBackendType()` |
| 3.4 | Expandable row — Ollama Runtime Flags | Только для ollama, скрыты для llama_cpp | ❌ | `renderOllamaParams()` **не проверяет тип бэкенда** — показывает секцию Ollama Runtime Flags для всех бэкендов. Для llama_cpp данные будут пустыми (берутся из `backend.ollama?.runtimeFlags`) |
| 3.5 | Expandable row — Model Contexts | Корректны для llama_cpp (другие параметры контекста) | ⚠️ | Model Contexts берутся из `backend.ollama?.modelContexts`. Для llama_cpp будет пусто. Нет отдельной секции для llama.cpp параметров контекста |
| 3.6 | Expandable row — Loaded Models + Load/Unload | Правильный API для каждого типа | ⚠️ | Load/Unload идёт через `Api.backendModelOperation()` → `POST /api/v1/backends/${id}/models` без указания типа. Сервер должен сам определить куда слать (ollama или cppworker) |
| 3.7 | Model Management Modal — Pull | Только ollama; для llama_cpp — GGUF | ⚠️ | Pull доступен всем. Клиентской блокировки нет. Для llama_cpp нужна загрузка GGUF вместо pull |
| 3.8 | Кнопки агента (Restart/Logs/Config) | Только для ollama с агентом | ✅ | Кнопки в `renderAgentDetails()` показываются при `backend.hasAgent && backend.agentPort`. llama_cpp бэкенды не имеют агента |
| 3.9 | Disk/Network метрики | Для обоих типов | ✅ | В `renderAgentDetails()`, данные из `backend.system?.disk` и `backend.system?.network` |

---

## Этап 4: Models Page

**Файлы:** `webui/js/modules/renderers.js` (`modelsPage`, `modelsGrid`)

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 4.1 | Models Grid — бейджи типа | 🦙/🦒 для каждой модели | ❌ | `getBackendTypeBadge()` не вызывается в `modelsGrid`. Вместо типа показывается бейдж с именем бэкенда и статусом. Пользователь не видит тип модели (ollama/llama_cpp) |
| 4.2 | Model details (family/format/quantization) | Для обоих типов, не ollama-специфичные | ⚠️ | Поля `family`, `format`, `parameterSize`, `quantization` берутся из `runningModel.*`. Для llama_cpp эти поля будут пустыми (структура данных из Ollama API `/api/ps` не применима к llama.cpp) |
| 4.3 | Load/Unload/Delete кнопки | Правильный API для каждого типа | ⚠️ | `window.modelCardAction(modelName, action, backendId)` → `Api.backendModelOperation()` без указания типа. Сервер должен сам маршрутизировать |
| 4.4 | Model Management Modal доступность | Доступен из Models tab | ✅ | `openModelManageModal` вызывается по клику на model card или через кнопку |
| 4.5 | Search/filter | Поиск с учётом типа | ✅ | Search по имени модели, фильтрация бэкендов через `filterBackendsForUI` перед рендерингом |
| 4.6 | Refresh button | Обновление для правильного типа | ✅ | `refreshBackends()` в app.js обновляет данные с учётом текущего типа через `filterBackendsForUI` |

---

## Этап 5: GGUF Page (только llama_cpp)

**Файлы:** `webui/index.html` (вкладка `gguf`)

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 5.1 | Видимость вкладки | Только при `type === 'llama_cpp'` | ✅ | `toggleGgufTab()` в `BackendTypeFilter.updateUI()` скрывает/показывает корректно |
| 5.2 | Загрузка GGUF | Upload/register через cppworker API | ⚠️ | Нет отдельных функций в `api.js`. Страница GGUF в index.html существует, но JS-логика не проанализирована. Требуется дополнительная проверка |
| 5.3 | Список GGUF | Корректное отображение | ⚠️ | Не проверено — требуется анализ кода страницы GGUF |

---

## Этап 6: Agents Page

**Файлы:** `webui/js/modules/renderers.js` (`agentsPage`, `renderAgentDetails`)

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 6.1 | Видимость вкладки Agents | Скрыта при `llama_cpp` | ❌ | **Вкладка Agents ВСЕГДА видна.** `BackendTypeFilter.updateUI()` не содержит `toggleAgentsTab()`. В index.html вкладка Agents не имеет классов скрытия |
| 6.2 | Данные агентов | Только ollama-агенты | ✅ | `agentsPage()` вызывает `Api.agentsStats()` — эндпоинт возвращает только зарегистрированных агентов (ollama), llama_cpp не регистрирует агентов |
| 6.3 | Agent details (platform) | Корректный platform mode | ✅ | `renderAgentDetails()` показывает platform из данных агента |

---

## Этап 7: Monitor (`monitor.html`)

**Файлы:** `webui/monitor.html`, `webui/js/monitor/ui-renderer.js`, `webui/js/monitor/monitor-app.js`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 7.1 | Stats bar | Учёт `allowedTypes` в подсчёте | ✅ | ui-renderer.js строки 142–150: фильтрация бэкендов по `data.cluster.effectiveBackendType`, агрегаты считаются по отфильтрованному массиву. При пустом типе — смешанный подсчёт |
| 7.2 | Cluster Resources | Корректный VRAM/RAM без смешивания | ✅ | Используются нормализованные поля из `adapt()`: `b.gpu.usagePercent`, `b.vram.usagePercent`, `b.system.cpuUsagePercent`. Фильтрация бэкендов перед рендерингом через `effectiveBackendType` |
| 7.3 | Models in Memory | Тип бэкенда для модели | ⚠️ | Бейдж 🦙/🦒 отображается в **таблице бэкендов** (`renderBackends()` строки 440–447). В **Models in Memory** (`renderModelsInMemory()`) тип бэкенда модели не показан — только имя модели, бэкенд, сессии, VRAM |
| 7.4 | Backends таблица | Бейджи 🦙/🦒, `data-backend-type`, порт | ✅ | Inline бейдж 🦙/🦒 в `<td>` рядом с ID бэкенда (`renderBackends()` строки 440–447). Порт: `b.ollamaPort` (только Ollama). Для llama_cpp может показывать некорректный порт |
| 7.5 | Queue / Dispatch Stats | Фильтрация по типу | ✅ | Данные рендерятся на основе уже отфильтрованного `data.cluster` с учётом `effectiveBackendType` |
| 7.6 | Фильтр типов в Monitor | Переключатель 🦙/🦒, синхронизация с Dashboard | ⚠️ | Отдельного переключателя в Monitor **нет**. Тип синхронизируется через `effectiveBackendType` из ClusterState API. Пользователь не может переключить тип из Monitor |

---

## Этап 8: Settings Page

**Файлы:** `webui/js/modules/renderers.js`, `webui/js/modules/config-io.js`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 8.1 | Operating Mode карточки | Фильтрация: replication только ollama, virtual_router только llama_cpp | ✅ | `toggleModeCards()` корректно скрывает несовместимые режимы, авто-переключает на standard |
| 8.2 | Сохранение настроек | `backendEngine` / `backendType` в payload | ✅ | `_saveToServer()` передаёт `backendEngine: 'llama_cpp'` / `'ollama_api'` + `operatingMode` |
| 8.3 | Agent Settings | Только для ollama | ✅ | `updateSettingSections()` скрывает `.settings-section-ollama` для llama_cpp и `.settings-section-llama` для ollama |
| 8.4 | Per-backend limits | Учёт типа бэкенда | ✅ | Per-backend limits работают через backend CRUD, учитывают тип |

---

## Этап 9: WebSocket / ClusterState

**Файлы:** `webui/js/app.js` (WebSocket), `internal/balancer/cluster_state.go`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 9.1 | ClusterState — поля типа | `effectiveBackendType`, `backendEngine` в сообщении | ✅ | `pkg/types/cluster_state.go`: поля `EffectiveBackendType` и `BackendEngine` в структуре. `GetClusterState()` в `cluster_state.go` заполняет оба поля. `effectiveBackendType` определяется по `BackendEngine` конфига или авто-определяется по типам зарегистрированных бэкендов |
| 9.2 | Обновление UI | Перерисовка при смене типа | ✅ | WebSocket handler в app.js вызывает `BackendTypeFilter.syncFromClusterState()` при получении state |
| 9.3 | Сессии и очередь | `allowedTypes` в ClusterState | ✅ | `filterBackendsByEffectiveType()` фильтрует бэкенды на серверной стороне перед отправкой ClusterState. Сессии и очередь отдаются в полном объёме |

---

## Этап 10: API-взаимодействие

**Файлы:** `webui/js/modules/api.js`

| № | Проверка | Ожидаемое поведение | Статус | Комментарий |
|---|----------|---------------------|--------|-------------|
| 10.1 | `addBackend` / `updateBackend` | Поле `type`, корректный порт | ✅ | `Api.createBackend(data)` и `Api.updateBackend(id, data)` пробрасывают data как есть. `type` передаётся если caller включил его (в app.js — да, из формы) |
| 10.2 | `loadModel` / `unloadModel` | Правильный URL для llama_cpp vs ollama | ⚠️ | Используется универсальный `Api.backendModelOperation()` → `POST /api/v1/backends/${id}/models`. Тип не передаётся — сервер сам должен определить по ID бэкенда |
| 10.3 | `pullModel` | Только ollama, блокировка для llama_cpp | ❌ | **Клиентской блокировки нет.** Pull делается через `backendModelOperation` с `operation: 'pull'` без проверки типа бэкенда |
| 10.4 | `restartAgent` / `agentLogs` | Только ollama с агентом | ✅ | `Api.restartAgent(backendId)` → `POST /api/v1/backends/${id}/agent/restart`. Вызывается только при `hasAgent`. `Api.agentLogs(backendId)` аналогично |
| 10.5 | `updateConfig` | `backendEngine` в payload | ✅ | `BackendTypeFilter._saveToServer()` передаёт `backendEngine: 'llama_cpp'` / `'ollama_api'` |

---

## Сводка

| Этап | ✅ Пройдено | ⚠️ Частично | ❌ Не пройдено | ⏳ Ожидает | Всего |
|------|------------|-------------|---------------|------------|-------|
| 1. BackendTypeFilter | 5 | 0 | 1 | 0 | 6 |
| 2. Dashboard | 4 | 1 | 1 | 0 | 6 |
| 3. Backends Page | 4 | 4 | 1 | 0 | 9 |
| 4. Models Page | 3 | 2 | 1 | 0 | 6 |
| 5. GGUF Page | 1 | 2 | 0 | 0 | 3 |
| 6. Agents Page | 2 | 0 | 1 | 0 | 3 |
| 7. Monitor | 3 | 3 | 0 | 0 | 6 |
| 8. Settings | 4 | 0 | 0 | 0 | 4 |
| 9. WebSocket/ClusterState | 3 | 0 | 0 | 0 | 3 |
| 10. API | 3 | 1 | 1 | 0 | 5 |
| **Итого** | **33** | **10** | **6** | **0** | **51** |

---

## Найденные проблемы

| ID | Этап | № | Серьёзность | Описание | Файл | Статус |
|----|------|---|-------------|----------|------|--------|
| B-01 | 1 | 1.4 | 🔴 P0 | Вкладка Agents **всегда видна**, не скрывается для `llama_cpp`. `updateUI()` не содержит `toggleAgentsTab()` | `backend-type-filter.js:210-215` | Открыта |
| B-02 | 2 | 2.1 | 🟡 P1 | `runtimeCluster()` показывает Ollama Runtime флаги для всех бэкендов, включая llama_cpp. Нет фильтрации по типу | `renderers.js:96-146` | Открыта |
| B-03 | 2 | 2.4 | 🟡 P1 | Порт в `backendsPage`: всегда `b.ollamaPort \|\| 11434`, для llama_cpp должен быть `b.cppWorkerPort` | `renderers.js:675` | Открыта |
| B-04 | 3 | 3.2 | 🟡 P1 | Порт в таблице backendsPage не учитывает `cppWorkerPort` для llama_cpp бэкендов | `renderers.js:675` | Открыта |
| B-05 | 3 | 3.4 | 🟡 P1 | `renderOllamaParams()` не проверяет тип бэкенда — показывает секцию Ollama Runtime Flags для llama_cpp (будет пустой) | `renderers.js` | Открыта |
| B-06 | 4 | 4.1 | 🟡 P1 | `getBackendTypeBadge()` не вызывается в `modelsGrid` — нет бейджей 🦙/🦒 на карточках моделей | `renderers.js:838` | Открыта |
| B-07 | 6 | 6.1 | 🔴 P0 | Вкладка Agents всегда видна в сайдбаре, даже для `llama_cpp`. Дублирует B-01 | `backend-type-filter.js`, `index.html` | Открыта |
| B-08 | 10 | 10.3 | 🟡 P1 | Pull Model доступен для llama_cpp бэкендов без клиентской блокировки. Должен быть скрыт/заменён на GGUF upload | `api.js`, `app.js` | Открыта |
| B-09 | 3 | 3.5 | 🟢 P2 | Model Contexts берутся из `backend.ollama?.modelContexts`. Для llama_cpp будет пусто — нет секции с llama.cpp параметрами контекста | `renderers.js` | Открыта |
| B-10 | 4 | 4.2 | 🟢 P2 | Model details (family/format/quantization) из Ollama API `/api/ps` — для llama_cpp будут пустыми. Нужна отдельная структура или fallback | `renderers.js:844-847` | Открыта |
| B-11 | 7 | 7.3 | 🟢 P2 | В Models in Memory (Monitor) не отображается тип бэкенда модели | `ui-renderer.js` | Открыта |
| B-12 | 7 | 7.6 | 🟢 P2 | В Monitor нет переключателя 🦙/🦒 — фильтрация только через серверный `effectiveBackendType` | `monitor.html`, `ui-renderer.js` | Открыта |
