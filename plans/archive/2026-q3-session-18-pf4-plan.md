# Session 18 — PF-4 fix: flaky `TestOpenAIChat_SlowFirstToken_HoldsConnection`

> **Создано:** 2026-06-27 (после Session 17, commit `c4d11fe`)
> **Приоритет:** 🔴 **P0** — единственный оставшийся pre-existing test failure; блокирует зелёный CI merge per roadmap Q3 2026 секция 11.
> **Ветка:** `feature/rpc-model-distribution` → новая ветка `fix/pf4-slow-first-token-flaky`
> **Оценка:** 1–2 часа

---

## 0. Контекст

В `plans/pre-existing-test-failures.md` задокументировано:

> **PF-4**: `TestOpenAIChat_SlowFirstToken_HoldsConnection` — timeout 50s в `httptest.Server.Close()`
> **Файл**: `tests/first_byte_timeout_test.go:114`
> **Симптом**:
> ```
> panic: test timed out after 2m0s
>     running tests:
>       TestOpenAIChat_SlowFirstToken_HoldsConnection (50s)
> ```
> **Корневая причина**: flaky test — `httptest.Server.Close()` зависает на ожидании
> завершения активных горутин. Тест проверяет timeout first-byte, но при teardown
> WaitGroup не уменьшается. Известная проблема Go `httptest` + custom timeout.
>
> **Рекомендация (из плана)**: добавить явный `srv.CloseClientConnections()` перед `srv.Close()`.

**Соседний тест `TestOpenAIChat_HeaderTimeout_StillWorks`** (lines 199–256) уже использует
правильный паттерн teardown — принудительное закрытие listener + клиентских соединений:

```go
defer func() {
    // hung-сервер держит соединения открытыми; Close может зависнуть.
    // Принудительно закрываем listener, не дожидаясь завершения handler'ов.
    upstream.Listener.Close()
    upstream.CloseClientConnections()
}()
```

**PF-4 тест НЕ применяет этот паттерн** — отсюда hang при `defer upstream.Close()`.

---

## 1. Затронутые файлы

| Файл | Изменение |
|---|---|
| `tests/first_byte_timeout_test.go` (lines 31–117, 143–145) | Применить `Listener.Close() + CloseClientConnections()` teardown в `TestOpenAIChat_SlowFirstToken_HoldsConnection` |
| `plans/pre-existing-test-failures.md` | Обновить статус PF-4 с ⬜ Backlog на ✅ FIXED 2026-06-27 (Session 18) |
| `CHANGELOG.md` | Добавить подсекцию `### Fixed (Session 18 — PF-4: flaky test teardown)` |

**Не требуется менять:**
- `internal/balancer/proxy_first_byte_timeout.go` — production-код корректен (подтверждено Session 13 PF-5 fix).
- `internal/balancer/proxy.go`, `proxy_request.go` — никаких изменений.
- Production behaviour.

---

## 2. План работ

### 2.1 Аудит (5 мин)

Проверить что в тесте есть:
- `defer upstream.Close()` (line 145) — недостаточно, hang при SlowFirstToken.
- `upstream := startSlowSSEServer(...)` (line 31) — slow SSE handler, НЕ hung-сервер, поэтому hang не от активного handler'а напрямую, а от **зависшего на 50s timer'а** в keepalive-цикле.

**Уточнение корневой причины** (отличается от docs):
- `startSlowSSEServer` использует `time.NewTimer(firstTokenDelay=50s)` + `time.NewTicker(15s)` в keepalive-цикле.
- Handler возвращается только после `break keepalive` через 50s.
- Пока handler не вернулся, `httptest.Server.Close()` блокируется на `sync.WaitGroup` (`Shutdown` ждёт активные соединения).
- Тест сделан с `done := make(chan bool, 1)` + `select ... case <-time.After(70*time.Second)`, но:
  - `time.After(70s)` срабатывает ПЕРВЫМ (раньше чем upstream отдаст все чанки к ~52s).
  - Когда `t.Fatal` срабатывает в горутине, `defer upstream.Close()` тоже зависает.
  - Go testing framework в `t.Fatal` вызывает `runtime.Goexit()` → defer выполняется → `upstream.Close()` → hang.

**Два независимых места где можно зависнуть:**
1. `defer upstream.Close()` (после `t.Fatal`) — hang на WaitGroup.
2. Сам тест может пройти (если upstream завершится до 70s), но тогда `proxy.Shutdown` или ещё что-то.

### 2.2 Реализация фикса (15 мин)

**Изменение 1**: в `TestOpenAIChat_SlowFirstToken_HoldsConnection` заменить:
```go
upstream := startSlowSSEServer(50*time.Second, []string{"hello", " world", "[DONE]"})
defer upstream.Close()
```
на:
```go
upstream := startSlowSSEServer(50*time.Second, []string{"hello", " world", "[DONE]"})
defer func() {
    // slow SSE-сервер ждёт 50s перед отправкой первого токена;
    // httptest.Server.Close() зависнет на WaitGroup, ожидая handler'ов.
    // Принудительно закрываем listener и активные соединения.
    upstream.Listener.Close()
    upstream.CloseClientConnections()
}()
```

