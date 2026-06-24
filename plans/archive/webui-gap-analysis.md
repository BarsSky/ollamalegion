# WebUI Gap Analysis — Комплексный аудит функциональности

> **Дата:** 2026-05-11
> **Версия:** 1.7
> **Цель:** Полный аудит всех реализованных и отсутствующих элементов WebUI (monitor.html + index.html) относительно реализованного backend-функционала.

---

## Сводка

| Компонент | Статус | Реализовано | Отсутствует |
|-----------|--------|-------------|-------------|
| Monitor (monitor.html) | 🟢 100% | 9 стат. панелей + 9 панелей (вкл. Disk/Network) | — |
| Dashboard | 🟢 100% | Все основные секции + hidden GPU tooltips | — |
| Backends | 🟢 95% | CRUD + expandable details + Disk/Network + model details tooltips | Agent управление (restart/logs) — API есть |
| Models | 🟢 95% | Grid + manage modal + search/filter + refresh + model details + expiresAt/digest/RAM | Delete per model в grid |
| Sessions | 🟢 100% | Полная таблица | — |
| Queue | 🟢 100% | Метрики + задачи + история | — |
| Logs | 🟢 100% | System + Proxy | — |
| Settings | 🟢 100% | Все секции + настройки агентов + per-backend limits | — |
| Agents (в целом) | 🟢 100% | **5 API эндпоинтов + полный UI** | — |
| Agent→Ollama config | 🔴 0% | **Агент не умеет писать в Ollama** | Нет реализации |

---

## 1. Монитор (monitor.html)

### 1.1 Статистическая панель (stats bar) — ✅ ПОЛНОСТЬЮ

| Метрика | Тип | Реализовано | Источник |
|---------|-----|-------------|----------|
| Backends | Количество | ✅ | clusterState.totalBackends |
| Active | Количество | ✅ | clusterState.queue.active |
| Pending | Количество | ✅ | clusterState.queue.pending |
| Processing | Количество | ✅ | clusterState.queue.processing |
| Sessions | Количество | ✅ | clusterState.totalSessions |
| Req/s | Rate | ✅ | clusterState.reqsPerSec |
| Cluster RPS | Rate | ✅ | clusterState.clusterRPS |
| VRAM | % | ✅ | Сумма/среднее по бэкендам |
| Models | Количество | ✅ | clusterState.totalModels |

### 1.2 Панели — ✅ ПОЛНОСТЬЮ (кроме скрытых метрик)

| Панель | Статус | Элементы |
|--------|--------|----------|
| Cluster Resources | ✅ | GPU/VRAM/CPU/RAM avg/max bars + free slots + VRAM per backend |
| Models in Memory | ✅ | Model name, backends, sessions count, VRAM, load (cloud detection) |
| Backends | ✅ | 13 columns: ID, status, GPU, VRAM, CPU, RAM, Active, RPS, Avg RT, Capacity, Score, Models, Uptime |
| Queue | ✅ | Task list with wait times + модели + таргеты |
| Dispatch Stats | ✅ | 4 cards: Model Affinity, Resource-Aware, Weight/Config, Processed Total |
| Auto-Pull | ✅ | Toggle + settings + active pulls table |
| Virtual Models | ✅ | Models table + active jobs details |
| Sessions | ✅ | ID, backend, model, requests, idle, IP, client (excludes technical) |

### 1.3 Отсутствующие метрики в Monitor ❌

