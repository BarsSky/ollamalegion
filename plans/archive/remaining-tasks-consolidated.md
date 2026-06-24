# Консолидированный список оставшихся задач OllamaLegion

> **Дата:** 2026-06-18
> **Источник:** Анализ 13 файлов-трекеров из `docs/` и `plans/` + верификация кода
> **Цель:** Единый актуальный список реальных недоделок, без дублирования и устаревших данных

---

## Легенда

| Статус | Обозначение | Описание |
|--------|-------------|----------|
| ✅ | Выполнено | Задача реализована (подтверждено кодом) |
| ❌ | Не реализовано | Задача требует выполнения |
| ⚠️ | Частично | Часть функциональности реализована |
| 🏗️ | Архитектурное | Требует архитектурного решения |
| 🔴 | P0 | Критично — блокирует production |
| 🟡 | P1 | Важно — ограничивает функциональность |
| 🟢 | P2 | Улучшение — не блокирует работу |
| ⚪ | P3 | Косметика / архитектурный долг |

---

## 1. Выполненные этапы (для истории)

Все задачи в перечисленных ниже планах **полностью выполнены** и не требуют повторной реализации:

| План/Трекер | Статус | Задач | Дата последнего обновления |
|------------|--------|-------|---------------------------|
| `docs/audit-report-2026-04-28.md` | ✅ 100% | 10/10 | 2026-05-08 |
| `docs/audit-tracking.md` | ✅ 100% | 10/10 | 2026-05-08 |
| `plans/consolidated-plan-2026-05-03.md` | ✅ 100% | 148/148 | 2026-05-13 |
| `plans/implementation-plan-2026-05-05-remaining.md` | ✅ 100% | 92/92 | 2026-05-06 |
| `plans/unimplemented-elements-plan-2026-05-06.md` | ✅ 100% | 13/13 | 2026-05-07 |
| `docs/remaining-real-plan.md` | ✅ 100% | 28/28 | 2026-05-11 |
| `docs/refactoring-roadmap.md` | ✅ 100% | Все фазы (1-6) | 2026-05-13 |
| `docs/webui-gap-analysis.md` | ✅ 100% P0-P2 | 100% | 2026-06-15 |
| Backend type isolation | ✅ 100% | 19/19 (в коде) | 2026-06-14 (коммит `affb4e8`) |
| P0 баги аудита (Master Token, TLS, утечка) | ✅ 100% | 3/3 | 2026-04-28 |
| Стабилизация стриминга (S.1–S.8) | ✅ 100% | 8/8 | 2026-05-06 |
| Queue Dispatch метрики | ✅ 100% | 3/3 | 2026-05-07 |
| WebUI Монитор рефакторинг | ✅ 100% | 5/5 | 2026-05-07 |
| Фоновые контроллеры (Unload, WeightTuner) | ✅ 100% | 2/2 | 2026-05-07 |
| Сбор modelSize агентом | ✅ 100% | 1/1 | 2026-05-07 |
| Документация API | ✅ 100% | 2/2 | 2026-05-07 |
| n_ctx — config.json contextLength: 4096→0 | ✅ | 1/1 | 2026-06-17 |
| n_ctx — RAM fallback + auto-reload config | ✅ | 2/2 | 2026-06-17 |
| Streaming P-4 (TransferEncodingError) | ✅ | 1/1 | 2026-06-09 |
| WebUI B-01..B-08, B-11 (backend type badges) | ✅ | 8/8 | 2026-06-14 |

### Ключевые реализованные архитектурные решения

- **Backend type isolation** (коммит `affb4e8`):
  - `selectBackend(model, bt)` — фильтрация по типу
  - `expandCandidates(modelName, allowedTypes)` — фильтрация кандидатов
  - `proxyRequest` — проверка `IsModeCompatibleWithBackendType()`
  - `getBackendPort()` — для `llama_cpp` нет fallback на `OllamaPort`
  - `normalizeBackendType()` — пустой тип → `ollama` (обратная совместимость)
  - CRUD валидация через `handlers_backends.go`
  - 15 тестов в `tests/backend_type_isolation_test.go` (858 строк)

- **n_ctx resolver**:
  - 3-tier: body → per-model profile → default profile / loaded metrics
  - `X-Cpp-Ctx` header как upper limit
  - RAM fallback (`CPPWORKER_RAM_FALLBACK_N_CTX=true`)
  - Auto-reload на балансировщике (`AutoReloadNCtx: true`)

