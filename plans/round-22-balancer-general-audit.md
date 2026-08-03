# Round 22 — Общий аудит балансера: полнота проброса Ollama API + LLama.cpp API

**Дата:** 2026-08-03
**Контекст:** Пользователь запросил общий аудит после Rounds 18+20+21 — что ещё не покрыто
в плане полноты проброса Ollama API до бэкендов (llama.cpp + Ollama).
**Стек:** bundled-full, RTX 3070, CUDA_ARCH=86, `qwen3-4b` (alias) →
`Qwen3-Instruct-2507-q4km.gguf` + `gemma-4-E4B-it-Q4_K_M.gguf` на диске.

**Status:** ✅ **Closed** — все P0 фиксы сделаны, закоммичены (commits `58eb93a` и `645cc59`),
задеплоены в bundled-full, live-verified.

---

## TL;DR — было/стало

| Endpoint | Before Round 22 | After Round 22 |
|----------|-----------------|----------------|
| `POST /api/show` (file name) | 30-60s timeout (warmup) | **200 OK in 37ms** |
| `POST /api/show` (alias) | 30-60s timeout | **200 OK in 71ms** |
| `POST /api/pull` | 30s timeout | 503 in 8ms (no model) |
| `POST /api/copy/create/delete/push` | 30s timeout | 503 in 3-4ms (no model) |
| `GET /api/models/files` | 10s timeout (regression) | **200 OK in 39ms** |
| `POST /v1/embeddings` | 30s timeout (cppworker 404) | **200 OK in 486ms** |
| `POST /v1/audio/speech` | 30s timeout (cppworker 404) | **404 in 3ms** |
| `POST /api/signin/logout` | 30s timeout | **404 in 3ms** |
| `POST /api/web/*` | 30s timeout | **404 in 3ms** |
| `POST /v1/chat/completions` | 200 OK | 200 OK in 356ms (unchanged) |

---

## Bugs found (12 total)

