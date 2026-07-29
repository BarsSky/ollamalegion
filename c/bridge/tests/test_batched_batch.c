// test_batched_batch.c — regression test для build_batched_batch (Round 15.1).
//
// ЗАЧЕМ: Round 15.1 добавляет multi-sequence batch для true parallel inference.
// build_batched_batch собирает N sequences в ОДИН llama_batch с явным seq_id
// для каждого токена, что позволяет llama_decode выполнить forward pass
// для всех sequences параллельно в одной CUDA-операции.
//
// Этот тест проверяет контракт build_batched_batch (по аналогии с
// test_batch_n_tokens.c для build_batch_with_seq):
//   1. n_sequences=0 → no-op batch (n_tokens=0)
//   2. все sequences empty → no-op batch
//   3. n_sequences=2 (нормальный case) →
//      - batch.n_tokens == sum(sequences[i].n_tokens)
//      - tokens, seq_id, pos, logits корректно распределены между sequences
//      - последний токен каждой sequence имеет logits=1
//   4. n_sequences=2 mixed (1 empty, 1 non-empty) →
//      - empty sequence пропускается (не добавляет токенов)
//      - non-empty sequence обрабатывается нормально
//   5. n_sequences=3 (3 sequences) →
//      - все 3 sequences корректно в batch
//      - offset правильно инкрементируется
//
// Если кто-то в будущем сломает контракт (например, забудет batch.n_tokens=...
// как в Round 13 регрессии) — этот тест поймает сразу.

#include "bridge_internal.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <assert.h>

// Helper: assertion с message
#define CHECK(cond, msg) do { \
    if (!(cond)) { \
        fprintf(stderr, "FAIL: %s (line %d): %s\n", #cond, __LINE__, msg); \
        exit(1); \
    } \
} while (0)

#define CHECK_EQ(a, b, msg) do { \
    long long _a = (long long)(a); \
    long long _b = (long long)(b); \
    if (_a != _b) { \
        fprintf(stderr, "FAIL: %s == %s (line %d): got %lld, expected %lld — %s\n", \
                #a, #b, __LINE__, _a, _b, msg); \
        exit(1); \
    } \
} while (0)

