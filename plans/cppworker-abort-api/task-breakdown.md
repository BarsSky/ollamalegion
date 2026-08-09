# Task Breakdown — cppworker Abort API

**Created**: 2026-08-09
**Status**: Ready for C developer
**Estimated effort**: 8-13 hours (C) + 1-2 hours (Go integration)

## Prerequisites

- Linux/macOS development environment (или WSL2)
- CMake 3.20+
- GCC 11+ (для stdatomic.h) или MinGW (Windows)
- Familiarity with C11 atomics
- llama.cpp source tree (для проверки что abort не ломает existing tests)

## Phase 1: C API (4-6 hours)

### Task 1.1: Add abort flag to InternalModel struct
- **File**: `c/bridge/bridge_internal.h`
- **Action**: Добавить `atomic_int abort_requested;` в `struct InternalModel`
- **Note**: atomic_int требует `<stdatomic.h>` — добавить include
- **Verify**: `grep "atomic_int" c/bridge/bridge_internal.h`

### Task 1.2: Initialize abort flag in LoadModel
- **File**: `c/bridge/bridge.c`
- **Action**: В функции `bridge_load_model_internal` после `im = calloc(...)`:
  ```c
  atomic_init(&im->abort_requested, 0);
  ```
- **Verify**: Загрузка модели через cppworker должна работать как раньше

### Task 1.3: Reset abort flag at start of bridge_infer_stream
- **File**: `c/bridge/bridge.c`, function `bridge_infer_stream`
- **Action**: В самом начале (после `im` lookup):
  ```c
  atomic_store(&im->abort_requested, 0);
  ```
- **Why**: После успешного infer флаг должен быть сброшен для следующего вызова

### Task 1.4: Add abort check in generation loop
- **File**: `c/bridge/bridge.c`, function `bridge_infer_stream`
- **Action**: В цикле generation (между llama_decode батчами):
  ```c
  // В цикле:
  if (atomic_load(&im->abort_requested)) {
      // Cleanup partial state
      if (out_buf) free(out_buf);
      if (result.error_msg) bridge_free_string(result.error_msg);
      result.output = NULL;
      result.output_len = 0;
      result.status = BRIDGE_ERR_ABORTED;
      result.error_msg = bridge_strdup("generation aborted by user");
      return result;
  }
  ```
- **Critical**: Проверять ПОСЛЕ каждого llama_decode, ДО sampling/sampling-related work

### Task 1.5: Implement bridge_request_abort
- **File**: `c/bridge/bridge.c`
- **Action**: Добавить новую функцию:
  ```c
  int bridge_request_abort(ModelHandle model) {
      if (model == NULL) return -1;
      InternalModel* im = (InternalModel*)model;
      atomic_store(&im->abort_requested, 1);
      return 0;
  }
  ```
- **Verify**: `nm -D libollamalegion_bridge.so | grep bridge_request_abort`

### Task 1.6: Implement bridge_request_abort_all
- **File**: `c/bridge/bridge.c`
- **Action**: Пройти по глобальному списку моделей и abort all. Требует
  thread-safe итерации по списку (использовать существующий mutex).
  ```c
  void bridge_request_abort_all(void) {
      pthread_mutex_lock(&g_models_mutex);
      for (each model in g_models_list) {
          atomic_store(&model->abort_requested, 1);
      }
      pthread_mutex_unlock(&g_models_mutex);
  }
  ```
- **Note**: Зависит от существующей структуры — может потребоваться refactor

### Task 1.7: Implement bridge_is_aborted (for diagnostics)
- **File**: `c/bridge/bridge.c`
- **Action**: Простая проверка atomic flag
  ```c
  bool bridge_is_aborted(ModelHandle model) {
      if (model == NULL) return false;
      InternalModel* im = (InternalModel*)model;
      return atomic_load(&im->abort_requested) == 1;
  }
  ```

### Task 1.8: Add stub versions for `llama_stub` build tag
- **File**: `c/bridge/bridge_stub.go`
- **Action**: Go-side stub для cgo build tag. C-side stub в bridge.c под
  `#ifdef GO_BRIDGE_LLAMA_STUB` уже есть паттерн. Добавить:
  ```c
  #ifdef GO_BRIDGE_LLAMA_STUB
  int bridge_request_abort(ModelHandle model) { return 0; }
  void bridge_request_abort_all(void) {}
  bool bridge_is_aborted(ModelHandle model) { return false; }
  #endif
  ```

## Phase 2: Header updates (30 min)

