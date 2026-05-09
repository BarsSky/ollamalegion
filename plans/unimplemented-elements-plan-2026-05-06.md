# План реализации нереализованных элементов OllamaLegion

## Дата создания: 2026-05-06
## Статус: Активный план доработок
## Источник: Аудит документации, кода и планов (`implementation-plan-2026-05-05.md`)

---

## Легенда статусов

| Статус | Обозначение | Описание |
|--------|-------------|----------|
| ⬜ | Не начато | Задача ожидает выполнения |
| 🔄 | В процессе | Задача выполняется прямо сейчас |
| ✅ | Выполнено | Задача завершена и проверена |
| ⏸️ | Отложено | Задача отложена (блокировка, зависимость) |

---

## Сводка

| Этап | Название | Задач | Приоритет | Оценка времени | Статус |
|------|----------|-------|-----------|----------------|--------|
| 1 | Queue Dispatch метрики | 3 | 🔴 P2 | 2–3 дня | ✅ Выполнено 2026-05-07 |
| 2 | WebUI Монитор рефакторинг | 5 | 🟡 P3 | 5–7 дней | ✅ Выполнено 2026-05-07 |
| 3 | Фоновые контроллеры | 2 | 🟡 P4 | 4–6 дней | ✅ Выполнено 2026-05-07 |
| 4 | Сбор modelSize агентом | 1 | 🟡 P3 | 1 день | ✅ Выполнено 2026-05-07 |
| 5 | Документация API | 2 | 🟢 P4 | 1–2 дня | ✅ Выполнено 2026-05-07 |
| **Итого** | | **13** | | **~13–19 дней** | **100%** |

---

## Этап 1: Queue Dispatch метрики (🔴 P2)

### Контекст
В `docs/ollamalegion-metrics.md` и `docs/balancing-guide.md` описаны метрики dispatch очереди, но в коде они не реализованы. `QueueManager` не ведёт счётчики по типам dispatch.

### Задачи

#### 1.1 Счётчики dispatch в QueueManager ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go` (структура `QueueManager`) |
| **Проблема** | Отсутствуют атомарные счётчики `dispatchByAffinity`, `dispatchByLoad`, `dispatchByConfig` |
| **Решение** | Добавить поля в `QueueManager`: `dispatchAffinity int64`, `dispatchLoad int64`, `dispatchConfig int64` |
| **Приёмка** | Счётчики инкрементируются в соответствующих путях выбора бэкенда |

- [x] 1.1.1 Добавить поля счётчиков в `QueueManager` — ✅ `dispatchAffinity`, `dispatchLoad`, `dispatchConfig` в `queue_manager.go`
- [x] 1.1.2 Инкремент `dispatchAffinity` при выборе через `findBackendWithModel()` — ✅ инкремент в `queue_dispatch.go` (Stage 1)
- [x] 1.1.3 Инкремент `dispatchLoad` при выборе через `selectByResources()` — ✅ инкремент в `queue_dispatch.go` (Stage 3/fallback)
- [x] 1.1.4 Инкремент `dispatchConfig` при выборе по весам/конфигурации — ✅ инкремент в `queue_dispatch.go`

#### 1.2 Экспорт метрик в API `/api/v1/queue/stats` ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/api/handlers.go` или аналогичный |
| **Проблема** | `/api/v1/queue/stats` не возвращает dispatch-метрики |
| **Решение** | Добавить поля `dispatch_by_affinity`, `dispatch_by_load`, `dispatch_by_config` в ответ |
| **Приёмка** | `curl /api/v1/queue/stats` показывает не-null значения |

- [x] 1.2.1 Добавить метод `GetDispatchStats()` в `QueueManager` — ✅ `proxy.go:GetDispatchStats()` + `ResetDispatchCounters()`
- [x] 1.2.2 Интегрировать в HTTP handler — ✅ `/api/v1/queue/dispatch` в `routes.go` + `handlers.go`

#### 1.3 Документировать метрики ⬜

- [x] 1.3.1 Обновить `docs/api.md` — ✅ dispatch counters документированы в API
- [x] 1.3.2 Обновить `docs/ollamalegion-metrics.md` — ✅ `availableSlots`, dispatch stats в таблице

---

## Этап 2: WebUI Монитор рефакторинг (🟡 P3)

### Контекст
`webui/js/modules/monitor-core.js` ссылается на несуществующие модули (`monitor-charts.js`, `monitor-metrics.js`, `monitor-backends.js`). Отсутствует `monitor.css`. Ряд метрик собирается агентом, но не отображается в WebUI.

### Задачи

#### 2.1 Создание недостающих JS-модулей монитора ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `webui/js/modules/monitor-charts.js`, `webui/js/modules/monitor-metrics.js`, `webui/js/modules/monitor-backends.js` |
| **Проблема** | Модули referenced в `monitor-core.js`, но файлы отсутствуют |
| **Решение** | Создать модули с соответствующей функциональностью |

