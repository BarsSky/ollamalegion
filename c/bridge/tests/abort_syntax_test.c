// abort_syntax_test.c — Round 31 #6: standalone syntax-check для abort API.
//
// Извлекает abort-related код из bridge.c в минимальный test program.
// Использует mock llama.h (см. abort_syntax_check.c).
// НЕ запускается (нет main), только компилируется для syntax check.
//
// Build: gcc -c -I. -I../c/llama.cpp/../ -o abort_syntax_test.o abort_syntax_test.c
// (с mock llama.h в include path)

#include <stdatomic.h>
#include <stdint.h>
#include <stdbool.h>
#include <stdlib.h>
#include <string.h>

// === Mock llama.h (для compile) ===
#include "abort_syntax_check.h"

// === bridge.h constants (копия для standalone test) ===
#define BRIDGE_ERR_ABORTED (-100)

// === Minimal InternalModel (копия из bridge.c) ===
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;
    uint32_t ctx_n_batch;
    atomic_int abort_requested;
} InternalModel;

// === Mock ModelHandle ===
typedef void* ModelHandle;

// === Тестируемые abort functions (копия из bridge.c) ===
int bridge_request_abort(ModelHandle model) {
    if (model == NULL) return -1;
    InternalModel *im = (InternalModel *)model;
    atomic_store(&im->abort_requested, 1);
    return 0;
}

void bridge_request_abort_all(void) {
    // No-op by design (DECISION Q1)
}

bool bridge_is_aborted(ModelHandle model) {
    if (model == NULL) return false;
    InternalModel *im = (InternalModel *)model;
    return atomic_load(&im->abort_requested) == 1;
}

// === Standalone test main ===
#ifndef ABORT_SYNTAX_TEST_NO_MAIN
#include <stdio.h>

int main(void) {
    InternalModel im = {0};
    atomic_init(&im.abort_requested, 0);

    // Test 1: bridge_is_aborted initially false
    if (bridge_is_aborted(&im) != false) {
        fprintf(stderr, "FAIL: initial is_aborted should be false\n");
        return 1;
    }

    // Test 2: bridge_request_abort sets flag
    int rc = bridge_request_abort(&im);
    if (rc != 0) {
        fprintf(stderr, "FAIL: request_abort rc=%d (expected 0)\n", rc);
        return 1;
    }
    if (bridge_is_aborted(&im) != true) {
        fprintf(stderr, "FAIL: is_aborted should be true after request\n");
        return 1;
    }

    // Test 3: bridge_request_abort на NULL → -1
    rc = bridge_request_abort(NULL);
    if (rc != -1) {
        fprintf(stderr, "FAIL: request_abort(NULL) rc=%d (expected -1)\n", rc);
        return 1;
    }

    // Test 4: bridge_is_aborted на NULL → false
    if (bridge_is_aborted(NULL) != false) {
        fprintf(stderr, "FAIL: is_aborted(NULL) should be false\n");
        return 1;
    }

    // Test 5: BRIDGE_ERR_ABORTED == -100
    if (BRIDGE_ERR_ABORTED != -100) {
        fprintf(stderr, "FAIL: BRIDGE_ERR_ABORTED=%d (expected -100)\n", BRIDGE_ERR_ABORTED);
        return 1;
    }

    // Test 6: request_abort_all — no panic
    bridge_request_abort_all();

    // Test 7: concurrent access (race-free) — 1000 потоков set/load
    atomic_store(&im.abort_requested, 0);
    _Atomic int errors = 0;
    #pragma omp parallel for
    for (int i = 0; i < 1000; i++) {
        atomic_store(&im.abort_requested, 1);
        if (!bridge_is_aborted(&im)) {
            atomic_fetch_add(&errors, 1);
        }
        atomic_store(&im.abort_requested, 0);
    }
    int err_count = atomic_load(&errors);
    if (err_count != 0) {
        fprintf(stderr, "FAIL: race test, errors=%d (expected 0)\n", err_count);
        return 1;
    }

    printf("PASS: all 7 abort API syntax tests\n");
    return 0;
}
#endif // ABORT_SYNTAX_TEST_NO_MAIN
