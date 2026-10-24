# Реальный план оставшихся задач

## Дата: 2026-05-11
## Контекст: Анализ кода показал, что 90% Блоков 2 и 5 уже реализованы

---

## Итоги анализа кода

### Блок 2 (Оптимизация распределения) — 14 из 18 задач ✅ уже в коде

| # Задачи | Статус | Комментарий |
|----------|--------|-------------|
| 2.1.1 activeSlots исправлен | ✅ | `slot_manager.go` — правильная логика, нет `Min(1, ...)` |
| 2.1.2 Model Affinity + capacity | ✅ | `backend_selector.go` — loadRatio < threshold |
| 2.1.3 Подгрузка модели | ✅ | `findLessLoadedBackendAny()` + `warmupModel()` |
| 2.2.1-2.2.4 Фаза 1 (баги) | ✅ | Все исправления в коде |
| 2.3.1-2.3.4 Фаза 2 (expandCandidates) | ✅ | `candidate.go`, `types.go` |
| 2.4.1-2.4.5 Фаза 3 (scoring) | ✅ | `scoring.go` — полная enhanced формула |
| 2.5.1-2.5.4 Фаза 4 (queue dispatch) | ✅ | `queue_dispatch.go`, `dispatchAffinity/Load/Config` |
| 2.6.1 Candidate Backends в мониторе | ✅ | `webui/monitor.html` — панель candidates; `internal/api/handlers.go: handleCandidates()` |
| 2.6.2 Model Load Feasibility | ✅ | `webui/monitor.html` — индикатор VRAM; `webui/js/monitor/ui-renderer.js: renderFeasibility()` |
| 2.6.3 Load-тест: 4 users × 2 models | ✅ | `scripts/load_test_comprehensive.py` — Сценарий A (4 пользователя, чередование моделей) |
| 2.6.4 Load-тест: full load + warmup | ✅ | `scripts/load_test_comprehensive.py` — Сценарий B (3 фазы: normal → overload → warmup) |
| 2.6.5 balancing-guide.md | ✅ | Актуален (проверено) |
| 2.7 Параметры конфигурации | ✅ | Все параметры в коде |
| 2.8 Фоновые контроллеры | ✅ | Prewarm, ModelInstance, Unload, WeightTuner |

### Блок 5 (Расширение API Ollama) — 10 из 10 задач ✅ уже в коде

| # Endpoint | Статус | Файл |
|------------|--------|------|
| POST /api/show | ✅ | `ollama_router.go: handleShow()` |
| POST /api/create | ✅ | `ollama_router.go: handleCreate()` |
| POST /api/copy | ✅ | `ollama_router.go: handleCopy()` |
| POST /api/pull | ✅ | `ollama_router.go: handlePull()` |
| POST /api/push | ✅ | `ollama_router.go: handlePush()` |
| POST /api/delete | ✅ | `ollama_router.go: handleDelete()` |
| GET /api/version | ✅ | `ollama_router.go: handleVersion()` |
| Агрегация /api/tags | ✅ | `ollama_router.go: handleTags()` |
| Маршрутизация моделей | ✅ | `findBackendWithModel()` |
| Проксирование запросов | ✅ | `proxyHTTP()` |

---

## Реальный план работ (4 задачи)

### Задача 1: Секция «Candidate Backends» в мониторе
- **Описание**: Добавить новую панель в `monitor.html`, которая показывает, какие бэкенды являются кандидатами P1-P4 для каждой модели
- **Файлы**: `webui/monitor.html`, `webui/js/monitor/ui-renderer.js`, `webui/js/monitor/api.js`
- **API**: Нужен новый endpoint в `internal/api/handlers.go` — `/api/v1/candidates` (возвращает expandCandidates для каждой модели)

### Задача 2: Индикатор «Model Load Feasibility»
- **Описание**: Показывать, сколько моделей ещё можно загрузить на каждый бэкенд (VRAM free / avg model size)
- **Файлы**: `webui/js/monitor/ui-renderer.js`, `webui/monitor.html`

### Задача 3: Load-тест «4 пользователя, 2 модели»
- **Описание**: Добавить сценарий в `load_test_comprehensive.py`

### Задача 4: Load-тест «Полная загрузка + подгрузка на свободный»
- **Описание**: Добавить сценарий с 3 этапами: normal → overload → warmup на free backend

---

**Оценка времени:** ~4-6 часов
**Приоритет:** P3 (расширенная функциональность)
