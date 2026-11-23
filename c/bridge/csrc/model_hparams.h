// model_hparams.h — R705 (2026-11-24): доступ к РЕАЛЬНОЙ ширине головы K/V.
//
// Объявления для C-кода моста (bridge.c) поверх C++-реализации
// csrc/model_hparams.cpp. Отдельный заголовок нужен, потому что сам bridge.c
// компилируется как C11 и не может включать приватные C++-заголовки llama.cpp
// (src/llama-model.h). См. комментарий в model_hparams.cpp об источнике чисел.
#pragma once

#ifdef __cplusplus
extern "C" {
#endif

// Возвращает n_embd_head_k_full из llama_hparams (attention.key_length, если он
// есть в GGUF, иначе n_embd/n_head — так решил llama.cpp при загрузке).
// -1 = недоступно (NULL-модель или не-transformer архитектура).
int ollamalegion_model_head_dim_k_full(const struct llama_model * model);
int ollamalegion_model_head_dim_v_full(const struct llama_model * model);

#ifdef __cplusplus
}
#endif
