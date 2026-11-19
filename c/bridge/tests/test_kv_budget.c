// test_kv_budget.c — R590 (2026-11-19): ctest для арифметики KV-cache и потолка
// n_ctx по свободной VRAM (c/bridge/kv_budget.c).
//
// ЖИВОЙ ФАКТ, ради которого этот тест существует. Мост (bridge.c) считал KV для
// f16 в 16 раз больше, отдавая это число балансеру как `max_vram_n_ctx`:
//   2026-11-19, RTX 3070, Qwen3-4B-Instruct-2507-Q4_K_M, KV f16:
//     мост:        kv_per_token=2359296 bytes
//     llama.cpp:   CUDA0 KV buffer = 4096.00 MiB + CPU KV = 512.00 MiB
//                  на 32768 ячеек / 36 слоёв  →  147456 B/токен
//     2359296 / 147456 = 16.0
// Три ожидаемых числа ниже — не выдуманные: каждое взято из лога, а не из
// формулы (см. ссылки в комментариях), поэтому тест ловит и занижение, и
// завышение.
//
// Запуск: ctest -R test_kv_budget --output-on-failure
#include "kv_budget.h"

#include <stdio.h>

static int failures = 0;
static int checks = 0;

static void check_ull(unsigned long long got, unsigned long long want, const char* what) {
    checks++;
    if (got != want) {
        failures++;
        printf("  FAIL %s: got %llu, want %llu\n", what, got, want);
    } else {
        printf("  ok   %s (%llu)\n", what, got);
    }
}

static void check_ll(long long got, long long want, const char* what) {
    checks++;
    if (got != want) {
        failures++;
        printf("  FAIL %s: got %lld, want %lld\n", what, got, want);
    } else {
        printf("  ok   %s (%lld)\n", what, got);
    }
}

