//go:build !llama_stub

// head_dim.c — R705 (2026-11-24): чистая логика «ширина головы K/V и её источник».
//
// ПРОБЛЕМА, РАДИ КОТОРОЙ ПОЯВИЛСЯ ФАЙЛ. bridge_get_model_metadata считал ширину
// головы как n_embd / n_heads с комментарием «n_embd_head_k/v are NOT in public
// llama.h header». Это верно только для моделей, у которых
// <arch>.attention.key_length совпадает с n_embd/n_heads. На реальных моделях
// стенда — не совпадает:
//
//   Qwen3-4B:        n_embd/n_heads = 2560/32 = 80,    key_length = 128;
//   gemma-4-E4B:     2560/8 = 320,                     key_length = 512;
//   Inkling-2-Mini:  2304/24 = 96,                     key_length = 128;
//   Qwen3.8-9B:      4096/16 = 256,                    key_length = 256 (совпало).
//
// Заниженная ширина → заниженный KV/токен (Qwen3-4B — на 60 %) → завышенное
// «сколько влезет окна» → неверные решения балансера (R659/AutoTune) и врущая
// панель. Поэтому число берётся из модели, а не пересчитывается.
//
// ОТКУДА ЧИСЛА (по исходникам c/llama.cpp b12642 в этом репозитории):
//   - src/llama-model.cpp:1659-1663 — hparams.n_embd_head_k_full := n_embd/n_head,
//     затем переопределяется ключом LLM_KV_ATTENTION_KEY_LENGTH
//     (<arch>.attention.key_length); для V — LLM_KV_ATTENTION_VALUE_LENGTH;
//   - src/llama-hparams.h:92-95 — поля n_embd_head_k_full/v_full;
//   - src/llama-model.h:812-818 — struct llama_model с полем llama_hparams hparams;
//   - src/llama-model.cpp:4004-4012 — llama_model_meta_val_str отдаёт любую
//     сохранённую пару GGUF строкой (gguf_kv_to_str, :1509-1511), поэтому наличие
//     ключа key_length проверяется через неё.
//
// Таким образом hparams — ИСТОЧНИК ИСТИНЫ («чем движок реально считает KV»), а
// метаданные нужны лишь чтобы отличить «ключ в файле был» (факт, source=GGUF) от
// «llama.cpp подставил n_embd/n_head» (оценка, source=COMPUTED).
#include "head_dim.h"

bridge_head_dim bridge_head_dim_resolve(int n_embd, int n_heads,
                                        int hparams_k, int hparams_v,
                                        int meta_key_length, int meta_value_length) {
    bridge_head_dim out;
    out.head_dim_k = 0;
    out.head_dim_v = 0;
    out.source = BRIDGE_HEAD_DIM_SOURCE_UNKNOWN;

    // Ширина головы «по формуле» — то, что llama.cpp подставляет, если ключа нет.
    // При n_heads <= 0 (не-transformer архитектура) формулы не существует.
    int formula = (n_heads > 0) ? (n_embd / n_heads) : 0;

    int k = (hparams_k > 0) ? hparams_k : 0;
    int v = (hparams_v > 0) ? hparams_v : 0;

    if (meta_key_length > 0 || meta_value_length > 0) {
        // Ключ <arch>.attention.key_length/value_length в файле ЕСТЬ — это и есть
        // источник hparams, поэтому число считается фактом.
        if (meta_key_length > 0) {
            k = meta_key_length;
        }
        if (meta_value_length > 0) {
            v = meta_value_length;
        }
        out.source = BRIDGE_HEAD_DIM_SOURCE_GGUF;
    } else if (k > 0 || v > 0) {
        // Ключа нет: значения hparams — это ровно n_embd/n_head (дефолт llama.cpp).
        // Публиковать их как «из модели» было бы неправдой, поэтому COMPUTED.
        out.source = BRIDGE_HEAD_DIM_SOURCE_COMPUTED;
    }

    // Страховки: значение могло не дойти от моста (старая сборка) — берём формулу;
    // V без своего ключа равна K (так же поступает llama.cpp).
    if (k <= 0) {
        k = formula;
    }
    if (v <= 0) {
        v = k;
    }
    if (k <= 0 || v <= 0) {
        // Публиковать нечего: ни ширины, ни n_embd/n_heads (BERT-подобные модели).
        // Источник не выдаём за факт — UNKNOWN.
        out.source = BRIDGE_HEAD_DIM_SOURCE_UNKNOWN;
        return out;
    }

    out.head_dim_k = k;
    out.head_dim_v = v;
    if (out.source == BRIDGE_HEAD_DIM_SOURCE_UNKNOWN) {
        // k/v получились только формулой (hparams пуст, ключа нет) — это оценка.
        out.source = BRIDGE_HEAD_DIM_SOURCE_COMPUTED;
    }
    return out;
}
