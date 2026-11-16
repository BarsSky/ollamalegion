//go:build !llama_stub

// prefix_cache.c — R493 (2026-11-16): чистая логика кэша префилла.
// См. prefix_cache.h: почему это отдельный файл и что он даёт.
#include "prefix_cache.h"

#include <stdlib.h>
#include <string.h>
#include <strings.h>

// MIN_REUSE — ниже этой длины общий префикс не считается выгодным: вызов
// llama_memory_seq_rm и копирование буфера стоят больше, чем декод десятка
// токенов. 16 выбрано как «заметно меньше любого реального хода агента и
// заметно больше накладных расходов».
static const int PREFIX_CACHE_MIN_REUSE = 16;

int bridge_prefix_common_len(const int32_t* a, int na, const int32_t* b, int nb) {
    if (a == NULL || b == NULL || na <= 0 || nb <= 0) {
        return 0;
    }
    int n = (na < nb) ? na : nb;
    int i = 0;
    for (; i < n; i++) {
        if (a[i] != b[i]) {
            break;
        }
    }
    return i;
}

int bridge_prefix_cache_enabled(void) {
    const char* v = getenv("CPPWORKER_PREFIX_CACHE");
    if (v == NULL || v[0] == '\0') {
        return 1; // default ON: это оптимизация без изменения контракта клиенту
    }
    if (strcmp(v, "0") == 0 || strcasecmp(v, "false") == 0 ||
        strcasecmp(v, "no") == 0 || strcasecmp(v, "off") == 0) {
        return 0;
    }
    return 1;
}

int bridge_prefix_cache_min_reuse(void) {
    return PREFIX_CACHE_MIN_REUSE;
}
