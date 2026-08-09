// test_abort_api.c — Round 31 #6: standalone unit tests для abort API.
//
// Покрывает сценарии T1, T2, T7, T8, T9 из PLAN.md §5.1 (не требует libllama).
// T3-T6 (abort during long gen, abort+retry, 10 concurrent, 100 goroutines race)
// требуют реальной модели и libllama — помечены как #if 0 и документированы.
//
// Build (standalone, MinGW / Linux / macOS):
//   gcc -std=c99 -DTEST_ABORT_STANDALONE -o test_abort_api test_abort_api.c -lpthread
//   ./test_abort_api
//
// Build (с libllama, для T3-T6):
//   cmake ../.. -DCOMMON_LIB=$(realpath ../../llama.cpp/build/common/libllama-common.a)
//   make test_abort_api
//   ./test_abort_api
//
// Exit code: 0 = PASS, non-zero = FAIL.

#ifndef TEST_ABORT_STANDALONE
// Если не standalone, подключаем реальные headers из bridge
#include "bridge.h"
#include "bridge_internal.h"
#include <stdatomic.h>
#include <string.h>
#else
// Standalone mode — минимальный mock для compile без libllama
#include <stdatomic.h>
#include <stdint.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <pthread.h>

// Mock BRIDGE_ERR_ABORTED
#define BRIDGE_ERR_ABORTED (-100)

// Mock ModelHandle (как в bridge.h)
typedef void* ModelHandle;

// Mock InternalModel (как в bridge_internal.h)
typedef struct {
    int placeholder;
    atomic_int abort_requested;
} InternalModel;

// === Минимальная копия abort API из bridge.c (для тестирования) ===
// В production используется реальная реализация из bridge.c.
// Здесь — копия с теми же сигнатурами для standalone тестов.

int bridge_request_abort(ModelHandle model) {
    if (model == NULL) return -1;
    InternalModel* im = (InternalModel*)model;
    atomic_store(&im->abort_requested, 1);
    return 0;
}

void bridge_request_abort_all(void) {
    // В production проходит по g_models_list.
    // Для standalone теста — no-op (тесты T5 с concurrent работают иначе).
}

bool bridge_is_aborted(ModelHandle model) {
    if (model == NULL) return false;
    InternalModel* im = (InternalModel*)model;
    return atomic_load(&im->abort_requested) == 1;
}

#endif // TEST_ABORT_STANDALONE

// ============================================================
// Test framework (минимальный)
// ============================================================

static int tests_run = 0;
static int tests_failed = 0;

#define ASSERT_TRUE(cond, msg) do { \
    tests_run++; \
    if (!(cond)) { \
        fprintf(stderr, "FAIL: %s (line %d): %s\n", __func__, __LINE__, msg); \
        tests_failed++; \
    } else { \
        printf("PASS: %s (line %d): %s\n", __func__, __LINE__, msg); \
    } \
} while (0)

#define ASSERT_EQ(actual, expected, msg) do { \
    tests_run++; \
    long _a = (long)(actual); \
    long _e = (long)(expected); \
    if (_a != _e) { \
        fprintf(stderr, "FAIL: %s (line %d): %s — got %ld, want %ld\n", \
                __func__, __LINE__, msg, _a, _e); \
        tests_failed++; \
    } else { \
        printf("PASS: %s (line %d): %s == %ld\n", __func__, __LINE__, msg, _a); \
    } \
} while (0)

// ============================================================
// T1: bridge_request_abort(NULL) → return -1, no crash
// ============================================================
static void test_T1_abort_null_handle(void) {
    int rc = bridge_request_abort(NULL);
    ASSERT_EQ(rc, -1, "bridge_request_abort(NULL) should return -1");
}

// ============================================================
// T2: bridge_request_abort(valid_model) → flag = 1
// ============================================================
static void test_T2_abort_valid_model(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);
    int rc = bridge_request_abort(&im);
    ASSERT_EQ(rc, 0, "bridge_request_abort(valid) should return 0");
    int flag = atomic_load(&im.abort_requested);
    ASSERT_EQ(flag, 1, "abort_requested flag should be 1 after abort");
}

// ============================================================
// T7: bridge_is_aborted() после abort → return true
// ============================================================
static void test_T7_is_aborted_after_abort(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);
    atomic_store(&im.abort_requested, 0);  // baseline
    bool aborted_before = bridge_is_aborted(&im);
    ASSERT_EQ(aborted_before, false, "IsAborted should be false before abort");

    bridge_request_abort(&im);

    bool aborted_after = bridge_is_aborted(&im);
    ASSERT_EQ(aborted_after, true, "IsAborted should be true after abort");
}