- **WebUI Gap Analysis** (P0-P2):
  - Agents page (5 API endpoints + full UI)
  - Model management (Load/Unload/Delete в grid)
  - Agent actions (Restart/Logs/Config)
  - Disk/Network панель в Monitor
  - gguf Settings tab (backend load options + per-model profiles)
  - i18n (en.js 918 keys, ru.js 824 keys)

---

## 2. Реально оставшиеся задачи

### 2.1 Backend type isolation — WebUI индикаторы (P1 🟡)

Задачи, описанные в `docs/backend-type-isolation-plan.md` Этап 4-5, которые **ещё НЕ реализованы**:

| # | Задача | Файл | Функция | Приоритет |
|---|--------|------|---------|-----------|
| 1.1 | Бейджи 🦙/🦒 в `backendsTable()` Dashboard | `webui/js/modules/renderers.js` | `backendsTable()` | 🟡 P1 |
| 1.2 | Бейджи 🦙/🦒 в `capacityCard()` | `webui/js/modules/renderers.js` | `capacityCard()` | 🟡 P1 |
| 1.3 | Бейджи 🦙/🦒 в `backendsPage()` management table | `webui/js/modules/renderers.js` | `backendsPage()` | 🟡 P1 |
| 1.4 | CSS-стили `.backend-type-*` | `webui/css/custom.css` | — | 🟡 P1 |
| 1.5 | Фильтры «Все \| 🦙 Ollama \| 🦒 llama.cpp» в Dashboard | `webui/js/modules/renderers.js`, `webui/index.html`, `webui/js/app.js` | — | 🟢 P2 |
| 1.6 | Фильтры в Backends management table | `webui/js/modules/renderers.js` | — | 🟢 P2 |
| 1.7 | Фильтры в Monitor таблице | `webui/js/monitor/ui-renderer.js`, `webui/monitor.html` | — | 🟢 P2 |

**Оценка времени:** 3-5 часов

---

### 2.2 Unfinished code audit — build-tag stub'ы (P3 ⚪)

Из `docs/unfinished-code-audit.md`:

| # | Задача | Файл | Тип | Приоритет |
|---|--------|------|-----|-----------|
| 2.1 | NVML stub (build-tag) — корректное поведение, не требует изменений | `internal/agent/nvml_stub.go` | 🏗️ Архитектурный долг | ⚪ P3 |
| 2.2 | NVML Windows stub (build-tag) — корректное поведение | `internal/agent/nvml_windows.go` | 🏗️ Архитектурный долг | ⚪ P3 |
| 2.3 | llama.cpp stub (build-tag) — корректное поведение | `c/bridge/llama.go_stub` | 🏗️ Архитектурный долг | ⚪ P3 |
| 2.4 | `webui/js/modules/setup-wizard.js` — частичный выбор типа бэкенда | `webui/js/modules/setup-wizard.js` | 🟡 P1 | ⚪ P3 |
| 2.5 | `webui/js/modules/cocoindex.js` — предполагает только Ollama API | `webui/js/modules/cocoindex.js` | 🟢 P2 | ⚪ P3 |

**Комментарий:** Build-tag stub'ы являются архитектурным долгом, но корректно работают. Для Windows GPU метрик требуется реализация через WMI/DXGI (см. п. 2.4).

