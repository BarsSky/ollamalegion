# C-Side Patch — Round 31 #6 Abort API

**Target files**: `c/bridge/bridge_internal.h`, `c/bridge/bridge.h`, `c/bridge/bridge.c`
**Build system**: `c/bridge/CMakeLists.txt` (optional, для тестов)
**Requires**: C11 `<stdatomic.h>`, GCC 11+ / MinGW-w64 / Clang 14+
**Effort**: 4-6h для опытного C developer
**Go-side уже реализован** в `c/bridge/bridge.go` — `RequestAbort/RequestAbortAll/IsAborted` скомпилируются только если C-функции существуют. Применение этого патча ОБЯЗАТЕЛЬНО для работоспособности.

---

## 1. `c/bridge/bridge_internal.h` — добавить atomic field

**Файл**: `c/bridge/bridge_internal.h` (строки 10-14: include guard + includes)

**Действие 1.1**: Добавить `#include <stdatomic.h>` рядом с существующими includes.

```c
// bridge_internal.h:10-14
#ifndef BRIDGE_INTERNAL_H
#define BRIDGE_INTERNAL_H

#include "llama.h"
#include <stdint.h>
#include <stdatomic.h>  // NEW: Round 31 #6 (atomic_int в InternalModel)
```

**Действие 1.2**: Добавить `atomic_int abort_requested;` в `struct InternalModel` (строки 43-54).

```c
// bridge_internal.h:43-54 (текущий код)
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    // NOTE: sampler больше НЕ хранится в InternalModel...
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;
    uint32_t ctx_n_batch;
} InternalModel;
```

**Заменить на**:
```c
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    // NOTE: sampler больше НЕ хранится в InternalModel...
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;
    uint32_t ctx_n_batch;
    // Round 31 #6 (2026-08-09): soft-cancel flag.
    // atomic_int — thread-safe set из Go cgo thread (RequestAbort) и
    // read из C thread (bridge_infer_stream). Memory ordering: relaxed
    // (default для atomic_store/load без явного параметра в C11).
    // 0 = not aborted (или abort отменён / infer завершён нормально),
    // 1 = abort запрошен, infer должен выйти на ближайшей check point.
    atomic_int abort_requested;
} InternalModel;
```

---

## 2. `c/bridge/bridge.h` — добавить declarations

**Файл**: `c/bridge/bridge.h` (строки 222-228: error codes; после строки 209: free functions)

**Действие 2.1**: Добавить `BRIDGE_ERR_ABORTED` в секцию error codes (после `BRIDGE_ERR_BAD_REQUEST`, перед `BridgeErrorInfo`).

```c
// bridge.h:222-228 (текущий)
#define BRIDGE_OK                       0
#define BRIDGE_ERR_GENERIC              1
#define BRIDGE_ERR_N_CTX_NEEDS_RELOAD   2
#define BRIDGE_ERR_PROMPT_TOO_LONG      3
#define BRIDGE_ERR_GPU_OOM              4
#define BRIDGE_ERR_BAD_REQUEST          5
```

**Дополнить**:
```c
#define BRIDGE_OK                       0
#define BRIDGE_ERR_GENERIC              1
#define BRIDGE_ERR_N_CTX_NEEDS_RELOAD   2
#define BRIDGE_ERR_PROMPT_TOO_LONG      3
#define BRIDGE_ERR_GPU_OOM              4
#define BRIDGE_ERR_BAD_REQUEST          5
// Round 31 #6 (2026-08-09): soft cancel от Go-стороны (ctx.Done, user Stop,
// RequestAbort от balancer). ОТРИЦАТЕЛЬНОЕ значение специально — чтобы легко
// отличать от positive int (token count) и от других BRIDGE_ERR_*.
#define BRIDGE_ERR_ABORTED              (-100)
```

**Действие 2.2**: Добавить declarations в секцию функций (после `bridge_free_string`, ~строка 212).

```c
// bridge.h — после bridge_free_string (line 212)
// ============================================================
// Round 31 #6 (2026-08-09): Abort API
// ============================================================

// bridge_request_abort — пометить текущий infer для данной модели как aborted.
// Проверяется между llama_decode батчами в bridge_infer / bridge_infer_stream
// / bridge_batched_decode. Модель остаётся загруженной и пригодна для
// повторного использования. Thread-safe (atomic_store).
// Возвращает 0 при успехе, -1 если model == NULL.
int bridge_request_abort(ModelHandle model);

// bridge_request_abort_all — DECISION Q1: no-op в C (см. PLAN.md §8).
// cppworker shutdown итерирует свой Go-side registry и вызывает
// bridge_request_abort на каждую. C-функция оставлена для API completeness.
void bridge_request_abort_all(void);

// bridge_is_aborted — диагностика (для тестов и логов).
// Возвращает true если был подан abort request, false иначе.
bool bridge_is_aborted(ModelHandle model);
```

---

## 3. `c/bridge/bridge.c` — implementation

### 3.1 Init atomic flag после успешной загрузки

**Файл**: `c/bridge/bridge.c:994` (внутри `bridge_load_model_internal`, после `im->ctx_n_ctx = ctx_params.n_ctx;`)