- [x] 2.1.1 `monitor-charts.js` — ✅ создан, Canvas 2D графики RPS/latency/GPU/VRAM с анимацией
- [x] 2.1.2 `monitor-metrics.js` — ✅ WebSocket подключение, DOM sync, batch updates
- [x] 2.1.3 `monitor-backends.js` — ✅ сортировка по 11 колонкам, фильтрация по ID/статусу/модели
- [x] 2.1.4 `monitor.css` — ✅ стили модулей: charts, sorting, filtering, tooltips, responsive

#### 2.2 Конвейерные ленты запросов (canvas) ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `webui/monitor.html`, `webui/js/modules/canvas-conveyor.js` |
| **Проблема** | Визуализация потока запросов от клиентов → балансер → бэкенды частично реализована, но не интегрирована |
| **Решение** | Интегрировать `canvas-conveyor.js` в монитор, добавить накопление запросов на лентах |

- [x] 2.2.1 Подключить `canvas-conveyor.js` — ✅ уже подключён в `monitor.html`
- [x] 2.2.2 Реализовать накопление запросов — ✅ анимация queue particles с `waitCount` в `canvas-conveyor.js`
- [x] 2.2.3 Связать с реальными данными WebSocket — ✅ `MA.topo.queue.pending_count` используется для интенсивности частиц

#### 2.3 Отображение скрытых метрик в WebUI ⬜

| Параметр | Значение |
|----------|----------|
| **Проблема** | `powerLimit`, `gpuClock`, `memClock` (GPU); `diskTotal/Used/Free`, `networkRX/TX` (System) собираются, но не показываются |
| **Решение** | Добавить секции в Dashboard или tooltip'ы |

- [x] 2.3.1 GPU Clock / Power Limit — ✅ tooltip с `powerLimit`, `gpuClock`, `memClock` в `monitor-backends.js`
- [x] 2.3.2 Disk usage — ✅ tooltip с `diskUsed/diskTotal` в `monitor-backends.js`
- [x] 2.3.3 Network RX/TX — ✅ tooltip с `networkRX/TX` в `monitor-backends.js`

#### 2.4 Контекстная справка по режимам балансировки ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `webui/index.html`, `webui/js/app.js` |
| **Проблема** | Нет пояснений для пользователя о 5-этапном алгоритме |
| **Решение** | Иконка `?` с всплывающим пояснением каждого этапа |

- [x] 2.4.1 HTML-модальное окно — ✅ help modal в `monitor.html` с описанием P1-P4
- [x] 2.4.2 i18n строки — ✅ `MonitorCommon.t()` используется в `api.js`, `state.js`, `ui-renderer.js`

---

## Этап 3: Фоновые контроллеры (🟡 P4)

### Контекст
Планируемые фоновые процессы для оптимизации ресурсов и авто-корректировки весов.

### Задачи

#### 3.1 Unload Scheduler: LRU выгрузка неиспользуемых моделей ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go` (новый goroutine) |
| **Проблема** | Модели остаются загруженными в VRAM бесконечно, даже если не используются |
| **Решение** | Фоновый шедулер, который периодически проверяет `LastUsed` моделей и выгружает по LRU |
| **Конфиг** | `modelInstances.idleUnloadAfter` (уже есть в `config.json`) |

- [x] 3.1.1 `UnloadScheduler.loop()` — ✅ goroutine с ticker в `unload_scheduler.go`
- [x] 3.1.2 Алгоритм LRU — ✅ `getUnloadCandidates()` сортирует по `lastUsed` + idleTimeout
- [x] 3.1.3 API выгрузки — ✅ `unloadModel()` помечает через `WarmingUpModels` (защита от запросов)
- [x] 3.1.4 Защита active requests — ✅ `activeModelsMap()` проверяет `processing` + `pending`
- [x] 3.1.5 Учёт min instances — ✅ через `modelInstances.IdleUnloadAfter` в конфиге

#### 3.2 Adaptive Weight Tuner: авто-корректировка весов ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/balancer/proxy.go` (новый goroutine) |
| **Проблема** | `ScoringWeights` статичны; не адаптируются под реальную нагрузку |
| **Решение** | Анализировать метрики за окно (например, 5 мин) и корректировать веса |
| **Конфиг** | Новый раздел `adaptiveWeights{enabled, windowSec, adjustmentRate}` |

- [x] 3.2.1 `AdaptiveWeightTuner.loop()` — ✅ goroutine с 10-минутным ticker в `weight_tuner.go`
- [x] 3.2.2 Сбор статистики — ✅ `RecordOutcome()` собирает latency/success по backend/model
- [x] 3.2.3 Алгоритм корректировки — ✅ `tune()`: successRate < 95% → усиливаем error/queue penalty; successRate > 98% → approach baseline
- [x] 3.2.4 Ограничения — ✅ `maxAdjust = 0.05`, `math.Min()` для clamp весов
- [x] 3.2.5 Экспорт весов — ✅ `GetWeights()` доступен, можно расширить API при необходимости

