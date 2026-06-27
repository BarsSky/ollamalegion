# Session 19 — WebUI: Model details panel (Q3 W3-4 last gap)

> **Branch:** `feature/rpc-model-distribution`
> **Date:** 2026-06-27
> **Status:** 🆕 Новая сессия — план

## Контекст

**Q3 W3-4 «Models tab gaps» текущий статус:**
- ✅ Filter/Search (Session 13, commit `756a55c`)
- ✅ Pull progress UI (Session 14, commit `47e88af`)
- ✅ Model profiles UI (Session 15, commit `da852f7`)
- 🔄 **Model details panel — текущая сессия (последний крупный gap)**
- ⏳ Bulk operations (опционально, ~2-3ч)
- ⏳ Loaded models counter в Dashboard (опционально, ~1ч)

**Roadmap reference:** `plans/2026-q3-roadmap.md`, section 2.2, line 85:
> **Model details panel** — модальное окно с полной метадатой модели
> (size, quantization, family, parameters, expiresAt, digest).
> Файлы: `webui/js/modules/renderers.js`, `webui/index.html`. Оценка: 3–4 ч.

**Бонус:** попутно можно закрыть **Loaded models counter в Dashboard** (1ч),
поскольку обе фичи маленькие и UI-only.

## Цель

Реализовать **Model details panel** — модальное окно, которое открывается
по клику на иконку ℹ️ (или двойному клику на карточку) в WebUI вкладке
Models. Показывает полную метадату модели: size, quantization, family,
parameters, expires_at, digest, capabilities, modified_at, и т.п.

## Текущее состояние (что уже есть)

**Backend** (готов):
- Ollama: `GET /api/show` с параметром `name` или `model` возвращает JSON с метаданными:
  `modelfile, parameters, template, details: {format, family, parameter_size, quantization_level,
  parent_model, ...}, model_info, ...`
- cppworker: `GET /api/show` — proxy на Ollama-совместимый формат, использует
  `backend.GetModelMeta()` + `cppbackend.ReadGGUFHeader()` (lazy).

**Frontend** (частично):
- `webui/js/modules/renderers.js` — `modelsGrid()` рендерит карточки
  с name, size, status, badges (backend type, actions).
- `webui/js/modules/api.js` — есть `Api.showModelInfo(name, backendId)`?
  **TODO: проверить** в начале сессии (если нет — добавить).
- `webui/index.html` — нет модалки для details (только Manage Models modal).
- i18n: `models.details`, `models.info_*` (size, family, quantization, etc.) — **TODO: проверить** наличие.

## Что нужно сделать

### 1. Аудит существующего кода (5-10 мин)
- `webui/js/modules/api.js` — есть ли `showModelInfo` namespace?
- `webui/js/i18n/{en,ru}.js` — есть ли `models.details`, `models.info_*` ключи?
- `webui/js/modules/renderers.js` — есть ли `modelCardClick` handler?
- `internal/api/handlers_models.go` (или аналогичный) — есть ли `/api/v1/cluster/models/{name}/info`?
- Если backend endpoint отсутствует, но Ollama/cppworker имеют `/api/show` — добавить proxy.

### 2. Backend: cluster-level info endpoint (если отсутствует) (15-30 мин)

