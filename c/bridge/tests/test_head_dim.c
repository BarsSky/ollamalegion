// test_head_dim.c — R705 (2026-11-24): standalone-тест решения «ширина головы K/V».
//
// Без libllama: проверяется ЧИСТАЯ функция bridge_head_dim_resolve (c/bridge/head_dim.c),
// в которую bridge_get_model_metadata передаёт значения llama_hparams и наличие
// ключа <arch>.attention.key_length.
//
// ЖИВОЙ ФАКТ, ради которого это существует (стенд, 2026-11-24, замеры по заголовкам
// моделей): мост публиковал head_dim_k = n_embd/n_heads, а двигатель считает KV по
// <arch>.attention.key_length:
//
//	Qwen3-4B:        80  вместо 128 → KV на 60 % меньше;
//	gemma-4-E4B:     320 вместо 512 → KV на 60 % меньше;
//	Inkling-2-Mini:  96  вместо 128 → KV на 33 % меньше;
//	Qwen3.8-9B:      256 = 256       → числа не меняются.
//
// Сборка и запуск (как остальные standalone-тесты моста):
//   gcc -I.. -o test_head_dim test_head_dim.c ../head_dim.c && ./test_head_dim
//
// КРАСНОТА НА СТАРОМ КОДЕ. На состоянии до R705 этой функции/файла нет — тест не
// собирается; сами числа в старом bridge.c считались формулой, что и закреплено
// ожиданиями ниже (80/320/96 — это ровно то, что публиковалось раньше).
#include "../head_dim.h"

#include <stdio.h>
#include <string.h>

static int failures = 0;

static void expect_int(const char * case_name, const char * what, int got, int want) {
    if (got != want) {
        printf("FAIL %s: %s = %d, want %d\n", case_name, what, got, want);
        failures++;
    }
}

static void expect_source(const char * case_name, bridge_head_dim hd, int want) {
    if (hd.source != want) {
        printf("FAIL %s: source = %d, want %d\n", case_name, hd.source, want);
        failures++;
    }
}

// Qwen3-4B: n_embd=2560, n_heads=32, key_length=value_length=128 (файл стенда).
// hparams = 128 (llama.cpp прочитал ключ), формула дала бы 80.
static void case_qwen3_4b(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2560, 32, 128, 128, 128, 128);
    expect_int("qwen3-4b", "K", hd.head_dim_k, 128);
    expect_int("qwen3-4b", "V", hd.head_dim_v, 128);
    expect_source("qwen3-4b", hd, BRIDGE_HEAD_DIM_SOURCE_GGUF);

    // НЕ-ВАКУУМНОСТЬ: это НЕ прежняя формула.
    if (hd.head_dim_k == 2560 / 32) {
        printf("FAIL qwen3-4b: head_dim совпал с формулой n_embd/n_heads (80) — ключ проигнорирован\n");
        failures++;
    }
}

// gemma-4-E4B: n_embd=2560, n_heads=8, key_length=value_length=512 (формула — 320).
static void case_gemma4(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2560, 8, 512, 512, 512, 512);
    expect_int("gemma-4-e4b", "K", hd.head_dim_k, 512);
    expect_int("gemma-4-e4b", "V", hd.head_dim_v, 512);
    expect_source("gemma-4-e4b", hd, BRIDGE_HEAD_DIM_SOURCE_GGUF);
    if (hd.head_dim_k == 2560 / 8) {
        printf("FAIL gemma-4-e4b: head_dim совпал с формулой (320)\n");
        failures++;
    }
}

// Inkling-2-Mini: n_embd=2304, n_heads=24, key_length=128 (формула — 96).
static void case_inkling(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2304, 24, 128, 128, 128, 128);
    expect_int("inkling-2-mini", "K", hd.head_dim_k, 128);
    expect_source("inkling-2-mini", hd, BRIDGE_HEAD_DIM_SOURCE_GGUF);
    if (hd.head_dim_k == 2304 / 24) {
        printf("FAIL inkling-2-mini: head_dim совпал с формулой (96)\n");
        failures++;
    }
}

// Qwen3.8-9B: key_length = 256 = n_embd/n_heads — числа НЕ меняются.
static void case_qwen38_unchanged(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(4096, 16, 256, 256, 256, 256);
    expect_int("qwen3.8-9b", "K", hd.head_dim_k, 256);
    expect_int("qwen3.8-9b", "V", hd.head_dim_v, 256);
    expect_source("qwen3.8-9b", hd, BRIDGE_HEAD_DIM_SOURCE_GGUF);
    expect_int("qwen3.8-9b", "K == n_embd/n_heads", hd.head_dim_k, 4096 / 16);
}

// Обратная совместимость: ключа в файле нет, hparams пришёл дефолтом n_embd/n_head.
// Число обязано остаться прежним (80), но источник — COMPUTED, не GGUF.
static void case_no_key_falls_back_to_computed(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2560, 32, 80, 80, 0, 0);
    expect_int("no-key", "K", hd.head_dim_k, 80);
    expect_int("no-key", "V", hd.head_dim_v, 80);
    expect_source("no-key", hd, BRIDGE_HEAD_DIM_SOURCE_COMPUTED);
}

// Старый мост: hparams не передан (0), ключа нет — чистая формула, COMPUTED.
static void case_hparams_missing(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2560, 32, 0, 0, 0, 0);
    expect_int("hparams-missing", "K", hd.head_dim_k, 80);
    expect_int("hparams-missing", "V", hd.head_dim_v, 80);
    expect_source("hparams-missing", hd, BRIDGE_HEAD_DIM_SOURCE_COMPUTED);
}

// value_length опционален: есть только key_length — V считается равной K
// (ровно как в llama.cpp, src/llama-model.cpp:1662-1663).
static void case_value_length_optional(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(2560, 32, 128, 0, 128, 0);
    expect_int("value-optional", "K", hd.head_dim_k, 128);
    expect_int("value-optional", "V", hd.head_dim_v, 128);
    expect_source("value-optional", hd, BRIDGE_HEAD_DIM_SOURCE_GGUF);
}

// Не-transformer модель (BERT-подобная): голов нет, ширины нет — публиковать
// нечего, источник UNKNOWN (а не «computed 0»).
static void case_non_transformer(void) {
    bridge_head_dim hd = bridge_head_dim_resolve(768, 0, 0, 0, 0, 0);
    expect_int("non-transformer", "K", hd.head_dim_k, 0);
    expect_int("non-transformer", "V", hd.head_dim_v, 0);
    expect_source("non-transformer", hd, BRIDGE_HEAD_DIM_SOURCE_UNKNOWN);
}

int main(void) {
    case_qwen3_4b();
    case_gemma4();
    case_inkling();
    case_qwen38_unchanged();
    case_no_key_falls_back_to_computed();
    case_hparams_missing();
    case_value_length_optional();
    case_non_transformer();

    if (failures != 0) {
        printf("test_head_dim: %d failure(s)\n", failures);
        return 1;
    }
    printf("test_head_dim: OK (qwen3-4b 128, gemma-4 512, inkling 128, qwen3.8 256, fallback computed)\n");
    return 0;
}
