# Settings busy + long-conversation cutoff — analysis & fix plan

**Date**: 2026-08-04
**Status**: Investigation complete, fixes pending
**Reported by**: user (через 3 сообщения подряд в сессии)

## 3 проблемы

### #1: WebUI не даёт доступ к настройкам модели во время генерации
**Симптом**: на странице бэкенда (GGUF Models → backend detail) в процессе
генерации ответа моделью, секция "Настройки" / per-model profile недоступна
(или apply-операция зависает на 30-60+ секунд).

**Root cause** (найдено):
1. `applyModelProfile` (handlers_cppworker_profiles.go:247) делает
   reload модели на всех бэкендах где она загружена.
2. `reloadModelOnCppWorker` POST'ит `/api/models/reload` на cppworker.
3. `handleReloadModel` (handlers_model.go:v0.5.11) проверяет InFlight counter
   через `WaitZero(req.Name, 0)` (строка 1140):
   ```go
   inflight.WaitZero(req.Name, 0) // 0 = без таймаута
   ```
   Это блокируется на ВСЁ время текущей генерации (может быть 30-60+ сек).
4. `applyProfileWithProgress` (cppworker-params.js:529) показывает modal
   "Применение профиля…" с шагами "save" и "reload". Modal не закрывается
   пока apply не завершится.

UI side:
- Modal блокирует взаимодействие с панелью (modal overlay)
- Даже если закрыть modal, страница бекенда "зависает" потому что
  `state.detailLoading = true` остаётся до refresh

**Что не блокирует (работает нормально)**:
- Per-model profile **list/get/put** (не требует reload) — работает мгновенно.
- Другие tabs на странице (About, Models, Downloads) — рендерятся независимо.

### #2: Достаточно ли функционала у балансера и бэкенда?
**Ответ**: **Да, функционал complete**. Timeout-ы все 3-tier resolver'ы:

| Timeout | Default | Источник |
|---|---|---|
| `StreamTimeout` (общий streaming) | **600s** (10 min) | `config.Balancing.StreamTimeout` |
| `StreamingIdleTimeout` (per-model adaptive) | **120s** (2 min) | `config.Balancing.StreamingIdleTimeout` + per-model profile |
| `FirstByteTimeout` (HTTP headers) | **600s** (10 min) | `config.Balancing.FirstByteTimeout` |
| `RequestTimeout` (non-streaming) | **120s** | `config.Balancing.RequestTimeout` |
| cppworker `WriteTimeout` | **30 min** | `main.go:75` flag default |
| cppworker `IdleTimeout` | **120s** | `main.go:347` |
| cppworker `ReadTimeout` | **30s** | `main.go:345` |

Все 4 балансер-уровневых timeout'а имеют **3-tier resolver**:
1. **Per-model profile** (config.LlamaCppModelProfiles[modelName]) — user-configurable через WebUI
2. **ModelLatencyTracker** (автоматический расчёт на основе истории генерации)
3. **Heuristic** по размеру GGUF файла (для моделей без истории)
4. **Global config** (Balancing.*)
5. **Default** (если ничего не задано)

cppworker тоже имеет конфигурируемые timeout'ы через env (`CPPWORKER_WRITE_TIMEOUT`).

### #3: Обрыв ответа при длительном общении — hardcoded timeout или streaming?
**Скорее всего НЕ hardcoded timeout**, а **n_ctx overflow** или **client-side timeout**.

**Возможные причины** (от более вероятной к менее):
1. **n_ctx overflow**: контекст модели (n_ctx=16384 для Qwen3, 32768-262144 для gemma-4)
   заполняется по мере роста диалога. При overflow llama.cpp обрезает
   prompt → может потерять system message / последние сообщения →
   модель "забывает" инструкции → генерирует обрезанный или мусорный ответ.
   **Признак**: длина prompt в логах растёт, потом внезапно сбрасывается.

2. **cppworker `WriteTimeout = 30 min`** (настраивается через `--write-timeout` flag
   и `CPPWORKER_WRITE_TIMEOUT` env): при streaming inference, если запись
   response занимает > 30 минут (например, длинный ответ на большой prompt),
   Go HTTP server принудительно закрывает соединение. **Признак**:
   total response time > 30 min, в логах "write timeout".

3. **Client-side timeout**: OpenWebUI / Cline / Roo имеют свои таймауты
   (обычно 60-120s для idle в SSE). Если cppworker делает паузу между
   чанками > client idle timeout, клиент закрывает соединение.
   **Признак**: только в конкретном клиенте, другие работают.

4. **Per-model StreamTimeout (600s default)**: общий таймаут на всю
   streaming-сессию. Если генерация идёт > 10 минут, balancer закрывает
   стрим. **Признак**: ответ обрывается на 10 минуте, в логах balancer
   "stream timeout".

**Диагностика** (что посмотреть):
- `docker logs ol-bundled-full-cppworker-gpu | grep -E "n_ctx|prompt exceeds|context size"` — есть ли overflow
- `docker logs ol-bundled-full-balancer | grep -E "stream timeout|context deadline"` — есть ли timeout
- `docker logs ol-bundled-full-cppworker-gpu | grep "write timeout"` — есть ли cppworker write timeout
- WebUI monitor → GGUF Models → backend → ActiveQueries (in-flight counter)
  показывает количество активных запросов; если > 0 при cutoff — генерация
  идёт, если 0 — balancer уже отвалился

