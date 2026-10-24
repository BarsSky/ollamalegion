# Round 35 — CppWorker bundled-with-agent r35 (2026-08-12)

> **Назначение:** bugfix раунд, закрывающий crash-loop цепочку
> "Cline 65K request → balancer preflight → cppworker SIGSEGV".
> Включает 4-фазный preflight (Round 34 follow-up) + reload→load
> fallback + cgo SIGSEGV recover + idleUnload SIGSEGV guard.

## 0. Контекст

Пользователь сообщил дважды за одну сессию:

1. "проверил через cline — модель загрузилась и тут же сбросилась"
2. "не получилось опять по запросу клиента перезагрузить корректно модель"

Live-диагностика показала crash-loop: cppworker SIGSEGV'ит в cgo вызове
C++ `common_chat_templates_apply` → `exit code 2` → Docker auto-restart
→ балансер в стейл-кэше "loaded" → Cline получает 502/503 cascade.

## 1. Round 34 follow-up (commit `6140a59`)

**4-фазный preflight + 5 каскадных bug fixes.**

Реализация была в working tree с прошлой сессии, но не была
закоммичена. Восстановлена отдельным commit'ом `6140a59`.

### Phase 0 — compose env vars

`deployments/docker-compose.bundled-full.yml`: добавлены
`LB_NCTX_RELOAD_TIMEOUT_SEC`, `LB_NCTX_PREFLIGHT_ASYNC_RELOAD`,
`LB_NCTX_PREFLIGHT_ASYNC_RETRY_AFTER_SEC`. Без них compose не
подхватывал env из `.env`-файла → async mode выключен.

### Phase 1 — stream dialog с keepalives

`internal/balancer/preflight_stream_dialog.go` (новый файл):
SSE `: keepalive\n\n` / NDJSON `{"keepalive": true}` во время async
reload, чтобы Cline / OpenAI streaming клиенты не таймаутили.

### Phase 2 — profile mismatch detection

`internal/balancer/preflight_nctx.go`: расширил preflight с
детекта только `n_ctx` mismatch до детекта `kv_cache_type`,
`flash_attn`, `use_mmap`. Если хоть один param отличается от
загруженной модели → trigger reload.

### Phase 3 — SetLastKnownNCtx on load/unload

`cmd/cppworker/balancer_register.go`: cppworker notify'ит balancer
на каждом load/unload. Метрики в синке с реальным состоянием.

### Phase 4 — optimal auto-tune params

`internal/balancer/nctx_reload_adaptive.go` +
`nctx_reload_handlers.go`: reload payload всегда содержит
`gpuLayers=-2, flashAttn=-1, useMmap=true` для best-fit.

### Bug fixes

- **Bug A** `cluster_state.go:250` — orphan `mu.Unlock()` →
  `fatal error: sync: unlock of unlocked mutex` через 75 min uptime
- **Bug B** `nctx_reload_config_bridge.go` — `LB_NCTX_PREFLIGHT_ENABLED`
  env override (config.json не имел поля, никакого override не было)
