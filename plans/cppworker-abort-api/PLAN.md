# cppworker Abort API — Implementation Plan

**Status**: Ready for C developer
**Created**: 2026-08-09
**Owner**: TBD (C developer needed for Phase 1-2)
**Related**: `spec.md`, `task-breakdown.md`, `workarounds.md`, `README.md`
**Round**: Round 31 #6 (closed-with-W1, planning W2)

---

## 0. TL;DR

Реализовать soft-cancel через atomic flag в `c/bridge/bridge.c`:
- 1 atomic int per `InternalModel`
- Проверка перед каждым `llama_decode` (prompt phase, gen phase, batched phase)
- Новый return code `BRIDGE_ERR_ABORTED` (-100)
- Go-binding `bridge.RequestAbort(model)` + интеграция в cppworker через watcher goroutine
- **Реально закрывает gap** там, где текущий callback-based cancel НЕ работает (prompt phase + batched_decode + status distinction)

**Hard cancel (pthread_kill) — НЕ в скоупе** (см. spec.md §3).

**Все 5 открытых вопросов решены 2026-08-09** — см. §8. Решения: Q1=Go-side, Q2=5-строчный getter, Q3=гибрид Ollama/OpenAI, Q4=убрать safety timer, Q5=per-model v1.

---

## 1. Gap Analysis (что реально отсутствует)

Спека `spec.md` описывает abort API так, будто в текущем коде cancel вообще нет. Это **не совсем так** — есть существующий механизм, который покрывает 95% streaming-cases. Разберём gap честно.

### 1.1 Что УЖЕ работает без abort API

`bridge_infer_stream` (c/bridge/bridge.c:1552) уже умеет прерывать цикл генерации через возврат `0` из callback:

```c
// bridge.c:1699-1702
if (callback(token_text, token_len, user_data) == 0) {
    llama_sampler_free(sampler);
    return 0; // клиент остановил стриминг
}
```

Go-сторона в `cmd/cppworker/handlers_chat.go:596-601`, `handlers_generate.go:427-432`, `handlers_openai.go:1475-1480` уже проверяет `ctx.Done()` внутри callback и возвращает `false`, что транслируется в `0` в C.

**Результат**: streaming cancel между токенами **уже работает**. Cancel latency = время генерации одного токена (~50-200ms для gemma-4).

### 1.2 Чего реально НЕ хватает (3 точки разрыва)

| # | Gap | Где | Влияние |
|---|-----|-----|---------|
| **G1** | Prompt decoding phase не прерывается | `bridge.c:1615-1636` (loop без callback) | Длинный prompt (Cline 70K chars = ~20K tokens) декодируется без возможности cancel. На 7B модели при batch=512 это 40+ llama_decode calls × ~50ms = 2 секунды блокировки, в худшем случае с большим n_ctx — дольше |
| **G2** | Batched parallel decode не прерывается | `bridge.c:398-520` (`bridge_batched_decode`) | Multi-slot сценарий (n_parallel > 1) не имеет cancel-хука вообще |
| **G3** | Status code не отличает cancelled от success | `bridge.c:1701` — return 0 при cancel | Метрики, логи, balancer-side accounting путают cancelled request с успешным. Невозможно корректно посчитать `cancelled_count` для telemetry |

### 1.3 Что даёт реализация W2 из spec

- Закрывает G1, G2, G3 одновременно
- Cancel latency в prompt phase: с 2-5s → 0 (между батчами промпта)
- Появляется structured `BRIDGE_ERR_ABORTED` для корректной телеметрии
- VRAM/slot release ускоряется на 2-5s в worst case (для cancel во время prompt decode)

---

## 2. Точки вставки в существующий код

### 2.1 `c/bridge/bridge_internal.h:43-54` (InternalModel struct)

**Действие**: добавить `atomic_int abort_requested;`

```c
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;
    uint32_t ctx_n_batch;
    // Round 31 #6: soft-cancel flag. atomic_int — потокобезопасно
    // для set из Go cgo thread (RequestAbort) и read из C thread
    // (bridge_infer_stream). Memory ordering: relaxed (нам не важна
    // синхронизация других полей, только флаг).
    atomic_int abort_requested;
} InternalModel;
```