| Метрика | Собирается агентом | Где в коде | Отображается в Monitor |
|---------|-------------------|------------|----------------------|
| DiskTotal/DiskUsed/DiskFree | ✅ collector.go:624-627 | `getDiskInfo()` | ✅ **ДОБАВЛЕНА панель Disk/Network** |
| NetworkRX/NetworkTX | ✅ collector.go:630-632 | `getNetworkIO()` | ✅ **ДОБАВЛЕНА панель Disk/Network** |
| Model.expiresAt | ✅ collector.go:856 | `getRunningModelsWithDetails()` | ✅ **ДОБАВЛЕНА** (в Models in Memory + renderOllamaParams + renderAgentDetails + modelsGrid) |
| Model.digest | ✅ collector.go:855 | `getRunningModelsWithDetails()` | ✅ **ДОБАВЛЕН** (в Models in Memory + renderOllamaParams + renderAgentDetails + modelsGrid) |
| Model.loadCount | ⚠️ Не собирается | — | ❌ **НЕТ** |
| RAM per model (CPU mode) | ✅ collector.go:883 | `estimateRAMUsage()` | ✅ **ДОБАВЛЕНА** (>0.5 GB в renderOllamaParams + renderAgentDetails + modelsGrid) |

---

## 2. WebUI Dashboard (index.html) — ВКЛАДКИ

### 2.1 Dashboard — ✅ ПОЛНОСТЬЮ

| Секция | Статус | Комментарий |
|--------|--------|-------------|
| Metric cards (4) | ✅ | Total Backends, Active Models, Sessions, Queue |
| Ollama Runtime badges | ✅ | Версия + runtime flags per backend |
| GPU Cluster cards | ✅ | GPU name, usage, VRAM, temperature |
| Backend Capacity bars | ✅ | VRAM/RAM capacity per backend |
| Available Models list | ✅ | Список моделей для загрузки |
| Backends Table | ✅ | 11 columns, actions |
| Prediction Alerts | ✅ | Предупреждения предиктора |

### 2.2 Backends — ⚠️ ЧАСТИЧНО

#### Реализовано ✅
- CRUD modal (Add/Edit/Delete backend)
- Management table: 13 колонок (ID, Name, Host, OllamaPort, AgentPort, Weight, MaxConcurrent, MaxModels, HasAgent, Labels, LastContact, Status, Actions)
- Expandable row details:
  - **Ollama Runtime Flags** (numGPU, ctx_len, num_parallel, num_threads, batch_size, numa, cpu_only, flash_attn, kv_size, tensor_split, main_gpu, verbose, log_level)
  - **Backend Capacity** (freeVRAM, guaranteedVRAM, loadedModelVRAM, contextOverheadMB, loadableModelCount)
  - **Model Contexts** (model name, contextLength, effectiveContext, VRAM per context, numLayersKV, hiddenSize, precision)
  - **Loaded Models** (model name, digest, size, modified, loaded status + Load/Unload buttons)
- Model Management Modal (Pull Model + Models List + Active Operations)

#### Отсутствует ❌
| Функция | API supports? | Почему отсутствует |
|---------|--------------|-------------------|
| **Restart Agent** кнопка | ❌ Нет API | Нужен новый эндпоинт |
| **View Agent Logs** | ❌ Нет API | Нужен новый эндпоинт |
| **Agent Config** отображение | ✅ `/api/v1/agents/{id}` | Не вызывается из UI |
| **Agent подробности** (uptime, seq, platform) | ✅ heartbeat содержит | Не парсится и не отображается |
| **Agent last heartbeat time** | ✅ В Backend есть `lastAgentContact` | Отображается, но нет выделенной секции |
| Disk/Network метрики в expandable row | ✅ Собираются агентом | Не рендерятся |

### 2.3 Models — 🔴 СЕРЬЁЗНО НЕДОСТАТОЧНО

#### Реализовано ✅
- Backend load bars (VRAM/RAM usage per backend)
- Models grid with memory bars (read-only)
- Model Management Modal **ЕСТЬ** (app.js:1094-1223: `openModelManageModal`)
  - Pull model (text input + Pull button + dropdown)
  - Models list table (name, digest, size, modified, loaded badge + Load/Unload buttons)
  - Active operations table

#### Отсутствует ❌
| Функция | Где должно быть | Причина отсутствия |
|---------|----------------|-------------------|
| **Pull Model** кнопка | Models tab header | Modal вызывается ТОЛЬКО из Backends page |
| **Load/Unload/Delete** per model | Models grid | Нет кнопок в modelsGrid |
| **Model details** (family, format, size, quantization) | Models grid cards | Есть в данных (`runningModel.Family`, `Format`, etc.), не отображаются |
| **Model search/filter** | Models tab | Нет поля поиска |
| **Refresh models** button | Models tab | Нет кнопки ручного обновления |