- **Bug C** `llamacpp_handlers_inference.go:113` — preflight для
  OpenAI path (Cline bypass'ил Ollama preflight)
- **Bug D** `preflight_nctx.go:123` — `NumCtx int` в
  `openAIChatRequestRaw` (top-level OpenAI num_ctx игнорировался)
- **Bug E** `preflight_nctx.go:285` — `RequestedNCtxOverride` check
  в `DecidePreflight` (явный client request > loaded → reload)

## 2. Round 35 main (commit `df7962e`)

### Bug: "model loaded then immediately reset"

**Симптомы:** Cline 65K request → preflight → reload → cppworker
SIGSEGV → exit code 2 → auto-restart → loop.

**Root cause (3 связанных бага):**

1. **cppworker `/api/models/reload` 404 для не загруженной модели**
   (`handlers_model.go:1085`). Reload требует loaded state — не
   подходит для preflight, который может сработать на stale cache
   сразу после cppworker restart.

2. **Balancer preflight использует только `/api/models/reload`**
   (`nctx_reload_handlers.go:788` и `nctx_reload.go:719`). Stale
   `loaded_models: 1` от предыдущего cppworker instance → reload →
   404 → Cline 503+Retry-After loop до 120s → ECONNREFUSED.

3. **cppworker `IdleUnloadManager` SIGSEGV** в `bridge_free_model`
   при `referenceTime.IsZero()`. `now.Sub(zeroTime)` = 631 трлн
   наносекунд → unload сразу → use-after-free в C-bridge.

### Fix (3 файла + 1 config + 1 compose)

| File | Change |
|------|--------|
| `internal/balancer/nctx_reload_handlers.go:786` | async reload path: `/api/models/reload` → `/api/models/load` (идемпотентный) |
| `internal/balancer/nctx_reload.go:765-825` | sync reload path: добавил 404→load fallback после existing 202→poll handling |
| `internal/cppbackend/model_manager.go:538-549` | `checkAndUnload`: `IsZero()` guard перед `Sub()` (SIGSEGV-фикс) |
| `config/cppworker-defaults.json:41` | `idleUnloadMinutes: 120 → 0` (off по дефолту) |
| `deployments/docker-compose.cppworker-bundled-with-agent.yml:40` | image tag r34 → r35 |

## 3. Round 35b (commit `f22dc13`)

### Bug: cgo SIGSEGV в `buildChatPrompt` cgo call

**Симптомы:** После Round 35 деплоя — cppworker r34 image всё ещё
работал (r35 image ещё не build'ился). cgo вызов C++
`common_chat_templates_apply` (chat_thinking.cpp:69) периодически
SIGSEGV'ит в cgo execution → exit code 2 → crash-loop.

**Stack trace:**
```
SIGSEGV: segmentation violation
signal arrived during cgo execution
runtime.cgocall(...)
ollama-loadbalancer/c/bridge._Cfunc_bridge_apply_chat_template(...)
ollama-loadbalancer/c/bridge.(*ModelHandle).ApplyChatTemplate.func5(...)
	ollama-loadbalancer/c/bridge/bridge.go:834
```

**Root cause:** C++ `common::chat` модуль llama.cpp'а имеет багу
state corruption после multiple load/reload циклов. C-сторона
теряет консистентность, и `llama_chat_apply_template` (простой C
fallback) тоже падает.

**Fix:** `cmd/cppworker/handlers_chat.go:281-310` — обернул
`backend.ApplyChatTemplateWithThinking` (C++ путь) в
`func() { defer recover() ... }()`. При cgo SIGSEGV recover()
ловит panic → fallback на `backend.ApplyChatTemplate` (стабильный
C API). Для gemma-4 нативный путь всё равно ничего не даёт
(template не поддерживает enable_thinking), так что C++ путь
создавал crash opportunities без пользы.

**Trade-off:** SIGSEGV в cgo IS catchable в том же goroutine
через `recover()` (Go runtime конвертирует в `runtime.sigpanic`).
C state после SIGSEGV может быть corrupted — fallback через
`llama_chat_apply_template` (чистая template-функция, без model
state) обычно работает, но не гарантированно. Для production
нужен реальный fix в C++ `common::chat` модуле llama.cpp
(отложено).

## 4. Build & deploy

- cppworker image build: ~18 минут (CUDA compilation, 933s для
  llama.cpp + 4m для Go + bridge)
- Image: `ollama-legion/cppworker:gpu-86-abort-r35` (3.86GB)
- Deploy gotchas:
  - `CPPWORKER_GPU_TAG=86-abort-r35` в BOTH `deployments/.env` AND
    `deployments/.env.bundled-with-agent` (compose auto-loads `.env`)
  - Use `--no-build` для `docker compose up` (иначе пытается
    rebuild из source)
  - Force-recreate container после image change

## 5. Verification (live, 2026-08-12)

| Scenario | Result |
|----------|--------|
| Unloaded + Cline 65K | ✅ load → 200 OK (3m26s, 18s chat) |
| 32K + Cline 65K | ✅ reload (503+Retry-After: 5) → 200 OK |
| 65K + Cline 65K | ✅ fast 200 OK (3.2s, 12 completion tokens) |
| Cppworker uptime | 26+ min без SIGSEGV (r35 image) |
| Recover() wrapper | triggered 0 times (no crash to recover) |

## 6. Live state (2026-08-12 23:09 UTC)

- HEAD: `f22dc13` (Round 35b)
- Cppworker image: `ollama-legion/cppworker:gpu-86-abort-r35` (3.86GB)
- Balancer image: `ollama-legion/balancer:cppworker-bundled-r35`
- 5 контейнеров healthy
- Model: gemma-4-E4B-it-Q4_K_M, 65K ctx, loaded
- No SIGSEGV/panic в логах cppworker за 26+ минут

## 7. Reusable patterns (cross-project)

1. **Reload vs Load endpoint selection**: для backend'ов с lazy-load
   ВСЕГДА используй `load` (идемпотентный — handles not-loaded /
   different-params / same-params), а НЕ `reload` (требует precondition
   "model must be loaded"). Это критично для preflight в балансерах,
   потому что preflight может сработать на stale metrics cache и
   попасть в race window когда backend только что перезапустился.
   `handleReloadModel` оставь только для явного admin/UI reload.

2. **Stale cache window в metrics poller**: при рестарте backend'а
   его in-memory state сбрасывается, но poller ещё держит stale
   данные до следующего poll (30s по умолчанию). В это окно preflight
   может триггернуть операцию на основе stale данных. Решение A
   (выбрано): endpoint должен быть толерантен к "не загружено"
   состоянию. Решение B (более invasive): poller должен сразу
   очищать cache при health-check failure (ломает graceful restart).

3. **`time.Time{}.IsZero()` BEFORE `Sub()` — MUST**: классическая
   SIGSEGV-ловушка в Go. `now.Sub(time.Time{})` = 631139040000000000
   ns (год 0001 до 2026 = 2025 лет × 365 дней × 86400 сек × 1e9).
   Это всегда > любого timeout → операция (unload, evict, cleanup)
   немедленно, часто на не инициализированном state →
   use-after-free в C-bridge / nil deref в Go / SIGSEGV. Всегда
   проверяй `IsZero()` ПЕРЕД `Sub()`. Особенно важно для
   `atomic.Value.Load()` полей (могут вернуть nil interface).

4. **Two-step coupling для endpoint + endpoint-state**: когда
   endpoint имеет precondition (например "model must be loaded"),
   а его вызывает система которая может работать в "precondition
   может быть не выполнен" режиме (preflight, health check, recovery),
   ВСЕГДА добавляй fallback на идемпотентный endpoint (load). Это
   сложнее чем "просто чини precondition check", но robust для
   distributed систем где state может расходиться.

5. **`POST /api/models/reload` vs `POST /api/models/load` семантика**:
   - `reload` = "перезагрузи с новыми параметрами" (требует loaded
     state, precondition: model currently loaded). Используй для
     explicit admin/UI reload с уже-известным current state.
   - `load` = "загрузи (или перезагрузи если нужно)" (идемпотентный,
     handles all 3 cases). Используй для preflight, lazy-load,
     recovery после рестарта. Returns 202 с progressUrl для async
     mode, или 200 с "already_loaded" для dedup.

6. **cgo SIGSEGV recoverability**: SIGSEGV raised in cgo call IS
   catchable via `recover()` в том же goroutine, потому что Go
   runtime converts the signal to a Go panic via `runtime.sigpanic`.
   BUT: this only works if the cgo call returns (i.e., not infinite
   loop in C). And the C library state may be corrupted after SIGSEGV
   — subsequent cgo calls might also crash. For per-request cgo calls
   (like chat template), this is acceptable: catch, fall back, request
   fails, model may need reload. For long-lived C state, the recover()
   is only a stopgap — real fix is in C code.

7. **Two-file .env coordination**: `docker compose` auto-loads `.env`
   (no suffix) by default. If you have `.env.bundled-with-agent`
   (with descriptive suffix), compose IGNORES it unless you pass
   `--env-file .env.bundled-with-agent`. For a project with multiple
   compose files, maintain BOTH `.env` and `.env.<suffixed>` with
   the same key values, OR use `--env-file` consistently.

8. **`--no-build` for compose with image pinning**: when a compose
   service has both `build:` and `image:` (the `image:` pins the tag),
   `docker compose up` will try to rebuild by default (even if
   `image:` matches existing local). To use the existing image
   (e.g., freshly tagged locally), pass `--no-build`. This avoids
   30-60 min rebuilds for cuda images.

9. **cppworker C++ chat template path is fragile**: c/bridge/csrc/
   chat_thinking.cpp wraps llama.cpp's `common_chat_templates_apply`
   which uses C++ std::string, std::vector, common::chat module.
   This path periodically SIGSEGV'ит in cgo, especially after
   multiple model loads/reloads (state corruption). For production:
   prefer the simple C API `llama_chat_apply_template` (no C++ STL,
   no common module) — less features but much more stable.