**Include**: `#include <stdatomic.h>` в начало `bridge_internal.h` (уже подключается транзитивно через `llama.h` — проверить, при необходимости добавить явно).

### 2.2 `c/bridge/bridge.c:994` (LoadModel)

**Действие**: инициализировать флаг после успешной загрузки.

```c
im->ctx_n_ctx = ctx_params.n_ctx;
im->ctx_n_batch = ctx_params.n_batch;
atomic_init(&im->abort_requested, 0);  // NEW
```

### 2.3 `c/bridge/bridge.c:1552-1572` (bridge_infer_stream — вход)

**Действие**: сбрасывать флаг в начале каждого infer.

```c
int bridge_infer_stream(ModelHandle model, ...) {
    if (model == NULL || prompt == NULL || callback == NULL) {
        set_error("invalid arguments to bridge_infer_stream");
        return 1;
    }
    InternalModel *im = (InternalModel *)model;
    // Round 31 #6: сбросить флаг перед новым infer (модель переиспользуема
    // после abort, abort НЕ выгружает модель).
    atomic_store(&im->abort_requested, 0);  // NEW
    // ...reset_inference_state(im, seq_id) — существующий код
}
```

### 2.4 `c/bridge/bridge.c:1371` (prompt decode в bridge_infer)

**Действие**: проверять флаг ПЕРЕД `llama_decode` промпт-батча.

```c
// Перед строкой 1371 (если уже есть)
if (atomic_load(&im->abort_requested)) {
    free(tokens);
    result.status = BRIDGE_ERR_ABORTED;
    result.error_msg = strdup("generation aborted by user (prompt phase)");
    llama_batch_free(batch);
    return result;
}
if (llama_decode(im->context, batch) != 0) { /* existing */ }
```

**Аналогично** в `bridge_infer_stream`: между строками 1628 и 1630.

### 2.5 `c/bridge/bridge.c:1434` (gen decode в bridge_infer)

**Действие**: проверять флаг ПЕРЕД `llama_decode` gen-шага.

```c
// Перед строкой 1434
if (atomic_load(&im->abort_requested)) {
    free(output);
    result.status = BRIDGE_ERR_ABORTED;
    result.error_msg = strdup("generation aborted by user (gen phase)");
    llama_batch_free(gen_batch);
    llama_sampler_free(sampler);
    return result;
}
if (llama_decode(im->context, gen_batch) != 0) { /* existing */ }
```

**Аналогично** в `bridge_infer_stream`: между строками 1707 и 1709.

### 2.6 `c/bridge/bridge.c:1709` (gen decode в stream)

**Примечание**: тут уже есть callback, который ловит `ctx.Done()`. **Abort check НЕ дублирует** callback, а дополняет:
- callback срабатывает ПОСЛЕ генерации токена (между gen_decode и следующим gen_decode)
- abort check срабатывает ДО gen_decode (включая случай, когда callback ещё не вызван — например, очень длинные токены декодируются секунды)

**Действие**: оставить abort check **перед** gen_decode (line 1709) как defense-in-depth, **НЕ удалять** callback check (line 1699). Два независимых механизма, оба нужны:

```c
// 1699-1702 (existing): callback check
if (callback(token_text, token_len, user_data) == 0) {
    llama_sampler_free(sampler);
    return BRIDGE_ERR_ABORTED;  // CHANGED: 0 → BRIDGE_ERR_ABORTED (-100)
}

// 1707-1715 (existing + NEW abort check)
struct llama_batch gen_batch = build_batch_with_seq(...);
if (atomic_load(&im->abort_requested)) {  // NEW
    llama_batch_free(gen_batch);
    llama_sampler_free(sampler);
    return BRIDGE_ERR_ABORTED;
}
if (llama_decode(im->context, gen_batch) != 0) { /* existing */ }
```

**Изменение return code**: `return 0;` на строке 1701 → `return BRIDGE_ERR_ABORTED;` чтобы отличать cancelled от success.

