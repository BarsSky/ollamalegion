// test_prefix_cache.c — standalone-тесты чистой логики кэша префилла (R493).
//
// Без libllama и без GPU: проверяется только сравнение префиксов и
// выключатель. Само переиспользование KV требует llama_memory_seq_rm и
// проверяется живьём (см. docs/plans/2026-11-16-second-stand-qwen38-speed.md,
// раздел «что проверить»).
#include "../prefix_cache.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// Портативность: в CI тест собирается на Linux (setenv/unsetenv есть), локально
// на Windows их нет — там _putenv_s. Один и тот же тест, чтобы не держать две
// версии проверки.
#ifdef _WIN32
static void test_setenv(const char* name, const char* value) {
    char buf[256];
    snprintf(buf, sizeof(buf), "%s=%s", name, value);
    _putenv(buf);
}
static void test_unsetenv(const char* name) {
    char buf[256];
    snprintf(buf, sizeof(buf), "%s=", name);
    _putenv(buf);
}
static void test_unsetenv_force(const char* name) { test_unsetenv(name); }
#else
static void test_setenv(const char* name, const char* value) { setenv(name, value, 1); }
static void test_unsetenv(const char* name) { unsetenv(name); }
static void test_unsetenv_force(const char* name) { unsetenv(name); }
#endif

static int failures = 0;

static void check(const char* name, int got, int want) {
    if (got != want) {
        printf("FAIL %-55s got=%d want=%d\n", name, got, want);
        failures++;
    } else {
        printf("ok   %-55s %d\n", name, got);
    }
}

int main(void) {
    const int32_t seq_a[] = {1, 2, 3, 4, 5, 6};
    const int32_t seq_b[] = {1, 2, 3, 9, 9};
    const int32_t seq_c[] = {7, 8};
    const int32_t seq_same[] = {1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18};

    // T1: частичное совпадение
    check("T1 partial prefix (1,2,3) == 3", bridge_prefix_common_len(seq_a, 6, seq_b, 5), 3);
    // T2: полное совпадение — длина короткой
    check("T2 full match — min length", bridge_prefix_common_len(seq_a, 6, seq_a, 4), 4);
    // T3: нет общего префикса
    check("T3 no common prefix", bridge_prefix_common_len(seq_a, 6, seq_c, 2), 0);
    // T4: пустые входы
    check("T4 empty a", bridge_prefix_common_len(seq_a, 0, seq_b, 5), 0);
    check("T4 empty b", bridge_prefix_common_len(seq_a, 6, seq_b, 0), 0);
    // T5: NULL
    check("T5 NULL a", bridge_prefix_common_len(NULL, 6, seq_b, 5), 0);
    check("T5 NULL b", bridge_prefix_common_len(seq_a, 6, NULL, 5), 0);
    // T6: кэш короче промпта (типичный ход агента: кэш — прошлый промпт)
    check("T6 cache shorter than prompt", bridge_prefix_common_len(seq_same, 18, seq_same, 16), 16);

    // T7: выключатель по умолчанию включён
    test_unsetenv("CPPWORKER_PREFIX_CACHE");
    check("T7 default enabled", bridge_prefix_cache_enabled(), 1);
    // T8: явное выключение каждым из написаний
    const char* offs[] = {"0", "false", "no", "off", "OFF"};
    for (size_t i = 0; i < sizeof(offs) / sizeof(offs[0]); i++) {
        test_setenv("CPPWORKER_PREFIX_CACHE", offs[i]);
        check("T8 disabled", bridge_prefix_cache_enabled(), 0);
    }
    // T9: включение
    test_setenv("CPPWORKER_PREFIX_CACHE", "1");
    check("T9 enabled by 1", bridge_prefix_cache_enabled(), 1);
    test_setenv("CPPWORKER_PREFIX_CACHE", "on");
    check("T9 enabled by on", bridge_prefix_cache_enabled(), 1);
    test_unsetenv_force("CPPWORKER_PREFIX_CACHE");

    // T10: порог переиспользования положительный и осмысленный
    int min_reuse = bridge_prefix_cache_min_reuse();
    check("T10 min_reuse in [1, 256]", min_reuse >= 1 && min_reuse <= 256, 1);

    if (failures != 0) {
        printf("\n%d проверок провалено\n", failures);
        return 1;
    }
    printf("\nвсе проверки prefix_cache прошли\n");
    return 0;
}