### Task 2.1: Update bridge.h
- **File**: `c/bridge/bridge.h`
- **Action**: Добавить declarations (см. [spec.md](./spec.md#21-header-changes-cbridgebridgeh))

### Task 2.2: Update CHANGELOG
- **File**: `CHANGELOG.md` или `docs/CHANGELOG_C_BRIDGE.md`
- **Action**: Добавить запись про Round 31 #6

## Phase 3: C unit tests (2-3 hours)

### Task 3.1: Create test_abort_api.c
- **File**: `c/bridge/tests/test_abort_api.c`
- **Action**: Создать файл с тестами (см. [spec.md](./spec.md#41-c-unit-tests))

### Task 3.2: Update CMakeLists.txt
- **File**: `c/bridge/CMakeLists.txt`
- **Action**: Добавить `test_abort_api` в test executables

### Task 3.3: Test scenarios
- [ ] abort before infer starts (infer should return immediately with BRIDGE_ERR_ABORTED)
- [ ] abort during long generation (> 100 tokens), verify returns within 1 second
- [ ] abort + retry — model should work for next infer
- [ ] RequestAbortAll with 5 concurrent inferences — all return BRIDGE_ERR_ABORTED
- [ ] Abort non-existent model handle — returns -1, no crash
- [ ] Thread safety: 100 goroutines setting flag concurrently — no crash

## Phase 4: Go integration (1-2 hours)

### Task 4.1: Add RequestAbort to bridge.go
- **File**: `c/bridge/bridge.go`
- **Action**: Добавить Go bindings (см. [spec.md](./spec.md#24-go-side-integration-cbridgebridgego))

### Task 4.2: Add Go tests
- **File**: `c/bridge/bridge_abort_test.go`
- **Action**: Создать тесты (см. [spec.md](./spec.md#42-go-integration-tests))

### Task 4.3: Integrate into cppworker handlers
- **File**: `cmd/cppworker/handlers_inference.go`
- **Action**: При `r.Context().Done()` вызвать `bridge.RequestAbort(model)`
  (см. [spec.md](./spec.md#25-cppworker-integration))

## Phase 5: Live verification (1-2 hours)

### Task 5.1: Build cppworker with abort API
```bash
cd /workspace/ollama-legion/c/bridge
mkdir -p build && cd build
cmake ..
make -j
```

### Task 5.2: Restart cppworker in bundled-full
```bash
docker compose -p ol-bundled-full -f deployments/docker-compose.bundled-full.yml up -d --build cppworker-gpu
```

### Task 5.3: Live test — streaming cancel
```python
import requests
# Start long generation
r = requests.post(
    "http://localhost:18092/v1/chat/completions",
    json={"model": "gemma-4", "messages": [{"role": "user", "content": "Write a 1000 word essay"}], "stream": True, "max_tokens": 2000},
    stream=True, timeout=180,
)
# Cancel after 1 second
import time
start = time.time()
tokens = 0
for chunk in r.iter_lines():
    if time.time() - start > 1.0:
        r.close()  # ← This should trigger abort
        break
    if chunk:
        tokens += 1
print(f"received {tokens} tokens before cancel")
# Check cppworker log for "generation aborted by user"
```

### Task 5.4: Verify VRAM/slot freed
```bash
# Before cancel:
nvidia-smi | grep "MiB"  # ~5000 MiB used

# After cancel + 1 second:
nvidia-smi | grep "MiB"  # Should return to baseline
```

### Task 5.5: Verify with multiple concurrent aborts
- Запустить 5 parallel generations
- Cancel все через 1s
- Проверить что все вернули BRIDGE_ERR_ABORTED в течение 1.5s

## Success Criteria

- [ ] Все C unit tests PASS
- [ ] Все Go integration tests PASS
- [ ] Round 31 #1 + W2 (abort API) полностью покрывают cancel use cases
- [ ] Cancel latency < 100ms (single batch time)
- [ ] VRAM/slot освобождаются в течение 1s после cancel
- [ ] Нет regressions в Round 25-31 фиксах (reasoning translation, profile sync, etc.)
- [ ] Sanitizers (ASan, TSan) clean

## Risks

| Risk | Mitigation |
|------|------------|
| Race condition в abort check | Использовать C11 stdatomic (memory_order_relaxed) + thorough testing |
| Memory leak при abort mid-decode | Explicit cleanup в branch (free partial buffers) |
| Abort игнорируется если llama_decode зависает | Round 31 #1 fallback — terminate stream via TCP close |
| Re-entrancy (abort внутри abort) | Atomic flag, no callbacks |
| Windows MinGW compatibility | pthread.h НЕ используется в abort check (только stdatomic.h) |

## Estimated Total Effort

| Phase | Effort | Owner |
|-------|--------|-------|
| Phase 1 (C API) | 4-6h | C developer |
| Phase 2 (Header) | 0.5h | C developer |
| Phase 3 (C tests) | 2-3h | C developer |
| Phase 4 (Go) | 1-2h | Go developer |
| Phase 5 (Live verify) | 1-2h | Both |
| **Total** | **8-13h** | **C developer (primary)** |

## Open Questions

1. **Должна ли abort_request_abort работать для unloaded model?**
   - Сейчас: `model == NULL → return -1`
   - Альтернатива: `return 0` (no-op)
   - Decision: TBD (зависит от UX в cppworker)

2. **Что делать с mid-decode abort (если хотим hard cancel через pthread_kill)?**
   - Сейчас: НЕ делаем (out of scope)
   - Спецификация: см. [spec.md Section 3](./spec.md#3-optional-hard-cancel-via-pthreadkill)
   - Decision: НЕ рекомендуется для первой итерации

3. **Должна ли abort отменять ТОЛЬКО текущий stream, или весь model state?**
   - Сейчас: только текущий infer (model остается loaded)
   - Альтернатива: `bridge_unload` после abort
   - Decision: текущий infer only (model preserved)
