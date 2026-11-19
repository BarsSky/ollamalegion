//go:build !llama_stub

// kv_budget.c — R590 (2026-11-19): реализация чистой арифметики KV-cache.
// Зачем вынесено из bridge.c и что было сломано — см. kv_budget.h.
//
// Первая строка `//go:build !llama_stub` обязательна: Go компилирует .c-файлы
// этого пакета через cgo, а под тегом llama_stub (cgo выключен) любой .c без
// этого ограничения валит сборку — «C source files not allowed when not using
// cgo or SWIG». Так же помечены prefix_cache.c и window_fit.c.
#include "kv_budget.h"

// kv_budget_block — байты и элементы в одном блоке типа.
// Для f16 блок = 1 элемент, для квантованных — 32 элемента + fp16-масштаб
// (q8_0: 32 байта данных + 2 байта scale = 34; q4_0: 16 + 2 = 18).
static void kv_budget_block(kv_budget_type type,
                            unsigned long long *block_bytes,
                            unsigned long long *block_elems) {
    switch (type) {
        case KV_BUDGET_TYPE_Q8_0:
            *block_bytes = 34;
            *block_elems = 32;
            return;
        case KV_BUDGET_TYPE_Q4_0:
            *block_bytes = 18;
            *block_elems = 32;
            return;
        case KV_BUDGET_TYPE_F16:
        default:
            *block_bytes = 2;
            *block_elems = 1;
            return;
    }
}

unsigned long long kv_budget_row_bytes(int n_kv_heads, int kv_head_dim, kv_budget_type type) {
    if (n_kv_heads <= 0 || kv_head_dim <= 0) {
        return 0;
    }
    unsigned long long block_bytes = 0;
    unsigned long long block_elems = 1;
    kv_budget_block(type, &block_bytes, &block_elems);

    unsigned long long elems = (unsigned long long)n_kv_heads * (unsigned long long)kv_head_dim;
    return elems * block_bytes / block_elems;
}

long long kv_budget_per_token(const kv_budget_shape *shape,
                              kv_budget_type type_k,
                              kv_budget_type type_v) {
    if (shape == 0 || shape->kv_layers <= 0) {
        return 0;
    }
    unsigned long long row_k = kv_budget_row_bytes(shape->n_kv_heads, shape->kv_head_dim, type_k);
    unsigned long long row_v = kv_budget_row_bytes(shape->n_kv_heads, shape->kv_head_dim, type_v);
    if (row_k == 0 || row_v == 0) {
        return 0;
    }
    return (long long)shape->kv_layers * (long long)(row_k + row_v);
}

long long kv_budget_max_n_ctx(long long free_bytes, long long kv_per_token) {
    if (kv_per_token <= 0 || free_bytes <= 0) {
        return 0;
    }
    return free_bytes / kv_per_token;
}

const char *kv_budget_type_name(kv_budget_type type) {
    switch (type) {
        case KV_BUDGET_TYPE_Q8_0:
            return "q8_0";
        case KV_BUDGET_TYPE_Q4_0:
            return "q4_0";
        case KV_BUDGET_TYPE_F16:
        default:
            return "f16";
    }
}

// kv_budget_default_reserve_mb — дефолт резерва VRAM. Обязан совпадать с
// defaultVRAMOverheadMB в internal/cppbackend/kv_cache_type.go (2048).
#define KV_BUDGET_DEFAULT_RESERVE_MB 2048

int kv_budget_reserve_mb(const char* env_value) {
    if (env_value == 0) {
        return KV_BUDGET_DEFAULT_RESERVE_MB;
    }
    // Пробелы и пустая строка — «не задано»: как у Go (strings.TrimSpace + "").
    const char* p = env_value;
    while (*p == ' ' || *p == '\t' || *p == '\r' || *p == '\n') {
        p++;
    }
    if (*p == '\0') {
        return KV_BUDGET_DEFAULT_RESERVE_MB;
    }
    // Знак разбираем явно: strtol вернул бы отрицательное, а по контракту
    // отрицательное = «мусор» → дефолт (в Go: `err != nil || n < 0`).
    int negative = 0;
    if (*p == '+' || *p == '-') {
        negative = (*p == '-');
        p++;
    }
    long value = 0;
    int digits = 0;
    while (*p >= '0' && *p <= '9') {
        if (value > 100000000L) { // защита от переполнения: такое значение всё равно мусор
            return KV_BUDGET_DEFAULT_RESERVE_MB;
        }
        value = value * 10 + (long)(*p - '0');
        digits++;
        p++;
    }
    if (digits == 0) {
        return KV_BUDGET_DEFAULT_RESERVE_MB; // не число
    }
    while (*p == ' ' || *p == '\t' || *p == '\r' || *p == '\n') {
        p++;
    }
    if (*p != '\0') {
        return KV_BUDGET_DEFAULT_RESERVE_MB; // хвост после числа — мусор
    }
    if (negative) {
        return KV_BUDGET_DEFAULT_RESERVE_MB;
    }
    if (value > (long)(2147483647)) {
        return KV_BUDGET_DEFAULT_RESERVE_MB;
    }
    return (int)value;
}

long long kv_budget_max_n_ctx_reserved(long long free_bytes, long long reserve_bytes,
                                       long long kv_per_token) {
    if (kv_per_token <= 0 || free_bytes <= 0) {
        return 0;
    }
    if (reserve_bytes < 0) {
        reserve_bytes = 0;
    }
    if (reserve_bytes >= free_bytes) {
        // Резерв съел всю свободную VRAM: места под KV нет вообще. Ноль, а не
        // отрицательное число — «не влезает» не должно мимикрировать под «влезает».
        return 0;
    }
    return (free_bytes - reserve_bytes) / kv_per_token;
}
