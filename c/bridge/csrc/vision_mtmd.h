// vision_mtmd.h — R248/B2: C-обёртка над mtmd (mmproj-проектор llama.cpp).
//
// Зачем C++: публичный API mtmd.h принимает ссылки на параметры
// (mtmd_context_params &) и требует C++17, поэтому вызовы живут в
// csrc/vision_mtmd.cpp, а bridge.c (C11) видит только extern "C" границу.
//
// Контракт: загрузка проектора возможна только ПОСЛЕ загрузки текстовой
// модели (mtmd_init_from_file привязывается к конкретной llama_model).
#ifndef OLLAMALEGION_BRIDGE_VISION_MTMD_H
#define OLLAMALEGION_BRIDGE_VISION_MTMD_H

#ifdef __cplusplus
extern "C" {
#endif

// Загружает mmproj-проектор и привязывает его к текстовой модели.
// llama_model_ptr — opaque llama_model*, возвращённый bridge_load_model.
// out_ctx — opaque mtmd_context*; при ошибке остаётся NULL.
// Возвращает 0 при успехе, -1 при ошибке (текст — в err).
int bridge_mtmd_load(void* llama_model_ptr, const char* mmproj_path,
                     void** out_ctx, char* err, int err_cap);

// Освобождает проектор. NULL-safe.
void bridge_mtmd_free(void* ctx);

// 1 — проектор загружен и умеет vision, 0 — иначе.
int bridge_mtmd_vision_supported(void* ctx);

#ifdef __cplusplus
}
#endif

#endif // OLLAMALEGION_BRIDGE_VISION_MTMD_H
