// model_hparams.cpp — R705 (2026-11-24): РЕАЛЬНАЯ ширина головы K/V из llama.cpp.
//
// ЗАЧЕМ ЭТОТ ФАЙЛ. Go-сторона публиковала head_dim_k/head_dim_v, посчитанные в
// bridge.c как n_embd / n_heads. Для моделей, у которых <arch>.attention.key_length
// не совпадает с n_embd/n_heads (Qwen3-4B: 128 против 80; gemma-4-E4B: 512 против
// 320; Inkling-2-Mini: 128 против 96), публикуемое число было меньше реального,
// поэтому KV/токен и «сколько окна влезет» в балансере считались заниженными.
//
// ИСТОЧНИК (по исходникам c/llama.cpp b12642 в этом репозитории):
//   - src/llama-hparams.h:92-95    — поля n_embd_head_k_full/v_full (+swa);
//   - src/llama-model.cpp:1659-1663 — hparams.n_embd_head_k_full = n_embd/n_head,
//     затем переопределяется ключом LLM_KV_ATTENTION_KEY_LENGTH
//     (<arch>.attention.key_length), то же для value_length;
//   - src/llama-model.cpp:1683-1693 — SWA-варианты (LLM_KV_ATTENTION_KEY_LENGTH_SWA);
//   - src/llama-hparams.h:518-519   — публичные аксессоры n_embd_head_k/v(il),
//     учитывающие per-layer SWA-массивы (src/llama-hparams.cpp:140-152);
//   - src/llama-model.h:812-818     — struct llama_model с полем `llama_hparams hparams`.
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ C++ ФАЙЛ. В b12642 в публичном include/llama.h НЕТ символов
// llama_model_n_embd_head_k/_v (проверено: есть только n_embd/n_head/n_head_kv),
// а доступ к hparams живёт в приватном заголовке src/llama-model.h. Мост — C
// (bridge.c), C++-классы он включать не может, поэтому чтение вынесено в
// маленький C++-TU с extern "C"-фасадом. CMake моста добавляет
// ${LLAMA_CPP_ROOT}/src в include-пути, а Dockerfile-стадии bridge-builder
// копируют src/ из стадии llama-builder.
//
// ГРАНИЦА. Здесь ТОЛЬКО чтение уже посчитанных llama.cpp полей — никакого
// «своего» расчёта. Если поле 0 (не-transformer модель) — возвращаем -1, и
// вызывающий обязан уйти на fallback (там это видно в head_dim_source).
#include "llama.h"
#include "llama-model.h"

extern "C" {

int ollamalegion_model_head_dim_k_full(const struct llama_model * model) {
    if (model == nullptr) {
        return -1;
    }
    const uint32_t v = model->hparams.n_embd_head_k_full;
    return (v > 0) ? (int) v : -1;
}

int ollamalegion_model_head_dim_v_full(const struct llama_model * model) {
    if (model == nullptr) {
        return -1;
    }
    const uint32_t v = model->hparams.n_embd_head_v_full;
    return (v > 0) ? (int) v : -1;
}

} // extern "C"