```c
// bridge.c:994 (текущий)
im->ctx_n_ctx = ctx_params.n_ctx;
im->ctx_n_batch = ctx_params.n_batch;
```

**Заменить на**:
```c
im->ctx_n_ctx = ctx_params.n_ctx;
im->ctx_n_batch = ctx_params.n_batch;
// Round 31 #6: init abort flag (atomic).
atomic_init(&im->abort_requested, 0);
```

### 3.2 Reset abort flag в начале `bridge_infer_stream`

**Файл**: `c/bridge/bridge.c:1558-1572` (начало `bridge_infer_stream`)

```c
// bridge.c:1558-1572 (текущий)
int bridge_infer_stream(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params,
    StreamCallback callback,
    void* user_data
) {
    if (model == NULL || prompt == NULL || callback == NULL) {
        set_error("invalid arguments to bridge_infer_stream (model/prompt/callback is NULL)");
        return 1;
    }
    InternalModel *im = (InternalModel *)model;
    // Сбрасываем KV-cache и сэмплер перед новым запросом — иначе
    // после 1-2 инференсов контекст забит и llama_decode падает
    // с "failed to find a memory slot".
    reset_inference_state(im, seq_id);
```

**Заменить на**:
```c
int bridge_infer_stream(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params,
    StreamCallback callback,
    void* user_data
) {
    if (model == NULL || prompt == NULL || callback == NULL) {
        set_error("invalid arguments to bridge_infer_stream (model/prompt/callback is NULL)");
        return 1;
    }
    InternalModel *im = (InternalModel *)model;
    // Round 31 #6: сбросить abort флаг перед новым infer.
    // Модель переиспользуема после abort, флаг = 0 в начале каждого infer.
    atomic_store(&im->abort_requested, 0);
    // Сбрасываем KV-cache и сэмплер перед новым запросом — иначе
    // после 1-2 инференсов контекст забит и llama_decode падает
    // с "failed to find a memory slot".
    reset_inference_state(im, seq_id);
```

### 3.3 Reset abort flag в начале `bridge_infer`

**Файл**: `c/bridge/bridge.c:1308+` (начало `bridge_infer`, сразу после `im` lookup)

**Действие**: добавить ту же строку `atomic_store(&im->abort_requested, 0);` сразу после получения `im` (найти точную строку — `InternalModel *im = (InternalModel *)model;`).

### 3.4 Abort check в prompt phase (bridge_infer_stream)

**Файл**: `c/bridge/bridge.c:~1628-1630` (перед `llama_decode` для prompt batch)

```c
// bridge.c:~1628-1630 (текущий)
        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            set_error("llama_decode failed for prompt batch...");
            llama_batch_free(batch);
            return 1;
        }
```

**Дополнить ПЕРЕД** `if (llama_decode...)`:
```c
        // Round 31 #6: abort check в prompt phase.
        // Закрывает G1 (длинный prompt не прерывается без этого).
        if (atomic_load(&im->abort_requested)) {
            free(tokens);
            set_error("generation aborted by user (prompt phase)");
            llama_batch_free(batch);
            return BRIDGE_ERR_ABORTED;
        }
        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            set_error("llama_decode failed for prompt batch...");
            llama_batch_free(batch);
            return 1;
        }
```

### 3.5 Abort check в gen phase (bridge_infer_stream)

**Файл**: `c/bridge/bridge.c:1707-1709` (перед `llama_decode` для generation step)

**Действие**: то же самое — добавить `if (atomic_load(&im->abort_requested)) { cleanup; return BRIDGE_ERR_ABORTED; }` перед `llama_decode`.

### 3.6 Abort check в `bridge_batched_decode`

**Файл**: `c/bridge/bridge.c:463-465` (перед `llama_decode` в `bridge_batched_decode`)

```c
// bridge.c:463-465 (текущий)
    // Шаг 3: ОДИН llama_decode. Locking — на стороне вызывающего (Go BatchedScheduler
    // держит instance.mu, как Round 8 сделал для bridge_infer_stream).
    int decode_rc = llama_decode(im->context, batch);
```

**Дополнить ПЕРЕД**:
```c
    // Round 31 #6: abort check в batched path. Закрывает G2.
    if (atomic_load(&im->abort_requested)) {
        set_error("batched decode aborted by user");
        llama_batch_free(batch);
        return BRIDGE_ERR_ABORTED;
    }
    // Шаг 3: ОДИН llama_decode. Locking — на стороне вызывающего (Go BatchedScheduler
    // держит instance.mu, как Round 8 сделал для bridge_infer_stream).
    int decode_rc = llama_decode(im->context, batch);
```

### 3.7 Abort check в `bridge_infer` (sync path)

**Файл**: `c/bridge/bridge.c:1371` (prompt decode) и `bridge.c:1434` (gen decode)

**Действие**: добавить аналогичные abort checks в ОБА места (перед каждым `llama_decode` в `bridge_infer`). Синхронный path тоже должен быть прерываемым — иначе `bridge.Infer` блокирует на длинном prompt.

### 3.8 Изменить return code в existing callback cancel