### 2.7 `c/bridge/bridge.c:465` (bridge_batched_decode)

**Действие**: проверять флаг ПЕРЕД `llama_decode` в batched path. Это закрывает G2.

```c
// Перед строкой 465
if (atomic_load(&im->abort_requested)) {
    set_error("batched decode aborted by user");
    llama_batch_free(batch);
    return BRIDGE_ERR_ABORTED;
}
int decode_rc = llama_decode(im->context, batch);
```

### 2.8 `c/bridge/bridge.c:~1990` (новые функции)

**Действие**: добавить новые функции ПЕРЕД `#endif // GO_BRIDGE_LLAMA_STUB guard` (строка 1993).

```c
// ============================================================
// Round 31 #6: Abort API (2026-08-09)
// ============================================================

// bridge_request_abort — пометить инференс для данной модели как aborted.
// Thread-safe (atomic_store). Можно вызывать из любой горутины / cgo thread.
// Возвращает 0 при успехе, -1 если model == NULL.
int bridge_request_abort(ModelHandle model) {
    if (model == NULL) return -1;
    InternalModel *im = (InternalModel *)model;
    atomic_store(&im->abort_requested, 1);
    return 0;
}

// bridge_request_abort_all — DECISION (2026-08-09, Q1): no-op в C.
// Реализация: cppworker shutdown итерирует свой Go-side registry
// моделей (Backend.models в internal/cppbackend/backend.go:109) и
// вызывает bridge.RequestAbort на каждую. Это упрощает C-side и
// не требует нового global mutex. Оставлен как C-export для API
// completeness (Go-binding RequestAbortAll) и для будущего
// использования если добавим g_models_list.
void bridge_request_abort_all(void) {
    // No-op: см. DECISION Q1.
    // Go-сторона: cmd/cppworker при shutdown делает
    //   for _, h := range backend.GetAllHandles() { bridge.RequestAbort(h) }
}

// bridge_is_aborted — диагностика (для тестов и логов).
bool bridge_is_aborted(ModelHandle model) {
    if (model == NULL) return false;
    InternalModel *im = (InternalModel *)model;
    return atomic_load(&im->abort_requested) == 1;
}
```

**DECISION Q1 (resolved 2026-08-09)**: `bridge_request_abort_all` — **Go-side** (cppworker shutdown итерирует свой `Backend.models` map и вызывает `bridge.RequestAbort` на каждый handle). C-функция существует как no-op для API completeness. Это упрощает C-side, не добавляет нового global mutex, и переиспользует уже thread-safe итерацию по registry на Go-стороне.

### 2.9 `c/bridge/bridge.h` (header)

**Действие**: добавить декларации.

```c
// В секции "Коды ошибок" (после BRIDGE_ERR_BAD_REQUEST):
#define BRIDGE_ERR_ABORTED (-100)

// В секции "Функции bridge" (после bridge_free_string):
int  bridge_request_abort(ModelHandle model);
void bridge_request_abort_all(void);
bool bridge_is_aborted(ModelHandle model);
```

---

## 3. Go-side integration

### 3.1 `c/bridge/bridge.go` (real build)

**Действие**: добавить CGo-binding.

```go
// В районе строки 35 (после import "encoding/json"):

/*
#include <stdlib.h>
#include "bridge.h"

// В C-коде: extern-прототипы (для cgo signature)
extern int bridge_request_abort(ModelHandle model);
extern void bridge_request_abort_all(void);
extern bool bridge_is_aborted(ModelHandle model);
*/
import "C"

// Функции:

// RequestAbort — Round 31 #6: пометить инференс как aborted.
// Thread-safe. Можно вызывать из любой горутины (включая ctx.Done watcher).
//
// После abort bridge_infer_stream вернёт BRIDGE_ERR_ABORTED (-100).
// Вызывающий код ОБЯЗАН дождаться завершения текущего infer через
// channel/wg (abort не прерывает C-blocking call мгновенно — только
// между llama_decode батчами).
//
// Модель НЕ выгружается. После abort можно переиспользовать через
// bridge.GenerateStream снова (abort флаг сбрасывается в начале).
func RequestAbort(model *ModelHandle) error {
    if model == nil {
        return fmt.Errorf("bridge.RequestAbort: nil model")
    }
    rc := C.bridge_request_abort(model.ptr)
    if rc != 0 {
        return fmt.Errorf("bridge_request_abort failed: %d", rc)
    }
    return nil
}

// RequestAbortAll — abort ВСЕ активные инференсы.
// Используется при shutdown / panic / config reload.
func RequestAbortAll() {
    C.bridge_request_abort_all()
}

// IsAborted — диагностика: был ли подан abort request.
func IsAborted(model *ModelHandle) bool {
    if model == nil {
        return false
    }
    return bool(C.bridge_is_aborted(model.ptr))
}
```

