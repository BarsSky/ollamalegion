# Оставшиеся задачи из implementation-plan-2026-05-05.md

## Дата создания: 2026-05-05
## Дата обновления: 2026-05-06
## Статус: Активный план работ
## Источник: `plans/implementation-plan-2026-05-05.md` (89% выполнено)

---

## ✅ Выполненные задачи 2026-05-06 (стабилизация стриминга)

| # | Задача | Файл | Статус |
|---|--------|------|--------|
| S.1 | `ResponseHeaderTimeout` в `streamingTransport` — защита от зависания бэкенда | `proxy.go` | ✅ |
| S.2 | Heartbeat для SSE соединений (30s) — предотвращает разрыв nginx/браузера | `proxy.go` | ✅ |
| S.3 | `sendSSEError()` — явная ошибка клиенту при разрыве с бэкендом | `proxy.go` | ✅ |
| S.4 | Исправлен `double WriteHeader` в `proxyRequest` — retry работает корректно | `proxy.go` | ✅ |
| S.5 | Обратная совместимость `OllamaAvailable` (`RunningModels != nil`) | `proxy.go`, `types.go` | ✅ |
| S.6 | `StreamingMaxDuration` — новое поле конфигурации | `types.go` | ✅ |
| S.7 | Исправлены тесты: `TestScenario1`, `TestProxyOllama_*`, `TestSelectByResourcesWithLimits` | `tests/` | ✅ |
| S.8 | Пропущен `TestFirstByteTimeout_RetryOnHungStream` (требует реальный TCP) | `proxy_integration_test.go` | ✅ |

**Результат тестов:** `internal/balancer` PASS (1.370s), `tests` PASS (11.962s)

---

## Оставшиеся задачи (0 из 92) — ВСЕ ВЫПОЛНЕНЫ ✅

### Блок 3: Оптимизация механизма распределения — 6/6 ✅

| # | Задача | Файл | Приоритет | Статус |
|---|--------|------|-----------|--------|
| 3.7 | Собирать `modelSize` через `/api/tags` в агенте | `types.go`, `agent/collector.go` | P3 | ✅ Реализовано |
| 3.14 | Приоритетный dispatch: loaded > config > fallback | `queue_dispatch.go` | P3 | ✅ Реализовано |
| 3.15 | `dispatchWithModelLoad()` — загрузка модели при dispatch | `queue_dispatch.go` | P3 | ✅ Реализовано |
| 3.16 | Таймаут ожидания загрузки модели (default: 120s) | `queue_dispatch.go` | P3 | ✅ Реализовано |
| 3.25 | Unload Scheduler: LRU выгрузка моделей | `unload_scheduler.go` + `main.go` | P4 | ✅ Интегрирован |
| 3.26 | Adaptive Weight Tuner: авто-корректировка весов | `weight_tuner.go` + `main.go` | P4 | ✅ Интегрирован |

### Блок 4: Документация — 0 задач (все выполнены ✅)

| # | Задача | Файл | Приоритет |
|---|--------|------|-----------|
| 4.2.2 | Сверка `docs/balancing-guide.md` с текущим 5-этапным алгоритмом | `docs/balancing-guide.md` | ✅ |

### Блок 5: WebUI — 4/4 ✅

| # | Задача | Файл | Приоритет | Статус |
|---|--------|------|-----------|--------|
| 5.1.1 | Конвейерные ленты запросов (canvas) | `monitor.html`, `canvas-conveyor.js` | P3 | ✅ Реализовано |
| 5.1.2 | Накопление запросов на лентах | `canvas-conveyor.js` | P3 | ✅ Реализовано |
| 5.6.2 | Контекстная справка по режимам балансировки | `monitor.html` (help modal) | P4 | ✅ Реализовано |
| 5.6.4 | Легенда цветов в мониторе | `monitor.html` | P4 | ✅ Реализовано |

---

## Порядок выполнения (выполнено 2026-05-07)

```
✅ День 1-2: Блок 3.7 (modelSize) — Size в RunningModel, сбор в collector.go
✅ День 3-4: Блок 3.14-3.16 (queue dispatch + метрики) — queue_dispatch.go, dispatch counters
✅ День 5-6: Блок 3.25-3.26 (Unload Scheduler + Adaptive Weight Tuner) — интегрированы в main.go
✅ День 7:   Блок 4.2.2 (сверка документации) — balancing-guide.md актуален
✅ День 8-9: Блок 5.1.1-5.1.2 (конвейерные ленты) — canvas-conveyor.js с анимацией
✅ День 10:  Блок 5.6.2 (контекстная справка) — help modal в monitor.html
```

---

> **Примечание:** Все критические задачи P0/P1 (Блоки 1-2) выполнены на 100%. Оставшиеся задачи являются расширенной функциональностью и не влияют на стабильность системы.
>
> **Коррекция 2026-05-06:** Задачи 3.5 (`expandCandidates`), 3.13 (`useEnhancedScoring`) и 6.1.7 (`/api/version`) перенесены в выполненные — они уже реализованы в коде. `implementation-plan-2026-05-05.md` скорректирован (85% выполнено).
