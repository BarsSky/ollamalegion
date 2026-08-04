# Round 18 P0.3 — Per-user parallel limit (`MaxParallelPerUser`)

**Status**: planned (2026-08-04)
**Parent**: [Round 18 audit](round-18-audit.md), [Round 18 user decisions](round-18-user-decisions.md) (3)
**Goal**: Fair-share — один пользователь не может занять все слоты бэкенда

## Проблема

Текущее состояние:
- `InFlightCounter` — per-MODEL счётчик (используется для reload-protection)
- `MaxParallel` — per-backend (n_slots из llama.cpp, определяется размером VRAM)
- **Per-USER лимита нет** — один клиент может открыть 100 параллельных запросов и занять всё

Типичный сценарий: баг в клиенте (цикл ретраев) → 50 параллельных /v1/chat/completions → backend полностью занят одним юзером, остальные получают 429/timeout.

## Решение

**Per-user parallel admission**: счётчик `UserTracker` + `getUserID(r)` + check в каждом streaming handler.

### Конфиг

```go
// internal/cppbackend/config.go
type Config struct {
    // ...
    MaxParallelPerUser int  // 0 = unlimited (default), >0 = max concurrent per user
}
```

Env: `CPPWORKER_MAX_PARALLEL_PER_USER` (default `0` = отключено для backward-compat).

**Round 18 user decision (3)**: per-user parallel limit admin-configured.

### `getUserID(r)` — где брать user identifier

Приоритет (по убыванию):
1. `X-User-Id` header (если непустой, после sanitization)
2. `RemoteAddr` (с убранным портом: `192.0.2.1:54321` → `192.0.2.1`)
3. `"anonymous"` (fallback)

**Sanitization**:
- max length 128 chars
- reject control chars (newline, tab, NUL) — заменяем на `_`
- allow: alnum, `_`, `-`, `.`, `@`, `:`, ` ` (пробел)

### `UserTracker` — per-user parallel counter

```go
// internal/cppbackend/user_tracker.go
type UserTracker struct {
    mu       sync.Mutex
    counters map[string]int
}

func NewUserTracker() *UserTracker
func (t *UserTracker) TryAcquire(userID string, max int) bool  // atomic check-and-increment
func (t *UserTracker) Release(userID string)                   // decrement
func (t *UserTracker) Current(userID string) int
func (t *UserTracker) Snapshot() map[string]int                // all non-zero
func (t *UserTracker) Reset(userID string)
```

**Semantics**:
- `TryAcquire` возвращает `false` если текущее значение `>= max` (НЕ инкрементирует)
- `max <= 0` → `TryAcquire` всегда возвращает `true` (unlimited)
- `userID == ""` → "anonymous" (default bucket)
- `Release` не опускает ниже 0 (защита от double-release)
- nil-safe (методы no-op на nil receiver)

### Admission в 4 streaming handler'ах

В каждом из `handleChat`, `handleGenerate`, `handleV1ChatCompletions`, `handleV1Completions` после `ensureModelLoaded` (или до — но ДО тяжёлых операций):

```go
userID := getUserID(r)
if max := currentConfig.MaxParallelPerUser; max > 0 {
    if !backend.UserTracker().TryAcquire(userID, max) {
        writeError(w, http.StatusTooManyRequests,
            fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d", userID, max))
        return
    }
    defer backend.UserTracker().Release(userID)
}
```

**Где ставить check**: ПОСЛЕ `ensureModelLoaded` (т.к. model load дорогой, не хочется грузить модель для запроса, который будет отвергнут). Но есть tradeoff: если admission check после load, то user со 100 параллельными загрузками модели получит 100 успешных load'ов, а потом 99 отказов. Для single-slot backends (max_slots=1) это может выглядеть как slow admission.

**Решение**: ставим check СРАЗУ после парсинга `req.Model` (до `ensureModelLoaded`). Если у user 5+ параллельных, 6-й получает 429 немедленно. Model load дорогой, но на него тоже есть InFlight (per-model, для reload protection).