### 3.2 `c/bridge/bridge_stub.go` (stub build)

**Действие**: добавить stub-реализации для build tag `llama_stub`.

```go
// В районе строки 25 (после типа ModelHandle):

// RequestAbort — stub: no-op (нет реальной C-bridge).
func RequestAbort(model *ModelHandle) error {
    return nil
}

// RequestAbortAll — stub: no-op.
func RequestAbortAll() {}

// IsAborted — stub: всегда false.
func IsAborted(model *ModelHandle) bool {
    return false
}
```

### 3.3 `cmd/cppworker/` — интеграция в handler'ы

**Стратегия**: НЕ модифицировать `handlers_chat.go`, `handlers_generate.go`, `handlers_openai.go` напрямую. Создать обёртку `cmd/cppworker/abort_watcher.go`:

```go
// abort_watcher.go — Round 31 #6: spawn watcher goroutine для ctx.Done → bridge.RequestAbort.
//
// Использование:
//
//     ctx, cancel := context.WithCancel(r.Context())
//     defer cancel()
//     abortWatcher := NewAbortWatcher(ctx, modelHandle)
//     defer abortWatcher.Wait()  // НЕ Stop, см. ниже
//
//     // существующий callback остаётся без изменений
//     callback := func(token string) bool { ... ctx.Done() check ... }
//     err := backend.GenerateStream(modelName, prompt, params, callback)
//
// При ctx.Done() watcher немедленно вызывает bridge.RequestAbort(model).
// Cancel latency: между текущим и следующим llama_decode (~50-200ms).
//
// DECISION Q4 (2026-08-09): НЕТ safety timer (24h) в select —
// dead code без пользы. Handler ВСЕГДА использует r.Context()
// (cancellable), значит ctx.Done() сработает в течение минут.
// Precondition: "watcher требует cancellable ctx". Если передан
// context.Background() — caller получит leaked goroutine, и это
// его баг, не наш. Документировать в godoc.
package main

import (
    "context"
    "C:/Ollama/ollamalegion/c/bridge"
    "C:/Ollama/ollamalegion/internal/logger"
)

type AbortWatcher struct {
    done chan struct{} // closed после run() завершения
}

func NewAbortWatcher(ctx context.Context, model *bridge.ModelHandle) *AbortWatcher {
    w := &AbortWatcher{done: make(chan struct{})}
    go w.run(ctx, model)
    return w
}

func (w *AbortWatcher) run(ctx context.Context, model *bridge.ModelHandle) {
    defer close(w.done)
    <-ctx.Done()  // Блокируется пока ctx не отменён
    log := logger.Get()
    if err := bridge.RequestAbort(model); err != nil {
        log.Warnw("AbortWatcher: bridge.RequestAbort failed",
            "error", err, "model", model.Path)
    } else {
        log.Infow("AbortWatcher: abort requested via C-bridge",
            "ctx_err", ctx.Err().Error())
    }
}

// Wait — блокирует до завершения watcher (для тестов и shutdown).
// В production handler не обязан вызывать — defer abortWatcher.Wait()
// не нужен, потому что watcher goroutine либо уже done, либо
// вот-вот done (ctx отменён при возврате handler'а).
// Метод оставлен для тестов которые хотят синхронизироваться.
func (w *AbortWatcher) Wait() { <-w.done }
```

**Интеграция** в 3 handler'а (минимальные изменения, ~3 строки на handler):

