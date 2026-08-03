# Round 22 — Общий аудит балансера: полнота проброса Ollama API + LLama.cpp API

**Дата:** 2026-08-03
**Контекст:** Пользователь запросил общий аудит после Rounds 18+20+21 — что ещё не покрыто
в плане полноты проброса Ollama API до бэкендов (llama.cpp + Ollama).
**Стек:** bundled-full, RTX 3070, CUDA_ARCH=86, `qwen3-4b` (alias) →
`Qwen3-Instruct-2507-q4km.gguf` + `gemma-4-E4B-it-Q4_K_M.gguf` на диске.

---

## TL;DR

| # | Severity | Issue | Round 18/20 covered? |
|---|----------|-------|----------------------|
| 1 | **P0** | Read/MGMT endpoints (show, pull, copy, create, delete, push, blobs, models/files) вызывают warmup, что вешает их на 10-30s timeout | **НЕТ** (не покрыто) |
| 2 | **P0** | `resolveModelPath` ломается для `name=alias` если на диске 2+ .gguf файла (auto-pick требует `len==1`) | **НЕТ** (не покрыто) |
| 3 | **P0** | OpenAI `/v1/embeddings` не пробрасывается (cppworker не имеет llama_cpp handler для этого пути) | **НЕТ** (не покрыто) |
| 4 | **P1** | `/api/show` не работает для `name=<alias>` если на бэкенде ещё не загружена модель (то же что #2 на стороне show handler) | косвенно (через #2) |
| 5 | **P1** | `/api/ps` после auto-load failure возвращает `{"models":null}` (Round 19 hotfix правит только успешный кейс) | косвенно |
| 6 | **P1** | Sync model load timeout = 10s, но реальный load 5GB модели = 50-70s. Только `/api/chat` (где клиент ждёт ответа) получает реальный load, для read endpoints это просто timeout | косвенно |
| 7 | **P2** | OpenAI `/v1/audio/*`, `/v1/images/*` — нет реализации, но balancer не возвращает явный 404, а виснет в timeout | **НЕТ** |
| 8 | **P2** | OpenWebUI/Cline отправляют `OPTIONS` (CORS preflight) — нужна проверка, поддерживает ли balancer | **НЕТ** |
| 9 | **P2** | No metrics: количество "phantom load attempts" / "skipped warmup for read endpoint" / "alias resolution failures" — нельзя мониторить | **НЕТ** |
| 10 | **P3** | `/api/push` — Ollama registry, не нужен для llama.cpp, должен явно возвращать 501 | **НЕТ** (cppworker уже возвращает 501, но balancer тратит 30s на warmup перед этим) |
| 11 | **P3** | `/api/signin`, `/api/logout`, `/api/web/*` — отсутствуют полностью, должны явно 404 | **НЕТ** |
| 12 | **P3** | Headers pass-through: balancer не пробрасывает `X-Request-Id` от клиента (генерирует свой) | **НЕТ** |

---

## Подробный анализ P0-issues

### BUG #1: Read/MGMT endpoints вызывают warmup (10-30s timeout)

**Симптомы (live, текущий билд):**
- `POST /api/show {"name":"qwen3-4b"}` — timeout 60s (vs ожидание ~30ms)
- `POST /api/pull {"name":"qwen3-4b"}` — timeout 30s
- `POST /api/copy {"source":...,"destination":...}` — timeout 30s
- `POST /api/create {"name":"test","modelfile":"..."}` — timeout 30s
- `POST /api/delete {"name":"q-copy"}` — timeout 30s
- `POST /api/push {"name":"..."}` — timeout 30s (cppworker отдаёт 501, но balancer ждёт warmup 30s)
- `GET /api/models/files` — в Round 21 c. timeout 10s (раньше 33ms) — **regression!**

**Root cause:**
- `Proxy.ServeHTTP` → `selectBackend(model)` → `selectByResources` (P4 fallback) — backend
  выбран. **Но** `selectBackend` имеет "3. Sync Model Load" (P3) который
  вызывает `warmupModel(backendID, ..., model)` для **каждого** запроса, если
  `SyncModelLoad.Enabled=true` и модель не loaded на бэкенде.
- warmup инициирует POST /load к cppworker → cppworker не может загрузить (см. BUG #2)
  → возвращает 500 → balancer всё равно ждёт syncTimeout (10s по умолчанию)
- Для read/mgmt endpoints модель **НЕ нужна** в VRAM — они работают с файлом
  на диске или просто регистрируют/удаляют имя.

**Fix:**
- Добавить whitelist путей, для которых `SyncModelLoad` и `warmup` SKIP'аются:
  - `/api/show`, `/api/pull`, `/api/copy`, `/api/delete`, `/api/create`, `/api/push`
  - `/api/blobs/*`
  - `/api/models/files` (read-only disk scan)
- Реализовать в `selectBackend` или в `Proxy.ServeHTTP` до вызова `selectBackend`:
  ```go
  if isReadOnlyOrMgmtEndpoint(r.URL.Path) {
      // skip warmup, just route to backend that has the model on disk
  }
  ```
- Альтернатива: добавить флаг в `selectBackend` (например, `SkipWarmup bool`).

### BUG #2: resolveModelPath не находит alias когда на диске 2+ файла

**Симптомы (live, текущий билд):**
```
auto-load failed: cppworker error (HTTP 500): load failed: load model qwen3-4b: load model failed: failed to load model from models/qwen3-4b.gguf
```

**Root cause:**
- В `internal/cppbackend/model_manager.go` `resolveModelPath(modelName string)`:
  - Шаг 1: `FindModelByPath("qwen3-4b")` — ищет в `m.ggufFiles` (map файлов на диске)
    - Точное совпадение `m.ggufFiles["qwen3-4b"]` — нет
    - Без расширения `m.ggufFiles["qwen3-4b.gguf"]` — нет
    - `Contains(lower(name), "qwen3-4b")` — "Qwen3-Instruct-2507-q4km" не содержит "qwen3-4b",
      "gemma-4-E4B-it-Q4_K_M" не содержит "qwen3-4b"
  - Шаг 2: glob `modelsDir/qwen3-4b*.gguf` — ничего
  - Шаг 3 (Round 21 P0.1): `if len(files) == 1 { auto-pick }` — len=2, не сработало
  - Шаг 4: fallback `modelsDir/qwen3-4b.gguf` — файл не существует → 500

**Fix:**
- ModelManager должен помнить `name → path` маппинг при успешной загрузке.
  Это решает "после idle-unload, alias = qwen3-4b не резолвится в правильный файл".
- Изменения в `internal/cppbackend/model_manager.go`:
  - Добавить `nameHistory map[string]string` (name → path)
  - При `LoadModel(name, path)` после успеха — `m.nameHistory[name] = path`
  - В `FindModelByPath`/`resolveModelPath` — Шаг 0: проверить `nameHistory` ПЕРВЫМ
- Заодно улучшить Шаг 3 (auto-pick):
  - Если `len(files) == 2` и у одного из них `name` частично совпадает с `requested_name` (например, contains "qwen3") — auto-pick более вероятный

### BUG #3: OpenAI /v1/embeddings не пробрасывается

**Симптомы (live):**
- `POST /v1/embeddings {"model":"qwen3-4b","input":"hi"}` — timeout 30s

**Root cause:**
- cppworker имеет `/api/embed` и `/api/embeddings` (Round 21 P0.3)
- Но НЕ имеет `/v1/embeddings` (OpenAI-style)
- Balancer `routeRequest` определяет backendType=llama_cpp (по пути) → вызывает `LlamaCppRouter.Route`
- LlamaCppRouter не имеет handler для `/v1/embeddings` → fall through к основному proxy
- Proxy отправляет на cppworker → 404 → wait + timeout

**Fix:**
- Добавить в `internal/balancer/llamacpp_handlers_readonly.go`:
  - Handler `handleOpenAIEmbeddings` — проксирует `/v1/embeddings` →
    `/api/embed` на cppworker, конвертит формат
  - Зарегистрировать в роутере
- ИЛИ проще: добавить redirect-style proxy: balancer получает `/v1/embeddings`,
  переписывает path в `/api/embed`, отправляет на cppworker, конвертит response обратно.

---

## Дополнительные P1/P2

### BUG #4-6: см. таблицу выше

### BUG #7: OpenAI /v1/audio/* /v1/images/* — явный 404
- `routeRequest` определяет bt="" (неизвестный путь) → fall through на main proxy
- Main proxy отправляет на cppworker → 404 → wait + timeout 30s
- Должен быть **early 404** для явно-неподдерживаемых путей, чтобы не висеть

### BUG #8: CORS preflight
- OpenWebUI и Cline отправляют `OPTIONS` для cross-origin
- Если balancer не отвечает на OPTIONS — preflight fail, запрос не проходит
- Проверить, есть ли в middleware `OPTIONS` handler

### BUG #9: Отсутствие observability
- Не считаются счётчики: "phantom load attempts", "skipped warmup", "alias resolution failures"
- Невозможно мониторить "балансер ведёт себя плохо"

### BUG #10-12: Явные ошибки вместо timeout
- /api/push — должен 501 (cppworker уже делает это, но balancer тратит 30s)
- /api/signin, /api/web/* — должны 404 явно, не timeout
- /api/blobs/* — должны быть проксированы (upload/download digest), сейчас unknown

---

## План исправлений (по порядку)

### Шаг 1: P0-фиксы
1. **Fix #1** — Read/MGMT endpoints SKIP warmup (proxy.go + routing)
2. **Fix #2** — ModelManager `name → path` history (model_manager.go)
3. **Fix #3** — OpenAI `/v1/embeddings` проксирование (llamacpp_handlers_readonly.go)

### Шаг 2: P1-фиксы
4. **Fix #4** — явная обработка alias в show (вызов `resolveModelPath` ДО check loaded)
5. **Fix #5** — `/api/ps` после auto-load failure — cache last successful loaded list
6. **Fix #6** — увеличить syncTimeout для конкретных endpoints

### Шаг 3: P2-фиксы
7. **Fix #7** — early 404 для unknown OpenAI endpoints
8. **Fix #8** — CORS preflight handler
9. **Fix #9** — счётчики observability
10. **Fix #10-12** — явные 501/404 для /api/push, /api/signin, /api/web/*, /api/blobs/*

### Шаг 4: Verify
- Все 19 test_api_coverage.py T1-T19 должны проходить
- Дополнить verify suite тестами для исправленных багов
- Live-verify на bundled-full стеке

### Шаг 5: Tag
- v0.5.4 = Round 17.3 + R19 + R21 + R22 (P0+P1+P2)

---

## Известные ограничения (не Round 22 scope)

- Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive — model broken by design, tag-parser не поможет
- /v1/realtime — WebSocket, не реализован (out of scope)
- Tool calling для моделей без native support (требует chat template override)
- Multi-modality (vision/audio) — нет в cppworker