### `GenerationInfo` уже содержит `user_id` (из P0.2)

`/api/infer/active` уже возвращает `user_id` в каждой записи. **Ничего менять не нужно**.

### Новый endpoint: `/api/infer/users`

```
GET /api/infer/users
→ {"users": [{"user_id": "alice", "current": 2}, ...], "max_per_user": 4}
```

Auth: `X-API-Token`. Используется для admin-мониторинга (WebUI или CLI).

### Default behaviour

- `MaxParallelPerUser = 0` (default) → admission SKIPPED полностью, поведение идентично v0.5.5
- `MaxParallelPerUser = 4` → каждый user может иметь не более 4 параллельных запросов

## Files

**New**:
- `internal/cppbackend/user_tracker.go` — `UserTracker` struct
- `internal/cppbackend/user_tracker_test.go` — unit tests (~6 tests)
- `cmd/cppworker/user_id.go` — `getUserID(r)` helper
- `cmd/cppworker/user_id_test.go` — unit tests (~4 tests)
- `cmd/cppworker/handlers_user.go` — `handleInferUsers` (GET /api/infer/users)
- `cmd/cppworker/handlers_user_test.go` — handler test
- `plans/round-18-p0.3-user-parallel.md` — этот план

**Modified**:
- `internal/cppbackend/config.go` — `MaxParallelPerUser int` field
- `internal/cppbackend/backend.go` — `userTracker *UserTracker`, `NewUserTracker()` init, `UserTracker()` accessor
- `cmd/cppworker/main.go` — `MaxParallelPerUser` env loading
- `cmd/cppworker/router.go` — `/api/infer/users` route
- `cmd/cppworker/handlers_chat.go` — admission + defer Release
- `cmd/cppworker/handlers_generate.go` — same
- `cmd/cppworker/handlers_openai.go` — `handleV1ChatCompletions` + `handleV1Completions`
- `CHANGELOG.md` — v0.5.6 entry

**Total LoC**: ~350 строк (mostly tests)

## Tests (unit)

`UserTracker`:
1. `TestUserTracker_BasicTryAcquireRelease` — в пределах лимита → true; Release → next → true
2. `TestUserTracker_ExceedsLimit` — на 4-м TryAcquire с max=3 → false
3. `TestUserTracker_UnlimitedMax` — max=0 → всегда true
4. `TestUserTracker_NilSafe` — методы на nil → no panic
5. `TestUserTracker_Snapshot` — не показывает user с 0
6. `TestUserTracker_Concurrent` — 100 горутин, 4 max → ровно 4 acquire=true в любой момент

`getUserID`:
1. `TestGetUserID_XUserId` — header есть → header value
2. `TestGetUserID_RemoteAddr` — header нет, RemoteAddr есть → IP
3. `TestGetUserID_Anonymous` — ничего нет → "anonymous"
4. `TestGetUserID_Sanitize` — длинный ID → truncated, control chars → `_`

## Live verify

Bundled-full stack, image с v0.5.6:

1. `curl -H "X-User-Id: alice" /v1/chat/completions ...` (4 параллельно, max=4) → 200 × 4
2. `curl -H "X-User-Id: alice" /v1/chat/completions ...` (5-й) → **429** "exceeded MaxParallelPerUser=4"
3. `curl -H "X-User-Id: bob" /v1/chat/completions ...` → 200 (другой user — independent bucket)
4. `curl /api/infer/users` → `{"users":[{"user_id":"alice","current":4}], "max_per_user":4}`
5. После завершения любого из alice's: 6-й → 200 (slot released)

## Out of scope

- Per-model per-user limit (например "alice может 4 на qwen3, 2 на gemma4") — отдельный P1
- Token-bucket rate limiting (запросов в минуту, не параллельных) — отдельный P2
- Auth (кто такой "alice" — откуда верифицировать identity) — ortho question, default trust X-User-Id header
