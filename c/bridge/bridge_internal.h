// bridge_internal.h — internal C API exposed ONLY for tests.
//
// НЕ добавлять в стабильный API bridge.h. Этот header используется
// regression-тестами в c/bridge/tests/ для проверки контракта
// build_batch_with_seq (Round 13 regression: n_tokens инициализируется
// в 0 и должен быть явно выставлен после populate loop).
//
// Любые изменения сигнатуры build_batch_with_seq должны отражаться здесь.

#ifndef BRIDGE_INTERNAL_H
#define BRIDGE_INTERNAL_H

#include "llama.h"
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

// build_batch_with_seq — создаёт llama_batch с явным seq_id для каждого токена.
//
// Контракт (Round 13 + fix 2026-07-29):
//   - input n_tokens > 0:
//       * batch.n_tokens == n_tokens
//       * batch.token[i] == tokens[i] для i ∈ [0, n_tokens)
//       * batch.seq_id[i][0] == seq_id для всех i
//       * batch.pos[i] == start_pos + i
//       * batch.logits[n_tokens - 1] == 1 (last token needs logits for sampling)
//       * batch.logits[i] == 0 для i < n_tokens - 1
//   - input n_tokens <= 0:
//       * возвращается batch с n_tokens=0 (no-op for llama_decode)
//
// Возвращаемый batch должен быть освобождён через llama_batch_free().
struct llama_batch build_batch_with_seq(
    llama_token* tokens, int32_t n_tokens, llama_seq_id seq_id, llama_pos start_pos
);

// build_batched_batch — Round 15.1 (2026-07-29): true batched parallel.
//
// Контракт:
//   - n_sequences == 0:
//       * возвращается batch с n_tokens=0 (no-op)
//   - все sequences[i].n_tokens == 0:
//       * возвращается batch с n_tokens=0 (no-op)
//   - иначе (n_sequences > 0, хотя бы одна sequence non-empty):
//       * batch.n_tokens == sum(sequences[i].n_tokens) для non-empty sequences
//       * batch.token[offset+j] == sequences[i].tokens[j]
//       * batch.seq_id[offset+j][0] == sequences[i].seq_id
//       * batch.pos[offset+j] == sequences[i].start_pos + j
//       * batch.logits[последний_токен_каждой_sequence] == 1
//       * batch.logits[остальные] == 0
//       * offset между sequences корректно выставлен
//
// Поведение для смешанных sequences (часть n_tokens=0, часть >0):
//   - Пустые sequences пропускаются (не добавляют токенов).
//   - Это эквивалентно "caller не включил этот slot в batch".
//
// Возвращаемый batch должен быть освобождён через llama_batch_free().
//
// Полная декларация и rationale — в bridge.h:CBridgeBatchedSeq.
struct llama_batch build_batched_batch(
    const struct CBridgeBatchedSeq* sequences, int32_t n_sequences
);

#ifdef __cplusplus
}
#endif

#endif // BRIDGE_INTERNAL_H