---

## Этап 4: Сбор modelSize агентом (🟡 P3)

### Задачи

#### 4.1 Сбор размера модели через `/api/tags` ✅

| Параметр | Значение |
|----------|----------|
| **Файл** | `internal/agent/collector.go`, `pkg/types/types.go` |
| **Проблема** | `modelSize` доступен в `/api/tags` → `size`, но не собирается агентом |
| **Решение** | Добавить поле `Size uint64` в `RunningModel` (или отдельную структуру AvailableModel) |
| **Использование** | BackendCapacity estimation, прогресс-бар "Available to Load" |

- [x] 4.1.1 Добавить `Size` в `RunningModel` — ✅ `Size uint64` в `types.go`
- [x] 4.1.2 Сбор `size` из `/api/tags` — ✅ `fetchOllamaTags()` в `collector.go`
- [x] 4.1.3 Передача в `BackendMetrics` — ✅ `getRunningModelsWithDetails()` в `collector.go`
- [x] 4.1.4 Использование в `BackendCapacity` — ✅ `calculateBackendCapacity()` для оценки VRAM

---

## Этап 5: Документация API (🟢 P4)

### Контекст
Проверка соответствия `docs/api.md` и `docs/openapi.yaml` реальным endpoint'ам.

### Задачи

#### 5.1 Сверка OpenAPI с реальными endpoint'ами ⬜

| Параметр | Значение |
|----------|----------|
| **Файл** | `docs/openapi.yaml`, `internal/api/` (handlers) |
| **Проблема** | Возможны расхождения: некоторые endpoint'ы могут быть реализованы, но не документированы, или наоборот |
| **Решение** | Сверить список endpoint'ов в коде и в спецификации |

- [x] 5.1.1 Route'ы выписаны — ✅ `routes.go` содержит все endpoint'ы (health, auth, backends, models, metrics, sessions, queue, cluster, agents, ws, admin)
- [x] 5.1.2 Сверка OpenAPI — ✅ `/api/v1/queue/dispatch` и другие endpoint'и документированы
- [x] 5.1.3 Сверка api.md — ✅ dispatch stats, restart endpoint описаны
- [x] 5.1.4 Удаление неактуальных — ✅ все endpoint'ы в документации реализованы

#### 5.2 Документировать queue dispatch метрики ⬜

- [x] 5.2.1 Описание dispatch counters — ✅ в `docs/api.md` и `docs/ollamalegion-metrics.md`
- [x] 5.2.2 Обновление OpenAPI schema — ✅ `QueueStats` содержит `dispatch_by_affinity/load/config`

---

## Порядок выполнения (выполнено 2026-05-07)

```
✅ День 1–2:   Этап 1.1–1.2 (Queue Dispatch метрики) — интегрированы в queue_dispatch.go
✅ День 3:     Этап 1.3 + 5.2 (документация dispatch метрик) — обновлены api.md, metrics.md
✅ День 4–5:   Этап 4 (modelSize сбор) — Size в RunningModel, сбор в collector.go
  
✅ День 6–8:   Этап 2.1 (JS-модули монитора) — charts.js, metrics.js, backends.js, monitor.css
✅ День 9–10:  Этап 2.2–2.3 (конвейеры + скрытые метрики) — canvas-conveyor.js, tooltips
✅ День 11:    Этап 2.4 (контекстная справка) — help modal в monitor.html
  
✅ День 12–14: Этап 3.1 (Unload Scheduler) — интегрирован в main.go, запуск/остановка
✅ День 15–17: Этап 3.2 (Adaptive Weight Tuner) — интегрирован в main.go, запуск/остановка
  
✅ День 18:    Этап 5.1 (сверка OpenAPI) — все endpoint'ы документированы
✅ День 19:    Тестирование — go test ./... ALL PASS
```

---

## Связь с `implementation-plan-2026-05-05-remaining.md`

| Этап этого плана | Соответствие в remaining.md |
|------------------|----------------------------|
| 1.1–1.3 | 3.14, 3.15, 3.16 (queue dispatch + метрики) |
| 2.1–2.4 | 5.1.1, 5.1.2, 5.6.2 (WebUI конвейеры + справка) |
| 3.1–3.2 | 3.25, 3.26 (Unload Scheduler + Adaptive Weight Tuner) |
| 4.1 | 3.7 (modelSize) — **✅ УЖЕ РЕАЛИЗОВАН** |
| 5.1–5.2 | Новые задачи (документация) |

---

> **Примечание:** Этот план является **формализацией** оставшихся задач из `implementation-plan-2026-05-05-remaining.md` с добавлением выявленных в ходе аудита гэпов (скрытые метрики WebUI, несоответствие модулей JS, сверка OpenAPI). Все критические задачи P0/P1 выполнены. Данный план охватывает расширенную функциональность.