# Test Fixes — 2026-06-05

Документ описывает фиксы 5 предсуществующих падающих тестов в `internal/balancer/`,
обнаруженных после завершения 9-шагового плана cppworker-preflight-nctx-session-report.

## Сводка

| Тест | Файл | Симптом | Корневая причина | Фикс |
| --- | --- | --- | --- | --- |
| `TestAutoPullFullScenario` | `auto_pull_test.go` | 0 generate-запросов, 503 "no llama.cpp backend available" | LlamaCppRouter.Route перехватывает `/api/generate` до `proxyRequest` | `proxy.llamaCppRouter = nil` + добавлен `modelContextKey` в request context |
| `TestSelectBackendAllBusy` | `proxy_test.go:815` | `assert.Empty` падает — возвращается `backend-1` | `selectByResources` не отсеивает backend при `active >= maxReqs` (queueing-aware) | Ассерт обновлён под least-loaded fallback |
| `TestSelectByResources` | `backend_selector_test.go:129` | `assert.Empty` падает при `ActiveReqs=10` | То же — queueing-aware fallback | Ассерт обновлён под `b1`/`b2` |
| `TestTwoClientsSameIP_DifferentSessions` | `proxy_integration_test.go:95` | 0 сессий — 503 до создания сессии | LlamaCppRouter.Route перехватывает `/api/generate` | `proxy.llamaCppRouter = nil` |
| `TestLoadBalancing_MultipleClients` | `proxy_integration_test.go:150` | 12/12 клиентов получают 503 | То же | `proxy.llamaCppRouter = nil` |

**Итог**: 5/5 тестов починены, полный пакет `go test ./internal/balancer/ -count=1` PASS за 17.8s.

## Корневая причина (одна для трёх тестов)

После добавления `LlamaCppRouter` (cppworker-интеграция) `routeRequest` в
`internal/balancer/router.go` работает так:

```go
bt := p.determineRequestBackendType(r)
allowed := p.getDefaultAllowedTypes()  // для mode == "" → [Ollama, LlamaCpp]
// determineRequestBackendType возвращает "" (длина > 1) для /api/generate

// Пробуем llama.cpp роутер если тип LlamaCpp или не указан (смешанный кластер)
if (bt == "" || bt == types.BackendTypeLlamaCpp) && p.llamaCppRouter != nil {
    if p.llamaCppRouter.Route(w, r) {  // ← сработает для /api/generate
        return true
    }
}
```

`LlamaCppRouter.Route` для `/api/generate` вызывает `handleGenerate`, который делает:

```go
backendID := lr.proxy.selectBackend(model, types.BackendTypeLlamaCpp)
if backendID == "" {
    writeJSON(w, 503, "no llama.cpp backend available")
}
```

Тестовые бэкенды в `createTestConfig()` (и аналогичных в
`proxy_integration_test.go`) имеют пустой `Type`, что после `normalizeBackendType`
становится `""` или `BackendTypeOllama`. Поэтому `selectBackend(model, BackendTypeLlamaCpp)`
возвращает `""` → 503 **до** того, как основной Ollama-flow (`proxyRequest`) получит шанс
обработать запрос.

В production эта проблема не проявляется, потому что у реальных бэкендов явно задан
`Type` (`ollama` или `llama_cpp`), и `selectBackend` находит подходящий.

## Принцип фикса: «fix the test, not the production code»

Согласно плану из `plans/cppworker-preflight-nctx-session-report.md` (шаг 1),
production-код (`router.go`, `llamacpp_router.go`) считается корректным. Тесты писались
под основной Ollama-flow и должны идти через `proxyRequest`, а не через `LlamaCppRouter.Route`.

## Что изменено

### `internal/balancer/auto_pull_test.go` — `TestAutoPullFullScenario`

**Изменение 1**: после `proxy := NewProxy(config)` добавлено:
```go
proxy.llamaCppRouter = nil
```
с комментарием, поясняющим, что тест проверяет Ollama-flow и должен идти через
основной `ServeHTTP → proxyRequest`.

