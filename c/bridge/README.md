# c/bridge — C-bridge layer для Ollama Legion

C-обёртка над `llama.cpp` для cppworker. Прямой CGo интерфейс, не HTTP.

## Назначение

cppworker (`cmd/cppworker`) использует `cgo` для вызова C-функций bridge
вместо `os/exec` на llama-cli. Это даёт:

- **Прямое управление** над `llama_context` и `llama_model` (atomic flags,
  KV-cache manipulation, custom batched decode).
- **Кооперативный cancel** через `bridge_request_abort` (Round 31 #6) —
  client disconnect → atomic flag → `BRIDGE_ERR_ABORTED` в течение <100ms
  (single batch time).
- **Структурированные ошибки** через `BridgeErrorInfo` (code + context)
  вместо парсинга stderr.

## Структура

```
c/bridge/
├── bridge.c              # Реализация (1500+ строк)
├── bridge.h              # Public API (Go + C)
├── bridge_internal.h     # InternalModel struct, IM() macro
├── bridge.go             # Go-биндинги для cgo
├── bridge_stub.go        # Stub для unit-тестов (build tag: llama_stub)
├── CMakeLists.txt        # Build libollamalegion_bridge.a
├── ABORT_API_C_PATCH.md  # Round 31 #6: reference для abort API
└── tests/
    ├── abort_syntax_test.c    # 7 syntax-check тестов (gcc -Wall -Wextra)
    ├── test_abort_api.c       # 13 unit-тестов (standalone, PLAN.md §5.1)
    ├── test_batched_batch.c
    ├── test_batch_n_tokens.c
    └── CMakeLists.txt
```

## Build

### Standalone (для тестов abort API)

```bash
cd c/bridge/tests
gcc -std=c99 -DTEST_ABORT_STANDALONE -o test_abort_api test_abort_api.c -lpthread
./test_abort_api
# Expected: "*** ALL TESTS PASSED ***"
```

### Full build (с libllama)

```bash
# Stage 1: build llama.cpp
cd c/llama.cpp && mkdir -p build && cd build
cmake .. -DGGML_CUDA=ON -DLLAMA_BUILD_TESTS=OFF -DLLAMA_BUILD_EXAMPLES=OFF
cmake --build . --config Release -j$(nproc)

# Stage 2: build bridge
cd ../../bridge
mkdir -p build && cd build
cmake .. -DCMAKE_BUILD_TYPE=Release
cmake --build . --config Release
# → build/libollamalegion_bridge.a

# Stage 3: build Go binary (cgo)
cd ../../../cmd/cppworker
CGO_ENABLED=1 CGO_CFLAGS="..." CGO_LDFLAGS="..." go build -o cppworker .
```

## Quick example (abort API)

```c
#include "bridge.h"

ModelHandle model = bridge_load_model(&config, &err);
if (!model) { /* handle error */ }

// In another thread (cancel handler):
bridge_request_abort(model);  // atomic_store(&im->abort_requested, 1)

// In inference thread:
int rc = bridge_infer_stream(model, prompt, &params, callback, userdata);
if (rc == BRIDGE_ERR_ABORTED) {
    // Cleanup, but model is still valid for next call
}

// Verify flag (debug / tests):
if (bridge_is_aborted(model)) { /* ... */ }

// After abort: model stays loaded, can be reused
rc = bridge_infer_stream(model, next_prompt, &params, callback, userdata);
```

## Go-side

```go
import "ollama-loadbalancer/c/bridge"

// Legacy: handle доступен только после возврата LoadModel.
// Подходит для кода без cancellable load (rpcworker, startup preload).
handle, err := bridge.LoadModel(config)
// ...
err = bridge.RequestAbort(handle)  // → ErrAborted
if errors.Is(err, bridge.ErrAborted) { /* cancelled */ }
```

### R60.57 follow-up: cancellable load (early handle exposure)

```go
import "ollama-loadbalancer/c/bridge"

// New API для cancellable load (cppworker's LoadModelWithOpts + ctx.Done watcher).
// C-bridge экспонирует early-allocated handle в earlyHandle.ptr СРАЗУ после
// malloc + atomic_init, ДО blocking llama_model_load_from_file.
// Это позволяет watcher goroutine вызвать bridge.RequestLoadAbort(handle)
// во время load — handle уже валиден.
earlyHandle := &bridge.ModelHandle{path: cfg.ModelPath}
handle, err := bridge.LoadModelWithEarlyHandle(cfg, earlyHandle)
// В другой горутине (например, ctx.Done watcher):
if earlyHandle.ptr != nil {
    bridge.RequestLoadAbort(earlyHandle)  // atomic flag → cancel at next checkpoint
}
```

**Семантика:**

- `LoadModelWithEarlyHandle(cfg, earlyHandle)` — caller pre-allocates `earlyHandle`,
  C writes `(ModelHandle)im` to `earlyHandle.ptr` BEFORE blocking load.
- В error paths C сбрасывает `*out_handle = NULL` ПЕРЕД free() — watcher не
  держит dangling pointer.
- `earlyHandle` обязателен (non-nil). Используется runtime.Pinner для
  защиты от GC relocation во время C-вызова.
- Thread-safety: caller синхронизирует доступ к `earlyHandle.ptr`
  (atomic.Pointer или mutex). C writes — pointer-sized, naturally atomic.

**См. также:** `internal/cppbackend/backend.go:LoadModelWithOpts` — reference
implementation watcher pattern (Phase 3-5, commit 4682f2f).

## Quick example (load-cancel API)

```c
#include "bridge.h"

// Caller-provided slot для early handle exposure:
ModelHandle *early_slot = ...;  // pointer на ModelHandle variable

struct llama_model_params mp = llama_model_default_params();
ModelConfig cfg = { .model_path = "model.gguf", .n_ctx = 4096, ... };

// C пишет (ModelHandle)im в early_slot СРАЗУ после malloc+init,
// ДО llama_model_load_from_file. Watcher goroutine может abort'нуть load.
ModelHandle model = bridge_load_model(&cfg, NULL, early_slot);

// В watcher goroutine (например, после ctx.Done):
if (*early_slot != NULL) {
    bridge_request_load_abort(*early_slot);  // atomic flag → cancel at checkpoint
}

// Free когда load завершён (success или abort):
bridge_free_model(model);
```

## Tests

```bash
# C standalone (без libllama)
cd c/bridge/tests && gcc -DTEST_ABORT_STANDALONE test_abort_api.c -o test && ./test

# Go abort tests (с stub llama)
go test -tags llama_stub ./c/bridge/ ./cmd/cppworker/ -run "Abort|IsAborted|AbortWatcher"
```

## Связанная документация

- **[docs/BRIDGE_API.md](../docs/BRIDGE_API.md)** — полный reference C API
  (все функции, параметры, коды ошибок, lifecycle).
- **[plans/cppworker-abort-api/PLAN.md](../plans/cppworker-abort-api/PLAN.md)** —
  Round 31 #6 план: gap analysis, design decisions, AC, risks.
- **[c/bridge/ABORT_API_C_PATCH.md](ABORT_API_C_PATCH.md)** — reference
  для abort-патча (atomic_init/store/load patterns, 6 check points).

## Changelog

- **R60.57 follow-up (2026-09-13)** — `bridge_load_model` принимает
  `ModelHandle* out_handle` для early handle exposure. C-bridge пишет
  `(ModelHandle)im` в `*out_handle` СРАЗУ после malloc+init, ДО blocking
  load. Это позволяет Go-side watcher goroutine вызвать
  `bridge_request_load_abort(handle)` во время load. В error paths
  `*out_handle` сбрасывается в NULL ПЕРЕД free(). Go-биндинг: новая
  функция `LoadModelWithEarlyHandle(cfg, earlyHandle)` (legacy
  `LoadModel` остаётся backward compatible).
- **R60.57 (2026-09-13)** — Load-cancel API (mirror Round 31 #6 для фазы
  model load): `bridge_request_load_abort`, `bridge_request_load_abort_all`,
  `bridge_is_load_aborted`, 3 checkpoints в `bridge_load_model`.
- **Round 31 #6 (2026-08-09)** — Abort API (3 функции, BRIDGE_ERR_ABORTED=-100,
  atomic flag per InternalModel).
- **Round 25-31** — tokenize/token-to-piece, batched_decode, chat template,
  kv_cache_type, override-tensors.
- **Round 13** — multi-slot batched inference (seq_id).

См. [CHANGELOG.md](../CHANGELOG.md) для полной истории.
