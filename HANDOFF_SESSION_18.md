# Handoff: Session 17 → Session 18

> **Подготовлено:** 2026-06-27, 17:48 MSK
> **Куда:** следующая сессия (после закрытия ACT MODE)
> **Branch:** `feature/rpc-model-distribution` @ `e36e6c6`
> **Следующая задача:** Session 18 — PF-4 fix (flaky test teardown)

---

## 1. Что сделано в этой сессии (2026-06-27)

### 1.1 Session 17 (UI-аудит) — коммит `c4d11fe`

Полный аудит хардкоженных цветов в CSS WebUI, рефакторинг с использованием
theme-aware CSS custom properties. Детальный план был сохранён в
`plans/2026-q3-session-17-css-audit-plan.md` (если нужно восстановить контекст).

**Commit:** `c4d11fe feat(webui): Session 17 - theme-aware CSS tokens + hardcoded color audit`

**Метрики (6 файлов, +286/-64):**

| Файл | Хардкож. цветов до | Стало | Δ |
|---|---:|---:|---:|
| `webui/css/themes.css` | 0 | 0 (только +40 токенов) | +40 × 2 темы |
| `webui/css/components.css` | 106 | 66 | −40 (−37%) |
| `webui/css/monitor-app.css` | 20 | 12 | −8 (−40%) |
| `webui/css/pages.css` | 16 | 8 | −8 (−50%) |
| `webui/css/data.css` | 14 | 9 | −5 (−36%) |
| **Итого** | **178** | **123** | **−55 (−31%)** |

**Build & syntax:** `cppworker-stub.exe` + `balancer-stub.exe` собрались,
`node --check webui/js/app.js` + `cppworker-params.js` прошли.

### 1.2 План Session 18 (PF-4 fix) — коммит `e36e6c6`

Детальный план устранения последнего pre-existing test failure.
**Файл:** `plans/2026-q3-session-18-pf4-plan.md` (274 строки, 6 секций).

**Commit:** `e36e6c6 docs(plans): Session 18 task - PF-4 flaky teardown fix`

**Содержание плана:**
- Контекст (что такое PF-4, откуда взялся).
- Затронутые файлы (только test, не production).
- 3 точечных изменения в `tests/first_byte_timeout_test.go`.
- Команды верификации.
- 10 acceptance criteria.
- Чеклист из 12 шагов.
- Backlog Session 19+ (8 кандидатов).

### 1.3 Drift check (подтверждение актуальности плана)

Перечитал `tests/first_byte_timeout_test.go` lines 20-150 + 225-265 в текущей сессии.
**Все 3 точки в плане совпадают с реальным кодом** (drift = 0):
- Line 28-29: `startSlowSSEServer(50*time.Second, ...)` + `defer upstream.Close()` ✅
- Line 111-112: `case <-time.After(70 * time.Second): t.Fatal("...70s")` ✅
- Lines 121-126: `TestOpenAIChat_HeaderTimeout_StillWorks` использует паттерн-референс ✅
- Lines 234-243: keepalive-цикл `startSlowSSEServer` с `case <-timer.C / <-ticker.C` ✅

---

## 2. Текущее состояние репозитория

```
$ git log --oneline -5
e36e6c6 docs(plans): Session 18 task - PF-4 flaky teardown fix      ← HEAD
c4d11fe feat(webui): Session 17 - theme-aware CSS tokens + ...        ← Session 17
db9f3c4 feat(profiles): Session 16 - per-model Parallel + ...         ← Session 16
569afc5 docs(plans): Session 16 task — Per-Model Profiles: ...        ← Session 16 plan
da852f7 feat(webui): Model profiles UI with advanced fields ...        ← Session 15

$ git status --short
? c/llama.cpp                                                          ← subtree, всегда untracked
? check.ps1 check.py en_keys.txt lines.txt missing.txt ru_keys.txt tabs.py tabs.txt  ← артефакты subagent'а, не нужны
```

**Ветка:** `feature/rpc-model-distribution` (на ней же работали Sessions 15-17).
**Все commit'ы запушены:** нужно проверить `git fetch` перед началом следующей сессии.

---

## 3. Что нужно сделать в Session 18

### 3.1 Цель

