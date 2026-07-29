#ifndef OLLAMALEGION_BRIDGE_H
#define OLLAMALEGION_BRIDGE_H

#include <stdint.h>
#include <stdbool.h>

#ifdef __cplusplus
extern "C" {
#endif

// ============================================================
// Типы данных для передачи между Go и C
// ============================================================

// ModelHandle — opaque указатель на загруженную модель
typedef void* ModelHandle;

// GPUDeviceInfo — информация о GPU устройстве
typedef struct {
    int index;
    uint64_t vram_total_mb;
    uint64_t vram_free_mb;
    char name[256];
    int compute_capability_major;
    int compute_capability_minor;
} GPUDeviceInfo;

// InferenceResult — результат инференса
typedef struct {
    char* output;        // аллоцированная C строка (будет освобождена bridge_free_string)
    int output_len;
    int status;          // 0 = успех, 1 = ошибка
    char* error_msg;     // сообщение об ошибке (если status != 0)
} InferenceResult;

// GenerationParams — параметры генерации.
//
// ПРИМЕЧАНИЕ: n_ctx_override — это per-request переопределение n_ctx,
// которое используется для pre-flight проверки. Реальный n_ctx контекста
// устанавливается при LoadModel (immutable в llama.cpp). Если n_ctx_override
// > текущего im->ctx_n_ctx, клиенту возвращается informative ошибка с
// предложением перезагрузить модель с большим n_ctx.
typedef struct {
    int n_predict;          // max tokens to generate (-1 = no limit)
    int n_keep;             // number of tokens to keep from initial prompt
    int n_draft;            // number of tokens to draft for speculative decoding
    int n_batch;            // batch size
    float temperature;
    float top_p;
    float top_k;
    float repeat_penalty;
    float frequency_penalty;
    float presence_penalty;
    int seed;               // -1 = random seed
    int token_timings;      // 0/1 — включить замер времени токенов
    // Antiprompts (стоп-последовательности) — набор C-строк, при появлении
    // которых в декодированной выдаче стрим завершается, и сами токены
    // не отправляются клиенту. Используется для gemma (<end_of_turn>,
    // <start_of_turn>user) и других chat-моделей, чьи внутренние EOS
    // токены llama_vocab_is_eog могут не срабатывать при naive-prompt.
    const char** antiprompts;     // массив C-строк (NULL-оконченный)
    int n_antiprompts;            // количество элементов (>= 0; 0 = выключено)
    // Per-request n_ctx override (0 = use effective n_ctx from loaded model).
    // Если задан > 0 и меньше im->ctx_n_ctx, используется для расчёта
    // доступной ёмкости (n_ctx_override - n_predict). Если > im->ctx_n_ctx
    // — возвращается ошибка «effective n_ctx too small for request, reload
    // model with larger n_ctx».
    int n_ctx_override;
    // Round 13 (2026-07-28): sequence id for multi-slot batched inference.
    // 0 = single-slot legacy (KV-cache cleared on every call via
    //     llama_memory_clear; all tokens use seq_id 0).
    // > 0 = use slot seq_id in llama_batch_get_one; clear only this slot's
    //       KV-cache via llama_memory_seq_rm(mem, seq_id, -1, -1).
    // Caller (Go slot manager) is responsible for assigning unique seq_ids
    // per concurrent slot (slot = 0..n_parallel-1).
    // В Round 13 НЕ активирует настоящий параллельный инференс (forward pass
    // всё равно сериализуется через inst.mu), но даёт state isolation
    // между concurrent calls без необходимости reset_inference_state.
    int seq_id;
} GenerationParams;

// GpuSplitConfig — конфигурация распределения по GPU
typedef struct {
    int num_gpus;
    float* tensor_split;    // массив float[num_gpus] с пропорциями
    int main_gpu;           // индекс главного GPU
    int n_gpu_layers;       // количество слоёв на GPU (-1 = все)
} GpuSplitConfig;

// ModelConfig — конфигурация загрузки модели
typedef struct {
    const char* model_path;        // путь к GGUF файлу
    int n_ctx;                      // размер контекста (default: 4096)
    int n_batch;                    // размер батча (default: 512)
    int n_threads;                  // количество потоков CPU (default: кол-во ядер)
    int n_threads_batch;            // количество потоков для батча
    int n_ubatch;                   // размер микро-батча
    int n_gpu_layers;               // количество слоёв на GPU (-1 = все)
    int main_gpu;                   // индекс главного GPU
    int flash_attn_type;            // llama_flash_attn_type: -1=auto, 0=disabled, 1=enabled
    int numa;                       // 0/1 — NUMA оптимизация
    float* tensor_split;            // пропорции для multi-GPU (NULL если авто)
    int tensor_split_len;           // длина массива tensor_split
    // Phase 8 P.4 (2026-07-11): split_mode enum from llama.h.
    //   0=LLAMA_SPLIT_MODE_NONE (single GPU)
    //   1=LLAMA_SPLIT_MODE_LAYER (default, pipeline parallel, stable)
    //   2=LLAMA_SPLIT_MODE_ROW (deprecated row split)
    //   3=LLAMA_SPLIT_MODE_TENSOR (experimental, requires NCCL + Flash Attn + dense model)
    // -1 = use llama.cpp default (LAYER).
    int split_mode;                 // -1 (default) / 0 / 1 / 2 / 3
    int vocab_only;                 // 0/1 — загрузить только словарь
    int use_mmap;                   // 0/1 — использовать mmap
    int use_mlock;                  // 0/1 — заблокировать память
    int rope_scaling_type;          // тип RoPE scaling (0=unspecified, 1=linear...)
    float rope_freq_base;           // базовая частота RoPE (default: 10000.0)
    float rope_freq_scale;          // масштаб частоты RoPE (default: 1.0)
    // Session 16 (2026-06-27): Parallel + KVCacheType.
    //
    // В актуальной llama.cpp (b4500+) поля n_parallel нет. Используем
    // llama_context_params::n_seq_max (uint32_t) — max number of sequences
    // (i.e. distinct states). 0 = дефолт (= 1, single-slot).
    // > 0 = multi-slot batched generation; требует больше VRAM
    // (KV-cache × parallel).
    int n_parallel;
    //
    // kv_cache_type в Go API: "f16" | "q8_0" | "q4_0" | "" (inherit default).
    // В C-bridge маппим в пару (type_k, type_v) = enum ggml_type из ggml.h:
    //   "f16"  → GGML_TYPE_F16  (1) — полная точность, ~2×VRAM vs Q8_0
    //   "q8_0" → GGML_TYPE_Q8_0 (8) — -50% VRAM, минимальная потеря качества
    //   "q4_0" → GGML_TYPE_Q4_0 (2) — -75% VRAM, заметная потеря на длинных контекстах
    // 0 = наследовать дефолт cppworker (F16).
    int kv_cache_type;
    // Round 7: override-tensors (per-tensor GPU/CPU redirection).
    //
    // Is used for MoE: keep attention on GPU, expert tensors in RAM.
    // Each entry = (regex-pattern, buft-name).
    // buft names resolved via ggml_backend_buft_name:
    //   "CPU"  -> ggml_backend_cpu_buffer_type()
    //   "CUDA0" / "CUDA1" -> ggml_backend_dev_buffer_type(dev_get(i))
    // --ot-style parsing (pattern=CPU) NOT done here.
    // Caller passes already-split arrays. NULL/0 = no override.
    const char** override_tensor_patterns;
    const char** override_tensor_buft_names;
    int         override_tensor_count;
} ModelConfig;

// ============================================================
// Функции bridge
// ============================================================

// Инициализация backend
void bridge_init(void);

// Получение количества GPU
int bridge_get_gpu_count(void);

// Получение информации о GPU
int bridge_get_gpu_info(int gpu_index, GPUDeviceInfo* info);

// Загрузка GGUF модели
ModelHandle bridge_load_model(const ModelConfig* config, char** error_msg);

// Выгрузка модели
void bridge_free_model(ModelHandle model);

// Инференс (синхронный)
InferenceResult bridge_infer(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params
);

// Стриминг инференс (через callback)
typedef int (*StreamCallback)(const char* token, int token_len, void* user_data);
int bridge_infer_stream(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params,
    StreamCallback callback,
    void* user_data
);

// Получение эмбеддингов
InferenceResult bridge_get_embeddings(
    ModelHandle model,
    const char* text
);

// Получение метаданных модели
typedef struct {
    char* description;      // GGUF metadata (general.description)
    char* architecture;     // model architecture name
    int context_length;
    int n_layers;
    int n_heads;
    int n_head_kv;        // GQA kv heads (public API in llama.h)
    int n_embd_head_k;    // K head dim (computed: n_embd / n_heads)
    int n_embd_head_v;    // V head dim (computed: n_embd / n_heads)
    int n_embd;
    int n_vocab;
    uint64_t size_total;    // размер файла в байтах
} ModelMetadata;
ModelMetadata bridge_get_model_metadata(ModelHandle model);

// Освобождение результата инференса
void bridge_free_inference_result(InferenceResult* result);

// Освобождение метаданных
void bridge_free_model_metadata(ModelMetadata* metadata);

// Освобождение строки (для error_msg и т.д.)
void bridge_free_string(char* str);

// Получение последней ошибки (legacy: только текст, не различает причины)
const char* bridge_last_error(void);

// ============================================================
// Коды ошибок (структурированный error-info API)
// ============================================================
// Возвращаются из bridge_infer / bridge_infer_stream и параллельно
// доступны через bridge_get_last_error_info().
#define BRIDGE_OK                       0
#define BRIDGE_ERR_GENERIC              1  // неструктурированная ошибка (см. last_error)
#define BRIDGE_ERR_N_CTX_NEEDS_RELOAD   2  // n_ctx_override > текущего n_ctx модели; возможен auto-reload бэкенда с большим n_ctx
#define BRIDGE_ERR_PROMPT_TOO_LONG      3  // prompt_tokens + n_predict > n_ctx, и n_ctx_override не помогает (hard error)
#define BRIDGE_ERR_GPU_OOM              4  // нехватка VRAM при попытке аллокации слоёв модели
#define BRIDGE_ERR_BAD_REQUEST          5  // некорректные параметры (например, n_ctx <= 0 в override)

// Структурированное описание последней ошибки. Заполняется C-кодом
// при любом не-OK-возврате из bridge_infer / bridge_infer_stream.
// Числовые поля (current_n_ctx, required_n_ctx, actual_tokens, n_predict,
// n_ctx_override, max_vram_n_ctx) заполняются для кодов 2 и 3; для
// остальных кодов они равны 0, а message содержит человекочитаемое описание.
typedef struct {
    int  code;                  // один из BRIDGE_ERR_* (см. выше)
    int  current_n_ctx;         // фактический n_ctx загруженной модели
    int  required_n_ctx;        // минимальный n_ctx, который нужен для запроса
    int  actual_tokens;         // размер prompt в токенах
    int  n_predict;             // запрошенное число генерируемых токенов
    int  n_ctx_override;        // значение n_ctx_override из params (0 если не задан)
    int  max_vram_n_ctx;        // оценочный максимум n_ctx для текущей VRAM (0 если неизвестно)
    char message[768];          // человекочитаемое описание ошибки
} BridgeErrorInfo;

// Возвращает указатель на статический потокобезопасный BridgeErrorInfo.
// Память не аллоцируется, копировать не нужно. Действительно до следующего
// вызова bridge_infer / bridge_infer_stream / bridge_load_model.
const BridgeErrorInfo* bridge_get_last_error_info(void);

// Версия llama.cpp
const char* bridge_version(void);

// ============================================================
// ============================================================
// Tokenization utilities
// ============================================================

// bridge_count_tokens
// Токенизирует строку с помощью загруженной модели и возвращает число токенов.
// Если токенизация не удалась — возвращает -1.
int32_t bridge_count_tokens(
    ModelHandle model,
    const char* text
);

// ============================================================
// Chat template — применяет tokenizer.chat_template из GGUF
// ============================================================

// bridge_apply_chat_template
// Достаёт tokenizer.chat_template из метаданных GGUF и применяет
// его к переданным сообщениям через llama_chat_apply_template().
//   system               — системный промпт (или NULL)
//   user_contents        — массив указателей на user-сообщения
//   n_user               — количество user-сообщений
//   assistant_contents   — массив указателей на assistant-сообщения (для multi-turn), или NULL
//   n_assistant          — количество assistant-сообщений
//   add_ass              — добавить ли токены начала assistant turn в конец
//   out_buf              — выходной буфер (заполняется null-terminated строкой)
//   out_buf_size         — размер выходного буфера
// Возвращает количество записанных байт (без \0), или -1 при ошибке,
// -2 если template в GGUF отсутствует.
int32_t bridge_apply_chat_template(
    ModelHandle model,
    const char* system,
    const char** user_contents,
    int32_t n_user,
    const char** assistant_contents,
    int32_t n_assistant,
    bool add_ass,
    char* out_buf,
    int32_t out_buf_size
);

// bridge_get_chat_template
// Возвращает raw chat template (tokenizer.chat_template или chat_template) из GGUF.
// out_buf заполняется null-terminated строкой, возвращает длину или -1.
int32_t bridge_get_chat_template(
    ModelHandle model,
    char* out_buf,
    int32_t out_buf_size
);

// ============================================================
// Round 14a (2026-07-28): native enable_thinking via common::chat
// ============================================================
//
// C-обёртка над common_chat_templates_apply (c/llama.cpp/common/chat.h:230),
// которая добавляет native enable_thinking параметр (отсутствует в
// low-level llama_chat_apply_template API, см. llama.h:1162).
//
// Round 11 (v0.4.9) использовал soft prompt injection для instruction-tuned
// моделей типа gemma-4-it. Round 14a добавляет NATIVE путь для моделей,
// которые в своём chat template поддерживают enable_thinking Jinja variable
// (Qwen3-thinking, DeepSeek-R1, GLM-Z1 и др.).
//
// C-структура для одного сообщения. Caller владеет массивом.
struct CBridgeChatMessage {
    const char* role;
    const char* content;
};
// CGo требует typedef для прямого доступа как `C.CBridgeChatMessage` в Go.
// Без typedef пришлось бы использовать `C.struct_CBridgeChatMessage` (verbose).
typedef struct CBridgeChatMessage CBridgeChatMessage;

// bridge_chat_templates_apply_with_thinking — C-обёртка над native API.
//
// Параметры:
//   model                    — загруженная модель (ModelHandle из bridge_load_model)
//   chat_template_override   — кастомный Jinja template (NULL = use GGUF default)
//   messages                 — массив {role, content} пар
//   n_messages               — количество сообщений
//   enable_thinking          — true = native thinking mode
//   add_generation_prompt    — true = добавить assistant turn tokens в конец
//   out_buf                  — выходной буфер (null-terminated)
//   out_buf_size             — размер выходного буфера
//   out_supports_thinking    — [OUT] true если template поддерживает thinking
//
// Возвращает:
//   >= 0  количество записанных байт (без \0)
//   -1    invalid args (model/messages/out_buf NULL, n_messages <= 0)
//   -2    (legacy: template not found, but common_chat_templates_init returns -3 now)
//   -3    common_chat_templates_init failed (no template in GGUF, invalid override)
//   -4    output buffer too small (caller может retry с большим out_buf)
int32_t bridge_chat_templates_apply_with_thinking(
    void* model,
    const char* chat_template_override,
    const struct CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking
);

// ============================================================
// Round 15.1 (2026-07-29): true batched parallel inference
// ============================================================
//
// Round 13 (v0.4.11) добавил per-seq_id support в llama_batch, но actual
// parallel inference всё равно сериализовался через inst.mu на Go-стороне
// (один forward pass за раз → batch содержит токены одной sequence).
//
// Round 15.1 (по дизайн-доку docs/plans/round-15-batched-parallel.md):
// 1. C-bridge `build_batched_batch` (этот шаг) — собирает ОДИН llama_batch
//    из N sequences (каждая со своим tokens, n_tokens, seq_id, start_pos).
//    Все sequences обрабатываются в ОДНОМ llama_decode call → real parallel.
// 2. Go BatchedScheduler (шаг 3.2) — собирает токены от N горутин,
//    флашит в один batch при заполнении/таймауте.
// 3. WebUI toggle + cppworker integration (шаг 3.3) — opt-in через config.
//
// Шаг 3.1 (C-bridge only) не меняет behavior существующих вызовов
// bridge_infer / bridge_infer_stream — это НОВЫЙ API для batched path.
// Round 13 multi-slot (inst.mu + per-seq_id) остаётся default, opt-in
// через `EnableBatchedParallel` в cppworker-defaults.json.

// CBridgeBatchedSeq — описание одной sequence для batched parallel inference.
// Caller владеет массивом tokens (НЕ копируется — только borrows).
// Аналогично build_batch_with_seq, но для N sequences в одном batch.
struct CBridgeBatchedSeq {
    const llama_token* tokens;    // массив токенов (входной, не копируется)
    int32_t n_tokens;             // число токенов в этой sequence (>= 0)
    llama_seq_id seq_id;          // уникальный seq_id для этой sequence
    llama_pos start_pos;          // начальная позиция (для prompt = 0)
};
// CGo требует typedef для доступа как C.CBridgeBatchedSeq в Go.
typedef struct CBridgeBatchedSeq CBridgeBatchedSeq;

// build_batched_batch — создаёт ОДИН llama_batch для N sequences.
//
// Каждый токен получает seq_id своей sequence (для KV-cache routing).
// В каждой sequence последний токен получает logits=1 (для sampling
// в следующей итерации), промежуточные — logits=0.
//
// Параметры:
//   sequences   — массив структур CBridgeBatchedSeq (входной, не копируется)
//   n_sequences — число sequences (>= 0; 0 = no-op batch)
//
// Возвращаемый batch должен быть освобождён через llama_batch_free().
//
// Контракт (n_sequences > 0, все sequences[i].n_tokens > 0):
//   - batch.n_tokens == sum(sequences[i].n_tokens)
//   - batch.token[offset + j] == sequences[i].tokens[j] для j ∈ [0, sequences[i].n_tokens)
//   - batch.seq_id[offset + j][0] == sequences[i].seq_id
//   - batch.pos[offset + j] == sequences[i].start_pos + j
//   - batch.logits[offset + sequences[i].n_tokens - 1] == 1
//   - batch.logits[другие] == 0
//
// Ограничения:
//   - Все sequences должны иметь разные seq_id (caller отвечает).
//   - n_sequences * n_seq_max не должно превышать n_seq_max контекста.
//
// Это аналог llama-cli's examples/parallel/parallel.cpp:254-490
// (common_batch_add с явным seq_id для каждого токена).
struct llama_batch build_batched_batch(
    const struct CBridgeBatchedSeq* sequences, int32_t n_sequences
);

#ifdef __cplusplus
}
#endif

#endif // OLLAMALEGION_BRIDGE_H
