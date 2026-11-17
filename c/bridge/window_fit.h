//go:build !llama_stub

// window_fit.h — R493 (2026-11-17): «влезает ли запрос в окно СЛОТА» и текст
// ошибки декода — чистые функции, без llama.cpp.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. В архиве второго стенда (2026-11-17) клиент получал
//
//	stream inference failed with code 1: llama_decode failed for prompt batch
//	(likely n_ctx overflow, prompt too long, or n_batch > n_ctx)
//	(current_n_ctx=0, required_n_ctx=0, max_vram_n_ctx=218989)
//
// при загруженном окне 131072 и промпте 80878 токенов на ОДИН слот: окно
// формально влезало, а ошибка (а) не называла фактов, (б) печатала нули как
// измерение и (в) не классифицировалась, поэтому балансер не мог среагировать
// (code 1 = generic, а не 2/3 про окно).
//
// Здесь — только арифметика и текст, поэтому это проверяется ctest-ом без GPU
// и без модели.

#ifndef BRIDGE_WINDOW_FIT_H
#define BRIDGE_WINDOW_FIT_H

#include <stddef.h>

// Коды совпадают с BRIDGE_ERR_* (bridge.h): 2 = нужен reload, 3 = промпт не влезает.
#define BRIDGE_WINDOW_FIT_OK        0
#define BRIDGE_WINDOW_FIT_RELOAD    2
#define BRIDGE_WINDOW_FIT_TOO_LONG  3

// bridge_window_effective_seq — окно ОДНОГО слота.
//
// llama.cpp при n_seq_max>1 делит суммарное окно между слотами, и запрос
// ограничен именно окном слота. Если n_ctx_seq неизвестен (0), считаем, что
// слот один, и берём суммарное окно.
int bridge_window_effective_seq(int n_ctx_total, int n_ctx_seq);

// bridge_window_fit_kind — классификация запроса против окна слота.
//
//   1) клиент ЯВНО попросил окно больше окна слота  → RELOAD  (нужна перезагрузка)
//   2) prompt + n_predict + 1 > окна слота          → TOO_LONG (не влезает)
//   3) иначе                                        → OK
//
// Ноль/отрицательное окно означает «неизвестно» → OK (решение принимает вызывающий).
int bridge_window_fit_kind(int actual_tokens, int n_predict,
                           int n_ctx_total, int n_ctx_seq, int n_ctx_override);

// bridge_window_suggest_batch — безопасный размер батча для этого окна.
//
// Причина отдельной функции: одна из трёх названных в ошибке причин — «n_batch >
// n_ctx». Батч больше окна слота не может быть обработан в принципе, поэтому
// его надо урезать ДО декода, а не сообщать об этом после отказа.
int bridge_window_suggest_batch(int n_batch, int n_ctx_total, int n_ctx_seq);

// bridge_decode_failure_message — текст отказа декода с ФАКТАМИ, а не догадками.
//
// Пишет в buf: промпт, n_predict, n_batch, суммарное окно и окно слота, и —
// если по числам запрос влезает — прямо говорит, что причина не в окне, а в
// памяти/раскладке, и что уменьшить. Возвращает длину записанного текста.
int bridge_decode_failure_message(char* buf, size_t buf_size,
                                  int actual_tokens, int n_predict, int n_batch,
                                  int n_ctx_total, int n_ctx_seq);

#endif // BRIDGE_WINDOW_FIT_H