```go
// В handleChatStream (handlers_chat.go:580+), handleGenerateStream
// (handlers_generate.go:417+), writeOpenAIChatStream (handlers_openai.go:1475+):
// ДОБАВИТЬ после `ctx := r.Context()`:

if handle, ok := backend.GetHandle(modelName); ok {
    _ = NewAbortWatcher(ctx, handle)  // fire-and-forget; goroutine exit on ctx.Done
}
```

### 3.3.1 `internal/cppbackend/backend.go` — добавить GetHandle (DECISION Q2)

**DECISION Q2 (resolved 2026-08-09)**: `Backend.GetHandle(name)` — **добавить** как новый метод. Существующий `Backend.models map[string]*modelInstance` (line 109) уже thread-safe под `mu.RLock`, и `modelInstance.handle *bridge.ModelHandle` (line 329) уже хранится. Нужен trivial getter — 5 строк.

```go
// Добавить в internal/cppbackend/backend.go (рядом с GetModel, ~line 1485):

// GetHandle возвращает *bridge.ModelHandle для загруженной модели.
// Используется abort_watcher для проброса в bridge.RequestAbort
// при ctx.Done(). Round 31 #6.
//
// Возвращает (handle, true) если модель загружена, (nil, false) иначе.
// Thread-safe: RLock на короткое время.
func (b *Backend) GetHandle(name string) (*bridge.ModelHandle, bool) {
    b.mu.RLock()
    defer b.mu.RUnlock()
    inst, exists := b.models[name]
    if !exists {
        return nil, false
    }
    return inst.handle, true
}

// GetAllHandles возвращает snapshot всех handles. Используется при
// cppworker shutdown для RequestAbort каждой модели. Round 31 #6.
func (b *Backend) GetAllHandles() []*bridge.ModelHandle {
    b.mu.RLock()
    defer b.mu.RUnlock()
    handles := make([]*bridge.ModelHandle, 0, len(b.models))
    for _, inst := range b.models {
        handles = append(handles, inst.handle)
    }
    return handles
}
```

**Backward compat**: новые методы, ничего не ломают. Существующий `GetModel` остаётся как был.

### 3.3.2 Wire protocol (DECISION Q3)

**DECISION Q3 (resolved 2026-08-09)**: гибридная схема для backward compat с OpenAI-клиентами.

| Endpoint | Финальный chunk при cancelled | Почему |
|----------|------------------------------|--------|
| `/api/chat` (Ollama NDJSON) | `done_reason: "cancelled"` + `cancelled: true` | Ollama spec нестрогий к `done_reason`; расширение безопасно |
| `/api/generate` (Ollama NDJSON) | `done_reason: "cancelled"` + `cancelled: true` | same |
| `/v1/chat/completions` (OpenAI SSE) | `finish_reason: "stop"` + `cancelled: true` field | OpenAI-клиенты строгие к `finish_reason`; "stop" безопасный fallback |
| `/v1/completions` (OpenAI SSE) | `finish_reason: "stop"` + `cancelled: true` field | same |

**Правило**: source of truth для metrics — internal `BRIDGE_ERR_ABORTED` (Go-side error). Клиентский `cancelled: true` флаг — для visibility, не для логики.

```go
// В Go callback wrapper (handlers_chat.go:596+):
// При возврате callback=false И context cancelled:
if ctx.Err() != nil {
    // cancelled — emit финальный chunk с cancelled: true
    finalChunk := map[string]interface{}{
        "model":    modelName,
        "done":     true,
        "done_reason": "cancelled",  // Ollama
        // "finish_reason": "stop",  // OpenAI (если нужно)
        "cancelled": true,
    }
    // ... marshal + write
}
```

---

## 4. Phase Breakdown

