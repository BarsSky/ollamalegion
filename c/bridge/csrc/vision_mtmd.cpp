// vision_mtmd.cpp — R248/B2/B3: mmproj passthrough через libmtmd.
//
// B2: загрузка проектора и явный факт vision-capability
//     (mmproj загружен + mtmd_support_vision() == true; никакой догадки
//     по имени модели — см. R248/B1 capabilities.vision).
// B3: префилл промпта с изображениями — декод картинок (stb через mtmd-helper),
//     токенизация с медиа-маркером и прогон чанков (текст + image-эмбеддинги)
//     через mtmd_helper_eval_chunks. Генерация после префилла остаётся за
//     bridge.c (тот же sampler-цикл, что и в текстовом пути).
#include "vision_mtmd.h"

#include <cstdio>
#include <cstring>
#include <vector>

#include "mtmd-helper.h"
#include "mtmd.h"

namespace {

// Лимит картинок на один запрос. Не про VRAM (её считает llama.cpp), а про
// защиту от запроса, который несёт десятки изображений: префилл такого
// запроса занимает минуты и всё равно не влезет в контекст.
constexpr int kMaxImagesPerRequest = 8;

int fail(char* err, int err_cap, const char* msg) {
    if (err != nullptr && err_cap > 0) {
        std::snprintf(err, static_cast<size_t>(err_cap), "%s", msg);
    }
    return -1;
}

} // namespace

extern "C" int bridge_mtmd_load(void* llama_model_ptr, const char* mmproj_path,
                                void** out_ctx, char* err, int err_cap) {
    if (out_ctx != nullptr) {
        *out_ctx = nullptr;
    }
    if (out_ctx == nullptr) {
        return fail(err, err_cap, "mmproj: out param is null");
    }
    if (llama_model_ptr == nullptr) {
        return fail(err, err_cap, "mmproj: text model is not loaded");
    }
    if (mmproj_path == nullptr || mmproj_path[0] == '\0') {
        return fail(err, err_cap, "mmproj: empty projector path");
    }

    mtmd_context_params params = mtmd_context_params_default();
    params.use_gpu = true;
    params.n_threads = 4;

    // mmproj привязывается к текстовой модели: несовместимый проектор
    // обязан падать здесь, а не при первом изображении.
    mtmd_context* ctx = mtmd_init_from_file(
        mmproj_path, reinterpret_cast<const struct llama_model*>(llama_model_ptr),
        params);
    if (ctx == nullptr) {
        return fail(err, err_cap,
                    "mmproj: mtmd_init_from_file failed (projector incompatible "
                    "with model or file is not a valid mmproj)");
    }
    if (!mtmd_support_vision(ctx)) {
        mtmd_free(ctx);
        return fail(err, err_cap, "mmproj: projector has no vision capability");
    }

    *out_ctx = ctx;
    return 0;
}

extern "C" void bridge_mtmd_free(void* ctx) {
    if (ctx != nullptr) {
        mtmd_free(static_cast<mtmd_context*>(ctx));
    }
}

extern "C" int bridge_mtmd_vision_supported(void* ctx) {
    if (ctx == nullptr) {
        return 0;
    }
    return mtmd_support_vision(static_cast<const mtmd_context*>(ctx)) ? 1 : 0;
}

// R248/B3: медиа-маркер проектора (у SmolVLM — "<__media__>").
// Указатель живёт столько же, сколько mtmd_context (владеет mtmd).
extern "C" const char* bridge_mtmd_marker(void* ctx) {
    if (ctx == nullptr) {
        return nullptr;
    }
    return mtmd_get_marker(static_cast<const mtmd_context*>(ctx));
}

