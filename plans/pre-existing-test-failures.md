# Pre-existing test failures — план доработки

## Контекст

Во время фикса бага «пустой ответ gemma-4 в Cline» (2026-06-24) выявлены два
нестабильных теста, не связанных с основным багом. Оба требуют отдельной
доработки.

| # | Тест | Модуль | Связь с пустым ответом gemma |
|---|------|--------|------------------------------|
| 1 | `internal/api/TestBackendsHandler_Get` | REST API списка бэкендов | ❌ нет |
| 2 | `tests/TestServeHTTP_MixedCluster_RoutingByURLPath` | Балансировка в mixed cluster | ❌ нет |
| 3 | `cmd/cppworker/prestream_nctx_check_test.go` | Сломанный orphan-тест (ссылается на несуществующую `preStreamNCtxCheck`) | ❌ нет |

Все три сбоя **pre-existing** (присутствовали до фикса gemma), но блокируют зелёный
`go test ./...` и мешают CI.

---

## Задача #1: `TestBackendsHandler_Get` ожидает `total=2`, получает `1`

### Симптом

```bash
go test -tags llama_stub ./internal/api -count=1 -run "TestBackendsHandler_Get" -v
# → FAIL: TestBackendsHandler_Get (0.01s)
#     handlers_test.go:181:
#       Error: Not equal:
#         expected: 2
#         actual  : 1
```

### Затронутые файлы

- `internal/api/handlers_test.go:181` — тест
- `internal/api/handlers_backends.go:103-138` — production-код `listBackends`

### Корневая причина

`createTestServer` (`handlers_test.go:18-87`) создаёт два бэкенда:

```go
{ID: "backend1", Status: types.StatusHealthy,   ...},
{ID: "backend2", Status: types.StatusUnhealthy, ...},
```

`listBackends` (`handlers_backends.go:103-138`) фильтрует unhealthy-бэкенды
**по умолчанию** (by design — чтобы WebUI не показывал «мёртвые» ноды):

```go
includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"
unhealthyStatuses := map[types.BackendStatus]bool{
    types.StatusUnhealthy:         true,
    types.StatusOffline:           true,
    types.StatusDraining:          true,
    types.StatusOllamaUnavailable: true,
}
...
for _, backend := range allBackends {
    if !includeUnhealthy && unhealthyStatuses[backend.Status] {
        continue  // <-- отфильтровывает backend2
    }
    ...
}
```

Тест не передаёт `?includeUnhealthy=true` → `backend2` отфильтровывается →
возвращается `total: 1`. Это **не баг кода**, это **баг тестовой аксиомы**.

### Варианты фикса (выбрать один)

| # | Подход | Плюсы | Минусы |
|---|--------|-------|--------|
| **1** | Тест шлёт `?includeUnhealthy=true` и ожидает `2` | Полное покрытие фильтра | Меняется контракт теста |
| **2** | Тест ожидает `total == 1` (только healthy) | Не меняет тест-аксиому | Покрывает только happy path |
| **3** | Оба бэкенда в `createTestServer` создаются со `StatusHealthy` | Минимальные изменения | Прячет фильтр unhealthy от тестов |

**Рекомендация: вариант 1** — наиболее полное покрытие, проверяет и фильтр, и
happy path одновременно. Альтернатива — вариант 3, если важно не трогать
тест-аксиому.

### Acceptance criteria

- `go test -tags llama_stub ./internal/api -count=1 -run "TestBackendsHandler_Get"` завершается без ошибок.
- Все остальные тесты в `./internal/api` остаются зелёными (или сохраняют те
  же pre-existing failures, что и сейчас).
- Если выбран вариант 1: добавить docstring к тесту, поясняющий почему
  `includeUnhealthy=true` обязателен.

---

## Задача #2: `TestServeHTTP_MixedCluster_RoutingByURLPath` — timeout 60s

### Симптом

```bash
go test -tags llama_stub ./tests -count=1 -timeout 60s -run "TestServeHTTP_MixedCluster_RoutingByURLPath"
# → FAIL
#     panic: test timed out after 1m0s
#         running tests:
#             TestServeHTTP_MixedCluster_RoutingByURLPath (28s)
```

### Затронутые файлы

- `tests/backend_type_isolation_test.go:702-714` — тест `TestServeHTTP_MixedCluster_RoutingByURLPath`
- `tests/backend_type_isolation_test.go` — функция-хелпер `setupMixedCluster`
- `internal/balancer/proxy.go:711` — production `Proxy.ServeHTTP`
- `internal/balancer/proxy_request.go:220` — production `Proxy.proxyRequest`

### Stack trace (из лога panic)