int main(void) {
    printf("kv_budget\n");

    // ---- 1) f16: 2 байта на элемент, НЕ 32 --------------------------------
    // Регрессия R590: row_bytes=heads*dim*32 вместо heads*dim*2.
    check_ull(kv_budget_row_bytes(4, 213, KV_BUDGET_TYPE_F16), 1704,
              "f16: row_bytes(4 kv-heads, dim 213) = 4*213*2 = 1704 (было 27264 = *32)");

    // Живой лог 27B f16 (docs/plans/2026-09-25-qwen38-load-context-dup-and-infra.md:718):
    //   «head_dim=213, kv_per_token=218112 bytes» при 64 слоях.
    kv_budget_shape qwen27b_f16 = {4, 213, 64};
    check_ll(kv_budget_per_token(&qwen27b_f16, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16), 218112,
             "27B f16 (4 heads, dim 213, 64 слоя) = 218112 B/токен — как в живом логе");

    // ---- 2) f16, измеренный по llama.cpp 2026-11-19 (4B, RTX 3070) --------
    // llama_kv_cache: size = 4608.00 MiB (32768 cells, 36 layers); 36 слоёв при
    // n_kv_heads=8, head_dim=128.
    kv_budget_shape qwen4b_f16 = {8, 128, 36};
    check_ll(kv_budget_per_token(&qwen4b_f16, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16), 147456,
             "4B f16 (8 heads, dim 128, 36 слоёв) = 147456 B/токен — измерено llama.cpp (4608 MiB / 32768)");
    // Явная защита от возврата регрессии: прежнее значение было ровно в 16 раз больше.
    check_ll(kv_budget_per_token(&qwen4b_f16, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16) * 16, 2359296,
             "в 16 раз больше 147456 = 2359296 — ровно то, что печатал сломанный мост");

    // ---- 3) q8_0: 34 байта на 32 элемента --------------------------------
    // Живой стенд 27B/A10 (diag 2026-11-19): n_kv_heads=4, head_dim=213,
    // kv_layers=8 (планировщик memfit) → 2*8*4*213*34/32.
    kv_budget_shape qwen27b_q8 = {4, 213, 8};
    check_ull(kv_budget_row_bytes(4, 213, KV_BUDGET_TYPE_Q8_0), 905,
              "q8_0: row_bytes(4 kv-heads, dim 213) = 4*213*34/32 = 905");
    check_ll(kv_budget_per_token(&qwen27b_q8, KV_BUDGET_TYPE_Q8_0, KV_BUDGET_TYPE_Q8_0), 14480,
             "27B q8_0 (8 слоёв) = 14480 B/токен ≈ 14 КБ — согласуется с max_vram_n_ctx=251254 при ~3.47 GiB свободной VRAM");

    // ---- 4) q4_0: 18 байт на 32 элемента ---------------------------------
    // Живой лог в CHANGELOG (R83): «kv_layers=16 kv_head_dim=256 kv_per_token=18432».
    kv_budget_shape legacy_q4 = {4, 256, 16};
    check_ll(kv_budget_per_token(&legacy_q4, KV_BUDGET_TYPE_Q4_0, KV_BUDGET_TYPE_Q4_0), 18432,
             "q4_0 (4 heads, dim 256, 16 слоёв) = 18432 B/токен — как в живом логе R83");

    // ---- 5) K и V разных типов считаются раздельно -----------------------
    // f16 K + q4_0 V на той же форме: K=1704, V=4*213*18/32=479 (целочисленно)
    // → 8 слоёв * (1704+479) = 17464. Если бы тип K применялся к обоим (как до
    // R590), вышло бы 8*(1704+1704) = 27264 — тест это и различает.
    check_ull(kv_budget_row_bytes(4, 213, KV_BUDGET_TYPE_Q4_0), 479,
              "q4_0: row_bytes = 4*213*18/32 = 479 (целочисленное деление)");
    check_ll(kv_budget_per_token(&qwen27b_q8, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_Q4_0), 17464,
             "K=f16, V=q4_0: типы не смешиваются в один (было бы 27264, тип K на оба)");

    // ---- 6) Потолок n_ctx -------------------------------------------------
    // Свободно 1 GiB при 147456 B/токен → 7281 токен (1073741824 / 147456).
    check_ll(kv_budget_max_n_ctx(1024LL * 1024 * 1024, 147456), 7281,
             "max_n_ctx(1 GiB, 147456 B/токен) = 7281");
    // Согласованность с живым 27B: 251254 токена * 14480 B = свободная VRAM,
    // из которой мост и получил свой потолок.
    check_ll(kv_budget_max_n_ctx(251254LL * 14480, 14480), 251254,
             "max_n_ctx обратен per_token: из 251254*14480 байт получаем снова 251254");

    // ---- 7) Вырожденные входы: «нельзя посчитать» вместо нуля байт -------
    kv_budget_shape broken = {0, 213, 64};
    check_ll(kv_budget_per_token(&broken, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16), 0,
             "n_kv_heads=0 → per_token=0 (нельзя посчитать, а не «0 байт»)");
    kv_budget_shape no_layers = {4, 213, 0};
    check_ll(kv_budget_per_token(&no_layers, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16), 0,
             "kv_layers=0 → per_token=0");
    check_ll(kv_budget_per_token(0, KV_BUDGET_TYPE_F16, KV_BUDGET_TYPE_F16), 0,
             "shape=NULL → per_token=0 без падения");
    check_ll(kv_budget_max_n_ctx(1024LL * 1024 * 1024, 0), 0,
             "kv_per_token=0 → потолок 0 (деления на ноль нет)");
    check_ll(kv_budget_max_n_ctx(0, 147456), 0, "свободной VRAM нет → потолок 0");
    check_ll(kv_budget_max_n_ctx(-4096, 147456), 0, "отрицательная свободная VRAM → потолок 0");
    check_ull(kv_budget_row_bytes(0, 128, KV_BUDGET_TYPE_F16), 0, "row_bytes при 0 голов = 0");

    printf("\n%d checks, %d failures\n", checks, failures);
    return failures == 0 ? 0 : 1;
}
