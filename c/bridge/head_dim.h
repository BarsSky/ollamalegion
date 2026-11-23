// head_dim.h — R705 (2026-11-24): какая ширина головы K/V публикуется и откуда.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Решение «взять <arch>.attention.key_length или посчитать
// n_embd/n_heads» — единственное место, где рождается число head_dim_k, которое
// уезжает в /api/models, оттуда в балансер (KV/токен, R659, AutoTune) и в WebUI.
// Пока оно жило внутри bridge_get_model_metadata (C-функция, требующая живой
// llama.cpp), проверить его можно было только загрузкой модели — и занижение
// (Qwen3-4B: 80 вместо 128) жило незамеченным. Теперь решение вынесено в чистую
// функцию и проверяется standalone-тестом c/bridge/tests/test_head_dim.c.
//
// См. c/bridge/head_dim.c о том, откуда берутся числа.
#pragma once

#include "bridge.h"

// bridge_head_dim — итог: ширина головы K/V и её источник.
typedef struct {
    int head_dim_k; // элементов в голове K; 0 = неизвестно
    int head_dim_v; // элементов в голове V; 0 = неизвестно
    int source;     // BRIDGE_HEAD_DIM_SOURCE_*
} bridge_head_dim;

// bridge_head_dim_resolve — решить, что публиковать.
//
// Входы (>= 0 — значение есть, <= 0 — неизвестно):
//   n_embd, n_heads      — из публичного llama_model_* (формула-fallback);
//   hparams_k, hparams_v — llama_hparams.n_embd_head_k_full / _v_full, то есть
//                          ровно то, чем llama.cpp считает KV-cache;
//   meta_key_length,     — <arch>.attention.key_length / value_length из
//   meta_value_length      GGUF-метаданных загруженной модели (0 = ключа нет).
//
// ПРАВИЛА (совпадают с тем, как llama.cpp заполняет hparams,
// src/llama-model.cpp:1659-1663):
//   * ключ в файле есть        → берём hparams/ключ, источник GGUF (факт);
//   * ключа нет, hparams есть  → hparams содержит дефолт n_embd/n_head, и это
//                                ЧЕСТНО называется COMPUTED (прежняя формула);
//   * hparams нет             → формула n_embd/n_heads, источник COMPUTED;
//   * ни чисел, ни голов       → 0/0, источник UNKNOWN (публиковать нечего).
// value_length опционален: если он неизвестен, V считается равной K (так же
// поступает llama.cpp).
bridge_head_dim bridge_head_dim_resolve(int n_embd, int n_heads,
                                        int hparams_k, int hparams_v,
                                        int meta_key_length, int meta_value_length);
