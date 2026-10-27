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

#include <stddef.h>
#include <stdint.h>

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

// R248/B3: медиа-маркер проектора (например, "<__media__>"). NULL без проекта.
// Строка принадлежит mtmd_context — не освобождать.
const char* bridge_mtmd_marker(void* ctx);

// R248/B3: префилл промпта с изображениями. Промпт должен содержать
// медиа-маркер (mtmd_get_marker()) на месте каждой картинки, порядок
// bufs/lens совпадает с порядком маркеров.
//
// buf — БАЙТЫ ФАЙЛА (png/jpg), декодирование делает libmtmd (stb_image):
// Go-стороне не нужно разбирать форматы картинок.
//
// out_n_past — сколько позиций занял префилл (для генерации с этих позиций).
// Возвращает 0 при успехе; -1 с текстом в err:
//   vision_not_supported | image_invalid_request | too_many_images
//   image_decode_failed | unsupported_media | image_tokenize_failed
//   image_eval_failed | image_out_of_memory
int bridge_mtmd_prefill(void* mtmd_ctx, void* llama_ctx, const char* prompt,
                        const unsigned char* const* bufs, const size_t* lens,
                        int n_images, int n_batch, int seq_id,
                        char* err, int err_cap, int32_t* out_n_past);

#ifdef __cplusplus
}
#endif

#endif // OLLAMALEGION_BRIDGE_VISION_MTMD_H
