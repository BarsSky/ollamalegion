# C-Bridge Abort API — Specification

**Target**: `c/bridge/bridge.h`, `c/bridge/bridge.c`, `c/bridge/bridge.go`
**CppWorker**: `cmd/cppworker/`
**Goal**: Thread-safe, race-free abort of in-flight `bridge_infer_stream` calls.

## 1. Problem Statement

`bridge_infer_stream(ModelHandle, GenerationParams, stream_callback, user_data)` is a blocking C
function that calls `llama_decode` in a per-token loop. Currently:

- No mechanism to interrupt the loop externally.
- No way for Go-side `context.Done()` to propagate into the C call.
- Resource leak: VRAM, KV-cache, slot occupied until natural completion (60-120s for reasoning models).

## 2. Required C API (Minimum Viable)

### 2.1 Header changes (`c/bridge/bridge.h`)

```c
// ============================================================
// Round 31 #6: Abort API (2026-08-09)
// ============================================================

// BRIDGE_ERR_ABORTED — возвращается из bridge_infer_stream при abort.
// Положительные значения = количество сгенерированных токенов (как обычно).
// Ноль или отрицательные = ошибки (как BRIDGE_ERR_*).
#define BRIDGE_ERR_ABORTED (-100)

// bridge_request_abort — Помечает запрос к model как aborted.
// bridge_infer_stream прервёт цикл генерации в ближайшей точке проверки
// (между llama_decode батчами) и вернёт BRIDGE_ERR_ABORTED.
//
// После вызова bridge_request_abort модель остается загруженной и пригодна
// для повторного использования — abort НЕ выгружает модель.
//
// SAFETY: thread-safe. Можно вызывать из любого потока, включая Go-горутины
// через cgo. Atomic_int под капотом.
//
// Параметры:
//   model — opaque указатель (ModelHandle)
//
// Возвращает: 0 = abort request accepted, -1 = invalid model handle.
int bridge_request_abort(ModelHandle model);

// bridge_request_abort_all — Помечает все активные инференсы как aborted.
// Используется при shutdown или экстренном завершении cppworker.
void bridge_request_abort_all(void);

// bridge_is_aborted — Проверяет, был ли подан abort request.
// В основном для диагностики и тестов.
bool bridge_is_aborted(ModelHandle model);
```

### 2.2 C implementation (`c/bridge/bridge.c`)

```c
// Per-model abort flag. Atomic, so safe to set from Go cgo thread.
// Доступ через atomic_load / atomic_store (C11 stdatomic.h) или __atomic_* (GCC).
#include <stdatomic.h>

// В структуре InternalModel добавить:
//   atomic_int abort_requested;  // 0 = not aborted, 1 = abort

// В начале bridge_infer_stream:
//   atomic_store(&im->abort_requested, 0);  // сбрасываем перед каждым infer
//
// В цикле generation (между llama_decode батчами):
//   if (atomic_load(&im->abort_requested)) {
//       // Освободить partial output
//       return BRIDGE_ERR_ABORTED;
//   }
//
// В bridge_request_abort:
//   if (model == NULL) return -1;
//   InternalModel* im = (InternalModel*)model;
//   atomic_store(&im->abort_requested, 1);
//   return 0;
```

### 2.3 Threading model

```
┌─────────────┐                              ┌──────────────────┐
│ Go handler  │ ──bridge_infer_stream──▶    │  C bridge         │
│ goroutine   │ ◀──token callbacks──         │  (blocking C)    │
└─────────────┘                              └──────────────────┘
       │                                            ▲
       │  ctx.Done()                                │
       │       │                                    │ atomic flag
       │       ▼                                    │
       │  bridge_request_abort(model) ───────────────┘
       │       │
       ▼       ▼
   BRIDGE_ERR_ABORTED returned
   (after current llama_decode batch completes)
```

Important: **abort check happens BETWEEN llama_decode calls, not during**. This is safe
because llama_decode is a single forward pass that takes milliseconds to seconds. Cancel
latency is bounded by single batch time.

### 2.4 Go-side integration (`c/bridge/bridge.go`)