Устранить последний pre-existing test failure (PF-4), чтобы `go test ./...`
проходил зелёным. Это **P0 задача**, блокирует CI merge per roadmap Q3 секция 11.

### 3.2 Объём работ

- 1 файл с 3 точечными изменениями (`tests/first_byte_timeout_test.go`).
- 2 файла документации (`plans/pre-existing-test-failures.md`, `CHANGELOG.md`).
- Один коммит на новой ветке `fix/pf4-slow-first-token-flaky`.
- Без изменений в production-коде.

**Оценка:** 1-2 часа.

### 3.3 Пошаговый чеклист

#### Шаг 1: Создать ветку

```bash
cd c:/Ollama/ollamalegion
git checkout feature/rpc-model-distribution
git pull  # убедиться что есть e36e6c6
git checkout -b fix/pf4-slow-first-token-flaky
```

#### Шаг 2: Прочитать файлы (опционально, для контекста)

```bash
# Уже прочитано в текущей сессии, drift = 0:
# - tests/first_byte_timeout_test.go (lines 20-150, 225-265)
# - plans/pre-existing-test-failures.md (lines 1-200)
# - internal/balancer/proxy_first_byte_timeout.go (НЕ читался, но НЕ меняется)
```

#### Шаг 3: Изменение 1 — `tests/first_byte_timeout_test.go` line 29

**Найти:**
```go
	upstream := startSlowSSEServer(50*time.Second, []string{"hello", " world", "[DONE]"})
	defer upstream.Close()
```

**Заменить на:**
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

#### Шаг 4: Изменение 2 — `tests/first_byte_timeout_test.go` line 111-112

**Найти:**
```go
	case <-time.After(70 * time.Second):
		t.Fatal("request did not complete within 70s")
```

**Заменить на:**
```go
	case <-time.After(60 * time.Second):
		t.Fatal("request did not complete within 60s")
```

#### Шаг 5: Изменение 3 (defensive, опционально) — `tests/first_byte_timeout_test.go` lines 234-243

**Найти:**
```go
	keepalive:
		for {
			select {
			case <-timer.C:
				break keepalive
			case <-ticker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
```

**Заменить на:**
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

#### Шаг 6: Верификация

**6.1. Targeted test (главный AC):**
```bash
go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s -v
```

Ожидаемый результат:
```
=== RUN   TestOpenAIChat_SlowFirstToken_HoldsConnection
    first_byte_timeout_test.go:XXX: PASS ✅ slow first token held
--- PASS: TestOpenAIChat_SlowFirstToken_HoldsConnection (53.XXXs)   # ~50-55s
PASS
ok      ollama-loadbalancer/tests    53.XXXs
```

**6.2. Regression check (PF-5 не сломан):**
```bash
go test -tags llama_stub ./tests -run TestOpenAIChat_HeaderTimeout_StillWorks -count=1 -timeout 30s -v
```
Ожидаемый: PASS (без регрессий).

**6.3. Full test suite (общая регрессия):**
```bash
go test -tags llama_stub ./tests -count=1 -timeout 300s
go test -tags llama_stub ./internal/... -count=1 -timeout 60s
```
Ожидаемый: без новых FAIL'ов, все ранее зелёные тесты остаются зелёными.

**6.4. Build check:**
```bash
go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker
go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer
```
Ожидаемый: оба exit 0.

#### Шаг 7: Документация

**7.1. `plans/pre-existing-test-failures.md` line 215:**

Заменить:
```
| PF-4 | `TestOpenAIChat_SlowFirstToken_HoldsConnection` | `tests/first_byte_timeout_test.go:114` | ⬜ Backlog (flaky — `httptest.Server.Close()` зависает на `WaitGroup`, не относится к proxy-логике) |
```

На:
```
| PF-4 | `TestOpenAIChat_SlowFirstToken_HoldsConnection` | `tests/first_byte_timeout_test.go:114` | ✅ FIXED 2026-06-27 (Session 18 — принудительное закрытие listener + `CloseClientConnections()` в teardown, паттерн из `TestOpenAIChat_HeaderTimeout_StillWorks`) |
```

**7.2. `CHANGELOG.md`** — добавить подсекцию **после** Session 17 sub-section:

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
(строки 121-126), который уже использует `upstream.Listener.Close()` +
`upstream.CloseClientConnections()` для принудительного teardown hung-сервера.