// R248/B3: префилл промпта с изображениями.
//
// Промпт приходит УЖЕ с медиа-маркером (mtmd_get_marker(), у SmolVLM — "<__media__>")
// на месте каждой картинки, в том же порядке, что bufs/lens.
//
// Ошибки различаются намеренно: клиенту и оператору нужно знать, ЧТО сломалось
// (битая картинка vs. несовместимый проектор vs. нехватка контекста), а не
// получать общий "inference failed":
//   image_decode_failed   — stb не смог декодировать байты (не картинка/битый файл)
//   too_many_images       — больше лимита за один запрос
//   image_tokenize_failed — число маркеров в промпте не совпало с числом картинок
//   image_eval_failed     — llama_decode/mtmd_encode упал на чанках
extern "C" int bridge_mtmd_prefill(void* mtmd_ctx_void, void* llama_ctx_void,
                                   const char* prompt,
                                   const unsigned char* const* bufs,
                                   const size_t* lens, int n_images,
                                   int n_batch, int seq_id,
                                   char* err, int err_cap, int32_t* out_n_past) {
    if (out_n_past != nullptr) {
        *out_n_past = 0;
    }
    if (mtmd_ctx_void == nullptr) {
        return fail(err, err_cap, "vision_not_supported");
    }
    if (llama_ctx_void == nullptr || prompt == nullptr) {
        return fail(err, err_cap, "image_invalid_request");
    }
    if (n_images < 0 || n_images > kMaxImagesPerRequest) {
        return fail(err, err_cap, "too_many_images");
    }
    if (n_images > 0 && (bufs == nullptr || lens == nullptr)) {
        return fail(err, err_cap, "image_invalid_request");
    }
    if (out_n_past == nullptr) {
        return fail(err, err_cap, "image_invalid_request");
    }

    auto* mctx = static_cast<mtmd_context*>(mtmd_ctx_void);
    auto* lctx = static_cast<llama_context*>(llama_ctx_void);

    mtmd_helper_init_opt opt = mtmd_helper_init_opt_default();

    int rc = 0;
    // owned — во владении (освобождаем в конце), refs — массив, который
    // принимает mtmd_tokenize (const mtmd_bitmap **). Два вектора, потому что
    // mtmd_bitmap** нельзя конвертировать в const mtmd_bitmap** без хаков.
    std::vector<mtmd_bitmap*> owned;
    std::vector<const mtmd_bitmap*> refs;
    owned.reserve(static_cast<size_t>(n_images));
    refs.reserve(static_cast<size_t>(n_images));

    for (int i = 0; i < n_images && rc == 0; i++) {
        if (bufs[i] == nullptr || lens[i] == 0) {
            rc = fail(err, err_cap, "image_decode_failed");
            break;
        }
        mtmd_helper_bitmap_wrapper wrapper =
            mtmd_helper_bitmap_init_from_buf(mctx, bufs[i], lens[i], false, opt);
        if (wrapper.bitmap == nullptr) {
            rc = fail(err, err_cap, "image_decode_failed");
            break;
        }
        if (mtmd_bitmap_is_audio(wrapper.bitmap)) {
            // Аудио-вход в этом пайплайне не поддержан: честный отказ вместо
            // тихой подмены «картинка не распознана».
            mtmd_bitmap_free(wrapper.bitmap);
            rc = fail(err, err_cap, "unsupported_media");
            break;
        }
        owned.push_back(wrapper.bitmap);
        refs.push_back(wrapper.bitmap);
    }

    mtmd_input_chunks* chunks = nullptr;
    if (rc == 0) {
        chunks = mtmd_input_chunks_init();
        if (chunks == nullptr) {
            rc = fail(err, err_cap, "image_out_of_memory");
        }
    }

    if (rc == 0) {
        mtmd_input_text text;
        text.text = prompt;
        text.text_len = std::strlen(prompt);
        text.add_special = true;
        text.parse_special = true;

        int32_t tok_rc = mtmd_tokenize(mctx, chunks, &text,
                                      refs.empty() ? nullptr : refs.data(),
                                      static_cast<size_t>(n_images));
        if (tok_rc != 0) {
            // 1 = число медиа-маркеров не совпало с числом bitmaps,
            // 2 = ошибка токенизации текста. Оба случая — «промпт и
            // картинки не сошлись», клиенту важно увидеть именно это.
            rc = fail(err, err_cap, "image_tokenize_failed");
        }
    }

    if (rc == 0) {
        llama_pos n_past = 0;
        int32_t eval_rc = mtmd_helper_eval_chunks(mctx, lctx, chunks, 0,
                                                 static_cast<llama_seq_id>(seq_id),
                                                 n_batch, true, &n_past);
        if (eval_rc != 0 || n_past <= 0) {
            rc = fail(err, err_cap, "image_eval_failed");
        } else {
            *out_n_past = static_cast<int32_t>(n_past);
        }
    }

    if (chunks != nullptr) {
        mtmd_input_chunks_free(chunks);
    }
    for (mtmd_bitmap* bmp : owned) {
        mtmd_bitmap_free(bmp);
    }
    return rc;
}
