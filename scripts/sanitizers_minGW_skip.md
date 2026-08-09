# Sanitizers (ASan / TSan) — DEFERRED

**Status**: Skipped on MinGW/Windows. Defer to Linux/WSL.

## Why skipped

MinGW 13.2.0 не имеет `libasan` / `libtsan`:
```
$ gcc -fsanitize=address -o test.exe test.c
cannot find -lasan: No such file or directory
```

Sanitizers требуют Clang или GCC на Linux/macOS. На Windows нужен либо:
- WSL2 (Windows Subsystem for Linux) с нативным GCC/Clang
- clang-cl (LLVM для Windows) — частичная поддержка ASan

## Рекомендация для будущего C developer

```bash
# На WSL2 / Linux:
cd c/bridge/tests
cmake -S ../.. -B build \
  -DCMAKE_C_COMPILER=clang \
  -DCOMMON_LIB=$(realpath ../../llama.cpp/build/common/libllama-common.a)
cmake --build build --target test_abort_api

# Запуск с ASan:
ASAN_OPTIONS=detect_leaks=1:halt_on_error=0 \
  ./build/test_abort_api

# Запуск с TSan:
TSAN_OPTIONS=halt_on_error=0:second_deadlock_stack=1 \
  ./build/test_abort_api
```

## Что проверять с sanitizers

| Sanitizer | Цель | Что ищем |
|-----------|------|----------|
| **ASan** (Address Sanitizer) | Memory safety | Out-of-bounds, use-after-free, memory leaks, stack-buffer-overflow |
| **TSan** (Thread Sanitizer) | Data races | Гонки на `atomic_int abort_requested` между `bridge_request_abort` (Go cgo thread) и `bridge_infer_stream` (C thread) |
| **MSan** (Memory Sanitizer) | Uninitialized memory | Чтение `im->abort_requested` до инициализации (после `atomic_init`) |
| **UBSan** (Undefined Behavior) | Undefined behavior | Signed overflow, null deref, alignment violations |

## T6 из PLAN.md — специфический test

```c
// T6: 100 goroutines set flag concurrently на 1 model (TSan test)
void test_T6_concurrent_flag_writes(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);

    #define NUM_THREADS 100
    pthread_t threads[NUM_THREADS];
    for (int i = 0; i < NUM_THREADS; i++) {
        pthread_create(&threads[i], NULL, [](void* arg) {
            InternalModel* im = (InternalModel*)arg;
            // Mix of reads and writes
            for (int j = 0; j < 1000; j++) {
                if (j % 2 == 0) {
                    bridge_request_abort(im);
                } else {
                    bridge_is_aborted(im);
                }
            }
            return NULL;
        }, &im);
    }
    for (int i = 0; i < NUM_THREADS; i++) {
        pthread_join(threads[i], NULL);
    }
    // TSan: should report no data races
    // ASan: should report no memory errors
}
```

Запуск с TSan: должен пройти без warnings о data races на `abort_requested`.

## Заключение

Sanitizers пропущены в этой итерации (MinGW limitation).
Рекомендуется запускать перед каждым release на WSL2 / Linux CI.
См. `plans/cppworker-abort-api/PLAN.md` для деталей.