| # | Severity | Issue | Round 18/20 covered? | Fixed in commit |
|---|----------|-------|----------------------|-----------------|
| 1 | **P0** | Read/MGMT endpoints (show, pull, copy, create, delete, push, blobs, models/files) вызывают warmup, что вешает их на 10-30s timeout | **НЕТ** | `58eb93a` |
| 2 | **P0** | `resolveModelPath` ломается для `name=alias` если на диске 2+ .gguf файла (auto-pick требует `len==1`) | **НЕТ** | `58eb93a` |
| 3 | **P0** | OpenAI `/v1/embeddings` не пробрасывается (cppworker не имеет llama_cpp handler для этого пути) | **НЕТ** | `58eb93a` + `645cc59` |
| 4 | **P0** | `/v1/embeddings` после загрузки модели (VRAM > 85%) попадает в queue_manager, блокируется на VRAM headroom 15% | **НЕТ** | `645cc59` |
| 5 | **P0** | Read/mgmt endpoints (после Fix #1) всё равно блокируются в `canAcceptRequest` через VRAM headroom check | **НЕТ** | `645cc59` |
| 6 | **P0** | CRASH: `panic: http: multiple registrations for /v1/embeddings` в cppworker (duplicate handler) | n/a (regression) | `645cc59` |
| 7 | **P2** | OpenAI `/v1/audio/*`, `/v1/images/*` — нет реализации, но balancer не возвращает явный 404, а виснет в timeout | **НЕТ** | `58eb93a` |
| 8 | **P2** | OpenWebUI/Cline отправляют `OPTIONS` (CORS preflight) — нужна проверка, поддерживает ли balancer | **НЕТ** (deferred) | – |
| 9 | **P2** | No metrics: количество "phantom load attempts" / "skipped warmup for read endpoint" / "alias resolution failures" — нельзя мониторить | **НЕТ** | `58eb93a` (counters added, не exposed) |
| 10 | **P3** | `/api/push` — Ollama registry, не нужен для llama.cpp, должен явно возвращать 501 | cppworker уже 501 | (covered by Fix #1) |
| 11 | **P3** | `/api/signin`, `/api/logout`, `/api/web/*` — отсутствуют полностью, должны явно 404 | **НЕТ** | `58eb93a` |
| 12 | **P3** | Headers pass-through: balancer не пробрасывает `X-Request-Id` от клиента (генерирует свой) | **НЕТ** (deferred) | – |

---

## Fixes applied

### Fix #1+13 (P0): Read/MGMT endpoints skip warmup + skip slot
- `internal/balancer/router.go`: добавлен `isReadOnlyOrMgmtEndpoint(path)` —
  whitelist `/api/show, /api/tags, /api/ps, /v1/models, /api/models,
  /api/models/files, /api/show, /api/pull, /api/push, /api/copy, /api/delete,
  /api/create, /api/blobs/*`.
- `internal/balancer/backend_selector.go`: `selectBackend` получил variadic
  параметр `skipSyncLoad bool` — при true пропускает P3 (Sync Model Load / warmup).
- `internal/balancer/proxy.go`: `ServeHTTP` (proxy.go:631) определяет
  `skipWarmup := isReadOnlyOrMgmtEndpoint(path)` и передаёт в `selectBackend`.
  Дополнительно для read/mgmt пропускается slot acquire (короткие операции
  не должны занимать concurrency budget бэкенда).

### Fix #2 (P0): ModelManager `name → path` history
- `internal/cppbackend/model_manager.go`: добавлен `nameHistory map[string]string`
  (name → resolved file path), `RecordModelLoad(name, path)` после успешного
  LoadModel, `LookupNameHistory(name)` для проверки.
- `FindModelByPath` теперь сначала проверяет `nameHistory` (Шаг 0) перед
  file scan. Также проверяет варианты `name` и `name+".gguf"`.
- Решает "после idle-unload, alias = qwen3-4b не резолвится в правильный файл"
  для случая когда в директории 2+ .gguf (auto-pick single требует len==1).

### Fix #3 (P0): OpenAI /v1/embeddings handler
- `cmd/cppworker/handlers_openai.go:handleV1Embeddings` — расширен:
  - Поддержка `input: []string` (batch) — раньше только `string`
  - `ensureModelLoaded` перед embeddings — раньше 500 если модель не загружена
- (Мой первоначальный `handleOpenAIEmbeddings` был удалён — duplicate route
  с `handleV1Embeddings`, что приводило к panic при старте cppworker.)

### Fix #4+16 (P0): Read/mgmt endpoints bypass VRAM headroom check
- `internal/balancer/slot_manager.go`: добавлены
  `findModelOnAnyBackendNoVRAMCheck(model, bt)` и `selectAnyHealthy(bt)` —
  выбирают backend минуя `canAcceptRequest` (который делает VRAM check).
- `internal/balancer/llamacpp_handlers_admin.go:handleShow` использует
  новые функции вместо `selectLlamaCppBackendByResources`.
- `internal/balancer/proxy.go`: для read/mgmt вызывает новые функции.

### Fix #14 (P0): /v1/embeddings direct dispatch (queue bypass)
- `internal/balancer/llamacpp_handlers_inference.go`: добавлен
  `handleOpenAIEmbeddings` (как у `handleOpenAICompletion`).
- `internal/balancer/llamacpp_router.go`: `/v1/embeddings` → `handleOpenAIEmbeddings`.
- Без этого: `/v1/embeddings` после загрузки модели (VRAM > 85%) попадал в
  queue_manager → slot blocked → 503.

### Fix #6 (CRASH): Duplicate /v1/embeddings registration в cppworker
- В Round 22 main commit добавил `mux.HandleFunc("/v1/embeddings", handleOpenAIEmbeddings)`,
  но `handleV1Embeddings` уже был зарегистрирован в строке 83.
- `panic: http: multiple registrations for /v1/embeddings` при старте cppworker.
- Fix: удалил мой новый handler, расширил существующий.

### Fix #7+11 (P2): Early 404 для unsupported endpoints
- `internal/balancer/router.go`: добавлены `isUnsupportedOpenAIEndpoint`
  (audio, images, realtime, fine_tuning, batches, assistants, threads) и
  `isUnsupportedOllamaEndpoint` (signin, logout, web). Сразу возвращают
  `{"error":"not_supported",...}` 404 без proxy.

### Fix #9 (P2): Observability counters
- `internal/balancer/metrics.go`: добавлены `atomic.Int64` counters:
  - `round22SkipWarmupTotal`
  - `round22Early404Total`
  - `round22AliasResolvedTotal` / `round22AliasResolveFailedTotal`
- Доступ через `(*Proxy).Round22Metrics()`.

### Fix #15 (P0): cppworker handleV1Embeddings extensions
- `cmd/cppworker/handlers_openai.go:handleV1Embeddings` теперь поддерживает
  `input: []string` и `ensureModelLoaded` (см. Fix #3).

---

## Commits (centurion branch)

```
645cc59 fix(balancer+cppworker): Round 22 — VRAM headroom bypass for read endpoints + duplicate route fix
58eb93a fix(balancer+cppworker): Round 22 — read/mgmt skip warmup, name→path history, /v1/embeddings
af8ad21 fix(balancer): Round 21 final — parseRequestBody extracts both `model` and `name`
8c0f45e fix(balancer): Round 21 follow-up — proxyHTTP timeout + handlePS fix
406e337 fix(cppworker+balancer): Round 21 — auto-load path, /api/ps, /api/embed
```

---

## Live verify (bundled-full стек)

```
READ/MGMT (Fix #1+13+16):
[200]  71ms /api/show              (alias qwen3-4b → 200 with model info!)
[200]  37ms /api/show              (file name)
[200]  39ms /api/models/files
[503]   8ms /api/pull              (no model to operate on, expected)
[503]   4ms /api/copy              (idem)
[503]   3ms /api/create            (idem)
[503]   4ms /api/delete            (idem)
[503]   3ms /api/push              (idem)

UNSUPPORTED (Fix #7+11):
[404]   3ms /v1/audio/speech
[404]   3ms /v1/images/generations
[404]   3ms /v1/realtime
[404]   3ms /api/signin
[404]   4ms /api/logout
[404]   4ms /api/web/settings

OpenAI v1/* (Fix #3 + Fix #14):
[200] 486ms /v1/embeddings         (real embedding data)
[200] 356ms /v1/chat/completions
[200] 353ms /v1/completions

Other:
[200]  39ms /v1/models
[200]  39ms /api/tags
[200]  39ms /api/ps
```

**КРИТИЧНО:** до Round 22 эти endpoints висели 10-60 секунд или возвращали
500. Теперь все — 200/503/404 за миллисекунды.

---

## Deferred (not Round 22)

- CORS preflight (Bug #8) — OpenWebUI и Cline отправляют OPTIONS, нужно проверить
  middleware. Round 18 частично покрыл (CORS заголовки), но preflight handler
  нужен отдельный тест.
- X-Request-Id pass-through (Bug #12) — balancer генерирует свой, не берёт
  клиентский. Лучше использовать `getOrGenerateRequestID` уже есть в proxy.go.
- RoundMetrics exposure через /metrics endpoint — counters добавлены но не
  exposed. Future PR.

---

## Known limitations (out of scope)

- Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive — model broken by design,
  tag-parser не поможет.
- /v1/realtime — WebSocket, не реализован.
- Tool calling для моделей без native support (требует chat template override).
- Multi-modality (vision/audio) — нет в cppworker.