```go
// RequestAbort — Round 31 #6: пометить инференс как aborted.
// Thread-safe. Можно вызывать из любой горутины (включая signal handlers
// через select-case ctx.Done).
//
// После abort bridge_infer_stream вернёт BRIDGE_ERR_ABORTED.
// Вызывающий код ОБЯЗАН:
//   1. Дождаться завершения текущего bridge_infer_stream
//   2. Освободить InferenceResult через bridge_free_inference_result
//   3. Модель можно использовать повторно (abort не выгружает)
func RequestAbort(model ModelHandle) error {
    rc := C.bridge_request_abort(model)
    if rc != 0 {
        return fmt.Errorf("bridge_request_abort failed: %d", rc)
    }
    return nil
}

// RequestAbortAll — abort all active inferences. Используется при shutdown.
func RequestAbortAll() {
    C.bridge_request_abort_all()
}
```

### 2.5 cppworker integration (`cmd/cppworker/handlers_inference.go`)

```go
// В начале request handling (после парсинга body):
ctx, cancel := context.WithCancel(r.Context())
defer cancel()

// Запуск generation в goroutine для возможности cancel:
done := make(chan struct{})
var result *InferenceResult
var err error
go func() {
    defer close(done)
    result, err = bridge.GenerateStream(model, params, callback)
}()

// На отмене клиента (например, через r.Context().Done()):
go func() {
    <-ctx.Done()
    if ctx.Err() != nil {
        log.Warn("client cancelled, aborting generation", "model", model)
        bridge.RequestAbort(model)
    }
}()

// Дождаться завершения (нормального или abort):
<-done
```

## 3. Optional: Hard cancel via pthread_kill

**Status**: NOT RECOMMENDED для первой итерации. Можно добавить позже.

Идея: прервать blocking `llama_decode` через `pthread_kill(thread, SIGUSR1)`. Это ОЧЕНЬ опасно:
- llama_decode может быть в середине memory allocation
- C-стек может быть в inconsistent state
- Resource leak если не обработать правильно

**Требования**:
- Generation должна быть в отдельном pthread (не main thread)
- SIGUSR1 handler должен быть minimal (только atomic flag set)
- После возврата из llama_decode (через EINTR) — проверить флаг и exit gracefully
- Тщательное тестирование с sanitizers (ASan, TSan, MSan)

**Не рекомендуется** для production, потому что:
1. Сложность implementation
2. Race conditions между SIGUSR1 и normal flow
3. Платформо-зависимое поведение (Linux vs macOS vs Windows MinGW)
4. Round 31 #1 auto-stream workaround уже покрывает основной use case (cancel между чанками)

## 4. Testing

### 4.1 C unit-tests (`c/bridge/tests/test_abort_api.c`)

```c
// Test 1: abort before infer starts
// Test 2: abort during long generation (> 1000 tokens)
// Test 3: abort + retry — model should work after abort
// Test 4: RequestAbortAll with 10 concurrent inferences
// Test 5: Abort non-existent model handle (negative case)
```

### 4.2 Go integration tests (`c/bridge/bridge_abort_test.go`)

```go
// TestAbort_BeforeInfer — abort flag is set, infer returns immediately with BRIDGE_ERR_ABORTED
// TestAbort_DuringInfer — start long infer in goroutine, abort after 100ms, verify return
// TestAbort_AfterCompletion — set flag after infer completes, no-op
// TestAbort_MultipleInferences — abort one, others continue
```

### 4.3 Live verification (`cmd/cppworker/handlers_inference_test.go`)

```go
// Test 1: Send long prompt to cppworker
// Test 2: Cancel HTTP request (close TCP)
// Test 3: Verify cppworker frees slot within < 1 second (not 60+ seconds)
// Test 4: Verify VRAM released
```

## 5. Compatibility

- **Backward compat**: existing code без abort продолжает работать (флаг = 0 = no abort).
- **Build tag**: код компилируется с `-tags llama_stub` через `BRIDGE_NOOP` macro в stub mode.
- **Windows MinGW**: `pthread_kill` не доступен, поэтому hard cancel (Section 3) — Linux/macOS only.
  Soft cancel (Section 2) работает везде.

## 6. Estimated Effort

| Component | Effort | Notes |
|-----------|--------|-------|
| C API + atomic flag (Section 2) | 4-6 hours | Junior C developer |
| C unit tests | 2-3 hours | |
| Go-side integration | 1-2 hours | Junior Go developer |
| Live verification | 1-2 hours | Manual + scripts |
| Total | 8-13 hours | **MUST be C developer** |

Hard cancel (Section 3) — дополнительно 20+ hours, не рекомендуется.