**Файл**: `c/bridge/bridge.c:1699-1702` (existing callback-based cancel)

```c
// bridge.c:1699-1702 (текущий)
        if (callback(token_text, token_len, user_data) == 0) {
            llama_sampler_free(sampler);
            return 0; // клиент остановил стриминг
        }
```

**Заменить**:
```c
        // Round 31 #6: callback cancel → BRIDGE_ERR_ABORTED (а не 0 = success).
        // Закрывает G3: статус код отличает cancelled от natural end.
        if (callback(token_text, token_len, user_data) == 0) {
            llama_sampler_free(sampler);
            return BRIDGE_ERR_ABORTED;  // CHANGED: 0 → BRIDGE_ERR_ABORTED
        }
```

### 3.9 Реализовать новые функции (перед `#endif // GO_BRIDGE_LLAMA_STUB guard`)

**Файл**: `c/bridge/bridge.c:~1990` (перед строкой 1993 `#endif`)

```c
// Добавить перед "#endif // GO_BRIDGE_LLAMA_STUB guard"
// ============================================================
// Round 31 #6 (2026-08-09): Abort API implementation
// ============================================================

int bridge_request_abort(ModelHandle model) {
    if (model == NULL) return -1;
    InternalModel *im = (InternalModel *)model;
    atomic_store(&im->abort_requested, 1);
    return 0;
}

// DECISION Q1: bridge_request_abort_all — no-op. cppworker shutdown
// итерирует свой Backend.models registry и вызывает bridge_request_abort
// на каждую. C-функция оставлена для API completeness.
void bridge_request_abort_all(void) {
    // No-op by design (см. PLAN.md §2.8 DECISION Q1).
    // Если потребуется g_models_list — добавить в bridge.c и итерировать
    // здесь под mutex. На данный момент Go-сторона делает это эффективнее.
}

bool bridge_is_aborted(ModelHandle model) {
    if (model == NULL) return false;
    InternalModel *im = (InternalModel *)model;
    return atomic_load(&im->abort_requested) == 1;
}
```

---

## 4. Build & Verify (C developer)

```bash
cd c/bridge
mkdir -p build && cd build
cmake .. -DCMAKE_C_FLAGS="-fsanitize=address,thread -g -O1"
make -j
nm -D libollamalegion_bridge.so | grep -E "bridge_request_abort|bridge_is_aborted"
# Ожидаемый output:
# T bridge_request_abort
# T bridge_is_aborted
# T bridge_request_abort_all

# Quick smoke test (требует реальной модели):
./tests/test_abort_api
```

---

## 5. Checklist для C developer

- [ ] Section 1: bridge_internal.h — atomic field + include
- [ ] Section 2.1: bridge.h — BRIDGE_ERR_ABORTED macro
- [ ] Section 2.2: bridge.h — function declarations
- [ ] Section 3.1: bridge.c:994 — atomic_init
- [ ] Section 3.2: bridge.c:1558 — reset в начале bridge_infer_stream
- [ ] Section 3.3: bridge.c:1308 — reset в начале bridge_infer
- [ ] Section 3.4: bridge.c:~1628 — abort check в prompt phase (stream)
- [ ] Section 3.5: bridge.c:1707 — abort check в gen phase (stream)
- [ ] Section 3.6: bridge.c:463 — abort check в batched_decode
- [ ] Section 3.7: bridge.c:1371, 1434 — abort check в sync bridge_infer
- [ ] Section 3.8: bridge.c:1699 — return BRIDGE_ERR_ABORTED в callback cancel
- [ ] Section 3.9: bridge.c:~1990 — три новые функции
- [ ] Section 4: build + smoke test
- [ ] TSan gate: запустить ctest с `-fsanitize=thread`, no race
- [ ] ASan gate: запустить ctest с `-fsanitize=address`, no leak

---

## 6. Совместимость с stub build

`#ifndef GO_BRIDGE_LLAMA_STUB guard` (строка 1993) автоматически вырежет весь C код при stub build (build tag `llama_stub`). НЕ нужно добавлять отдельные `#ifdef` для stub — Go-side `bridge_stub.go` уже содержит stub-реализации `RequestAbort/RequestAbortAll/IsAborted` (no-op).

**Verify**:
```bash
go build -tags llama_stub ./...
# Должен собраться без warnings.
```

---

## 7. Rollback plan

Если что-то идёт не так на C-side:
```bash
cd /workspace/ollama-legion
git diff c/bridge/bridge.c c/bridge/bridge.h c/bridge/bridge_internal.h
git checkout c/bridge/bridge.c c/bridge/bridge.h c/bridge/bridge_internal.h
```

Go-side останется работоспособным (compile success, RequestAbort no-op через stub fallback), но `BRIDGE_ERR_ABORTED` не будет возвращаться — cancellation будет только через existing callback mechanism (95% случаев).

---

**Связанные документы**:
- `plans/cppworker-abort-api/PLAN.md` — полный план с обоснованиями
- `plans/cppworker-abort-api/spec.md` — оригинальная спека
- `plans/cppworker-abort-api/task-breakdown.md` — 5-phase breakdown