int main(void) {
    printf("test_batched_batch: starting Round 15.1 contract test\n");

    // ============================================================
    // Test 1: n_sequences=0 → no-op batch
    // ============================================================
    {
        printf("  test 1: n_sequences=0 (early return)\n");
        struct llama_batch batch = build_batched_batch(NULL, 0);
        CHECK_EQ(batch.n_tokens, 0, "n_sequences=0 should produce no-op batch");
        llama_batch_free(batch);
    }

    // ============================================================
    // Test 2: все sequences empty (n_tokens=0) → no-op batch
    // ============================================================
    {
        printf("  test 2: all sequences empty\n");
        llama_token dummy_tok = 1;
        struct CBridgeBatchedSeq seqs[2] = {
            {.tokens = &dummy_tok, .n_tokens = 0, .seq_id = 0, .start_pos = 0},
            {.tokens = &dummy_tok, .n_tokens = 0, .seq_id = 1, .start_pos = 0},
        };
        struct llama_batch batch = build_batched_batch(seqs, 2);
        CHECK_EQ(batch.n_tokens, 0, "all-empty sequences should produce no-op batch");
        llama_batch_free(batch);
    }

    // ============================================================
    // Test 3: n_sequences=2 (нормальный case, mixed sizes)
    //   seq0: 3 tokens at seq_id=0, start_pos=0
    //   seq1: 2 tokens at seq_id=1, start_pos=10
    // ============================================================
    {
        printf("  test 3: 2 sequences (3 + 2 tokens)\n");
        llama_token seq0_tokens[3] = {100, 101, 102};
        llama_token seq1_tokens[2] = {200, 201};
        struct CBridgeBatchedSeq seqs[2] = {
            {.tokens = seq0_tokens, .n_tokens = 3, .seq_id = 0, .start_pos = 0},
            {.tokens = seq1_tokens, .n_tokens = 2, .seq_id = 1, .start_pos = 10},
        };
        struct llama_batch batch = build_batched_batch(seqs, 2);
        CHECK_EQ(batch.n_tokens, 5, "total tokens = 3 + 2 = 5");

        // seq0: indices 0,1,2
        CHECK_EQ(batch.token[0], 100, "seq0[0]");
        CHECK_EQ(batch.token[1], 101, "seq0[1]");
        CHECK_EQ(batch.token[2], 102, "seq0[2]");
        CHECK_EQ(batch.seq_id[0][0], 0, "seq0 token 0 seq_id");
        CHECK_EQ(batch.seq_id[1][0], 0, "seq0 token 1 seq_id");
        CHECK_EQ(batch.seq_id[2][0], 0, "seq0 token 2 seq_id");
        CHECK_EQ(batch.pos[0], 0, "seq0[0] pos");
        CHECK_EQ(batch.pos[1], 1, "seq0[1] pos");
        CHECK_EQ(batch.pos[2], 2, "seq0[2] pos");
        CHECK_EQ(batch.logits[0], 0, "seq0[0] logits (not last)");
        CHECK_EQ(batch.logits[1], 0, "seq0[1] logits (not last)");
        CHECK_EQ(batch.logits[2], 1, "seq0[2] logits (last)");

        // seq1: indices 3,4
        CHECK_EQ(batch.token[3], 200, "seq1[0]");
        CHECK_EQ(batch.token[4], 201, "seq1[1]");
        CHECK_EQ(batch.seq_id[3][0], 1, "seq1 token 0 seq_id");
        CHECK_EQ(batch.seq_id[4][0], 1, "seq1 token 1 seq_id");
        CHECK_EQ(batch.pos[3], 10, "seq1[0] pos = start_pos=10");
        CHECK_EQ(batch.pos[4], 11, "seq1[1] pos = 10+1");
        CHECK_EQ(batch.logits[3], 0, "seq1[0] logits (not last)");
        CHECK_EQ(batch.logits[4], 1, "seq1[1] logits (last)");

        llama_batch_free(batch);
    }

    // ============================================================
    // Test 4: mixed (1 empty, 1 non-empty) → empty skipped
    //   seq0: 0 tokens (skip)
    //   seq1: 2 tokens (keep)
    // ============================================================
    {
        printf("  test 4: 2 sequences (0 + 2 tokens, mixed)\n");
        llama_token dummy_tok = 0;
        llama_token seq1_tokens[2] = {300, 301};
        struct CBridgeBatchedSeq seqs[2] = {
            {.tokens = &dummy_tok, .n_tokens = 0, .seq_id = 0, .start_pos = 0},  // empty
            {.tokens = seq1_tokens, .n_tokens = 2, .seq_id = 1, .start_pos = 5},
        };
        struct llama_batch batch = build_batched_batch(seqs, 2);
        // Empty sequence не добавляет токенов. Total = 0 + 2 = 2.
        CHECK_EQ(batch.n_tokens, 2, "total tokens = 0 + 2 = 2 (empty seq skipped)");

        // Только seq1 в batch.
        CHECK_EQ(batch.token[0], 300, "seq1[0]");
        CHECK_EQ(batch.token[1], 301, "seq1[1]");
        CHECK_EQ(batch.seq_id[0][0], 1, "seq1 token 0 seq_id");
        CHECK_EQ(batch.seq_id[1][0], 1, "seq1 token 1 seq_id");
        CHECK_EQ(batch.pos[0], 5, "seq1[0] pos = start_pos=5");
        CHECK_EQ(batch.pos[1], 6, "seq1[1] pos = 5+1");
        CHECK_EQ(batch.logits[0], 0, "seq1[0] logits (not last)");
        CHECK_EQ(batch.logits[1], 1, "seq1[1] logits (last)");

        llama_batch_free(batch);
    }

    // ============================================================
    // Test 5: 3 sequences (по 1 токену каждая) — multi-slot sampling
    //   seq0: 1 token at seq_id=10
    //   seq1: 1 token at seq_id=20
    //   seq2: 1 token at seq_id=30
    // ============================================================
    {
        printf("  test 5: 3 sequences (1 + 1 + 1 tokens, multi-slot)\n");
        llama_token t0 = 1000, t1 = 2000, t2 = 3000;
        struct CBridgeBatchedSeq seqs[3] = {
            {.tokens = &t0, .n_tokens = 1, .seq_id = 10, .start_pos = 0},
            {.tokens = &t1, .n_tokens = 1, .seq_id = 20, .start_pos = 0},
            {.tokens = &t2, .n_tokens = 1, .seq_id = 30, .start_pos = 0},
        };
        struct llama_batch batch = build_batched_batch(seqs, 3);
        CHECK_EQ(batch.n_tokens, 3, "total = 1+1+1 = 3");

        // Каждый токен — last в своей sequence (все logits=1).
        CHECK_EQ(batch.token[0], 1000, "seq0[0]");
        CHECK_EQ(batch.token[1], 2000, "seq1[0]");
        CHECK_EQ(batch.token[2], 3000, "seq2[0]");

        CHECK_EQ(batch.seq_id[0][0], 10, "seq0 seq_id");
        CHECK_EQ(batch.seq_id[1][0], 20, "seq1 seq_id");
        CHECK_EQ(batch.seq_id[2][0], 30, "seq2 seq_id");

        // Каждый — last token, поэтому logits=1.
        CHECK_EQ(batch.logits[0], 1, "seq0[0] logits (only token)");
        CHECK_EQ(batch.logits[1], 1, "seq1[0] logits (only token)");
        CHECK_EQ(batch.logits[2], 1, "seq2[0] logits (only token)");

        llama_batch_free(batch);
    }

    // ============================================================
    // Test 6 (Round 13 regression check): n_tokens != 0 после populate
    // ============================================================
    {
        printf("  test 6: n_tokens is set (Round 13 regression check)\n");
        llama_token tokens[2] = {1, 2};
        struct CBridgeBatchedSeq seqs[1] = {
            {.tokens = tokens, .n_tokens = 2, .seq_id = 0, .start_pos = 0},
        };
        struct llama_batch batch = build_batched_batch(seqs, 1);
        // Самый критичный check: n_tokens не равен 0.
        if (batch.n_tokens == 0) {
            fprintf(stderr, "FATAL: batch.n_tokens == 0 (Round 13 regression!)\n");
            exit(2);
        }
        CHECK_EQ(batch.n_tokens, 2, "n_tokens must be 2, not 0");
        llama_batch_free(batch);
    }

    printf("\ntest_batched_batch: ALL TESTS PASSED\n");
    return 0;
}