| Phase | Что | Effort | Owner | Зависит от |
|-------|-----|--------|-------|-----------|
| **P1** | C-side: atomic flag + check points (2.1-2.8) | 4-6h | C developer | — |
| **P2** | C unit tests (`c/bridge/tests/test_abort_api.c`) | 2-3h | C developer | P1 |
| **P3** | Go bindings (3.1, 3.2) | 1-2h | Go developer | P1 |
| **P4** | cppworker integration (3.3) — `Backend.GetHandle` + abort_watcher + handler hook (3-4 строки на handler) | 1-2h | Go developer | P3 |
| **P5** | Live verification + CMake build | 1-2h | Both | P4 |
| **P6** | Sanitizers (ASan, TSan) | 1-2h | C developer | P5 |
| **Total** | | **10-17h** | C primary | |

**В сравнении с `task-breakdown.md`** (8-13h): +2-4h на sanitizers и более широкие live tests.

**Уточнение по P4** (Q2 resolved 2026-08-09): `Backend.GetHandle` — 5 строк, не полноценный refactor. `Backend.GetAllHandles` для shutdown — ещё 5 строк. Общий overhead P4 минимальный.

---

## 5. Тестовые сценарии (конкретные)

### 5.1 C unit tests (`c/bridge/tests/test_abort_api.c`)

| # | Сценарий | Ожидаемый результат |
|---|----------|---------------------|
| T1 | `bridge_request_abort(NULL)` | return -1, no crash |
| T2 | `bridge_request_abort(valid_model)` → `bridge_infer_stream` НЕ запущен | abort флаг = 1, model валидный |
| T3 | Запустить long infer (>500 tokens) в thread, через 100ms вызвать `bridge_request_abort` | infer возвращает `BRIDGE_ERR_ABORTED` в течение 200ms (single batch time) |
| T4 | После abort — повторный `bridge_infer_stream` | успешно выполняется (abort не выгружает модель) |
| T5 | 10 concurrent `bridge_infer_stream` + 1 `bridge_request_abort_all` (или per-model abort) | все возвращают `BRIDGE_ERR_ABORTED` в течение 1.5s |
| T6 | 100 goroutines set flag concurrently на 1 model (TSan test) | no race, no crash |
| T7 | `bridge_is_aborted` после abort | return true |
| T8 | `bridge_is_aborted` после успешного infer (abort НЕ был вызван) | return false |
| T9 | `bridge_is_aborted(NULL)` | return false, no crash |

**Инфраструктура тестов**:
- Использовать small GGUF model (e.g. tinyllama-1b q4_0, ~700MB)
- Mock long generation: `n_predict=5000` на prompt "repeat the word 'test' 5000 times" → ~30s generation, достаточно для abort test
- Или использовать синтетический loop с programmable sleep (если test framework позволяет)

### 5.2 Go integration tests (`c/bridge/bridge_abort_test.go`)

```go
// TestAbort_BeforeInfer — моночный test, не требует реальной модели
func TestAbort_BeforeInfer(t *testing.T) {
    // Можно сделать с stub mode: загрузить любой GGUF, abort до infer
    // Но stub mode не вызывает реальный C-bridge. Нужен integration test
    // с реальной моделью → выделить в отдельный test (tag: integration)
}

// TestAbort_RequestAbort_NilModel — error path
func TestAbort_RequestAbort_NilModel(t *testing.T) {
    err := bridge.RequestAbort(nil)
    if err == nil { t.Fatal("expected error for nil model") }
}

// TestAbort_RequestAbort_ValidModel — happy path (требует реальной модели)
func TestAbort_RequestAbort_ValidModel(t *testing.T) {
    if testing.Short() { t.Skip("requires real model") }
    handle := loadTestModel(t)  // helper, загружает tinyllama
    defer bridge.FreeModel(handle)
    if err := bridge.RequestAbort(handle); err != nil {
        t.Fatalf("RequestAbort: %v", err)
    }
    if !bridge.IsAborted(handle) {
        t.Fatal("IsAborted should return true after RequestAbort")
    }
}
```

### 5.3 Live verification (`cmd/cppworker/handlers_inference_test.go`)

