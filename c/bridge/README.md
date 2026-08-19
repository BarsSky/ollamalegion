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

handle, err := bridge.LoadModel(config)
// ...
err = bridge.RequestAbort(handle)  // → ErrAborted
if errors.Is(err, bridge.ErrAborted) { /* cancelled */ }
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

- **Round 31 #6 (2026-08-09)** — Abort API (3 функции, BRIDGE_ERR_ABORTED=-100,
  atomic flag per InternalModel).
- **Round 25-31** — tokenize/token-to-piece, batched_decode, chat template,
  kv_cache_type, override-tensors.
- **Round 13** — multi-slot batched inference (seq_id).

См. [CHANGELOG.md](../CHANGELOG.md) для полной истории.