**Оценка времени:** 0 часов (stub'ы корректны)

---

### 2.3 Пропущенные тесты (P2 🟢)

| # | Задача | Файл | Строка | Проблема | Приоритет |
|---|--------|------|--------|----------|-----------|
| 3.1 | `TestTransferEncoding_BackendReturns503ThenRecovers` | `tests/proxy_streaming_test.go` | 638 | Условный skip при `connection refused` | 🟢 P2 |
| 3.2 | `TestSelectFreeBackendAny_Integration` | `tests/balancer_optimization_test.go` | 236 | `t.Skip("Cannot parse mock server URLs")` | 🟢 P2 |
| 3.3 | `TestE2E_ProxyStreaming` | `tests/integration_test.go` | 708 | Условный skip | 🟢 P2 |

**Оценка времени:** 2-3 часа

---

### 2.4 Windows GPU metrics (P2 🟢)

| # | Задача | Файл | Приоритет |
|---|--------|------|-----------|
| 4.1 | Реализовать `getGPUInfo()` через WMI/DXGI или `nvidia-smi` для Windows | `internal/agent/system_windows.go` | 🟢 P2 |
| 4.2 | Реализовать `getGPUMetrics()` для Windows | `internal/agent/system_windows.go` | 🟢 P2 |

**Комментарий:** `getNetworkIO()` уже реализован через `netstat -e` (проверено в `docs/unfinished-code-audit.md`). GPU метрики — единственная недостающая часть.

**Оценка времени:** 6-10 часов

---

### 2.5 Agent → Ollama config (архитектурное ограничение 🏗️)

| # | Задача | Статус | Приоритет |
|---|--------|--------|-----------|
| 5.1 | Реализовать механизм конфигурации Ollama через heartbeat response | ⬜ Не начато | 🟢 P2 |

**Комментарий:** Агент НЕ МОЖЕТ задавать настройки Ollama через REST API — Ollama не имеет публичного API изменения конфигурации. Единственный способ — env vars + restart. Это **архитектурное ограничение**, не баг. Подробнее: `docs/webui-gap-analysis.md` §4.

**Оценка времени:** Отложено до появления API у Ollama

---

### 2.6 Документация (P3 ⚪)

| # | Задача | Файл | Приоритет |
|---|--------|------|-----------|
| 6.1 | Создать `docs/backend-type-isolation.md` с описанием архитектуры типов | `docs/backend-type-isolation.md` | ⚪ P3 |
| 6.2 | Актуализировать `.clinerules` — раздел по backend type isolation | `.clinerules` | ⚪ P3 |
| 6.3 | Актуализировать `.clinerules` — раздел по Windows stub тестам | `.clinerules` | ⚪ P3 |

**Комментарий:** `.clinerules` уже частично обновлён в текущей сессии разработки.

**Оценка времени:** 1-2 часа

---

## 3. Архитектурные ограничения (не планируются к реализации)

| Ограничение | Причина | Статус |
|------------|---------|--------|
| Agent → Ollama runtime config | Ollama не имеет REST API для изменения конфигурации | ❌ Не реализуемо |
| `ollama pull <model>` для llama.cpp | Нет Ollama registry для llama.cpp backend | ✅ Возвращает HTTP 501 |
| RPC распределённый инференс | `internal/rpccoordinator/` создан, pipeline не завершён | 🏗️ P4 (не в этом релизе) |
| Virtual Model Router | `internal/virtualmodel/` создан, slice-to-slice execution не завершён | 🏗️ P4 (не в этом релизе) |

---

## 4. Сводная статистика

| Категория | Всего | Выполнено | Осталось |
|-----------|-------|-----------|----------|
| Блок 1: Backend type isolation (WebUI) | 7 | 0 | 7 |
| Блок 2: Build-tag stub'ы (арх. долг) | 3 | 0 | 3 |
| Блок 3: Пропущенные тесты | 3 | 0 | 3 |
| Блок 4: Windows GPU metrics | 2 | 0 | 2 |
| Блок 5: Agent→Ollama config (отложено) | 1 | 0 | 1 |
| Блок 6: Документация | 3 | 0 | 3 |
| **Итого реально** | **19** | **0** | **19** |
| Уже выполнено (подтверждено кодом) | ~200+ | 200+ | 0 |

---

## 5. Рекомендуемый порядок выполнения

```
Неделя 1 (приоритет):
  [ ] 1.1-1.4 — Бейджи 🦙/🦒 в Dashboard (P1) — 2-3 часа
  [ ] 1.5-1.7 — Фильтры по типу (P2) — 2-3 часа

Неделя 2:
  [ ] 3.1-3.3 — Исправление пропущенных тестов (P2) — 2-3 часа
  [ ] 6.1-6.3 — Документация (P3) — 1-2 часа

Неделя 3 (по возможности):
  [ ] 4.1-4.2 — Windows GPU metrics (P2) — 6-10 часов
  [ ] 2.4-2.5 — setup-wizard + cocoindex (P3) — 2-3 часа
```

---

## 6. Устаревшие планы (неактуальны, не использовать)

Следующие документы **НЕ ОТРАЖАЮТ** текущее состояние кода и содержат задачи, которые уже реализованы:

| Документ | Причина неактуальности |
|----------|----------------------|
| `docs/backend-type-isolation-plan.md` | Все 19 задач УЖЕ РЕАЛИЗОВАНЫ в коде (коммит `affb4e8`) |
| `plans/remaining-tasks-plan-2026-06-14.md` (Этап 1) | Утверждает 0% backend type isolation — в коде 100% |
| `docs/audit-comprehensive-plan.md` (фазы WebUI) | B-01..B-08, B-11 уже исправлены |
| `docs/remaining-real-plan.md` | Все 4 задачи реализованы |
| `plans/implementation-plan-2026-05-05-remaining.md` | 0/92 задач осталось |

**Важно:** При планировании работ ориентироваться ТОЛЬКО на текущий консолидированный документ.

---

## 7. История изменений

| Дата | Версия | Изменения |
|------|--------|-----------|
| 2026-06-18 | 1.0 | Первая версия — консолидация 13 трекеров, верификация по коду |