**Изменение 2**: уменьшить таймаут теста с 70s до 60s (хватает: 50s upstream + ~5s proxy + 5s margin):
```go
case <-time.After(70 * time.Second):
    t.Fatal("request did not complete within 70s")
```
на:
```go
case <-time.After(60 * time.Second):
    t.Fatal("request did not complete within 60s")
```

**Изменение 3** (опционально, defensive): `startSlowSSEServer` — в keepalive-цикле использовать `select` с `r.Context().Done()` для немедленного выхода при отмене клиента:
```go
keepalive:
for {
    select {
    case <-timer.C:
        break keepalive
    case <-ticker.C:
        fmt.Fprintf(w, ": keepalive\n\n")
        flusher.Flush()
    case <-r.Context().Done():
        return  // клиент отвалился — не ждём 50s
    }
}
```

Это ускорит teardown в общем случае.

### 2.3 Верификация (15 мин)

```powershell
# Из корня репозитория:
go test -tags llama_stub ./tests/first_byte_timeout_test.go -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s -v
```

Ожидаемый результат: `--- PASS: TestOpenAIChat_SlowFirstToken_HoldsConnection (53.5s)` (или близкое, ~50s upstream + 1-3s proxy).

Затем — full test suite:
```powershell
go test -tags llama_stub ./tests -count=1 -timeout 300s
```
Ожидаем: PF-4 PASS, остальные тесты сохраняют текущий статус (не должны быть сломаны).

Также проверить что PF-5 fix не сломан:
```powershell
go test -tags llama_stub ./tests -run "TestOpenAIChat_HeaderTimeout_StillWorks" -count=1 -timeout 30s -v
```

### 2.4 Документация (10 мин)

**`plans/pre-existing-test-failures.md`** — изменить строку 215:
```
| PF-4 | `TestOpenAIChat_SlowFirstToken_HoldsConnection` | `tests/first_byte_timeout_test.go:114` | ⬜ Backlog (flaky — `httptest.Server.Close()` зависает на `WaitGroup`, не относится к proxy-логике) |
```
на:
```
| PF-4 | `TestOpenAIChat_SlowFirstToken_HoldsConnection` | `tests/first_byte_timeout_test.go:114` | ✅ FIXED 2026-06-27 (Session 18 — принудительное закрытие listener + `CloseClientConnections()` в teardown, паттерн из `TestOpenAIChat_HeaderTimeout_StillWorks`) |
```

**`CHANGELOG.md`** — добавить подсекцию после Session 17:

```markdown
### Fixed (Session 18 — PF-4: flaky test teardown)

**Файл:** `tests/first_byte_timeout_test.go:114` (`TestOpenAIChat_SlowFirstToken_HoldsConnection`).

**Симптом:**
```
panic: test timed out after 2m0s
    running tests:
      TestOpenAIChat_SlowFirstToken_HoldsConnection (50s)
```

**Корневая причина:** `httptest.Server.Close()` в `defer upstream.Close()`
зависает на WaitGroup, ожидая завершения SSE-handler'а, который блокирован в
keepalive-цикле `time.NewTimer(50s)`. Production-код корректен — проблема
только в test teardown.

**Фикс:** применён паттерн из `TestOpenAIChat_HeaderTimeout_StillWorks`
(строки 134-138), который уже использует `upstream.Listener.Close()` +
`upstream.CloseClientConnections()` для принудительного teardown hung-сервера.

**Изменения в `tests/first_byte_timeout_test.go`:**
- `defer upstream.Close()` → `defer func() { upstream.Listener.Close(); upstream.CloseClientConnections() }()`.
- `time.After(70s)` → `time.After(60s)` (новый ceiling после фикса).
- (опционально) `startSlowSSEServer`: добавлен `<-r.Context().Done()` в keepalive-цикл.

**Acceptance criteria:**
- `go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s` → PASS за ~50-55s.
- `go test -tags llama_stub ./tests -count=1 -timeout 300s` → PF-4 PASS, остальные сохраняют статус.
- `TestOpenAIChat_HeaderTimeout_StillWorks` (PF-5) остаётся PASS (не сломан).
- `internal/balancer/proxy_first_byte_timeout.go` без изменений.
- `data/state.json` без изменений.

**NB:** все 7 pre-existing failures (PF-1 #1, PF-1 #2, PF-3, PF-4, PF-5, PF-6, PF-7) теперь ✅ FIXED. Q3 метрика "Все pre-existing failures закрыты" выполнена.
```

### 2.5 Коммит (5 мин)

