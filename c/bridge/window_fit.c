//go:build !llama_stub

// window_fit.c — R493 (2026-11-17): чистая логика окна слота и текст отказа
// декода. См. window_fit.h — почему это отдельный файл.
#include "window_fit.h"

#include <stdio.h>
#include <string.h>

int bridge_window_effective_seq(int n_ctx_total, int n_ctx_seq) {
    if (n_ctx_seq > 0) {
        return n_ctx_seq;
    }
    return n_ctx_total;
}

int bridge_window_fit_kind(int actual_tokens, int n_predict,
                           int n_ctx_total, int n_ctx_seq, int n_ctx_override) {
    int per_seq = bridge_window_effective_seq(n_ctx_total, n_ctx_seq);
    if (per_seq <= 0) {
        // Окно неизвестно — классифицировать нечем, решение за вызывающим.
        return BRIDGE_WINDOW_FIT_OK;
    }
    if (n_ctx_override > per_seq) {
        // Клиент просит окно больше, чем даёт слот: это законный повод для
        // перезагрузки (и раньше он терялся, если override <= суммарного окна).
        return BRIDGE_WINDOW_FIT_RELOAD;
    }
    if (actual_tokens <= 0) {
        return BRIDGE_WINDOW_FIT_OK;
    }
    int predict = n_predict > 0 ? n_predict : 0;
    long long total = (long long)actual_tokens + (long long)predict + 1;
    if (total > (long long)per_seq) {
        return BRIDGE_WINDOW_FIT_TOO_LONG;
    }
    return BRIDGE_WINDOW_FIT_OK;
}

int bridge_window_suggest_batch(int n_batch, int n_ctx_total, int n_ctx_seq) {
    int per_seq = bridge_window_effective_seq(n_ctx_total, n_ctx_seq);
    if (n_batch <= 0) {
        return 64; // исторический дефолт моста
    }
    if (per_seq > 0 && n_batch > per_seq) {
        return per_seq;
    }
    return n_batch;
}

int bridge_decode_failure_message(char* buf, size_t buf_size,
                                  int actual_tokens, int n_predict, int n_batch,
                                  int n_ctx_total, int n_ctx_seq) {
    if (buf == NULL || buf_size == 0) {
        return 0;
    }
    int per_seq = bridge_window_effective_seq(n_ctx_total, n_ctx_seq);
    long long need = (long long)actual_tokens + (long long)(n_predict > 0 ? n_predict : 0) + 1;
    int fits = (per_seq <= 0) || (need <= (long long)per_seq);
    int n = 0;
    if (fits) {
        // Числа НЕ подтверждают переполнение окна: говорим это прямо, иначе
        // оператор снова будет искать «n_ctx overflow» там, где его нет.
        // R521 (§8.3.6): подсказка больше не называет n_batch=128 (наш дефолт —
        // 64, то есть совет был заведомо невыполним) и не ссылается на
        // max_vram_n_ctx, который к моменту ошибки устарел (в архиве 234 810 при
        // фактических 37k). Причина при влезающих числах — ПАМЯТЬ, и совет
        // должен быть про неё: окно, слои на GPU, освобождение VRAM.
        n = snprintf(buf, buf_size,
            "llama_decode failed for prompt batch: prompt_tokens=%d, n_predict=%d, n_batch=%d, "
            "n_ctx=%d (per slot %d) — запрос влезает в окно по числам, значит причина не в окне, "
            "а в памяти/раскладке (VRAM под compute-buffer, частичный offload). "
            "Освободите VRAM: меньше слоёв на GPU, меньше окно, kv-cache q8_0/q4_0; "
            "n_batch=%d — это уже наш рабочий минимум, уменьшать его дальше смысла нет",
            actual_tokens, n_predict, n_batch, n_ctx_total, per_seq, n_batch);
    } else {
        n = snprintf(buf, buf_size,
            "llama_decode failed for prompt batch: prompt_tokens=%d + n_predict=%d + 1 = %lld > "
            "окно слота %d (n_ctx=%d, per slot %d, n_batch=%d). "
            "Уменьшите prompt/n_predict или перезагрузите модель с большим окном",
            actual_tokens, n_predict, need, per_seq, n_ctx_total, per_seq, n_batch);
    }
    if (n < 0) {
        buf[0] = '\0';
        return 0;
    }
    return n;
}
