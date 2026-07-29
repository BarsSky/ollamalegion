// test_batch_n_tokens.c — regression test для build_batch_with_seq (Round 13).
//
// ЗАЧЕМ: Round 13 (commit 80dfce9, v0.4.11) заменил llama_batch_get_one на
// llama_batch_init + build_batch_with_seq для multi-slot KV-cache support.
// llama_batch_init инициализирует batch.n_tokens = 0 (в отличие от старой
// llama_batch_get_one которая сама ставила n_tokens = n_tokens).
//
// В Round 13 build_batch_with_seq забыл выставить batch.n_tokens после populate
// loop → llama_decode видел пустой batch → возвращал -1 с
// "decode: n_tokens == 0". БАГ молча сломал ВСЮ inference в v0.4.11-12.
//
// FIX: aba9a90 (2026-07-29) добавил `batch.n_tokens = n_tokens;` после for loop.
//
// Этот тест проверяет контракт build_batch_with_seq:
//   1. n_tokens == N (явно выставлен после populate) — РЕГРЕССИЯ Round 13
//   2. tokens, seq_id, pos, logits заполнены правильно
//   3. n_tokens == 0 → early return path (n_tokens остается 0)
//
// Если кто-то в будущем уберёт `batch.n_tokens = n_tokens`, этот тест
// поймает регрессию сразу, без необходимости полного Docker build + deploy.

#include "bridge_internal.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>

// Helper: проверка assertion с message
#define CHECK(cond, msg) do { \
    if (!(cond)) { \
        fprintf(stderr, "FAIL [%s:%d]: %s\n", __FILE__, __LINE__, msg); \
        return 1; \
    } \
} while (0)

static int test_n_tokens_set_for_nonzero(void) {
    printf("test_n_tokens_set_for_nonzero: 5 tokens, seq_id=42, start_pos=100\n");
    llama_token tokens[5] = {10, 20, 30, 40, 50};
    struct llama_batch batch = build_batch_with_seq(tokens, 5, 42, 100);

    // CRITICAL: batch.n_tokens must equal n_tokens.
    // Round 13 regression: без явного n_tokens=... в build_batch_with_seq
    // здесь было бы 0 → llama_decode fails.
    CHECK(batch.n_tokens == 5, "batch.n_tokens != 5 (Round 13 regression?)");

    CHECK(batch.token[0] == 10, "token[0] != 10");
    CHECK(batch.token[1] == 20, "token[1] != 20");
    CHECK(batch.token[2] == 30, "token[2] != 30");
    CHECK(batch.token[3] == 40, "token[3] != 40");
    CHECK(batch.token[4] == 50, "token[4] != 50");

    CHECK(batch.n_seq_id[0] == 1, "n_seq_id[0] != 1");
    CHECK(batch.n_seq_id[4] == 1, "n_seq_id[4] != 1");
    CHECK(batch.seq_id[0][0] == 42, "seq_id[0][0] != 42");
    CHECK(batch.seq_id[4][0] == 42, "seq_id[4][0] != 42");

    CHECK(batch.pos[0] == 100, "pos[0] != 100 (start_pos)");
    CHECK(batch.pos[1] == 101, "pos[1] != 101");
    CHECK(batch.pos[4] == 104, "pos[4] != 104");

    // Last token has logits=1 (для sampling), остальные logits=0
    CHECK(batch.logits[0] == 0, "logits[0] != 0 (non-last)");
    CHECK(batch.logits[1] == 0, "logits[1] != 0 (non-last)");
    CHECK(batch.logits[2] == 0, "logits[2] != 0 (non-last)");
    CHECK(batch.logits[3] == 0, "logits[3] != 0 (non-last)");
    CHECK(batch.logits[4] == 1, "logits[4] != 1 (last)");

    llama_batch_free(batch);
    printf("  PASS\n");
    return 0;
}

static int test_single_token(void) {
    printf("test_single_token: 1 token\n");
    llama_token tokens[1] = {999};
    struct llama_batch batch = build_batch_with_seq(tokens, 1, 0, 0);

    CHECK(batch.n_tokens == 1, "batch.n_tokens != 1");
    CHECK(batch.token[0] == 999, "token[0] != 999");
    CHECK(batch.n_seq_id[0] == 1, "n_seq_id[0] != 1");
    CHECK(batch.seq_id[0][0] == 0, "seq_id[0][0] != 0");
    CHECK(batch.pos[0] == 0, "pos[0] != 0");
    // Single token: это он же last, так что logits=1
    CHECK(batch.logits[0] == 1, "logits[0] != 1 (only token = last)");

    llama_batch_free(batch);
    printf("  PASS\n");
    return 0;
}

static int test_n_tokens_zero_early_return(void) {
    printf("test_n_tokens_zero_early_return: n_tokens=0 → early return path\n");
    struct llama_batch batch = build_batch_with_seq(NULL, 0, 0, 0);

    // Early return path: возвращается batch с n_tokens=0 (no-op for llama_decode).
    CHECK(batch.n_tokens == 0, "batch.n_tokens != 0 (expected early-return no-op)");

    llama_batch_free(batch);
    printf("  PASS\n");
    return 0;
}

static int test_n_tokens_negative_early_return(void) {
    printf("test_n_tokens_negative_early_return: n_tokens=-1 → early return path\n");
    struct llama_batch batch = build_batch_with_seq(NULL, -1, 0, 0);

    CHECK(batch.n_tokens == 0, "batch.n_tokens != 0 (negative должен early-return)");

    llama_batch_free(batch);
    printf("  PASS\n");
    return 0;
}

static int test_seq_id_isolation(void) {
    printf("test_seq_id_isolation: 3 tokens, seq_id=7 (slot isolation)\n");
    // Round 13: critical для multi-slot KV-cache — каждый токен в batch
    // должен иметь правильный seq_id, иначе KV-cache mixing между slots.
    llama_token tokens[3] = {100, 200, 300};
    struct llama_batch batch = build_batch_with_seq(tokens, 3, 7, 50);

    CHECK(batch.n_tokens == 3, "batch.n_tokens != 3");
    CHECK(batch.seq_id[0][0] == 7, "seq_id[0][0] != 7");
    CHECK(batch.seq_id[1][0] == 7, "seq_id[1][0] != 7");
    CHECK(batch.seq_id[2][0] == 7, "seq_id[2][0] != 7");

    llama_batch_free(batch);
    printf("  PASS\n");
    return 0;
}

int main(void) {
    printf("=== build_batch_with_seq regression tests (Round 13 fix verification) ===\n\n");

    int failures = 0;
    failures += test_n_tokens_set_for_nonzero();
    failures += test_single_token();
    failures += test_n_tokens_zero_early_return();
    failures += test_n_tokens_negative_early_return();
    failures += test_seq_id_isolation();

    printf("\n");
    if (failures == 0) {
        printf("ALL PASSED (5/5)\n");
        printf("build_batch_with_seq contract holds — Round 13 regression защищён\n");
        return 0;
    } else {
        printf("FAILURES: %d / 5\n", failures);
        printf("Round 13 REGRESSION DETECTED: batch.n_tokens не выставлен —\n");
        printf("llama_decode увидит пустой batch и вернёт -1 с 'decode: n_tokens == 0'.\n");
        printf("Восстанови batch.n_tokens = n_tokens; в конце build_batch_with_seq.\n");
        return 1;
    }
}