```bash
git checkout -b fix/pf4-slow-first-token-flaky
# ... apply changes ...
git add tests/first_byte_timeout_test.go plans/pre-existing-test-failures.md CHANGELOG.md
git commit -m "fix(tests): PF-4 flaky teardown in TestOpenAIChat_SlowFirstToken_HoldsConnection

* tests/first_byte_timeout_test.go: use upstream.Listener.Close() +
  CloseClientConnections() in defer (same pattern as the working
  TestOpenAIChat_HeaderTimeout_StillWorks) so httptest.Server.Close()
  doesn't hang on WaitGroup waiting for the 50s slow-SSE handler.
* Lower test ceiling from 70s to 60s (50s upstream + 1-3s proxy + margin).
* (defensive) startSlowSSEServer: also exit keepalive loop on
  r.Context().Done() so an early client cancel returns immediately.
* plans/pre-existing-test-failures.md: mark PF-4 as ✅ FIXED Session 18.
* CHANGELOG: Session 18 sub-section with diagnosis + fix + AC.
* All 7 pre-existing failures (PF-1 #1/#2, PF-3..PF-7) now FIXED."
```

---

## 3. Acceptance criteria (полные)

1. `go test -tags llama_stub ./tests -run "TestOpenAIChat_SlowFirstToken_HoldsConnection" -count=1 -timeout 90s -v` завершается с `--- PASS` за <60 секунд (без `panic: test timed out`).
2. `go test -tags llama_stub ./tests -count=1 -timeout 300s` завершается без `FAIL` на PF-4.
3. `go test -tags llama_stub ./tests -run "TestOpenAIChat_HeaderTimeout_StillWorks" -count=1 -timeout 30s -v` остаётся PASS (не сломан PF-5).
4. `go test -tags llama_stub ./internal/... -count=1 -timeout 60s` остаётся зелёным (или с теми же pre-existing).
5. `go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker` — exit 0.
6. `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` — exit 0.
7. `git log` показывает новый commit `fix/pf4-slow-first-token-flaky` поверх `c4d11fe`.
8. `plans/pre-existing-test-failures.md` обновлён (PF-4 → ✅ FIXED 2026-06-27).
9. CHANGELOG.md содержит подсекцию Session 18.
10. Working tree clean (кроме `c/llama.cpp` untracked).

---

## 4. Связанные документы

- `plans/pre-existing-test-failures.md` — строки 188-220, 215 (статус PF-4).
- `plans/2026-q3-roadmap.md` — секция 11 "Метрики успеха Q3" (pre-existing failures = ✅).
- `internal/balancer/proxy_first_byte_timeout.go` — production-код first-byte timeout (НЕ меняется).
- `internal/balancer/proxy_request.go` — production proxy (НЕ меняется).

---

## 5. Порядок выполнения (чеклист для следующей сессии)

- [ ] Создать ветку `fix/pf4-slow-first-token-flaky` от `feature/rpc-model-distribution`.
- [ ] Прочитать `tests/first_byte_timeout_test.go` целиком (подтвердить line numbers).
- [ ] Применить изменение 1 (Listener.Close() + CloseClientConnections()).
- [ ] Применить изменение 2 (таймаут 70→60s).
- [ ] Применить изменение 3 (опционально, r.Context().Done() в keepalive).
- [ ] Запустить `go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s -v`.
- [ ] Запустить `go test -tags llama_stub ./tests -count=1 -timeout 300s` (full).
- [ ] Запустить `go test -tags llama_stub ./tests -run TestOpenAIChat_HeaderTimeout_StillWorks -count=1 -timeout 30s` (regression).
- [ ] Обновить `plans/pre-existing-test-failures.md` (PF-4 → FIXED).
- [ ] Добавить подсекцию Session 18 в `CHANGELOG.md`.
- [ ] Закоммитить.
- [ ] (Опционально) PR в `feature/rpc-model-distribution`.

---

## 6. Что осталось на Session 19+ (backlog)

После закрытия PF-4 (Session 18) все 7 pre-existing failures будут закрыты — Q3 метрика выполнена. Дальнейшие кандидаты (по приоритету):

| P | Задача | Источник | Оценка |
|---|---|---|---|
| P1 | Loaded models counter в Dashboard | roadmap 2.2 line 102 | 1ч |
| P1 | Export logs to CSV | roadmap 5.2 | 2ч |
| P1 | Notification system (toast) | roadmap 5.4 | 3ч |
| P1 | Models tab — Bulk operations | roadmap 2.2 line 86 | 2-3ч |
| P2 | Models tab — Model details panel | roadmap 2.2 line 85 | 3-4ч |
| P2 | Live tail Logs (WebSocket) | roadmap 5.3 | 4-5ч |
| P1 | rpc_coordinator production mode | roadmap 3.2 | 5-8 дней |
| P1 | virtual_router production mode | roadmap 3.3 | 5-7 дней |
| P3 | Audit TODO/FIXME/STUB маркеров | — | TBD |