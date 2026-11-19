// kv_budget_ggml.h — R590 (2026-11-19): адаптер «тип KV из llama.cpp → kv_budget_type».
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ЗАГОЛОВОК. kv_budget.c обязан оставаться чистым (никакого
// llama.h), иначе его нельзя прогнать standalone-тестом в CI и локально — а
// именно этой проверки не хватало, когда формула KV для f16 разошлась с
// реальностью в 16 раз (см. kv_budget.h). Поэтому единственное место, которое
// знает про GGML_TYPE_*, — этот адаптер, и его компилирует только bridge.c.
//
// НЕИЗВЕСТНЫЙ ТИП = f16. Это тот же выбор, что в Go
// (internal/cppbackend/kv_cache_type.go: kvCacheBytesPerElem, ветка default), и
// он консервативный: из поддерживаемых типов f16 даёт самый большой байт на
// элемент (2 против 34/32 и 18/32), то есть занижение потолка n_ctx, а не
// переполнение VRAM.
#ifndef BRIDGE_KV_BUDGET_GGML_H
#define BRIDGE_KV_BUDGET_GGML_H

#include "kv_budget.h"
#include "llama.h"

#ifdef __cplusplus
extern "C" {
#endif

static inline kv_budget_type kv_budget_type_from_ggml(enum ggml_type type) {
    switch (type) {
        case GGML_TYPE_Q8_0:
            return KV_BUDGET_TYPE_Q8_0;
        case GGML_TYPE_Q4_0:
            return KV_BUDGET_TYPE_Q4_0;
        case GGML_TYPE_F16:
        default:
            return KV_BUDGET_TYPE_F16;
    }
}

#ifdef __cplusplus
}
#endif

#endif // BRIDGE_KV_BUDGET_GGML_H
