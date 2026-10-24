# Полный аудит и план исправлений OllamaLegion

> Дата: 2026-06-17
> Статус: ✅ Анализ завершён, часть фиксов готова к применению
> Документ отслеживает все проблемы: от конфигурационных до архитектурных

---

## 📋 Содержание

1. [Сводка открытых проблем](#1-сводка-открытых-проблем)
2. [Фаза 0: Demo Mode — экстренный фикс](#2-фаза-0-demo-mode)
3. [Фаза 1-4: WebUI-баги (B-01–B-08, B-11)](#3-фаза-1-4-webui-баги)
4. [Фаза 5: Streaming-ошибки](#4-фаза-5-streaming-ошибки)
5. [Фаза 6: n_ctx auto-reload](#5-фаза-6-n_ctx-auto-reload)
6. [Фаза 7: Конфигурация и развёртывание](#6-фаза-7-конфигурация-и-развёртывание)
7. [Трекер выполнения](#7-трекер-выполнения)

---

## 1. Сводка открытых проблем

### Критические (production)

| ID | Описание | Фаза | Статус |
|---|---|---|---|
| **P-1** | `n_ctx=128000` клиента клампится к 4096 через `DefaultModelProfile.ContextLength` | Фаза 6 | ❌ Анализ готов |
| **P-2** | RAM fallback выключен (default `false`) — VRAM не хватает → модель не перезагружается | Фаза 6 | ❌ Анализ готов |
| **P-3** | "Server disconnected" при тяжёлых моделях — авто-загрузка блокирует клиента на 30+ сек | Фаза 5 | ❌ Анализ готов |
| **P-4** | `TransferEncodingError` — некорректный Content-Type при streaming-ошибке | Фаза 5 | ✅ Исправлено |
| **P-5** | Demo Mode показывает только `ollama` бэкенды, нет `llama_cpp` | Фаза 0 | ❌ Анализ готов |

### WebUI-баги (функциональные)

| ID | Описание | Файл | Статус |
|---|---|---|---|
| **B-01** | Agents tab всегда виден для `llama_cpp` | `renderers.js` | ❌ |
| **B-02** | `runtimeCluster()` показывает Ollama-флаги для ВСЕХ бэкендов | `renderers.js` | ❌ |
| **B-04** | Порт всегда отображается как `b.ollamaPort \|\| 11434` | `renderers.js` | ❌ |
| **B-05** | `renderOllamaParams()` не проверяет backend type | `renderers.js` | ❌ |
| **B-06** | `modelsGrid()` не вызывает `getBackendTypeBadge()` | `renderers.js` | ❌ |
| **B-07** | `toggleAgentsTab()` не вызывается корректно | `renderers.js` | ❌ |
| **B-08** | Pull Model видна для `llama_cpp` в `modelsGrid()` | `renderers.js` | ❌ |
| **B-11** | Нет бейджа backend type в Monitor Models in Memory | `ui-renderer.js` | ❌ |

---

## 2. Фаза 0: Demo Mode

### Проблема
`monitor/api.js:demoData()` (строки 66-183) возвращает жёстко закодированные `ollama`-бэкенды. Когда API недоступен, `enterDemoMode()` рендерит только Ollama-бэндинги, что вводит в заблуждение в `llama_cpp`-среде.

### Архитектурная причина
```javascript
function demoData() {
    return [
        { id: "demo-backend-1", name: "Ollama Server 1", ... , type: "ollama" },
        { id: "demo-backend-2", name: "Ollama Server 2", ... , type: "ollama" },
        // ↑ только ollama, нет llama_cpp
    ];
}
```

### Фикс
1. Добавить `llama_cpp` бэкенды в `demoData()`
2. Добавить визуальный индикатор "DEMO MODE" (затемнённый баннер)
3. Предотвратить авто-демо, если есть реальные данные в `MA.backends`

---

## 3. Фаза 1-4: WebUI-баги

### B-01/B-07: Agents tab
**Где:** `renderers.js`
**Причина:** `renderLlamaCppBackend()` не фильтрует `hasAgent` для `backend_type = 'llama_cpp'`
**Фикс:** Добавить проверку `backend_type` при рендеринге кнопок агента

### B-02: Ollama-флаги в runtimeCluster
**Где:** `renderers.js` → `runtimeCluster()`
**Причина:** Функция проверяет только наличие поля `b.ollamaPort`, а не `b.type`
**Фикс:** 
```javascript
if (b.type === 'ollama' && b.ollamaPort) {
    renderOllamaFlags(b, html);
}
```

### B-04: Порт бэкенда
**Где:** `renderers.js`
**Причина:** `b.ollamaPort || 11434` — для `llama_cpp` нужно `b.cppWorkerPort || 18091`
**Фикс:**
```javascript
const port = (b.type === 'llama_cpp') ? (b.cppWorkerPort || 18091) : (b.ollamaPort || 11434);
```

### B-05: Ollama-параметры
**Где:** `renderers.js` → `renderOllamaParams()`
**Причина:** Вызывается для всех бэкендов без проверки
**Фикс:** Добавить `if (b.type === 'ollama')` guard

### B-06: Бейджи в grid моделей
**Где:** `renderers.js` → `modelsGrid()`
**Причина:** Не вызывает `getBackendTypeBadge()`
**Фикс:** Добавить вызов для каждой модели в grid

### B-08: Pull Model для llama_cpp
**Где:** `renderers.js` → `modelsGrid()`
**Причина:** В модальном окне (B-08 fix) кнопка скрывается, но в `modelsGrid()` нет
**Фикс:** 
```javascript
if (b.backend_type !== 'llama_cpp') {
    html += `<button class="pull-btn">Pull Model</button>`;
}
```

### B-11: Monitor бейдж
**Где:** `ui-renderer.js` → `renderModelsInMemory()`
**Причина:** Не показывает бейдж `[llama_cpp]` / `[ollama]`
**Фикс:** Добавить бейдж backend type в карточку модели

---

## 4. Фаза 5: Streaming-ошибки

### P-3: "Server disconnected" при тяжёлых моделях

**Цепочка событий:**
```
1. Cline отправляет запрос к большой модели (70B+)
2. ensureModelLoadedOnBackend() → POST /api/models/load
3. CppWorker блокируется на CGo LoadModel (30-60 сек)
4. streamingClient (ResponseHeaderTimeout=0) ждёт
5. Если другой запрос приходит параллельно → 503 "model is loading"
6. OpenWebUI видит 503 → "Server disconnected"
```

**Корень #1:** Нет размера модели в GGUF → нет автоматического расчёта таймаута
**Корень #2:** При параллельных запросах все получают 503, а не queue-механизм

### P-4: TransferEncodingError

**Цепочка событий:**
```
1. CppWorker возвращает JSON-ошибку (не streaming) для streaming-запроса
2. proxyRequestLlamaCpp не проверяет Content-Type ответа cppworker
3. Go видит w.WriteHeader + последующий Write → chunked encoding
4. Клиент ждёт NDJSON → получает chunked encoding → TransferEncodingError
```

**Фикс:**
1. В `proxyRequestLlamaCpp()` проверять `Content-Type` upstream ответа перед входом в streaming-цикл
2. Если upstream вернул `application/json` (ошибка), проксировать как JSON, не входя в SSE-ридер
3. Удалить `resp.Header.Del("Transfer-Encoding")` в `handleStreamingResponse()` — Go управляет этим автоматически
4. Увеличить дефолтный `StreamTimeout` с 600s до динамического на основе размера модели

---

## 5. Фаза 6: n_ctx auto-reload

### Полный поток проблемы

```
Cline → {"model":"Qwen3.6-35B-A3B", "options":{"num_ctx":128000}}
  ↓
Балансировщик (handleGenerate / llamacpp_router.go):
  1. ExtractNumCtxFromBody(body) = 128000 ✅
  2. ResolveNumCtx(model="Qwen3.6-35B-...", body, backendID):
     a. Tier 1 (body): 128000 ↑
     b. Tier 2 (maxNumCtxForModel):
        - GetModelProfileNumCtx() = 0 (нет per-model profile)
        - GetDefaultModelProfileNumCtx() = 4096 ❌ ← **ВОТ ОН!**
        - getModelLoadedCtxFromMetrics() — не доходит, т.к. 4096 > 0
     c. Результат: clamped 128000 → 4096
  3. ApplyCppCtxHeader → X-Cpp-Ctx: 4096 (header)
  4. Тело запроса НЕ меняется (options.num_ctx = 128000 остаётся)
  ↓
CppWorker:
  1. buildGenerationParams: NCtxOverride = 128000 (из body)
  2. applyCppCtxHeader: X-Cpp-Ctx = 4096
     - NCtxOverride (128000) > headerLimit (4096) → CLAMP → 4096
  3. Инференс с NCtxOverride = 4096
  4. Prompt > 4096 токенов → bridge code 2 (ErrNCtxNeedsReload)
     {code:2, current_n_ctx:4096, required_n_ctx:128000, max_vram_n_ctx:4096, ...}
  ↓
Балансировщик (proxyRequestLlamaCpp):
  1. ParseCppWorkerError → *NCtxError{code=2, required=128000, max_vram=4096}
  2. handleNCtxReload → DecideReloadBackend:
     - AutoReloadNCtx = false (kill-switch) → DecisionNoOp
     - Если бы true: max_vram=4096, safety=0.85, safe_max=3481
       required=128000 > 3481 → DecisionReject
  3. HTTP 413/400 клиенту
  ↓
Cline видит: "requested n_ctx=128000 exceeds model's effective n_ctx=4096"
```

### Корневые причины (4 проблемы)

#### 🔴 Проблема #1: defaultModelProfile.ContextLength = 4096 (ГЛАВНАЯ)

**Файл:** `config/config.json:136`
```json
"defaultModelProfile": {
    "contextLength": 4096,
    ...
}
```

Функция `maxNumCtxForModel()` в `num_ctx_resolver.go:194`:
```go
// Tier 2: default profile из config (Phase D.3-fix)
if v := p.GetDefaultModelProfileNumCtx(); v > 0 {
    return v  // ← 4096 — clamping никогда не достигает Tier 3 (loaded model metrics)
}
```

**Фикс:** Убрать `contextLength` из `defaultModelProfile` или поднять до 65536.

**⚠️ Важно:** При полном удалении `contextLength` clamping будет полагаться на:
1. Per-model profile (если создан пользователем)
2. Реальный n_ctx загруженной модели из metrics (Tier 3)
3. Если оба 0 — clamping не производится, cppworker сам решает

#### 🟡 Проблема #2: max_vram_n_ctx = 4096

Для Qwen 35B Q4_K_M это слишком мало. Даже 8GB VRAM должно давать 8192-16384. Возможные причины:
- CppWorker стартует с `--ctx-size 4096` и bridge считает max_vram от текущего n_ctx
- GPU не детектится (CUDA не установлена?)
- Используется CPU-only режим без GPU layers

**Диагностика:** `GET /api/models` на cppworker + логи `nvidia-smi`

#### 🟡 Проблема #3: RAM fallback выключен

**Файл:** `cmd/cppworker/main.go:49`
```go
ramFallbackNCtx = flag.Bool("ram-fallback-n-ctx", false, ...)
```

Default `false`. Если не передан `--ram-fallback-n-ctx` или env `CPPWORKER_RAM_FALLBACK_N_CTX=true`, при недостатке VRAM cppworker НЕ пробует перезагрузить модель в RAM.

**Фикс:** Включить через `CPPWORKER_RAM_FALLBACK_N_CTX=true` в deploy/env.

#### 🟡 Проблема #4: AutoReloadNCtx на балансировщике выключен

**Файл:** `internal/balancer/nctx_reload.go:107`
```go
func DefaultNCtxReloadConfig() NCtxReloadConfig {
    return NCtxReloadConfig{
        AutoReloadNCtx: false,  // ← default!
        ...
    }
}
```

В `config/config.json` нет секции `balancing.nctxReload` — значит всегда `AutoReloadNCtx=false`.  
Даже если включить — VRAM лимит 4096 всё равно не даст reload до 128000.

---

## 6. Фаза 7: Конфигурация и развёртывание

### docker-compose.cppworker-bundled.yml

Необходимо убедиться, что:
1. CppWorker получает `CPPWORKER_RAM_FALLBACK_N_CTX=true`
2. Балансировщик имеет доступ к конфигу с исправленным `defaultModelProfile`
3. `LB_NCTX_RELOAD_ENABLED=true` для включения auto-reload на балансировщике

### Конфигурация `.env.bundled`

Добавить переменные:
```env
# RAM fallback для n_ctx (позволяет перезагрузить модель в RAM если VRAM не хватает)
CPPWORKER_RAM_FALLBACK_N_CTX=true
CPPWORKER_RAM_FALLBACK_MAX_N_CTX=128000
CPPWORKER_RAM_FALLBACK_GPU_LAYERS=0

# n_ctx auto-reload на балансировщике
LB_NCTX_RELOAD_ENABLED=true
LB_NCTX_RELOAD_MAX_N_CTX=131072
LB_NCTX_RELOAD_VRAM_SAFETY_FACTOR=0.85
LB_NCTX_RELOAD_TIMEOUT_SEC=120
```

---

## 7. Трекер выполнения

### ✅ Завершённый анализ

| Задача | Файлы исследованы | Статус |
|---|---|---|
| Demo mode stub | `monitor/api.js`, `ui-renderer.js` | ✅ Анализ |
| WebUI Dashboard (B-01–B-08) | `renderers.js` (1473 строки), `app.js`, `api.js` | ✅ Анализ |
| Monitor (B-11) | `ui-renderer.js`, `state.js`, `init.js` | ✅ Анализ |
| Streaming ошибки (P-3, P-4) | `proxy_request.go`, `streaming.go`, `llamacpp_transport.go`, `llamacpp_router.go`, `router.go` | ✅ Анализ |
| n_ctx auto-reload (P-1, P-2) | `nctx_reload.go`, `nctx_reload_handlers.go`, `nctx_reload_config_bridge.go`, `num_ctx_resolver.go`, `llamacpp_error.go`, `nctx_clamp.go`, `cmd/cppworker/main.go`, `config/config.json` | ✅ Анализ |

### ⬜ План фиксов (очередность)

```
Срочно (сейчас):
  [x] Шаг 1: config/config.json — убрать contextLength из defaultModelProfile
  [x] Шаг 2: .env.bundled — добавить CPPWORKER_RAM_FALLBACK_N_CTX=true
  [x] Шаг 3: Перезапустить стек

Документация:
  [x] Шаг 4: docs/nctx-troubleshooting.md — документ с диагностикой n_ctx

WebUI (после стабилизации):
  [x] Шаг 5-12: B-01 до B-08 (renderers.js)
  [x] Шаг 13: B-11 (ui-renderer.js)
  [x] Шаг 14: Demo mode fix (monitor/api.js)

Streaming (после стабилизации):
  [x] Шаг 15: Content-Type проверка в proxyRequestLlamaCpp
  [x] Шаг 16: Удалить resp.Header.Del("Transfer-Encoding")
  [x] Шаг 17: Динамический таймаут на основе размера модели
```

### Подробный трекер

| # | ID | Приоритет | Файл(ы) | Изменение | Сложность |
|---|---|---|---|---|---|
| 1 | F-01 | 🔴 CRIT | `config/config.json` | `contextLength: 0` или `65536` | 5 мин |
| 2 | F-02 | 🔴 CRIT | `.env.bundled` | `CPPWORKER_RAM_FALLBACK_N_CTX=true` | 2 мин |
| 3 | F-03 | 🔴 CRIT | `deployments/docker-compose.cppworker-bundled.yml` | Проверить проброс env | 5 мин |
| 4 | F-04 | 🟡 HIGH | `monitor/api.js` | Добавить llama_cpp в demoData() | 15 мин |
| 5 | F-05 | 🟡 HIGH | `monitor/api.js` | DEMO MODE баннер | 10 мин |
| 6 | F-06 | 🟡 HIGH | `renderers.js` | B-01/B-07: Agents tab guard | 10 мин |
| 7 | F-07 | 🟡 HIGH | `renderers.js` | B-02: Ollama-флаги guard | 10 мин |
| 8 | F-08 | 🟡 HIGH | `renderers.js` | B-04: Порт для llama_cpp | 5 мин |
| 9 | F-09 | 🟡 HIGH | `renderers.js` | B-05: renderOllamaParams guard | 5 мин |
| 10 | F-10 | 🟡 HIGH | `renderers.js` | B-06: Бейджи в grid | 15 мин |
| 11 | F-11 | 🟡 HIGH | `renderers.js` | B-08: Pull Model guard | 10 мин |
| 12 | F-12 | 🟡 HIGH | `ui-renderer.js` | B-11: Monitor бейдж | 10 мин |
| 13 | F-13 | 🟡 MED | `llamacpp_transport.go` | Content-Type check before streaming loop | ✅ Готово |
| 14 | F-14 | 🟡 MED | `streaming.go` | Удалить resp.Header.Del("Transfer-Encoding") | ✅ Готово |
| 15 | F-15 | 🟢 LOW | `proxy_request.go`, `proxy.go`, `model_latency_tracker.go` | Динамический StreamTimeout из GGUF metadata | ✅ Готово |

---

## Приложение A: Ключевые архитектурные моменты

### Как работает 3-tier resolver для n_ctx

```
ResolveNumCtx(model, body, backendID)
  │
  ├─ Tier 1: ExtractNumCtxFromBody(body)
  │   └─ options.num_ctx (Ollama) или num_ctx (OpenAI)
  │
  ├─ Tier 2: maxNumCtxForModel(model, backendID)
  │   ├─ per-model profile (LlamaCppModelProfiles[name].ContextLength)
  │   ├─ defaultModelProfile.contextLength ⚡ ← **4096**
  │   └─ loaded model metrics (ContextLength из llamaMetrics)
  │
  └─ Результат: clamped к потолку из Tier 2
```

### Как X-Cpp-Ctx header применяется на cppworker

```
ApplyCppCtxHeader(r, params):
  1. Читает X-Cpp-Ctx header = headerLimit
  2. Если params.NCtxOverride <= 0:
     params.NCtxOverride = headerLimit (дефолт от балансировщика)
  3. Если params.NCtxOverride > headerLimit:
     params.NCtxOverride = headerLimit (UPPER LIMIT semantics) ⚡
  4. NPredict = NCtxOverride / 2 (если NPredict == default)
```

### Взаимодействие RAM fallback и auto-reload

```
tryRamFallbackReload(model, requestedNCtx):
  1. if !ramFallbackNCtx → return false (выключено)
  2. if requestedNCtx > ramFallbackMaxNCtx → reject
  3. UnloadModel()
  4. LoadModelWithOpts(n_ctx=requestedNCtx, use_mmap=true, gpu_layers=fallbackGpuLayers)
  5. if success → retry inference
  6. if fail → return original error
```

---

## Приложение B: Как проверить исправление

### После фикса #1 (config.json)

```bash
# Проверить, что defaultModelProfile.contextLength = 0
curl -s http://localhost:18081/api/v1/config | jq '.defaultModelProfile.contextLength'
# Должно быть 0 или null
```

### После фикса #2 (RAM fallback)

```bash
# Проверить, что cppworker получил env
docker exec ollamalegion-cppworker-gpu-1 sh -c 'echo $CPPWORKER_RAM_FALLBACK_N_CTX'
# Должно быть "true"
```

### Интеграционный тест n_ctx

```bash
# Запрос с большим num_ctx
curl -X POST http://localhost:18081/api/generate \
  -H 'Content-Type: application/json' \
  -H 'X-API-Token: <token>' \
  -d '{"model":"Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive-Q4_K_M","prompt":"Hello","options":{"num_ctx":32000}}'

# Проверить, что ответ содержит поле "context_size" > 4096
# Или проверить логи балансировщика на предмет "reloading with n_ctx=N"
```

---

*Документ создан 2026-06-17. Последнее обновление: 2026-06-17.*