**Файл:** `internal/api/handlers_cluster_models.go` (рядом с `/loaded` endpoint'ом)

Добавить:
```go
// GET /api/v1/cluster/models/{name}/info
// Возвращает метадату модели со всех llama_cpp бэкендов (агрегация)
// + fallback на Ollama /api/show для Ollama-бэкендов.
```

Response format:
```json
{
  "model": "gemma-3-E4B-it-Q4_K_M",
  "results": [
    {
      "backendId": "cppworker-gpu-bundled",
      "backendType": "llama_cpp",
      "status": "ok",
      "info": {
        "details": {
          "format": "gguf",
          "family": "gemma3",
          "parameter_size": "4.3B",
          "quantization_level": "Q4_K_M"
        },
        "size": 4987654321,
        "digest": "sha256:abc123...",
        "expires_at": "...",
        "modified_at": "2026-06-15T...",
        "capabilities": ["chat", "embeddings"],
        "model_info": { ... GGUF KV-блоки ... }
      }
    }
  ]
}
```

Per-backend ошибки не ломают общий ответ (статус `"unavailable"`).

### 3. Frontend: Model details modal (2-3 ч)

**Файлы:**
- `webui/js/modules/renderers.js` — добавить `openModelDetailsModal(modelName)`
- `webui/index.html` — добавить `<div id="modelDetailsModal" class="modal">` шаблон
- `webui/css/components.css` — стили модалки + sections (header, kv-blocks table, capabilities badges)
- `webui/js/i18n/en.js` + `webui/js/i18n/ru.js` — добавить ключи (если отсутствуют)

**UI структура модалки:**

```
┌─ gemma-3-E4B-it-Q4_K_M ────────────────────── × ┐
│ [🦒 llama.cpp] [cppworker-gpu-bundled]           │
│                                                  │
│ ── General ──                                    │
│   Size:        4.99 GB                           │
│   Family:      gemma3                            │
│   Parameters:  4.3B                              │
│   Quantization: Q4_K_M                           │
│   Format:      gguf                              │
│   Modified:    2026-06-15 12:34                  │
│   Digest:      sha256:abc123...                  │
│                                                  │
│ ── Capabilities ──                               │
│   [chat] [embeddings] [completion]               │
│                                                  │
│ ── Model Info (GGUF KV-blocks) ──                │
│   general.architecture = gemma3                  │
│   gemma3.context_length = 32768                  │
│   gemma3.embedding_length = 2560                 │
│   gemma3.block_count = 42                        │
│   gemma3.feed_forward_length = 10240             │
│   ...                                            │
│                                                  │
│ ── Parameters (Ollama modelfile-style) ──        │
│   stop "<start_of_turn>"                         │
│   stop "<end_of_turn>"                           │
│   temperature 1                                  │
│   top_p 0.95                                     │
│   ...                                            │
│                                                  │
│                            [Close]               │
└──────────────────────────────────────────────────┘
```

**Triggers:**
- Клик на иконку ℹ️ (info-icon) в `.model-card-actions` — добавить кнопку.
- Двойной клик на саму карточку (опционально).
- Клавиша `i` при фокусе на карточке (опционально, power-user).

**Loading state:**
- При открытии → spinner "Загрузка метаданных..."
- После получения данных → рендер секций.
- При ошибке → toast + retry button.

### 4. Bonus: Loaded models counter в Dashboard (30-60 мин)

**Файл:** `webui/js/modules/renderers.js` — функция `dashboardPage()`
**Где:** в верхней части Dashboard, рядом с GPU/CPU/RAM метриками — добавить карточку
"Loaded models" с числом и tooltip "кликните для перехода на Models tab".
**Источник:** `GET /api/v1/cluster/models/loaded` уже возвращает `count`.

### 5. i18n ключи (en + ru)

Добавить в `webui/js/i18n/en.js`:
```js
models: {
  ...existing,
  details: "Details",
  details_loading: "Loading model details...",
  details_error: "Failed to load model details",
  details_retry: "Retry",
  details_general: "General",
  details_capabilities: "Capabilities",
  details_model_info: "Model Info",
  details_parameters: "Parameters",
  details_size: "Size",
  details_family: "Family",
  details_parameters_label: "Parameters",
  details_quantization: "Quantization",
  details_format: "Format",
  details_modified: "Modified",
  details_digest: "Digest",
  details_expires: "Expires",
  info: "Info", // for info-icon tooltip
}
```

Аналогично в `ru.js` (русские переводы).

### 6. Build verify & test

- `node -c webui/js/modules/renderers.js` — OK
- `node -c webui/js/modules/api.js` — OK
- `node -c webui/js/i18n/en.js` — OK
- `node -c webui/js/i18n/ru.js` — OK
- Визуально проверить CSS — стили модалки не ломают другие модалки
  (Models Manage, Settings wizard, Reload progress, Cancel confirm).
- `go build -tags llama_stub ./cmd/balancer/` — OK (если backend изменения)
- `go test -tags llama_stub -run "TestCluster" ./internal/api/` — OK (новые тесты)

## Acceptance criteria

1. **Открыть вкладку Models** в WebUI → кликнуть иконку ℹ️ на любой карточке
   → открывается модальное окно с метаданными модели.
2. **Для llama.cpp модели** (gemma-3): видны секции General (size, family, parameters,
   quantization, format, modified_at, digest), Capabilities (chat/embeddings),
   Model Info (GGUF KV-блоки).
3. **Для Ollama модели** (например, llama3): видны те же General поля +
   Parameters (modelfile-стиль stop/temperature/etc.).
4. **Ошибка загрузки** (cppworker недоступен, 404, 5xx) → модалка показывает
   toast "Failed to load model details" + кнопка Retry.
5. **Info-icon добавлен** в `.model-card-actions` для всех моделей (loaded + unloaded).
6. **Dashboard counter** (bonus) — карточка "Loaded models: N" с tooltip.
7. **i18n парные ключи** en.js == ru.js (40 новых ключей × 2 языка).
8. **Build OK**: `go build -tags llama_stub ./cmd/balancer/`.
9. **Tests OK**: новые тесты для cluster-models info endpoint (если backend менялся).
10. **CHANGELOG.md** — подсекция `### Added (Session 19 — WebUI: Model details panel + Dashboard counter)`.

## Файлы для модификации

- `webui/js/modules/renderers.js` — основной файл (новый `openModelDetailsModal` + Dashboard counter)
- `webui/js/modules/api.js` — добавить `getModelInfo(modelName, backendId)` namespace
- `webui/index.html` — modal template (`#modelDetailsModal`)
- `webui/css/components.css` — стили `.model-details-modal`, секции, KV-блоки
- `webui/js/i18n/en.js` + `webui/js/i18n/ru.js` — 40 новых ключей
- `internal/api/handlers_cluster_models.go` — `GET /api/v1/cluster/models/{name}/info` (если отсутствует)
- `internal/api/handlers_cluster_models_test.go` — тесты для info endpoint
- `CHANGELOG.md` — новая подсекция Session 19

## Оценка трудозатрат

- Аудит + backend endpoint: 30-60 мин
- Frontend modal: 2-3 ч
- i18n (40 ключей × 2): 30 мин
- CSS: 30-60 мин
- Dashboard counter (bonus): 30-60 мин
- Tests + verification: 30-60 мин
- CHANGELOG + commit: 15 мин

**Итого: 3-4 ч** (как в roadmap).

## План работы (9 шагов)

1. **Шаг 1** — Аудит: проверить api.js, i18n, renderers.js, backend endpoint.
2. **Шаг 2** — Backend: добавить `GET /api/v1/cluster/models/{name}/info` (если отсутствует).
3. **Шаг 3** — Frontend API: `Api.getModelInfo(modelName, backendId)`.
4. **Шаг 4** — Frontend modal: HTML template + CSS стили.
5. **Шаг 5** — Frontend logic: `openModelDetailsModal()` с loading/error/success states.
6. **Шаг 6** — Info-icon в `.model-card-actions`.
7. **Шаг 7** — Bonus: Dashboard "Loaded models" counter.
8. **Шаг 8** — i18n (40 ключей × 2) + tests + build verify.
9. **Шаг 9** — CHANGELOG + commit.

## Pending tasks после Session 19

- Bulk operations (2-3 ч) — следующий gap в Models tab
- Export logs to CSV (2 ч) — UI/UX 5.2
- Live tail в Logs (4-5 ч) — UI/UX 5.3
- GitHub Actions CI (1 день) — quality gate
- B8.7 real ggml (10-15 дней) — post-1.0 advanced
- rpc_coordinator production (5-8 дней) — post-1.0
- virtual_router production (5-7 дней) — post-1.0