// ============================================================
// T8: bridge_is_aborted() после успешного infer (abort НЕ вызван) → return false
// ============================================================
static void test_T8_is_aborted_after_success(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);

    // Симулируем successful infer: abort НЕ вызывается, flag остаётся 0
    bool aborted = bridge_is_aborted(&im);
    ASSERT_EQ(aborted, false, "IsAborted should be false after success (no abort called)");

    // Проверим что flag остался 0 (no side effects)
    int flag = atomic_load(&im.abort_requested);
    ASSERT_EQ(flag, 0, "abort_requested should still be 0 after IsAborted call");
}

// ============================================================
// T9: bridge_is_aborted(NULL) → return false, no crash
// ============================================================
static void test_T9_is_aborted_null_handle(void) {
    bool aborted = bridge_is_aborted(NULL);
    ASSERT_EQ(aborted, false, "IsAborted(NULL) should return false");
}

// ============================================================
// T10 (extra): atomic flag set/reset между abort calls
// ============================================================
static void test_T10_flag_reset_between_infers(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);

    // First infer: abort
    bridge_request_abort(&im);
    ASSERT_EQ(atomic_load(&im.abort_requested), 1, "flag should be 1 after abort");

    // Simulate next infer: production code resets flag at start of bridge_infer_stream.
    // Здесь проверяем что reset (atomic_store) работает.
    atomic_store(&im.abort_requested, 0);
    ASSERT_EQ(atomic_load(&im.abort_requested), 0, "flag should be 0 after reset");

    // Second infer: no abort
    bool aborted = bridge_is_aborted(&im);
    ASSERT_EQ(aborted, false, "IsAborted should be false after reset");
}

// ============================================================
// T11 (extra): abort + IsAborted idempotency
// ============================================================
static void test_T11_abort_idempotent(void) {
    InternalModel im;
    atomic_init(&im.abort_requested, 0);

    // Множественные abort calls — flag должен оставаться 1
    bridge_request_abort(&im);
    bridge_request_abort(&im);
    bridge_request_abort(&im);
    ASSERT_EQ(atomic_load(&im.abort_requested), 1, "flag should stay 1 after multiple aborts");

    bool aborted = bridge_is_aborted(&im);
    ASSERT_EQ(aborted, true, "IsAborted should return true after multiple aborts");
}

#if 0
// ============================================================
// Сценарии T3-T6 требуют реальной модели и libllama.
// Помечены как #if 0 для standalone сборки. Для их запуска нужно
// линковать test_abort_api.c с bridge.c + libllama-common.a через CMakeLists.
// Реализация в PLAN.md §5.1 — скопируйте туда при интеграции с libllama.
// ============================================================

// T3: Запустить long infer в thread, через 100ms вызвать abort.
// Ожидание: infer возвращает BRIDGE_ERR_ABORTED в течение 200ms.
// (Requires: model loaded, libllama linked)

// T4: После abort — повторный bridge_infer_stream успешно выполняется.
// (Requires: model loaded)

// T5: 10 concurrent infer + 1 bridge_request_abort_all → все abort в течение 1.5s.
// (Requires: 10 models или batched_decode support)

// T6: 100 goroutines set flag concurrently на 1 model. TSan должен быть clean.
// (Requires: TSan + threading)
#endif

// ============================================================
// Main
// ============================================================
int main(void) {
    printf("=== Round 31 #6: Abort API Unit Tests (standalone) ===\n\n");

    test_T1_abort_null_handle();
    test_T2_abort_valid_model();
    test_T7_is_aborted_after_abort();
    test_T8_is_aborted_after_success();
    test_T9_is_aborted_null_handle();
    test_T10_flag_reset_between_infers();
    test_T11_abort_idempotent();

    printf("\n=== Summary ===\n");
    printf("Tests run: %d\n", tests_run);
    printf("Tests failed: %d\n", tests_failed);
    printf("Tests passed: %d\n", tests_run - tests_failed);

    if (tests_failed == 0) {
        printf("\n*** ALL TESTS PASSED ***\n");
        return 0;
    } else {
        printf("\n*** %d TEST(S) FAILED ***\n", tests_failed);
        return 1;
    }
}
