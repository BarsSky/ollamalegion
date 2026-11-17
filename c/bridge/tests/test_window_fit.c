// test_window_fit.c — R493 (2026-11-17): ctest для pure-логики окна слота.
//
// Живой факт, ради которого это проверяется: архив второго стенда дал
// `llama_decode failed for prompt batch ... (current_n_ctx=0, required_n_ctx=0)`
// при prompt=80878 и загруженном окне 131072. Окно влезало, но проверка
// сравнивала суммарное окно с суммой, а не окно СЛОТА, и ошибка не называла
// фактов. Здесь проверяются: классификация (reload / too_long / ok), окно слота,
// урезание батча и текст отказа.
//
// Запуск: ctest -R test_window_fit --output-on-failure
#include "window_fit.h"

#include <stdio.h>
#include <string.h>

static int failures = 0;
static int checks = 0;

static void check(int cond, const char* what) {
    checks++;
    if (!cond) {
        failures++;
        printf("  FAIL %s\n", what);
    } else {
        printf("  ok   %s\n", what);
    }
}

int main(void) {
    printf("window_fit\n");

    // 1) Окно слота: если известно — берём его, иначе суммарное.
    check(bridge_window_effective_seq(131072, 0) == 131072, "окно слота = суммарное, когда слотов 1");
    check(bridge_window_effective_seq(131072, 65536) == 65536, "окно слота = n_ctx_seq при 2 слотах");

    // 2) Живой случай: prompt 80878 + n_predict 512 при окне 131072 → OK.
    check(bridge_window_fit_kind(80878, 512, 131072, 131072, 131072) == BRIDGE_WINDOW_FIT_OK,
          "живой случай 80878/131072 классифицируется как «влезает» (переполнения окна нет)");

    // 3) Тот же prompt при ДВУХ слотах (окно слота 65536) → TOO_LONG, а не generic.
    check(bridge_window_fit_kind(80878, 512, 131072, 65536, 0) == BRIDGE_WINDOW_FIT_TOO_LONG,
          "80878 при окне слота 65536 → TOO_LONG (раньше терялось: сравнивали с 131072)");

    // 4) Клиент просит больше окна слота, но меньше суммарного → RELOAD.
    check(bridge_window_fit_kind(1000, 10, 131072, 65536, 131072) == BRIDGE_WINDOW_FIT_RELOAD,
          "override больше окна слота → RELOAD (даже если меньше суммарного)");

    // 5) Неизвестное окно — не классифицируем.
    check(bridge_window_fit_kind(100000, 10, 0, 0, 0) == BRIDGE_WINDOW_FIT_OK,
          "окно неизвестно → OK (решение за вызывающим)");

    // 6) Батч больше окна урезается до окна слота — третий названный причиной
    //    случай «n_batch > n_ctx» не должен даже доходить до декода.
    check(bridge_window_suggest_batch(4096, 131072, 2048) == 2048, "n_batch урезается до окна слота");
    check(bridge_window_suggest_batch(512, 131072, 131072) == 512, "нормальный n_batch не трогаем");
    check(bridge_window_suggest_batch(0, 131072, 131072) == 64, "n_batch по умолчанию = 64");

    // 7) Текст отказа: факты вместо нулей + правильный вывод о причине.
    char buf[768];
    int n = bridge_decode_failure_message(buf, sizeof(buf), 80878, 512, 512, 131072, 131072);
    check(n > 0, "текст записан");
    check(strstr(buf, "prompt_tokens=80878") != NULL, "в тексте есть prompt_tokens");
    check(strstr(buf, "n_ctx=131072") != NULL, "в тексте есть n_ctx");
    check(strstr(buf, "причина не в окне") != NULL,
          "когда числа влезают — текст прямо говорит, что окно НЕ причина");
    check(strstr(buf, "current_n_ctx=0") == NULL, "в тексте нет ложных current_n_ctx=0");

    n = bridge_decode_failure_message(buf, sizeof(buf), 200000, 512, 512, 131072, 131072);
    check(n > 0 && strstr(buf, "> окно слота") != NULL,
          "когда не влезает — текст говорит «больше окна слота»");

    // 8) Граница буфера: не пишем за пределы.
    char small[16];
    bridge_decode_failure_message(small, sizeof(small), 1, 2, 3, 4, 4);
    check(small[sizeof(small) - 1] == '\0' || strlen(small) < sizeof(small),
          "короткий буфер не переполняется");

    printf("\n%s (%d проверок)\n", failures ? "FAILED" : "OK", checks);
    return failures ? 1 : 0;
}