**Изменение 2**: в request добавлен `modelContextKey`:
```go
req = req.WithContext(context.WithValue(req.Context(), modelContextKey, "test-model:latest"))
```
В production `ServeHTTP` устанавливает `modelContextKey` после парсинга body. Тест
отправляет запрос напрямую — ставим ключ вручную, чтобы `isModelNotFoundError` в
`proxyRequest` нашёл `modelFromCtx` и `EnsureModel` получил имя модели для auto-pull.

### `internal/balancer/proxy_test.go` — `TestSelectBackendAllBusy`

**До**:
```go
backend := proxy.selectBackend("", "")
assert.Empty(t, backend)
```

**После**:
```go
backend := proxy.selectBackend("", "")
assert.NotEmpty(t, backend, "queueing-aware: должен выбрать least-loaded backend для постановки в очередь")
assert.Contains(t, []string{"backend-1", "backend-2"}, backend)
```

### `internal/balancer/backend_selector_test.go` — `TestSelectByResources`

**До**:
```go
got2 := p.selectByResources(nil)
if got2 != "" {
    t.Errorf("selectByResources() with full load = %s, want empty", got2)
}
```

**После**:
```go
// Queueing-aware fallback: при полной загрузке selectByResources возвращает
// least-loaded backend (для последующей постановки в очередь), а не empty.
// Решение о реальной доступности слота принимает tryAcquireSlot в dispatchRequest:
// если слот занят — ErrNoBackendAvailable, queue requeue'ит запрос.
got2 := p.selectByResources(nil)
if got2 != "b1" && got2 != "b2" {
    t.Errorf("selectByResources() with full load = %s, want b1 or b2 (queueing-aware fallback)", got2)
}
```

### `internal/balancer/proxy_integration_test.go` — `TestTwoClientsSameIP_DifferentSessions` + `TestLoadBalancing_MultipleClients`

После `proxy := NewProxy(cfg)` добавлено:
```go
proxy.llamaCppRouter = nil
```

с комментарием-обоснованием, аналогичным `TestAutoPullFullScenario`.

## Почему queueing-aware fallback (а не empty) — корректное поведение

В `internal/balancer/backend_selector.go` функция `selectByResources` (P4 — финальный
fallback после P1/P2/P3) **намеренно** не отсеивает бэкенды с `active >= maxReqs` —
комментарий в коде явно объясняет это:

> Не отсеиваем бэкенд если слоты заняты — пусть `tryAcquireSlot` в `dispatchRequest`
> решает можно ли захватить. Если слот занят — `dispatch` вернёт `ErrNoBackendAvailable`
> и `queue requeue`'ит запрос, дожидаясь освобождения.

То есть новая семантика — **не «отказать при полной загрузке»**, а **«поставить в
очередь через `queueMgr`»**. Это и есть тот самый queueing-aware fallback, который
нативно поддерживается `QueueManager` (FIFO с `QueueTimeout` и `QueueMaxSize`).

## Верификация

```bash
# Шаг A1: TestAutoPullFullScenario
go test ./internal/balancer/ -run "TestAutoPullFullScenario" -v
# → PASS (0.04s)

# Шаг B1: TestSelectBackendAllBusy
go test ./internal/balancer/ -run "TestSelectBackendAllBusy" -v
# → PASS (0.00s)

# Шаг C1: TestSelectByResources
go test ./internal/balancer/ -run "TestSelectByResources$" -v
# → PASS (0.00s)

# Шаг D: полный пакет
go test ./internal/balancer/ -count=1 -timeout 5m
# → ok  ollama-loadbalancer/internal/balancer  17.827s
```

## Что НЕ вошло в этот план (out of scope)

- `go test ./tests/` падает с `bridge.c:7:10: fatal error: llama.h: No such file or directory`.
  Это **не связано** с нашими правками — для интеграционных тестов в `./tests/` требуется
  C-сборка llama.cpp (`llama.h` генерируется скриптом `build-llama-cpp.sh`). Это
  инфраструктурная проблема окружения, не регрессия от cppworker-интеграции.