**Изменения в `tests/first_byte_timeout_test.go`:**
- `defer upstream.Close()` → `defer func() { upstream.Listener.Close(); upstream.CloseClientConnections() }()`.
- `time.After(70s)` → `time.After(60s)` (новый ceiling после фикса).
- (defensive) `startSlowSSEServer`: добавлен `<-r.Context().Done()` в keepalive-цикл для немедленного выхода при отмене клиента.

**Acceptance criteria:**
- `go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s` → PASS за ~50-55s.
- `go test -tags llama_stub ./tests -count=1 -timeout 300s` → PF-4 PASS, остальные сохраняют статус.
- `TestOpenAIChat_HeaderTimeout_StillWorks` (PF-5) остаётся PASS (не сломан).
- `internal/balancer/proxy_first_byte_timeout.go` без изменений.
- `data/state.json` без изменений.

**NB:** все 7 pre-existing failures (PF-1 #1, PF-1 #2, PF-3, PF-4, PF-5, PF-6, PF-7) теперь ✅ FIXED. Q3 метрика "Все pre-existing failures закрыты" выполнена.
```

#### Шаг 8: Коммит

```bash
git add tests/first_byte_timeout_test.go plans/pre-existing-test-failures.md CHANGELOG.md
git status --short
# M  CHANGELOG.md
# M  plans/pre-existing-test-failures.md
# M  tests/first_byte_timeout_test.go

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

git log --oneline -1
# <NEW_HASH> fix(tests): PF-4 flaky teardown ...
```

#### Шаг 9: Опционально — PR в `feature/rpc-model-distribution`

```bash
git push origin fix/pf4-slow-first-token-flaky
# Создать PR через web UI или gh CLI:
gh pr create --base feature/rpc-model-distribution --head fix/pf4-slow-first-token-flaky --title "fix(tests): PF-4 flaky teardown (Session 18)" --body "..."
```

---

## 4. Acceptance criteria (полные)

Перед завершением Session 18 проверить **ВСЕ** пункты:

| # | Критерий | Команда / проверка |
|---|---|---|
| 1 | PF-4 тест PASS за <60s | `go test -tags llama_stub ./tests -run TestOpenAIChat_SlowFirstToken_HoldsConnection -count=1 -timeout 90s -v` |
| 2 | Полный test suite без FAIL на PF-4 | `go test -tags llama_stub ./tests -count=1 -timeout 300s` |
| 3 | PF-5 (HeaderTimeout) не сломан | `go test -tags llama_stub ./tests -run TestOpenAIChat_HeaderTimeout_StillWorks -count=1 -timeout 30s -v` |
| 4 | internal/... не сломан | `go test -tags llama_stub ./internal/... -count=1 -timeout 60s` |
| 5 | cppworker-stub собирается | `go build -tags llama_stub -o cppworker-stub.exe ./cmd/cppworker` (exit 0) |
| 6 | balancer-stub собирается | `go build -tags llama_stub -o balancer-stub.exe ./cmd/balancer` (exit 0) |
| 7 | Commit на ветке `fix/pf4-slow-first-token-flaky` поверх `e36e6c6` | `git log --oneline -2` |
| 8 | `plans/pre-existing-test-failures.md` обновлён (PF-4 → ✅ FIXED) | grep "PF-4" |
| 9 | `CHANGELOG.md` содержит подсекцию Session 18 | grep "Session 18" |
| 10 | Working tree clean (только `c/llama.cpp` untracked) | `git status --short` |

**После прохождения всех 10 критериев → Session 18 закрыта, Q3 метрика "Все pre-existing failures закрыты" выполнена.**

---

## 5. Backlog для Session 19+

После закрытия PF-4 все pre-existing failures зелёные. Дальнейшие кандидаты (по убыванию приоритета):

| Приоритет | Задача | Источник | Оценка | Сложность |
|---|---|---|---:|---|
| 🟡 P1 | Loaded models counter в Dashboard | roadmap 2.2 line 102 | 1ч | low |
| 🟡 P1 | Export logs to CSV | roadmap 5.2 | 2ч | low |
| 🟡 P1 | Notification system (toast) для критических событий | roadmap 5.4 | 3ч | medium |
| 🟡 P1 | Models tab — Bulk operations (multi-select) | roadmap 2.2 line 86 | 2-3ч | medium |
| 🟡 P2 | Models tab — Model details panel | roadmap 2.2 line 85 | 3-4ч | medium |
| 🟡 P2 | Live tail Logs через WebSocket | roadmap 5.3 | 4-5ч | medium |
| 🟡 P1 | rpc_coordinator production mode | roadmap 3.2 | 5-8 дней | high (server-side) |
| 🟡 P1 | virtual_router production mode | roadmap 3.3 | 5-7 дней | high (server-side) |
| ⚪ P3 | Audit TODO/FIXME/STUB в коде | — | TBD | TBD |

**Рекомендация для Session 19:** выбрать одну из P1 UI/UX задач (1-3ч), не превышающих размер одной сессии. Например: **Loaded models counter в Dashboard** (1ч, чисто UI, хороший quick-win).

---

## 6. Известные нюансы

### 6.1 Stub-бинарники

`cppworker-stub.exe` и `balancer-stub.exe` — артефакты сборки, в git
не коммитятся (есть в `.gitignore`). После тестов можно удалить:
```bash
rm -f cppworker-stub.exe balancer-stub.exe
```

### 6.2 Артефакты subagent'а

В `git status` видны untracked файлы:
```
? check.ps1 check.py en_keys.txt lines.txt missing.txt ru_keys.txt tabs.py tabs.txt
```

Это диагностические скрипты от subagent'а (сравнение i18n ключей, поиск TODO
маркеров). **НЕ коммитить**, удалить перед коммитом:
```bash
rm -f check.ps1 check.py en_keys.txt lines.txt missing.txt ru_keys.txt tabs.py tabs.txt
```

### 6.3 Ветка feature/rpc-model-distribution

Рабочая ветка для Sessions 15-18. После merge PR из `fix/pf4-slow-first-token-flaky`
можно либо оставить `feature/rpc-model-distribution` для следующих сессий,
либо начать cycle заново (merge в main, новая feature-ветка).

### 6.4 Production-код не меняется

Session 18 — **только test fixture**. Никаких изменений в:
- `internal/balancer/proxy_first_byte_timeout.go`
- `internal/balancer/proxy.go`
- `internal/balancer/proxy_request.go`
- `cmd/cppworker/*` 
- WebUI (`webui/`)

Если при выполнении тестов обнаружится что production-код **реально** содержит
баг (а не только test teardown) — стоп, эскалация, обсуждение плана.

---

## 7. Контекстные ссылки

- `plans/2026-q3-roadmap.md` — главный roadmap, секция 11 "Метрики успеха Q3".
- `plans/pre-existing-test-failures.md` — полный список PF-1..PF-7.
- `plans/2026-q3-session-18-pf4-plan.md` — детальный план Session 18.
- `plans/2026-q3-session-17-css-audit-plan.md` — план Session 17 (для истории).
- `CHANGELOG.md` — секции Sessions 4-17, новая подсекция Session 18.
- `internal/balancer/proxy_first_byte_timeout.go` — production first-byte timeout (НЕ меняется).
- `tests/first_byte_timeout_test.go` — целевой файл (3 точечных изменения).

---

## 8. Контрольный список (для следующей сессии)

Перед началом работы подтвердить:

- [ ] Ветка `feature/rpc-model-distribution` @ `e36e6c6` (HEAD).
- [ ] Прочитан `plans/2026-q3-session-18-pf4-plan.md` (если не читали в этой сессии).
- [ ] Прочитан `tests/first_byte_timeout_test.go` (drift check; строки 20-150 + 225-265).
- [ ] Создана новая ветка `fix/pf4-slow-first-token-flaky`.
- [ ] Удалены артефакты subagent'а (`check.ps1 check.py *.txt tabs.py`).
- [ ] Понимание что production-код не меняется.

После завершения Session 18:

- [ ] Все 10 acceptance criteria прошли.
- [ ] Commit сделан на ветке `fix/pf4-slow-first-token-flaky`.
- [ ] Working tree clean.
- [ ] (Опционально) PR создан и смержен в `feature/rpc-model-distribution`.

**Готово к старту Session 18.** ✅