## Fix plan (3 приоритета)

### Fix #1 (HIGH): WebUI settings panel — показывать "model busy" state
**Цель**: пользователь видит, что модель занята, и apply ждёт в фоне.

**Что делаем**:
1. `cmd/cppworker/handlers_model.go`: добавить endpoint
   `GET /api/models/active-queries?model=<name>` — возвращает
   `{"model":"X","activeQueries":N}`. Использует существующий
   `backend.InFlight().Get(modelName)`.

2. `webui/js/modules/gguf-renderer.js` (renderLoadedPane): показывать
   "🔴 Generating response (N active queries)" badge на loaded model
   card. Polling каждые 3s (через существующий refreshDetail).

3. `webui/js/modules/cppworker-params.js` (applyProfileWithProgress):
   если activeQueries > 0, показать warning "Model is currently generating
   — apply will wait for it to finish" перед отправкой.

4. `internal/api/handlers_cppworker_profiles.go` (applyModelProfile): 
   добавить check через `s.proxy.GetActiveQueriesForModel` (новый helper
   в proxy) и если > 0 — вернуть **HTTP 202 Accepted** с `{status:"busy",
   activeQueries:N, progressUrl:"/api/v1/cppworker/model-profiles/X/apply/progress"}` 
   (async apply). Иначе — current sync path.

### Fix #2 (HIGH): Apply profile в фоне если модель занята
**Цель**: settings apply НЕ блокирует UI — возвращает 202, реальный reload
в background goroutine, polling показывает прогресс.

**Что делаем**:
1. `internal/api/handlers_cppworker_profiles.go`: новый endpoint
   `GET /api/v1/cppworker/model-profiles/{name}/apply/progress` — SSE-стрим
   прогресса apply (как в v0.5.12 для load progress).
2. `applyModelProfile` детектит busy state, возвращает 202 + Location.
3. Background goroutine делает save + wait for inflight + reload.
4. `webui/js/modules/cppworker-params.js` (applyProfileWithProgress):
   получает 202, переключается на SSE-polling, modal остаётся но
   обновляется через events.

### Fix #3 (MEDIUM): n_ctx overflow detection
**Цель**: при риске overflow (estimated prompt > 0.8 * n_ctx) — warning
в UI, не crash.

**Что делаем**:
1. `internal/balancer/preflight_nctx.go`: уже есть `preflightNCtxReload`
   который детектит overflow. Расширить до `checkNCtxUsage`:
   - Возвращает 0 (ok), 1 (warning, > 80%), 2 (will reload), 3 (impossible).
2. `internal/balancer/llamacpp_router.go` (`handleOpenAIChatCompletions`):
   при `usage >= 1` добавить header `X-Model-Context-Warning: high` 
   с рекомендацией reduce conversation или increase n_ctx.
3. `webui/js/modules/gguf-renderer.js`: показывать context bar
   "Context: 12.3K / 16K tokens (77%)" в header чата.

### Fix #4 (LOW): chat context length metric в WebUI
**Цель**: показать пользователю сколько контекста занято в текущем чате.

**Что делаем**:
1. cppworker `/api/v1/chat` handler возвращает в response:
   `prompt_tokens`, `total_tokens`, `context_size`.
2. WebUI отображает progress bar: "Context: 12.3K / 16K tokens (77%)"
3. При > 90% — warning toast: "Контекст почти заполнен, начните новый чат".

## Files to modify

**New**:
- (none — используем существующую инфраструктуру)

**Modified (cppworker)**:
- `cmd/cppworker/handlers_model.go` — добавить `handleGetActiveQueries`
- `cmd/cppworker/router.go` — register `/api/models/active-queries`

**Modified (api server)**:
- `internal/api/handlers_cppworker_profiles.go` — async apply path (Fix #1, #2)
- `internal/api/proxy.go` (или `proxy_request.go`) — добавить helper
  `GetActiveQueriesForModel(modelName) int`

**Modified (webui)**:
- `webui/js/modules/gguf-renderer.js` — busy state badge, context bar
- `webui/js/modules/cppworker-params.js` — busy warning, SSE-polling for apply
- `webui/js/modules/api.js` — добавить `cppworkerActiveQueries.get(name)`

**Modified (balancer)**:
- `internal/balancer/preflight_nctx.go` — расширить checkNCtxUsage
- `internal/balancer/llamacpp_router.go` — добавить X-Model-Context-Warning header

## Tests
- `cmd/cppworker/handlers_model_active_queries_test.go` — 4 tests
- `internal/api/handlers_cppworker_profiles_busy_test.go` — 6 tests
- `internal/balancer/preflight_nctx_warning_test.go` — 4 tests

## Live verify (после фиксов)
- Apply profile во время генерации → 202 + SSE progress, не блокирует UI
- WebUI badge "🔴 Generating (N)" на loaded model во время streaming
- Long conversation → context bar показывает 77% usage, при 90% warning
- cppworker не падает на write timeout при разумной длине диалога

## Open question
**Какой client используется?** (Cline / OpenWebUI / Curl). Это поможет
отличить client-side timeout от server-side.

## Приоритеты
- Fix #1 + #2 (settings blocked) — **NEXT** (v0.5.13)
- Fix #3 (n_ctx overflow) — **v0.5.14** (нужен API change)
- Fix #4 (context metric) — **v0.5.14** (UI-only)