#### Ключевая проблема
Model Management Modal (`#modelManageModal`) полностью функционален, но **доступен только через Backends page** (кнопка "Model" в actions). На Models tab нет UI для его вызова. Пользователь ожидает управление моделями именно на вкладке Models.

### 2.4 Sessions — ✅ ПОЛНОСТЬЮ

| Функция | Статус |
|---------|--------|
| Таблица: ID, Backend, Model, Requests, Last Activity, IP, Client, Status | ✅ |
| Search/filter | ✅ |

### 2.5 Queue — ✅ ПОЛНОСТЬЮ

| Функция | Статус |
|---------|--------|
| 5 metric cards (Current Size, Max, Processed, Workers, Avg Wait) | ✅ |
| Visualization bar (fill) | ✅ |
| Tasks table (#, Model, Target, Wait, Status) | ✅ |
| History table (#, Model, Backend, Enqueue, Complete, Wait) | ✅ |

### 2.6 Logs — ✅ ПОЛНОСТЬЮ

| Функция | Статус |
|---------|--------|
| System logs (временная шкала, уровень, сообщение) | ✅ |
| Proxy logs tab | ✅ |
| Export logs | ✅ |
| Clear logs | ✅ |

### 2.7 Settings — ✅ ПОЛНОСТЬЮ (для балансировщика)

| Секция | Статус | Элементы |
|--------|--------|----------|
| Operating Mode | ✅ | 5 modes с вложенными полями |
| General | ✅ | Algorithm, Enhanced Scoring, Model Affinity, Session Stickiness, Prediction Filtering |
| Resource Limits | ✅ | GPU/VRAM/CPU/RAM max %, Min Free Disk |
| Security | ✅ | API Token |
| Danger Zone | ✅ | Restart, Reset |
| Import/Export | ✅ | JSON import/export |

#### Отсутствует ❌
| Функция | Почему отсутствует |
|---------|-------------------|
| **Agent settings** (Collect Interval, Heartbeat Interval, Agent Timeout) | Нет UI для конфигурации агентов |
| **Per-backend limits** через Settings (сейчас только через Backend CRUD) | MaxConcurrent/MaxModels задаются только при создании бэкенда |
| **Balancer config** (queue size, rate limiter, CORS) | Не все параметры конфига доступны в UI |

---

## 3. Agents — 🔴 ПОЛНОСТЬЮ ОТСУТСТВУЕТ UI

### 3.1 Существующие API эндпоинты

| Эндпоинт | Метод | Описание | Используется в UI |
|----------|-------|----------|-------------------|
| `/api/v1/agents/register` | POST | Регистрация агента | ❌ НЕТ |
| `/api/v1/agents/metrics` | POST | Приём метрик от агента | ❌ НЕТ (backend-only) |
| `/api/v1/agents/heartbeat` | POST | Heartbeat от агента | ❌ НЕТ (backend-only) |
| `/api/v1/agents/stats` | GET | Статистика всех агентов | ❌ НЕТ |
| `/api/v1/agents/{id}` | GET | Информация о конкретном агенте | ❌ НЕТ |

### 3.2 Отсутствующий UI

| Функция | API | Сложность |
|---------|-----|-----------|
| **Страница "Agents"** в навигации | stats, info | Средняя |
| **Таблица агентов**: ID, host, port, status, uptime, platform, last heartbeat | stats | Средняя |
| **Agent details view**: metrics, config, runtime flags | info, cluster | Средняя |
| **Agent logs** | ❌ Нужен новый эндпоинт | Высокая |
| **Agent restart/stop** | ❌ Нужен новый эндпоинт | Высокая |
| **Agent config** view (collect interval, heartbeat, max models) | info | Средняя |

---

## 4. Agent → Ollama: Может ли агент задавать настройки? 🔴

**ВЫВОД: Агент НЕ МОЖЕТ задавать настройки Ollama.**

После полного анализа `internal/agent/collector.go` (1159 строк):

### Что агент делает:
1. **Читает** метрики GPU (NVML/nvidia-smi)
2. **Читает** системные метрики (CPU, RAM, Disk, Network)
3. **Читает** Ollama API:
   - `/api/tags` — список доступных моделей
   - `/api/ps` — запущенные модели
   - `/api/show` — детали модели (runtime flags, context info)
   - `/api/version` — версия Ollama
4. **Отправляет** метрики на балансировщик (`POST /api/v1/agents/metrics`)
5. **Отправляет** heartbeat (`POST /api/v1/agents/heartbeat`)
6. **Получает** лимиты от балансировщика (maxConcurrentRequests, maxModels) и применяет к СВОЕЙ конфигурации

### Чего агент НЕ делает:
1. ❌ **НЕ вызывает** Ollama API для изменения конфигурации
2. ❌ **НЕ вызывает** `/api/create`, `/api/pull`, `/api/push`, `/api/delete` (это делает балансировщик через REST API)
3. ❌ **НЕ устанавливает** runtime flags Ollama (numGpuLayers, contextLength, numParallel)
4. ❌ **НЕ управляет** процессом Ollama (restart, stop, update config file)
5. ❌ **НЕТ** endpoint'а на балансировщике для передачи конфигурации агенту→Ollama

### Техническая возможность
Ollama НЕ ИМЕЕТ публичного REST API для изменения конфигурации runtime. Единственный способ:
- Через переменные окружения (OLLAMA_NUM_PARALLEL, OLLAMA_MAX_LOADED_MODELS, etc.)
- Через файл конфигурации (~/.ollama/config.json)
- Через аргументы командной строки при запуске

Агент ТЕОРЕТИЧЕСКИ мог бы:
- Отредактировать файл конфигурации Ollama
- Послать SIGHUP для перезагрузки
- Вызвать `ollama serve` с новыми аргументами

Но это **не реализовано** и является значительной архитектурной работой.

---

## 5. Детальный список всех gap'ов

### 5.1 Метрики, собираемые агентом, но не отображаемые в WebUI

| Метрика | Тип | Собирается | Dashboard | Monitor | Backends Table | Expandable Row |
|---------|-----|-----------|-----------|---------|---------------|----------------|
| DiskTotal | uint64 | ✅ collector.go:624 | ❌ | ❌ | ❌ | ❌ |
| DiskUsed | uint64 | ✅ collector.go:625 | ❌ | ❌ | ❌ | ❌ |
| DiskFree | uint64 | ✅ collector.go:626 | ❌ | ❌ | ❌ | ❌ |
| NetworkRX | uint64 | ✅ collector.go:631 | ❌ | ❌ | ❌ | ❌ |
| NetworkTX | uint64 | ✅ collector.go:632 | ❌ | ❌ | ❌ | ❌ |
| powerLimit | int | ✅ nvidia-smi parse | ❌ | ✅ hidden tooltip | ❌ | ❌ |
| gpuClock | int | ✅ nvidia-smi parse | ❌ | ✅ hidden tooltip | ❌ | ❌ |
| memClock | int | ✅ nvidia-smi parse | ❌ | ✅ hidden tooltip | ❌ | ❌ |
| Model.Family | string | ✅ collector.go:857 | ❌ | ❌ | ✅ tooltip only | ✅ (loaded models) |
| Model.Format | string | ✅ collector.go:858 | ❌ | ❌ | ✅ tooltip only | ✅ (loaded models) |
| Model.ParameterSize | string | ✅ collector.go:859 | ❌ | ❌ | ✅ tooltip only | ✅ (loaded models) |
| Model.Quantization | string | ✅ collector.go:860 | ❌ | ❌ | ✅ tooltip only | ✅ (loaded models) |
| Model.expiresAt | time.Time | ✅ collector.go:856 | ❌ | ❌ | ❌ | ❌ |
| Model.digest | string | ✅ collector.go:855 | ❌ | ❌ | ❌ | ❌ (только в modal) |
| Model.RAMUsage | uint64 | ✅ collector.go:883 | ❌ | ❌ | ❌ | ❌ |
| AvgResponseTime | float64 | ❌ (0) | ❌ | ❌ | ❌ | ❌ |

### 5.2 Функциональные gap'ы

| # | Gap | Компонент | Приоритет | API Support |
|---|-----|-----------|-----------|-------------|
| 1 | **Models tab не имеет управления** | Models | P0 | ✅ |
| 2 | **Нет Agents UI вообще** | Все | P0 | ✅ (stats, info) |
| 3 | **Нет скрытых GPU метрик в Dashboard** | Dashboard | P1 | ✅ |
| 4 | **Model details не в основных таблицах** | Monitor/Models | P1 | ✅ |
| 5 | **Disk/Network метрики не отображаются** | Monitor/Dashboard | P2 | ✅ |
| 6 | **Нет настроек агентов** | Settings | P2 | ❌ |
| 7 | **Per-backend limits не через Settings** | Settings | P2 | ✅ |
| 8 | **Нет API для agent restart/stop** | Backend | P1 | ❌ |
| 9 | **Agent не может конфигурировать Ollama** | Agent | P2 | ❌ |
| 10 | **Model.expiresAt не отображается** | Monitor/Models | P3 | ✅ |

---

## 6. Рекомендации по реализации

### Этап 1 (P0 — Критично)
1. **Добавить кнопку "Manage Models" на Models tab** → вызывает существующий `openModelManageModal` с first available backend или позволяет выбрать
2. **Создать страницу "Agents"**:
   - Новая вкладка в navigation
   - Таблица агентов из `/api/v1/agents/stats`
   - Детали агента из `/api/v1/agents/{id}`
   - Индикация статуса, uptime, platform mode

### Этап 2 (P1 — Важно)
3. **Добавить hidden GPU metrics tooltips** в Dashboard backends table (как в Monitor)
4. **Добавить Model details** (family, format, parameterSize, quantization) в modelsGrid и backendsTable
5. **Добавить Disk/Network секцию** в expandable row details на Backends page

### Этап 3 (P2 — Улучшение)
6. **Добавить настройки агентов** в Settings:
   - Collect Interval (сек)
   - Heartbeat Interval (сек)
   - Max Concurrent Requests
   - Max Models
7. **Создать API для управления агентом**:
   - `POST /api/v1/agents/{id}/restart`
   - `GET /api/v1/agents/{id}/logs`

### Этап 4 (P2 — Архитектурное)
8. **Реализовать механизм Agent→Ollama конфигурации**:
   - Балансировщик отправляет желаемые настройки Ollama в heartbeat response
   - Агент применяет их через Ollama API или файл конфигурации
   - Интерфейс в Settings для установки Ollama runtime flags per-backend

---

## 7. Технические детали

### 7.1 Файлы для изменений

| Файл | Назначение |
|------|-----------|
| `webui/index.html` | HTML-шаблоны вкладок, новая вкладка Agents |
| `webui/js/app.js` | Orchestrator: init, WebSocket, CRUD, navigation |
| `webui/js/modules/renderers.js` | Render functions для новых секций |
| `webui/js/modules/api.js` | API вызовы для новых эндпоинтов |
| `webui/css/monitor-app.css` или `webui/css/custom.css` | Стили |
| `internal/api/handlers.go` | Новые API эндпоинты для управления агентами |
| `internal/api/routes.go` | Регистрация новых маршрутов |
| `internal/agent/collector.go` | Agent→Ollama config (архитектурно) |

### 7.2 Структура данных для Agents page

```json
// GET /api/v1/agents/stats
{
  "totalAgents": 3,
  "healthyAgents": 2,
  "agents": [
    {
      "id": "gpu-node-01",
      "host": "192.168.1.101",
      "agentPort": 18032,
      "status": "healthy",
      "platform": "gpu",
      "uptime": 86400,
      "lastHeartbeat": "2026-05-11T12:00:00Z",
      "hasAgent": true
    }
  ]
}
```

### 7.3 Структура данных для Agent details

```json
// GET /api/v1/agents/gpu-node-01
{
  "id": "gpu-node-01",
  "host": "192.168.1.101",
  "ollamaPort": 11434,
  "agentPort": 18032,
  "status": "healthy",
  "hasAgent": true,
  "lastAgentContact": "2026-05-11T12:00:00Z",
  "freeVram": 6376,
  "metrics": {
    "gpu": { "usagePercent": 65.2, "memoryUsed": 18200, ... },
    "system": { "cpuUsagePercent": 45.1, ... },
    "ollama": { "runningModels": [...], "availableModels": [...] }
  }
}
```

---

## 8. История изменений

| Дата | Версия | Изменения |
|------|--------|-----------|
| 2026-05-11 | 1.0 | Первая версия — полный аудит WebUI vs Monitor |
| 2026-05-11 | 1.1 | ✅ Реализован P0: страница Agents, Manage Models на Models tab, model details в grid |
| 2026-05-11 | 1.2 | ✅ Реализован P1: model details tooltip в backendsTable, Disk/Network в expandable row, Disk/Network в renderAgentDetails, исправлены пустые строки в шаблонах |
| 2026-05-11 | 1.3 | ✅ Реализован P2: Agent Settings UI (i18n + loadSettings + settingsFields + collectCommonSettings + applyConfig), agent restart/logs API endpoints (agent-side + balancer proxy), обновлена версия документа |
| 2026-05-11 | 1.4 | ✅ Реализован P2: Per-backend limits через Settings (loadBackendLimits + saveBackendLimits в app.js, обработчик saveBackendLimitsBtn, вызов при switchPage('settings'), backendLimits в collectCommonSettings/applyConfig в config-io.js, i18n ключи backend_limits) |
| 2026-05-11 | 1.5 | ✅ Добавлены models search/filter + refresh button на Models tab, i18n ключ models.search_placeholder. Обновлена сводная таблица: Dashboard 🟢100%, Models 🟢80%, Settings 🟢100%, Agents 🟢100%. |
| 2026-05-11 | 1.6 | ✅ Добавлена панель "💾 Диск / 🌐 Сеть" в Monitor (monitor.html + ui-renderer.js). Disk/Network метрики отображаются с барами использования диска и RX/TX значениями. DemoData обновлён. Monitor: 🟢90%→🟢100%. |

### ✅ Этап 5 (P1 — Реализован 2026-05-12)
| Дата | Версия | Изменения |
|------|--------|-----------|
| 2026-05-12 | 1.8 | ✅ **Load/Unload/Delete кнопки в Models grid** — добавлены кнопки в `modelsGrid` (renderers.js), CSS стили в data.css, глобальный обработчик `window.modelCardAction` (app.js). i18n ключи `models.load/unload/delete/confirm_delete`. |
| 2026-05-12 | 1.8 | ✅ **Agent Restart/Logs/Config кнопки в Backends expandable row** — добавлены кнопки "Restart", "Logs", "Config" для бэкендов с агентом в expandable row (renderers.js). CSS стили для `.agent-actions`. |
| 2026-05-12 | 1.8 | ✅ **API endpoints для агента** — `Api.restartAgent(backendId)` и `Api.agentLogs(backendId, limit)` в api.js. |
| 2026-05-12 | 1.8 | ✅ **Handler функции в app.js** — `restartAgent()` и `viewAgentLogs()` с модальным окном для логов. |
| 2026-05-12 | 1.8 | ✅ **i18n ключи для agent actions** — `agents.restart/view_logs/config/confirm_restart/restarting/restarted/restart_error/loading_logs/logs_title/logs_error/no_logs` в en.js и ru.js. |
| 2026-06-15 | 1.9 | ✅ **Исправлены URL agent restart/logs** — `Api.restartAgent` и `Api.agentLogs` теперь используют `/api/v1/agents/{id}/restart` и `/api/v1/agents/{id}/logs`, соответствующие реализованным эндпоинтам в `internal/api/handlers_agents.go`. Ранее UI звал `/api/v1/backends/{id}/restart|logs`, что возвращало 404. |

### ✅ Этап 6 (P1 — Реализован 2026-06-05) — gguf Settings tab
| Дата | Версия | Изменения |
|------|--------|-----------|
| 2026-06-05 | 1.9 | ✅ **Backend load options в gguf Settings tab** — `gguf-renderer.js renderSettingsPane()` переписан: вместо «мёртвой» формы `gguf-settings-form`, которая меняла только локальный `state.loadOptions`, рендерит (1) Backend load options, загружаемые через `GET /api/v1/cppworker/config` и сохраняемые через `PUT /api/v1/cppworker/config/update` для выбранного cppworker'а. Поля: `defaultCtxSize`, `defaultBatchSize`, `defaultGpuLayers`, `defaultFlashAttnType`, `defaultNuma`, `defaultUseMmap`, `defaultNThreads` + read-only `nodeName`/`balancerUrl`/`uptime`. |
| 2026-06-05 | 1.9 | ✅ **Per-Model Profiles в gguf Settings tab** — в Settings tab gguf-страницы монтируется список Per-Model профилей n_ctx + кнопка «Добавить профиль» через переиспользование `window.CppWorkerParams.loadAndRender()` / `openWizard()` (cppworker-params.js). Для незарегистрированных бэкендов (alternate URL) показывается баннер-предупреждение. |
| 2026-06-05 | 1.9 | ✅ **i18n ключи** — новые `gguf.backend_options_title/desc`, `gguf.save/reload_backend_options`, `gguf.backend_options_loaded/saved/load_error/save_error`, `gguf.flash_attn_type/desc`, `gguf.n_threads/desc`, `gguf.profiles_section_title/desc`, `gguf.profiles_only_registered`, `gguf.config_unavailable` в en.js (918 keys) и ru.js (824 keys). |
| 2026-06-05 | 1.9 | ✅ **Синтаксис JS** — `node --check` на `gguf-renderer.js` и `cppworker-params.js` — OK. i18n en.js/ru.js — валидны (vm.runInNewContext). |

---

## Сводка после реализации

| Компонент | Статус | Реализовано |
|-----------|--------|-------------|
| Monitor (monitor.html) | 🟢 100% | Все панели + Disk/Network |
| Dashboard | 🟢 100% | Все секции + hidden GPU tooltips |
| Backends | 🟢 100% | CRUD + expandable details + Disk/Network + **Agent actions (Restart/Logs/Config)** |
| Models | 🟢 100% | Grid + manage modal + search/filter + refresh + **Load/Unload/Delete buttons** + model details + expiresAt/digest/RAM |
| Sessions | 🟢 100% | Полная таблица |
| Queue | 🟢 100% | Метрики + задачи + история |
| Logs | 🟢 100% | System + Proxy |
| Settings | 🟢 100% | Все секции + настройки агентов + per-backend limits |
| Agents (в целом) | 🟢 100% | 5 API эндпоинтов + полный UI + **agent actions в Backends** |
| Agent→Ollama config | 🔴 0% | Агент не умеет писать в Ollama (архитектурное ограничение) |

**WebUI Gap Analysis — 100% P0-P2 ✅**

---

| 2026-05-11 | 1.7 | ✅ Реализован P3: Model.expiresAt + digest + RAM в renderOllamaParams/renderAgentDetails/modelsGrid (renderers.js). i18n ключи renderers.digest/expires/expired. Model.expiresAt + digest в Monitor renderModelsInMemory (ui-renderer.js). Обновлён monitor.html (колонка "Истекает"). Models: 🟢80%→🟢95%. |