```go
// TestLive_StreamingCancel_VRAMReleased — критичный integration test
func TestLive_StreamingCancel_VRAMReleased(t *testing.T) {
    if testing.Short() { t.Skip() }
    srv := setupRealCppWorker(t)
    defer srv.Close()

    vramBefore := readVRAMUsageMB(t)

    // Запустить long generation
    ctx, cancel := context.WithCancel(context.Background())
    req, _ := http.NewRequestWithContext(ctx, "POST",
        srv.URL+"/v1/chat/completions",
        strings.NewReader(`{"model":"gemma-4","stream":true,"max_tokens":2000,"messages":[{"role":"user","content":"Write a 1000 word essay"}]}`))

    resp, err := http.DefaultClient.Do(req)
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()

    // Прочитать 5 chunks, потом cancel
    scanner := bufio.NewScanner(resp.Body)
    chunks := 0
    for scanner.Scan() {
        chunks++
        if chunks >= 5 {
            cancel()
            break
        }
    }

    // Ждать освобождения VRAM (max 3s)
    deadline := time.Now().Add(3 * time.Second)
    for time.Now().Before(deadline) {
        vramNow := readVRAMUsageMB(t)
        if vramNow < vramBefore+100 {  // +100MB tolerance
            t.Logf("VRAM released in %v (chunks=%d)", time.Since(deadline.Add(-3*time.Second)), chunks)
            return
        }
        time.Sleep(100 * time.Millisecond)
    }
    vramAfter := readVRAMUsageMB(t)
    t.Fatalf("VRAM not released within 3s: before=%d after=%d", vramBefore, vramAfter)
}
```

### 5.4 Sanitizers (P6)

```bash
# CMake build с sanitizers
cd c/bridge/build
cmake -DCMAKE_C_FLAGS="-fsanitize=address,thread -g -O1" ..
make -j
ctest --output-on-failure

# Особое внимание: TSan будет детектировать race на atomic флаге
# если memory_order не указан явно. Использовать memory_order_relaxed
# для relaxed semantics (это и так default для atomic_store/load
# без параметров в C11).
```

---

## 6. Acceptance Criteria

Реализация считается complete, когда **ВСЕ** пункты выполнены:

- [ ] **AC1**: C unit tests (T1-T9) PASS
- [ ] **AC2**: Go integration tests PASS
- [ ] **AC3**: Live test (5.3) — VRAM released в течение 3s после cancel
- [ ] **AC4**: ASan clean (no memory leaks в abort path)
- [ ] **AC5**: TSan clean (no data race на atomic flag)
- [ ] **AC6**: Stub mode (`-tags llama_stub`) собирается без warnings
- [ ] **AC7**: Round 25-31 regression tests PASS (reasoning translation, profile sync, batched decode)
- [ ] **AC8**: Cancel latency в prompt phase < 500ms (long prompt + abort = exit в течение 500ms)
- [ ] **AC9**: Status code `BRIDGE_ERR_ABORTED` корректно проброшен в Go-side error (не маскируется как success)
- [ ] **AC10**: Документация обновлена: `CHANGELOG.md`, `c/bridge/README.md` (если есть), `docs/BRIDGE_API.md`

---

## 7. Риски и митигации

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| **R1**: Race в atomic flag (TSan fail) | High | Medium | Использовать `memory_order_relaxed` (default); TSan gate в CI; T1-T6 тесты с `-fsanitize=thread` |
| **R2**: Memory leak в abort path (ASan fail) | High | Low | Explicit cleanup: `llama_batch_free` + `llama_sampler_free` + `free(output)` во всех abort branches; AC4 gate |
| **R3**: Stub mode broken | Medium | Low | Добавить stub для всех 3 функций (3.2); CI build с `-tags llama_stub` |
| **R4**: Regression в Round 25-31 | High | Medium | AC7 gate; запуск полного test suite (`go test ./...`); live test на bundled-full |
| **R5**: model_registry refactor ломает existing backend API | Medium | Medium | Минимальный refactor: добавить `GetHandle(name)` method, не менять existing API; backward compatible |
| **R6**: Cancel не срабатывает в prompt phase (G1 не закрыт) | High | Low | Abort check в строке 1371 + тест T3 (long prompt + abort) |
| **R7**: Hard cancel нужен (W5) после реализации W2 | Low | Low | Round 31 #1 + W2 покрывают 99% use cases; W5 отложен в spec §3 |
| **R8**: Windows MinGW compatibility | Medium | Low | `<stdatomic.h>` поддерживается в MinGW-w64; `pthread_kill` НЕ используется (W5 out of scope) |

