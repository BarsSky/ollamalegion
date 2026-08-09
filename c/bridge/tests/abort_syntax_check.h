// Minimal llama.h mock — ТОЛЬКО для syntax-check abort API.
// Реальный build использует настоящий llama.h из c/llama.cpp/.
// Этот файл существует для CI-style проверки синтаксиса без полного
// llama.cpp source tree (нужен Docker build для реальной компиляции).

#ifndef LLAMA_H_MOCK_FOR_ABORT_CHECK
#define LLAMA_H_MOCK_FOR_ABORT_CHECK

#include <stdint.h>
#include <stdbool.h>

typedef int32_t llama_token;
typedef int32_t llama_seq_id;
typedef int32_t llama_pos;
struct llama_model;
struct llama_context;
struct llama_vocab;
struct llama_batch { int dummy; };
struct llama_memory;
typedef struct llama_memory *llama_memory_t;
typedef struct llama_batch llama_batch_t;

// Заглушки — реальные сигнатуры не используются в abort check.
struct llama_batch llama_batch_init(int, int, int);
void llama_batch_free(struct llama_batch);
int llama_decode(struct llama_context*, struct llama_batch);
struct llama_memory* llama_get_memory(struct llama_context*);
void llama_memory_clear(struct llama_memory*, bool);
void llama_memory_seq_rm(struct llama_memory*, llama_seq_id, int, int);

#endif // LLAMA_H_MOCK_FOR_ABORT_CHECK