```
goroutine 931 [select]:
  net/http.(*Transport).getConn(...)
  net/http.(*Client).Do(...)
  ollama-loadbalancer/internal/balancer.(*Proxy).proxyRequest
      c:/Ollama/ollamalegion/internal/balancer/proxy_request.go:220 +0x1c3d
  ollama-loadbalancer/internal/balancer.(*Proxy).ServeHTTP
      c:/Ollama/ollamalegion/internal/balancer/proxy.go:711 +0x30ca
  ollama-loadbalancer/tests.TestServeHTTP_MixedCluster_RoutingByURLPath
      c:/Ollama/ollamalegion/tests/backend_type_isolation_test.go:714 +0x214
```

### Корневая причина

`setupMixedCluster` создаёт `balancer.Proxy` с бэкендами:

```go
{ID: "llamacpp-1", Host: "localhost", OllamaPort: 11434, ...}
{ID: "llamacpp-2", Host: "localhost", OllamaPort: 11435, ...}
```

**`httptest.NewServer` для этих бэкендов НЕ поднимается** — на портах
`11434`/`11435` никто не слушает.

Когда `Proxy.ServeHTTP` пытается форварднуть `POST /api/generate`:

1. `proxyRequest` → `http.Client.Do(req)` (proxy_request.go:220)
2. TCP-connect к `127.0.0.1:11434` → `connectex: No connection could be made`
3. Production-настройка `Balancing.RequestTimeout = 30` (seconds) должна
   сработать через 30 сек.
4. **Но Go testing framework ставит свой alarm на 60 сек** (заданный флагом
   `-timeout 60s`) и срабатывает **раньше** как `panic: test timed out`.

### Варианты фикса (выбрать один)

| # | Подход | Плюсы | Минусы |
|---|--------|-------|--------|
| **1** | Поднять `httptest.NewServer` для каждого mock-бэкенда, вернуть URL через `backend.Host`/`backend.Port` | Минимальный рефакторинг | Больше boilerplate в setupMixedCluster |
| **2** | Ввести интерфейс `BackendTransport` (или `RoundTripper`) и подменять его в тестах мок-имплементацией без сети | Самый чистый, переиспользуемый | Требует рефакторинга `internal/balancer/proxy.go` |
| **3** | Установить `ctx` с малой длительностью (50ms) для прокси, чтобы `Proxy` вернул 503 раньше тестового timeout | Минимальные изменения | Тест проверяет не то, что заявлено |

**Рекомендация: вариант 2** — наиболее чистое решение для долгосрочной
поддержки тестов. Альтернатива — вариант 1, если нет ресурсов на рефакторинг.

### Acceptance criteria

- `go test -tags llama_stub ./tests -count=1 -timeout 30s -run "TestServeHTTP_MixedCluster_RoutingByURLPath"` завершается без panic и без timeout (за <5 секунд).
- Все остальные тесты в `./tests` остаются зелёными (или сохраняют те же pre-existing failures).
- Если выбран вариант 2: добавить unit-тесты для `BackendTransport` интерфейса (mock-имплементация покрывает: 200, 503, timeout, connection refused).
- Если выбран вариант 1: добавить docstring к `setupMixedCluster`, поясняющий, что поднимает `httptest.Server` на ephemeral-портах.

---

## Общий план работ

1. **Обсудить с командой** варианты фикса для обеих задач (acceptance criteria выше готовы).
2. **Создать ветку** `fix/pre-existing-test-failures` от `feature/rpc-model-distribution`.
3. **Реализовать выбранные варианты**:
   - Задача #1: один тест-файл, ~5 строк изменений.
   - Задача #2: ~50–200 строк изменений (в зависимости от варианта).
4. **Прогнать полный test suite**:
   - `go test -tags llama_stub ./cmd/cppworker -count=1` (базовая зелёная зона)
   - `go test -tags llama_stub ./internal/... -count=1 -timeout 60s`
   - `go test -tags llama_stub ./tests -count=1 -timeout 60s`
5. **Убедиться**, что нет новых сбоев и нет регрессий по сравнению с baseline.
6. **Code review**, merge в `feature/rpc-model-distribution`.

## Приоритет

- **Low** — баги не блокируют production, только test suite.
- **High** — если планируется CI с автозапуском тестов на каждый push.

## Связь с другими задачами

**Не связаны** с:

- Регрессом «пустой ответ gemma-4 в Cline» (исправлен 2026-06-24, см. CHANGELOG.md)
- RAM fallback для tools/n_ctx (см. `docs/runbook-tools.md`)
- OpenWebUI tool_calls runbook

## Затронутые модули (резюме)

```
internal/api/handlers_test.go         # задача #1
internal/api/handlers_backends.go     # задача #1 (только для справки)
tests/backend_type_isolation_test.go  # задача #2
internal/balancer/proxy.go            # задача #2 (если выбран вариант 2)
internal/balancer/proxy_request.go    # задача #2 (если выбран вариант 2)
```

## История

| Дата | Событие |
|------|---------|
| 2026-06-24 | Сбои обнаружены во время фикса «пустой ответ gemma-4» |
| 2026-06-24 | Задокументированы в `plans/pre-existing-test-failures.md` (этот файл) |