---

## 8. Resolved Decisions (2026-08-09)

| # | Вопрос | Решение | Обоснование |
|---|--------|---------|-------------|
| **Q1** | `bridge_request_abort_all` — C (g_models_list mutex) или Go (cppworker итерирует свой registry)? | **Go-side** | Упрощает C (нет нового global mutex); переиспользует уже thread-safe `Backend.models` map. C-функция остаётся как no-op для API completeness. |
| **Q2** | `model_registry.GetHandle(name)` — есть или создать? | **Создать `Backend.GetHandle(name)` в `internal/cppbackend/backend.go`** (5 строк) | Registry (`Backend.models`) и поле `modelInstance.handle` уже есть, нет экспорта наружу. Trivial getter под существующим `mu.RLock`. Также добавить `Backend.GetAllHandles()` для shutdown итерации. |
| **Q3** | `done_reason` для cancelled в NDJSON/SSE? | **Гибрид**: Ollama `done_reason: "cancelled"`, OpenAI `finish_reason: "stop"` + `cancelled: true` field | Ollama spec нестрогий к done_reason — безопасно расширить. OpenAI-клиенты строгие к finish_reason — "stop" safest fallback. Source of truth для metrics — internal `BRIDGE_ERR_ABORTED`. |
| **Q4** | Safety timer для abort_watcher (24h в драфте) | **Убрать таймер полностью** | Dead code: handler ВСЕГДА использует `r.Context()` (cancellable), значит ctx.Done() сработает в течение минут. 24h — magic number без пользы. Precondition: "watcher требует cancellable ctx". Если хочется defense-in-depth — лучше W3 (generation timeout) orthogonal к abort API. |
| **Q5** | Per-model vs per-slot abort при `n_parallel > 1`? | **Per-model для v1** (atomic flag в `InternalModel`, не per `seq_id`) | Спека говорит per-model. При n_parallel=2 abort отменит ОБА concurrent infer на этой модели — приемлемо для v1. Per-slot (по seq_id) — будущая итерация, требует дополнительного аргумента в `bridge_request_abort(model, seq_id)`. |

---

## 9. После реализации

- [ ] Update `c/bridge/bridge.h` doc comments (ссылка на Round 31 #6)
- [ ] Update `cmd/cppworker/CHANGELOG.md` (или где changelog)
- [ ] Update `docs/ARCHITECTURE.md` (если есть): добавить секцию про cancel flow
- [ ] Закрыть Round 31 как 7/7 (с #6 implemented) или оставить как 6/7 + W1 + W2 (если W5 не делаем)
- [ ] Memory entry: `cppworker abort API verified YYYY-MM-DD` с привязкой к этому плану

---

## 10. References

- **Spec**: [`spec.md`](./spec.md) — полная спецификация API
- **Task breakdown**: [`task-breakdown.md`](./task-breakdown.md) — оригинальный 5-phase план (нужно reconcile с этим PLAN.md)
- **Workarounds**: [`workarounds.md`](./workarounds.md) — W1 (auto-stream, Round 31 #1) уже реализован
- **README**: [`README.md`](./README.md) — обзор проблемы
- **Round 31 memory**: agent memory entry от 2026-08-09 (Round 31 #1 + #2 + #4 + #7 + #6 planning)
- **Codebase anchors**:
  - `c/bridge/bridge.c:1552` — `bridge_infer_stream` (target для abort check)
  - `c/bridge/bridge.c:1308` — `bridge_infer` (target для abort check)
  - `c/bridge/bridge.c:398` — `bridge_batched_decode` (target для abort check)
  - `c/bridge/bridge.c:1699-1702` — existing callback-based cancel
  - `cmd/cppworker/handlers_chat.go:596-601` — existing ctx.Done check в callback
  - `cmd/cppworker/handlers_generate.go:427-432` — same
  - `cmd/cppworker/handlers_openai.go:1475-1480` — same
