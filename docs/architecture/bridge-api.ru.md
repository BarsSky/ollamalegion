# Справочник по API C-bridge

Полный справочник по C API для `c/bridge/`. Используется cppworker через cgo.

> **Источник истины**: см. [c/bridge/bridge.h](../../c/bridge/bridge.h) — этот
> документ может отставать. При расхождении правым считается bridge.h.

## Оглавление

1. [Жизненный цикл](#жизненный-цикл)
2. [Загрузка модели](#загрузка-модели)
3. [Инференс](#инференс)
4. [Потоковый инференс](#потоковый-инференс)
5. [Батчевый decode (multi-slot)](#батчевый-decode-multi-slot)
6. [Эмбеддинги](#эмбеддинги)
7. [Токенизация](#токенизация)
8. [Chat-шаблон](#chat-шаблон)
9. [Abort API](#abort-api-round-31-6) *(Round 31 #6)*
10. [Обработка ошибок](#обработка-ошибок)
11. [Метаданные](#метаданные)
12. [Информация о GPU](#информация-о-gpu)

---

## Жизненный цикл

### `void bridge_init(void)`

Инициализирует backend llama.cpp. Вызывается один раз при старте cppworker,
до любой другой функции bridge. Безопасно вызывать несколько раз
(идемпотентно).

```c
bridge_init();
```

---

## Загрузка модели

### `ModelHandle bridge_load_model(const ModelConfig* config, char** error_msg)`

Загружает GGUF-модель с диска в память.

**Параметры**:
- `config` — см. [ModelConfig](#modelconfig-параметры-загрузки) ниже
- `error_msg` — выходной параметр, строка ошибки, выделенная в C (освобождает вызывающая сторона через `bridge_free_string`)

**Возвращает**: непрозрачный `ModelHandle` (NULL при ошибке, `*error_msg` установлен).

```c
ModelConfig cfg = {0};
cfg.model_path = "/app/models/qwen3-q4km.gguf";
cfg.n_ctx = 8192;
cfg.n_batch = 512;
cfg.n_gpu_layers = 999;  // all on GPU

char* err = NULL;
ModelHandle model = bridge_load_model(&cfg, &err);
if (!model) {
    fprintf(stderr, "load failed: %s\n", err);
    bridge_free_string(err);
    return 1;
}
```

### `void bridge_free_model(ModelHandle model)`

Выгружает модель и освобождает все связанные ресурсы. После этого вызова
`model` невалиден. Безопасно вызывать с NULL (no-op).

---

## Инференс

### `InferenceResult bridge_infer(ModelHandle model, const char* prompt, const GenerationParams* params)`

Синхронный инференс. Блокируется до завершения генерации или до abort.

**Возвращает**: `InferenceResult.output` (C-строка, освобождается через
`bridge_free_inference_result`), `status: 0` = успех, `1` = ошибка,
**или отрицательное значение** = специальный код (см.
[Error codes](#коды-ошибок)).

**Специальные значения**:
- `BRIDGE_ERR_ABORTED` (-100) — был запрошен abort, генерация отменена
- `BRIDGE_ERR_N_CTX_NEEDS_RELOAD` (2) — n_ctx слишком мал
- `BRIDGE_ERR_PROMPT_TOO_LONG` (3) — prompt+n_predict > n_ctx
- `BRIDGE_ERR_GPU_OOM` (4) — недостаточно VRAM

---

## Потоковый инференс

### `int bridge_infer_stream(ModelHandle model, const char* prompt, const GenerationParams* params, StreamCallback callback, void* user_data)`

Потоковый инференс. Вызывает `callback(token, token_len, user_data)` для
каждого сгенерированного токена.

**Возврат callback**: `1` = продолжить, `0` = остановить. Замечание: в
некоторых ветках кода callback вызывается *до* проверки abort — см.
[Abort semantics](#семантика-abort).

**Возвращает**: те же коды, что и `bridge_infer`. Поток может выдать
частичный вывод до возврата `BRIDGE_ERR_ABORTED`.

### `StreamCallback` typedef

```c
typedef int (*StreamCallback)(const char* token, int token_len, void* user_data);
```

---

## Батчевый decode (multi-slot)

(Используется `BatchedScheduler` для параллельной генерации по слотам.)

### `int bridge_batched_decode(...)`

Батчевый инференс по нескольким слотам. Семантику seq_id см. в
[Round 13 changelog](../../CHANGELOG.md). Каждый слот можно независимо
прервать через `bridge_request_abort` (в v1 отмена действует на все слоты
модели).

---

## Эмбеддинги

### `InferenceResult bridge_get_embeddings(ModelHandle model, const char* text)`

Генерирует вектор эмбеддингов для входного текста. Вывод — строковое
представление в JSON-подобном виде; при необходимости вызывающая сторона
парсит его в массив float.

---

## Токенизация

### `int32_t bridge_count_tokens(ModelHandle model, const char* text)`

Возвращает число токенов для текста или -1 при ошибке.

### `int32_t bridge_tokenize(ModelHandle model, const char* text, int32_t* out_buf, int32_t out_buf_size)`

Токенизирует текст в int32-буфер, выделенный вызывающей стороной. Возвращает
число записанных элементов (>= 0) или -1/-2 при ошибке. Если возврат == out_buf_size,
повторите с буфером большего размера.

### `int32_t bridge_token_to_piece(ModelHandle model, int32_t token, char* out_buf, int32_t buf_size)`

Преобразует одиночный токен в байты UTF-8. Возвращает число записанных байт
или -1/-2 при ошибке.

---

## Chat-шаблон

### `int32_t bridge_apply_chat_template(...)`

Применяет `tokenizer.chat_template` из метаданных GGUF. Возвращает число
байт, записанных в `out_buf`, -1 при ошибке, **-2, если шаблона нет в GGUF**.

(Детали параметров см. в [bridge.h](../../c/bridge/bridge.h))

---

## Abort API *(Round 31 #6)*

> Добавлено 2026-08-09. См. [docs/plans/cppworker-abort-api/PLAN.md](../plans/cppworker-abort-api/PLAN.md)
> для полного изложения обоснования дизайна.

### `int bridge_request_abort(ModelHandle model)`

Кооперативная отмена текущего/следующего инференса на этой модели.

**Семантика**:
- Устанавливает `atomic_int abort_requested = 1` в `InternalModel`.
- Проверяется между батчами `llama_decode` в `bridge_infer` /
  `bridge_infer_stream` / `bridge_batched_decode`.
- **Модель НЕ выгружается** — остаётся в памяти для следующего вызова.
- Потокобезопасно (`atomic_store` с `memory_order_relaxed`).
- Идемпотентно (несколько вызовов дают тот же эффект, что и один).

**Возвращает**: `0` при успехе, `-1` если `model == NULL`.

**Задержка отмены**: <100ms (время одного батча). Для длинных промптов может
потребоваться до одного полного батчевого decode (~1-2s на CUDA, дольше на CPU).

### `void bridge_request_abort_all(void)`

**РЕШЕНИЕ Q1 (2026-08-09)**: No-op в C. При завершении cppworker перебирает
свой реестр на стороне Go (`Backend.models`) и вызывает `bridge_request_abort`
для каждой модели. C-функция существует только для полноты API.

### `bool bridge_is_aborted(ModelHandle model)`

Диагностическая функция. Возвращает `true`, если abort был запрошен (и ещё не
поглощён сбросом флага при следующем инференсе), иначе `false`. Для NULL
возвращает `false`.

### Семантика abort

1. Вызывается `bridge_request_abort` (atomic store).
2. Следующий `llama_decode` в текущем/следующем инференсе проверяет флаг.
3. Если установлен: освобождаются текущий батч, сэмплер, выходной буфер; возвращается `BRIDGE_ERR_ABORTED`.
4. Флаг сбрасывается в 0 в **начале** каждого нового вызова `bridge_infer[_stream]`.
   То есть после abort повторный вызов infer работает без ручной очистки.

### Wire-протокол (ответы с отменой)

Когда `BRIDGE_ERR_ABORTED` доходит до Go:

- **Ollama API**: `done_reason: "cancelled"` + `cancelled: true`
- **OpenAI API**: `finish_reason: "stop"` + `cancelled: true` (обратная совместимость)

Это **per-request**: если несколько запросов в полёте на одной модели
разделяют один сигнал отмены, все они получают `BRIDGE_ERR_ABORTED`
(ограничение v1). Отмена по отдельным слотам отложена (PLAN.md Q5).

---

## Обработка ошибок

### Коды ошибок

| Код | Имя | Описание |
|------|------|-------------|
| 0 | `BRIDGE_OK` | Успех |
| 1 | `BRIDGE_ERR_GENERIC` | Неструктурированная ошибка (см. `bridge_last_error()`) |
| 2 | `BRIDGE_ERR_N_CTX_NEEDS_RELOAD` | n_ctx_override > текущего n_ctx, рассмотрите перезагрузку |
| 3 | `BRIDGE_ERR_PROMPT_TOO_LONG` | prompt + n_predict > n_ctx, жёсткая ошибка |
| 4 | `BRIDGE_ERR_GPU_OOM` | Недостаточно VRAM |
| 5 | `BRIDGE_ERR_BAD_REQUEST` | Некорректные параметры (например, n_ctx <= 0) |
| **-100** | **`BRIDGE_ERR_ABORTED`** | **Кооперативная отмена (R31 #6)** |

### `BridgeErrorInfo` struct

```c
typedef struct {
    int  code;                  // one of BRIDGE_ERR_*
    int  current_n_ctx;
    int  required_n_ctx;
    int  actual_tokens;
    int  n_predict;
    int  n_ctx_override;
    int  max_vram_n_ctx;
    char message[768];
} BridgeErrorInfo;
```

### `const BridgeErrorInfo* bridge_get_last_error_info(void)`

Возвращает структурированную информацию о последней ошибке. Указатель ведёт
на статическую thread-local память; **не освобождать**. Действителен до
следующего вызова `bridge_infer` / `bridge_infer_stream` /
`bridge_load_model`.

### `const char* bridge_last_error(void)`

Возвращает последнюю ошибку в виде человекочитаемой C-строки. Устаревший API,
в новом коде предпочитайте `bridge_get_last_error_info()`.

---

## Метаданные

### `ModelMetadata bridge_get_model_metadata(ModelHandle model)`

Возвращает метаданные GGUF: архитектуру, число слоёв, размер словаря и т. д.

```c
typedef struct {
    char* description;      // GGUF metadata
    char* architecture;     // model arch name
    int context_length;
    int n_layers;
    int n_heads;
    int n_head_kv;         // GQA kv heads
    int n_embd_head_k;
    int n_embd_head_v;
    int n_embd;
    int n_vocab;
    uint64_t size_total;   // file size in bytes
} ModelMetadata;
```

После использования вызовите `bridge_free_model_metadata`.

---

## Информация о GPU

### `int bridge_get_gpu_count(void)`

Число доступных CUDA-устройств. Возвращает 0, если GPU нет / CUDA не
скомпилирована.

### `int bridge_get_gpu_info(int gpu_index, GPUDeviceInfo* info)`

Возвращает информацию о GPU по индексу. Заполняет `GPUDeviceInfo`
(vram_total/free, name, compute capability). Возвращает 0 при успехе, -1 при
ошибке.

---

## `ModelConfig` (параметры загрузки)

> См. [c/bridge/bridge.h](../../c/bridge/bridge.h) для полной структуры. Ключевые поля:

| Поле | Описание | По умолчанию |
|-------|-------------|---------|
| `model_path` | Путь к GGUF-файлу | обязательно |
| `n_ctx` | Размер контекста | 4096 |
| `n_batch` | Размер батча | 512 |
| `n_ubatch` | Размер микро-батча | n_batch |
| `n_threads` | Потоки CPU | #cores |
| `n_gpu_layers` | Слоёв на GPU (-1 = все) | 0 (CPU) |
| `n_parallel` | KV-кэш под несколько слотов | 1 (single-slot) |
| `kv_cache_type` | 0=f16, 8=q8_0, 2=q4_0 | 0 (F16) |
| `split_mode` | 0=NONE, 1=LAYER, 3=TENSOR | -1 (по умолчанию LAYER) |
| `override_tensor_patterns` | Переопределение GPU/CPU по тензорам | NULL |
| `vocab_only` | Загружать только словарь (без весов) | 0 |
| `use_mmap` | Отображение файла модели в память (mmap) | 1 |
| `use_mlock` | Блокировка в RAM (без swap) | 0 |
| `flash_attn_type` | -1=auto, 0=off, 1=on | -1 |
| `tensor_split` | Пропорции для нескольких GPU | NULL (авто) |
| `main_gpu` | Индекс основного GPU | 0 |

---

## `GenerationParams` (параметры инференса)

| Поле | Описание | По умолчанию |
|-------|-------------|---------|
| `n_predict` | Макс. число токенов (-1 = без лимита) | -1 |
| `n_keep` | Сколько токенов сохранить из исходного промпта | 0 |
| `temperature` | Температура сэмплирования | 0.8 |
| `top_p` | Nucleus sampling | 0.95 |
| `top_k` | Top-K sampling | 40 |
| `repeat_penalty` | Штраф за повторы | 1.1 |
| `frequency_penalty` | OpenAI frequency penalty | 0.0 |
| `presence_penalty` | OpenAI presence penalty | 0.0 |
| `seed` | Сид RNG (-1 = случайный) | -1 |
| `antiprompts` | NULL-терминированный массив C-строк | NULL |
| `n_ctx_override` | n_ctx на один запрос (0 = наследовать) | 0 |
| `seq_id` | ID слота (0 = legacy single-slot) | 0 |
| `token_timings` | Замер задержек по токенам | 0 |

---

## Потокобезопасность

- `bridge_init` — идемпотентна, безопасно вызывать многократно.
- `bridge_request_abort` / `bridge_is_aborted` — потокобезопасны (атомарные операции).
- `bridge_load_model` — **НЕ потокобезопасна**. Вызывайте последовательно при старте.
- `bridge_infer` / `bridge_infer_stream` — **НЕ потокобезопасны для одной модели**.
  Одну модель в один момент времени может использовать только один инференс. Для
  параллельной генерации по слотам используйте `bridge_batched_decode` с `seq_id`.
- `bridge_get_last_error_info` — thread-local, безопасно вызывать конкурентно.

---

## История изменений

- **v0.5.16 (2026-08-09)** — Abort API (R31 #6): `bridge_request_abort`,
  `bridge_request_abort_all`, `bridge_is_aborted`, `BRIDGE_ERR_ABORTED=-100`.
- **Round 25-31** — batched_decode, kv_cache_type, override-tensors,
  улучшения chat-шаблонов.
- **Round 13** — multi-slot через `seq_id` и `bridge_batched_decode`.

См. [CHANGELOG.md](../../CHANGELOG.md) для полной истории.
