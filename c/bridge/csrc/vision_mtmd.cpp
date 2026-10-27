// vision_mtmd.cpp — R248/B2: загрузка mmproj-проектора через libmtmd.
//
// R248/B2 фиксирует факт возможности vision как ЯВНЫЙ запрос:
// mmproj загружен + mtmd_support_vision() == true. Никакой догадки
// по имени модели (см. R248/B1 — /v1/models capabilities.vision).
#include "vision_mtmd.h"

#include <cstdio>

#include "mtmd.h"

namespace {

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
