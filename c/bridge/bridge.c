//go:build !llama_stub

// ============================================================
// bridge.c — C-обёртка над llama.cpp для Go-вызова (CGo)
// Версия: 0.2.0 — реальный инференс через llama.cpp
// ============================================================

// При сборке с тегом llama_stub (флаг -DGO_BRIDGE_LLAMA_STUB,
// который выставляется cgo-директивой в c/bridge/bridge_stub.go) весь
// c-код в этом файле вырезается. Это позволяет собирать cppworker (и
// любой пакет, импортирующий ollama-loadbalancer/c/bridge) как stub
// без реального llama.cpp и без CGO-блокера "C source files not
// allowed when not using cgo or SWIG". Символы для Go предоставляет
// c/bridge/bridge_stub.go (с тегом //go:build llama_stub).
#ifndef GO_BRIDGE_LLAMA_STUB

#include "bridge.h"
#include "llama.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// Отключаем буферизацию stdout для Docker-логов — все printf выводятся немедленно.
// Без этого [bridge] сообщения могут не появляться в `docker compose logs`
// из-за line-buffering в контейнере.
__attribute__((constructor)) static void disable_stdout_buffering(void) {
    setbuf(stdout, NULL);
    setbuf(stderr, NULL);
}

// ============================================================
// Внутренние структуры
// ============================================================

// InternalModel — внутреннее представление загруженной модели.
// ctx_n_ctx хранит размер контекста, с которым модель фактически загружена.
// Это нужно для pre-flight проверки ёмкости перед llama_decode, чтобы
// вернуть внятную ошибку «prompt too long» вместо загадочного
// «llama_decode failed for prompt batch».
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    struct llama_sampler *sampler;
    struct llama_vocab *vocab;
    uint32_t ctx_n_ctx;       // n_ctx, с которым создан context (из llama_context_params)
    uint32_t ctx_n_batch;     // n_batch, с которым создан context
} InternalModel;

// ============================================================
// Внутреннее состояние
// ============================================================

static char last_error[1024] = {0};
static int initialized = 0;

// ============================================================
// Вспомогательные функции
// ============================================================

static void set_error(const char* msg) {
    strncpy(last_error, msg, sizeof(last_error) - 1);
    last_error[sizeof(last_error) - 1] = '\0';
}

// reset_inference_state — сбрасывает KV-cache и состояние сэмплера перед новым
// инференсом. Без этого prompt-токены и сгенерированные токены предыдущего
// запроса остаются в KV-cache, и через 1-2 запроса контекст переполняется
// с ошибкой "failed to find a memory slot". Также сбрасываем сэмплер, чтобы
// repetition_penalty / top_p / т.п. начинались с чистого состояния.
//
// Round 13 (2026-07-28): seq_id — параметр, определяющий область очистки.
//   seq_id == 0  — legacy single-slot: clear ВСЕ KV-cache (llama_memory_clear).
//                  Используется когда n_parallel=1 (default).
//   seq_id >  0  — multi-slot: clear только регион этого seq_id
//                  (llama_memory_seq_rm(mem, seq_id, -1, -1)).
//                  Другие concurrent calls на других seq_ids не затрагиваются.
//
// Примечание: в актуальной llama.cpp (b4500+, коммит 4414c04b9) функция
// llama_kv_self_clear УДАЛЕНА. Правильный путь: получить llama_memory_t
// через llama_get_memory() и вызвать llama_memory_clear(mem, data=true).
// Это эквивалентно «очистить все кэшированные KV» независимо от backend
// (Recurrent / AttentionBased).
static void reset_inference_state(InternalModel *im, llama_seq_id seq_id) {
    if (im == NULL) return;
    if (im->context != NULL) {
        llama_memory_t mem = llama_get_memory(im->context);
        if (mem != NULL) {
            if (seq_id == 0) {
                // Legacy single-slot: clear all KV-cache.
                llama_memory_clear(mem, true);
            } else {
                // Multi-slot: clear only this slot's KV-cache region.
                // Другие concurrent calls (другие seq_id) не затрагиваются.
                llama_memory_seq_rm(mem, seq_id, -1, -1);
            }
        }
    }
    if (im->sampler != NULL) {
        // Sampler один на context (Round 13 не делает per-slot sampler state).
        // reset безопасен — sampler state не переносится между seq_id.
        llama_sampler_reset(im->sampler);
    }
}

// build_batch_with_seq — создаёт llama_batch с явным seq_id для каждого токена.
// Round 13: заменяет llama_batch_get_one в bridge_infer / bridge_infer_stream
// чтобы корректно работать с multi-slot KV-cache.
//
// Использует llama_batch_init(n_tokens, 0, 1) — каждый токен может быть
// назначен максимум 1 sequence id (соответствует common_batch_add из
// examples/parallel/parallel.cpp, line 254).
//
// Caller ОБЯЗАН вызвать llama_batch_free(batch) после использования.
//
// Параметры:
//   tokens    — массив токенов (входной, не копируется)
//   n_tokens  — число токенов (>= 1)
//   seq_id    — llama_seq_id, который назначается каждому токену
//   start_pos — начальная позиция (для prompt: 0; для gen step i: prompt_len + i)
//
// В batch последний токен получает logits=1 (для sampling в следующей итерации),
// остальные logits=0 (промежуточные токены).
static struct llama_batch build_batch_with_seq(
    llama_token* tokens, int32_t n_tokens, llama_seq_id seq_id, llama_pos start_pos
) {
    // GGML_ASSERT в llama_batch_init требует n_tokens > 0.
    if (n_tokens <= 0) {
        // Возвращаем пустой batch (n_tokens=0). Безопасно для llama_decode (no-op).
        return llama_batch_init(1, 0, 1);
    }
    struct llama_batch batch = llama_batch_init(n_tokens, 0, 1);
    for (int32_t i = 0; i < n_tokens; i++) {
        batch.token[i]    = tokens[i];
        batch.pos[i]      = (llama_pos)(start_pos + i);
        batch.n_seq_id[i] = 1;
        // batch.seq_id[i] — массив llama_seq_id размера 1, pre-allocated by llama_batch_init
        batch.seq_id[i][0] = seq_id;
        // Last token in batch has logits=1 (нужно для llama_sampler_sample).
        batch.logits[i]   = (i == n_tokens - 1) ? 1 : 0;
    }
    // CRITICAL FIX (2026-07-29): llama_batch_init() инициализирует batch.n_tokens = 0
    // (см. c/llama.cpp/src/llama-batch.cpp:879). Без явного n_tokens=... здесь
    // llama_decode() видит пустой batch и возвращает -1 с "decode: n_tokens == 0".
    // БАГ введён в Round 13 (v0.4.11) при замене llama_batch_get_one на batch_init
    // для multi-slot support. Round 9-10 inference работал потому что
    // llama_batch_get_one сама ставила n_tokens = n_tokens.
    batch.n_tokens = n_tokens;
    return batch;
}

// ============================================================
// Структурированная ошибка (last_error_info) + мьютекс
// ============================================================
//
// last_error_info обновляется атомарно в каждой точке, где раньше просто
// вызывался set_error(). В Go-стороне её можно забрать через
// bridge_get_last_error_info() сразу после того, как bridge_infer/
// bridge_infer_stream вернул ненулевой код. Мьютекс защищает и чтение,
// и запись, потому что несколько goroutine могут одновременно дёргать
// bridge (C-функции реентрантны только если это явно поддерживается
// на уровне llama.cpp — мы исходим из сериализации).
//
// Кросс-платформенный мьютекс:
//   - Windows  → CRITICAL_SECTION (из <windows.h>)
//   - Linux/macOS/BSD → pthread_mutex_t
// В Windows мы НЕ используем pthread (MinGW не имеет pthread.h из коробки),
// в Linux/Docker — НЕ используем CRITICAL_SECTION (нет <windows.h>).
#ifdef _WIN32
#include <windows.h>
static CRITICAL_SECTION g_err_mutex;
static int g_err_mutex_initialized = 0;
static void err_mutex_lock(void) {
    if (!g_err_mutex_initialized) {
        InitializeCriticalSection(&g_err_mutex);
        g_err_mutex_initialized = 1;
    }
    EnterCriticalSection(&g_err_mutex);
}
static void err_mutex_unlock(void) {
    LeaveCriticalSection(&g_err_mutex);
}
#else
#include <pthread.h>
static pthread_mutex_t g_err_mutex = PTHREAD_MUTEX_INITIALIZER;
static void err_mutex_lock(void)   { pthread_mutex_lock(&g_err_mutex);   }
static void err_mutex_unlock(void) { pthread_mutex_unlock(&g_err_mutex); }
#endif
static BridgeErrorInfo g_last_error_info = {
    .code = BRIDGE_OK,
    .message = ""
};
// Оценочный максимум n_ctx, доступный текущей VRAM. Заполняется из bridge_load_model,
// читается bridge_check_ctx_capacity для поля max_vram_n_ctx. 0 = unknown.
static int g_max_vram_n_ctx = 0;

static void set_error_info(int code, const char* msg,
                           int current_n_ctx, int required_n_ctx,
                           int actual_tokens, int n_predict,
                           int n_ctx_override, int max_vram_n_ctx) {
    err_mutex_lock();
    g_last_error_info.code = code;
    g_last_error_info.current_n_ctx = current_n_ctx;
    g_last_error_info.required_n_ctx = required_n_ctx;
    g_last_error_info.actual_tokens = actual_tokens;
    g_last_error_info.n_predict = n_predict;
    g_last_error_info.n_ctx_override = n_ctx_override;
    g_last_error_info.max_vram_n_ctx = max_vram_n_ctx;
    if (msg != NULL) {
        strncpy(g_last_error_info.message, msg, sizeof(g_last_error_info.message) - 1);
        g_last_error_info.message[sizeof(g_last_error_info.message) - 1] = '\0';
    } else {
        g_last_error_info.message[0] = '\0';
    }
    // Параллельно обновляем legacy last_error, чтобы старый API работал
    if (msg != NULL) {
        strncpy(last_error, msg, sizeof(last_error) - 1);
        last_error[sizeof(last_error) - 1] = '\0';
    }
    err_mutex_unlock();
}

static void set_error_info_generic(const char* msg) {
    set_error_info(BRIDGE_ERR_GENERIC, msg, 0, 0, 0, 0, 0, g_max_vram_n_ctx);
}

static void clear_error_info(void) {
    err_mutex_lock();
    g_last_error_info.code = BRIDGE_OK;
    g_last_error_info.current_n_ctx = 0;
    g_last_error_info.required_n_ctx = 0;
    g_last_error_info.actual_tokens = 0;
    g_last_error_info.n_predict = 0;
    g_last_error_info.n_ctx_override = 0;
    g_last_error_info.max_vram_n_ctx = g_max_vram_n_ctx;
    g_last_error_info.message[0] = '\0';
    last_error[0] = '\0';
    err_mutex_unlock();
}

const BridgeErrorInfo* bridge_get_last_error_info(void) {
    return &g_last_error_info;
}

// check_antiprompts — проверяет накопленный output против списка antiprompts.
// Возвращает индекс совпавшего antiprompt в params->antiprompts или -1.
// Сравнение по суффиксу: если декодированная выдача заканчивается на
// antiprompt-строку, считаем что модель сгенерировала стоп-маркер
// (например, <end_of_turn> для gemma) и нужно прекращать генерацию.
static int check_antiprompts(const char* output, size_t output_len, const GenerationParams* params) {
    if (params == NULL || params->antiprompts == NULL || params->n_antiprompts <= 0 || output == NULL) {
        return -1;
    }
    for (int i = 0; i < params->n_antiprompts; i++) {
        const char* ap = params->antiprompts[i];
        if (ap == NULL) continue;
        size_t ap_len = strlen(ap);
        if (ap_len == 0 || ap_len > output_len) continue;
        if (strncmp(output + output_len - ap_len, ap, ap_len) == 0) {
            return i;
        }
    }
    return -1;
}

// trim_antiprompt_suffix — обрезает с конца output хвостовой antiprompt,
// чтобы пользователь его не увидел. Возвращает новую длину output.
static size_t trim_antiprompt_suffix(char* output, size_t output_len, int ap_index, const GenerationParams* params) {
    if (params == NULL || params->antiprompts == NULL || ap_index < 0 || ap_index >= params->n_antiprompts) {
        return output_len;
    }
    const char* ap = params->antiprompts[ap_index];
    if (ap == NULL) return output_len;
    size_t ap_len = strlen(ap);
    if (ap_len == 0 || ap_len > output_len) return output_len;
    output[output_len - ap_len] = '\0';
    return output_len - ap_len;
}

// bridge_check_ctx_capacity — pre-flight проверка, что prompt + n_predict
// помещаются в загруженный контекст (im->ctx_n_ctx). Без неё клиент получал
// загадочную ошибку «llama_decode failed for prompt batch» с неясным
// диагнозом. Теперь возвращаем внятное сообщение с указанием лимита,
// текущих значений и рекомендациями (reduce prompt, increase n_ctx, reduce
// n_predict). Учитывает n_ctx_override из params (если > 0):
//   - n_ctx_override <= im->ctx_n_ctx  → ёмкость = n_ctx_override
//   - n_ctx_override >  im->ctx_n_ctx  → код BRIDGE_ERR_N_CTX_NEEDS_RELOAD
//                                        (бэкенд может auto-reload с большим n_ctx)
//   - prompt+n_predict > n_ctx_override → код BRIDGE_ERR_PROMPT_TOO_LONG (hard error)
// Возвращает BRIDGE_OK (0) если ОК, иначе один из BRIDGE_ERR_* кодов.
static int bridge_check_ctx_capacity(InternalModel *im, const GenerationParams* params, int actual_tokens, int n_predict) {
    if (im == NULL || im->ctx_n_ctx == 0) return BRIDGE_OK;
    uint32_t effective_n_ctx = im->ctx_n_ctx;
    int n_ctx_override = (params != NULL ? params->n_ctx_override : 0);

    if (n_ctx_override > 0) {
        if ((uint32_t)n_ctx_override > im->ctx_n_ctx) {
            // Клиент запрашивает контекст больше, чем загружено — нужен reload.
            int required = n_ctx_override;
            char buf[640];
            snprintf(buf, sizeof(buf),
                "requested n_ctx=%d exceeds model's effective n_ctx=%u. "
                "Auto-reload may be possible if VRAM allows (max_vram_n_ctx=%d). "
                "Otherwise save a model profile with n_ctx=%d and reload the model "
                "(or send a smaller n_ctx in options.num_ctx)",
                n_ctx_override, im->ctx_n_ctx, g_max_vram_n_ctx, n_ctx_override);
            set_error_info(BRIDGE_ERR_N_CTX_NEEDS_RELOAD, buf,
                           (int)im->ctx_n_ctx, required,
                           actual_tokens, n_predict, n_ctx_override, g_max_vram_n_ctx);
            return BRIDGE_ERR_N_CTX_NEEDS_RELOAD;
        }
        effective_n_ctx = (uint32_t)n_ctx_override;
    }
    int64_t total = (int64_t)actual_tokens + (int64_t)n_predict + 1;
    if (total > (int64_t)effective_n_ctx) {
        int required = actual_tokens + n_predict + 1;
        char buf[640];
        snprintf(buf, sizeof(buf),
            "prompt too long for n_ctx: prompt_tokens=%d + n_predict=%d + 1 = %lld > n_ctx=%u "
            "(model loaded with n_ctx=%u, request asked for n_ctx=%d). "
            "Reduce prompt, set smaller max_tokens, or save a model profile with bigger n_ctx "
            "and reload the model",
            actual_tokens, n_predict, (long long)total, effective_n_ctx,
            im->ctx_n_ctx, n_ctx_override);
        set_error_info(BRIDGE_ERR_PROMPT_TOO_LONG, buf,
                       (int)im->ctx_n_ctx, required,
                       actual_tokens, n_predict, n_ctx_override, g_max_vram_n_ctx);
        return BRIDGE_ERR_PROMPT_TOO_LONG;
    }
    return BRIDGE_OK;
}

// ============================================================
// Инициализация
// ============================================================

void bridge_init(void) {
    initialized = 1;
    printf("[bridge] initialized (v0.2.0 — real llama.cpp)\n");
}

// ============================================================
// GPU Discovery — через CUDA Runtime API
// ============================================================

#ifdef GGML_USE_CUDA
#include <cuda_runtime.h>
#include <cuda.h>
#endif

int bridge_get_gpu_count(void) {
#ifdef GGML_USE_CUDA
    int count = 0;
    cudaError_t err = cudaGetDeviceCount(&count);
    if (err != cudaSuccess) {
        char buf[256];
        snprintf(buf, sizeof(buf), "cudaGetDeviceCount failed: %s", cudaGetErrorString(err));
        set_error(buf);
        return 0;
    }
    return count;
#else
    return 0;
#endif
}

int bridge_get_gpu_info(int gpu_index, GPUDeviceInfo* info) {
    if (info == NULL) return -1;
    memset(info, 0, sizeof(GPUDeviceInfo));
    info->index = gpu_index;
#ifdef GGML_USE_CUDA
    struct cudaDeviceProp prop;
    cudaError_t err = cudaGetDeviceProperties(&prop, gpu_index);
    if (err != cudaSuccess) {
        char buf[256];
        snprintf(buf, sizeof(buf), "cudaGetDeviceProperties for GPU %d failed: %s", gpu_index, cudaGetErrorString(err));
        set_error(buf);
        return -1;
    }
    strncpy(info->name, prop.name, sizeof(info->name) - 1);
    info->name[sizeof(info->name) - 1] = '\0';
    info->vram_total_mb = (uint64_t)(prop.totalGlobalMem / (1024 * 1024));
    size_t free_mem = 0, total_mem = 0;
    CUresult cu_err = cuMemGetInfo(&free_mem, &total_mem);
    if (cu_err == CUDA_SUCCESS) {
        info->vram_free_mb = (uint64_t)(free_mem / (1024 * 1024));
    } else {
        info->vram_free_mb = info->vram_total_mb;
    }
    info->compute_capability_major = prop.major;
    info->compute_capability_minor = prop.minor;
    return 0;
#else
    set_error("GPU discovery requires CUDA build (GGML_USE_CUDA not defined)");
    return -1;
#endif
}

// ============================================================
// Загрузка модели — РЕАЛЬНАЯ через llama.cpp
// ============================================================


// Round 7: resolve_buft_by_name — resolves a buft name ("CPU" / "CUDA0" / "CUDA1" / ...)
// to a ggml_backend_buffer_type_t handle. Returns NULL for unknown names.
// The handle is cached per-name; entries with NULL handle are skipped at override-build time.

struct ggml_backend_buffer_type * resolve_buft_by_name(const char *name) {
    if (name == NULL || name[0] == 0) return NULL;
    if (strcmp(name, "CPU") == 0) {
        return ggml_backend_cpu_buffer_type();
    }
    if (strncmp(name, "CUDA", 4) == 0) {
        int idx = 0;
        if (name[4] >= '0' && name[4] <= '9') idx = atoi(name + 4);
        if (idx >= 0 && idx < (int)ggml_backend_dev_count()) {
            return ggml_backend_dev_buffer_type(ggml_backend_dev_get(idx));
        }
    }
    for (size_t i = 0; i < ggml_backend_dev_count(); ++i) {
        ggml_backend_dev_t dev = ggml_backend_dev_get(i);
        ggml_backend_buffer_type_t buft = ggml_backend_dev_buffer_type(dev);
        if (buft != NULL && ggml_backend_buft_name(buft) != NULL &&
            strcmp(ggml_backend_buft_name(buft), name) == 0) {
            return buft;
        }
    }
    return NULL;
}

// 

ModelHandle bridge_load_model(const ModelConfig* config, char** error_msg) {
    if (!initialized) {
        set_error("bridge not initialized");
        if (error_msg) *error_msg = strdup(last_error);
        return NULL;
    }

    if (config == NULL || config->model_path == NULL) {
        set_error("model_path is required");
        if (error_msg) *error_msg = strdup(last_error);
        return NULL;
    }

    printf("[bridge] loading model: %s (ctx=%d, batch=%d, threads=%d, gpu_layers=%d)\n",
           config->model_path, config->n_ctx, config->n_batch,
           config->n_threads, config->n_gpu_layers);

    // Параметры загрузки модели
    struct llama_model_params model_params = llama_model_default_params();
    model_params.n_gpu_layers = config->n_gpu_layers;
    if (config->main_gpu >= 0) {
        model_params.main_gpu = config->main_gpu;
    }
    model_params.use_mmap = config->use_mmap;
    model_params.use_mlock = config->use_mlock;
    model_params.use_mlock = config->use_mlock;

    // Phase 8 P.4 (2026-07-11): multi-GPU tensor_split wiring.
    //
    // config->tensor_split (if non-NULL) — массив float[tensor_split_len] с
    // пропорциями распределения model по GPU (e.g. [0.5, 0.5] для 2 GPU).
    // Используется llama.cpp при n_gpu_layers > 0 И multi-GPU: layer split
    // mode распределяет слои между GPU пропорционально значениям массива.
    //
    // Default mode (split_mode=LLAMA_SPLIT_MODE_LAYER, value=1) — стабильный
    // pipeline parallelism. Layer split — production-ready.
    // Tensor split (LLAMA_SPLIT_MODE_TENSOR, value=3) — experimental, requires
    // NCCL + Flash Attn + dense model (NE MoE). Post-1.0.
    //
    // Если split_mode не задан в config → используем default (LAYER).
    if (config->split_mode >= 0 && config->split_mode <= 3) {
        model_params.split_mode = (enum llama_split_mode)config->split_mode;
    }
    if (config->tensor_split != NULL && config->tensor_split_len > 0) {
        model_params.tensor_split = config->tensor_split;
        fprintf(stderr,
            "[bridge] multi-GPU tensor_split: applied %d proportions [",
            config->tensor_split_len);
        for (int i = 0; i < config->tensor_split_len; i++) {
            fprintf(stderr, "%.2f%s",
                config->tensor_split[i],
                (i < config->tensor_split_len - 1) ? ", " : "");
        }
        fprintf(stderr, "], split_mode=%d\n", (int)model_params.split_mode);
    }

    // Round 7: override-tensors. If override_tensor_count > 0 we build
    // a NULL-terminated array of llama_model_tensor_buft_override and
    // attach it to model_params.tensor_buft_overrides (read by
    // llama_model_load_from_file internally).
    static struct llama_model_tensor_buft_override *buft_overrides = NULL;
    static int buft_overrides_count = 0;
    if (config->override_tensor_count > 0 &&
        config->override_tensor_patterns != NULL &&
        config->override_tensor_buft_names != NULL) {
        int n = config->override_tensor_count;
        // +1 for NULL-terminator (llama.cpp API requires).
        buft_overrides = calloc((size_t)(n + 1),
            sizeof(struct llama_model_tensor_buft_override));
        if (buft_overrides != NULL) {
            int valid = 0;
            for (int i = 0; i < n; i++) {
                const char *pat = config->override_tensor_patterns[i];
                const char *buft_name = config->override_tensor_buft_names[i];
                if (pat == NULL || pat[0] == 0) continue;
                ggml_backend_buffer_type_t buft = resolve_buft_by_name(buft_name);
                if (buft == NULL) continue;
                buft_overrides[valid].pattern = pat;
                buft_overrides[valid].buft = buft;
                ++valid;
            }
            if (valid > 0) {
                buft_overrides[valid].pattern = NULL;
                buft_overrides[valid].buft = NULL;
                model_params.tensor_buft_overrides = buft_overrides;
                buft_overrides_count = valid;
                fprintf(stderr,
                    "[bridge] override-tensors: applied %d entries\n",
                    valid);
            } else {
                free(buft_overrides);
                buft_overrides = NULL;
            }
        }
    }


    // Загружаем модель
    struct llama_model *model = llama_model_load_from_file(config->model_path, model_params);
    if (model == NULL) {
        char buf[512];
        snprintf(buf, sizeof(buf), "failed to load model from %s", config->model_path);
        set_error(buf);
        if (error_msg) *error_msg = strdup(buf);
        return NULL;
    }

    // Параметры контекста
    struct llama_context_params ctx_params = llama_context_default_params();
    ctx_params.n_ctx = config->n_ctx > 0 ? (uint32_t)config->n_ctx : 4096;
    ctx_params.n_batch = config->n_batch > 0 ? (uint32_t)config->n_batch : 512;
    ctx_params.n_threads = config->n_threads > 0 ? config->n_threads : 0;
    ctx_params.n_threads_batch = config->n_threads_batch > 0 ? config->n_threads_batch : 0;
// В актуальной llama.cpp (коммит 4414c04b9) llama_context_params хранит
// `flash_attn_type` (enum llama_flash_attn_type: AUTO=-1, DISABLED=0, ENABLED=1),
// а не `flash_attn` (bool). Ранний «D.6 фикс» основывался на ещё более новой
// версии, где bool появится, но в b4500+ enum сохраняется. Прямо пробрасываем
// значение: config->flash_attn_type: -1=auto (default), 0=disabled, 1=enabled.
// llama_context_default_params() уже инициализирует flash_attn_type в AUTO;
// правим только если клиент задал явно (0 или 1).
    if (config->flash_attn_type == 0) {
        ctx_params.flash_attn_type = LLAMA_FLASH_ATTN_TYPE_DISABLED;
    } else if (config->flash_attn_type > 0) {
        ctx_params.flash_attn_type = LLAMA_FLASH_ATTN_TYPE_ENABLED;
    } // else (== -1) — оставляем AUTO из default_params
    ctx_params.rope_freq_base = config->rope_freq_base > 0 ? config->rope_freq_base : 0.0f;
    ctx_params.rope_freq_scale = config->rope_freq_scale > 0 ? config->rope_freq_scale : 0.0f;

    // ============================================================
    // Session 16 (2026-06-27): Parallel + KVCacheType.
    //
    // В актуальной llama.cpp (b4500+) llama_context_params::n_parallel нет;
    // есть n_seq_max (max number of sequences). Это и есть parallel slot
    // count. 0 (default в llama_context_default_params) означает 1.
    //
    // Применяем только если явно > 0 — иначе оставляем default (= 1).
    // Значения > 1 нужны для multi-slot batched generation
    // (например, OpenWebUI / Cline параллельные запросы к одной модели).
    // VRAM растёт линейно: KV-cache × n_seq_max.
    if (config->n_parallel > 0) {
        ctx_params.n_seq_max = (uint32_t)config->n_parallel;
        printf("[bridge] parallel sequences (n_seq_max): %d\n", ctx_params.n_seq_max);
    }

    // kv_cache_type — управление квантизацией KV-cache через (type_k, type_v).
    // Маппинг из Go (kvCacheTypeToBridgeInt):
    //   0 = default (наследуем из llama_context_default_params → F16/F16)
    //   1 = Q8_0 (-50% VRAM, минимальная потеря)
    //   2 = Q4_0 (-75% VRAM, заметная потеря на длинных контекстах)
    // [EXPERIMENTAL] в llama.cpp — может не работать на всех бэкендах.
    switch (config->kv_cache_type) {
        case 1:
            ctx_params.type_k = GGML_TYPE_Q8_0;
            ctx_params.type_v = GGML_TYPE_Q8_0;
            printf("[bridge] KV cache type: Q8_0 (-50%% VRAM)\n");
            break;
        case 2:
            ctx_params.type_k = GGML_TYPE_Q4_0;
            ctx_params.type_v = GGML_TYPE_Q4_0;
            printf("[bridge] KV cache type: Q4_0 (-75%% VRAM)\n");
            break;
        case 0:
        default:
            // default — F16/F16 из llama_context_default_params()
            break;
    }

    // Включаем поддержку эмбеддингов — без этого llama_get_embeddings() всегда
    // возвращает NULL, и bridge_get_embeddings падает с ошибкой.
    // Флаг embeddings включает запись логов в KV-cache для всех токенов prompt.
    ctx_params.embeddings = true;

    // Создаём контекст. В новой llama.cpp `llama_new_context_with_model` deprecated;
    // используем `llama_init_from_model`.
    struct llama_context *context = llama_init_from_model(model, ctx_params);
    if (context == NULL) {
        char buf[512];
        snprintf(buf, sizeof(buf), "failed to create context for %s", config->model_path);
        set_error(buf);
        llama_model_free(model);
        if (error_msg) *error_msg = strdup(buf);
        return NULL;
    }

    // Получаем vocabulary
    const struct llama_vocab *vocab = llama_model_get_vocab(model);

    // Создаём сэмплер (стандартная цепочка)
    struct llama_sampler *sampler = llama_sampler_chain_init(llama_sampler_chain_default_params());
    llama_sampler_chain_add(sampler, llama_sampler_init_greedy());

    // Выделяем память под InternalModel
    InternalModel *im = (InternalModel *)malloc(sizeof(InternalModel));
    if (im == NULL) {
        set_error("out of memory");
        llama_sampler_free(sampler);
        llama_free(context);
        llama_model_free(model);
        if (error_msg) *error_msg = strdup("out of memory");
        return NULL;
    }

    im->model = model;
    im->context = context;
    im->sampler = sampler;
    im->vocab = (struct llama_vocab *)vocab;
    // Сохраняем фактические параметры контекста — они нужны для pre-flight
    // проверки ёмкости (prompt + n_predict <= n_ctx) перед llama_decode.
    // Без этого клиент получал загадочную ошибку «llama_decode failed» без
    // указания на реальную причину (переполнение контекста).
    im->ctx_n_ctx = ctx_params.n_ctx;
    im->ctx_n_batch = ctx_params.n_batch;

    // ============================================================
    // Оценка максимального n_ctx, доступного текущей VRAM
    // ============================================================
    // Нужна для поля max_vram_n_ctx в BridgeErrorInfo — Go-сторона
    // использует её в decideNCtx() (см. internal/balancer/nctx_reload.go)
    // чтобы понять, можно ли auto-reload с большим n_ctx.
    //
    // Формула: free_vram_after_model = free_vram - (model_size * n_gpu_layers / n_layers)
    //   (грубо: занятая VRAM ≈ вес модели * доля GPU-слоёв)
    // max_n_ctx = free_vram_after_model / kv_cache_per_token
    //
    // KV Cache per token (f16):
    //   K  cache: n_layers * n_kv_heads * head_dim * 2 bytes
    //   V  cache: n_layers * n_kv_heads * head_dim * 2 bytes
    //   head_dim = n_embd / n_heads
    //   Total per token = 4 * n_layers * n_kv_heads * (n_embd / n_heads)
    //
    // Для MHA (n_kv_heads == n_heads): 4 * n_layers * n_embd (pre-GQA формула)
    // Для GQA (n_kv_heads < n_heads): значительно меньше — правильно учитываем GQA.
    // Пример: Llama 70B (n_layers=80, n_embd=8192, n_heads=64, n_kv_heads=8):
    //   Без GQA: 4*80*8192 = 2,621,440 bytes/token (ЗАВЫШЕНИЕ в 8 раз!)
    //   С  GQA:  4*80*8*128 =   327,680 bytes/token (корректно)
    // Если данных нет (CPU-only, не CUDA) — оставляем 0 (unknown).
    // Go-сторона может пересчитать max_vram_n_ctx самостоятельно через GGUF-парсер
    // и estimateKVCacheMB() (см. internal/cppbackend/backend.go).
    // "Грубую ошибку оставлять нельзя" — формула корректна для GQA моделей.
    // ============================================================
    // ВАЖНО: Clamp на ctx_params.n_ctx применяется ТОЛЬКО если
    // estimated_max > 0. Если estimated_max == 0 (модель не влезает
    // в VRAM полностью), clamp на 4096 скрывает дефицит и Go-сторона
    // думает «VRAM позволяет 4096 токенов» — но на самом деле
    // модель едва поместилась, KV-cache некуда выделять.
    //
    // Проблема на A10 (24GB) с Qwen 35B Q4_K_M (~24GB):
    //   free_for_kv ≈ 0 → estimated_max = 0 → старый clamp давал 4096
    //   → Go думает «max_vram_n_ctx=4096» → reject на любой num_ctx > 3481
    //   → пользователь видит "requested n_ctx=128000 exceeds safe VRAM limit"
    //
    // Решение: не прятать estimated_max=0. Go-сторона получит 0,
    // поймёт «VRAM не хватает» и сможет предложить CPU-offload
    // (уменьшить n_gpu_layers) вместо reject'а с ложным max_vram_n_ctx.
    // ============================================================
#ifdef GGML_USE_CUDA
    {
        size_t free_bytes = 0, total_bytes = 0;
        CUresult cu_err = cuMemGetInfo(&free_bytes, &total_bytes);
        if (cu_err == CUDA_SUCCESS) {
            int n_layers      = (int)llama_model_n_layer(model);
            int n_embd        = (int)llama_model_n_embd(model);
            int n_heads       = (int)llama_model_n_head(model);
            int n_kv_heads    = (int)llama_model_n_head_kv(model);
            uint64_t model_size_bytes = llama_model_size(model);
            int gpu_layers = config->n_gpu_layers;
            if (gpu_layers < 0 || gpu_layers > n_layers) gpu_layers = n_layers;
            // После загрузки модели free_bytes (cuMemGetInfo) УЖЕ отражает
            // свободную VRAM за вычетом потребления модели и начального KV-cache.
            // НЕ вычитаем модель повторно — это double-counting, который даёт
            // free_for_kv ≈ 0 на A10 (24GB) с Qwen 35B Q4_K_M (~24GB).
            //
            // ВНИМАНИЕ: model_size_bytes и gpu_model_bytes вычисляются только
            // для диагностического вывода (log). Для расчёта KV-cache используем
            // free_bytes напрямую — оно уже корректно отражает остаток.
            int64_t free_for_kv = (int64_t)free_bytes;
            if (free_for_kv < 0) free_for_kv = 0;

            // KV Cache per token (f16) — GQA-формула:
            //   head_dim = n_embd / n_heads
            //   per_token = 4 * n_layers * n_kv_heads * head_dim
            int head_dim = (n_heads > 0) ? (n_embd / n_heads) : n_embd;
            if (n_kv_heads <= 0) {
                n_kv_heads = n_heads; // fallback для MHA
            }
            if (head_dim <= 0) head_dim = 1;
            int64_t kv_per_token = (int64_t)4 * (int64_t)n_layers *
                                   (int64_t)n_kv_heads * (int64_t)head_dim;

            // raw_estimated — оценка БЕЗ clamp'а. Может быть 0 если
            // free_for_kv < kv_per_token (модель не влезает в VRAM).
            int estimated_max = 0;
            if (kv_per_token > 0) {
                estimated_max = (int)(free_for_kv / kv_per_token);
            }

            // Clamp только если estimated_max > 0.
            // ВАЖНО: не поднимаем estimated_max до ctx_params.n_ctx
            // (старый опасный clamp, который маскировал дефицит VRAM).
            // На A10 (24GB) с Qwen 35B Q4_K_M (~24GB) после загрузки
            // модели может остаться 0.5-2GB → estimated_max=2000-8000.
            // Подъём до 4096 даёт ложную уверенность Go-стороне,
            // что VRAM хватает — auto-reload на 4096 вызовет OOM.
            // Верхний предел (8M токенов) для защиты от вырожденных
            // случаев (e.g. модель 1B на H100 80GB).
            if (estimated_max > 0) {
                if (estimated_max > 8388608) estimated_max = 8388608;
            } else {
                // estimated_max == 0 — модель не влезает в VRAM.
                // Не маскируем это значение! Go-сторона увидит 0
                // и сможет принять информированное решение
                // (CPU-offload, уменьшение gpu_layers и т.п.).
                printf("[bridge] WARNING: VRAM insufficient for KV-cache with current gpu_layers=%d "
                       "(free=%lld MB, gpu_model=%llu MB) — max_vram_n_ctx=0, "
                       "client should reduce n_gpu_layers or increase VRAM\n",
                       gpu_layers,
                       (long long)(free_bytes / (1024 * 1024)),
                       (unsigned long long)(model_size_bytes / (1024 * 1024)));
            }

            err_mutex_lock();
            g_max_vram_n_ctx = estimated_max;
            g_last_error_info.max_vram_n_ctx = estimated_max;
            err_mutex_unlock();
            printf("[bridge] VRAM-estimated max n_ctx = %d "
                   "(free=%lld MB, gpu_model=%llu MB, "
                   "n_layers=%d n_embd=%d n_heads=%d n_kv_heads=%d "
                   "head_dim=%d kv_per_token=%lld bytes)\n",
                   estimated_max,
                   (long long)(free_bytes / (1024 * 1024)),
                   (unsigned long long)(model_size_bytes / (1024 * 1024)),
                   n_layers, n_embd, n_heads, n_kv_heads,
                   head_dim, (long long)kv_per_token);
        } else {
            printf("[bridge] cuMemGetInfo failed — cannot estimate VRAM-based n_ctx\n");
        }
    }
#else
    printf("[bridge] GGML_USE_CUDA not defined — VRAM estimation skipped, max_vram_n_ctx=0\n");
#endif

    printf("[bridge] model loaded successfully: %s (effective n_ctx=%u, n_batch=%u)\n",
           config->model_path, im->ctx_n_ctx, im->ctx_n_batch);
    return (ModelHandle)im;
}

// ============================================================
// Chat template — применение GGUF tokenizer.chat_template
// ============================================================

// bridge_apply_chat_template — применяет chat template к списку сообщений.
// Достаёт tokenizer.chat_template из метаданных GGUF и вызывает llama_chat_apply_template.
// Возвращает количество записанных байт (без \0), или -1 при ошибке.
// Если template в GGUF отсутствует, возвращает -2.
int32_t bridge_apply_chat_template(
    ModelHandle model,
    const char* system,            // может быть NULL
    const char** user_contents,    // массив content-ов user-сообщений
    int32_t n_user,
    const char** assistant_contents, // массив content-ов assistant (для multi-turn), NULL если нет
    int32_t n_assistant,
    bool add_ass,                  // добавить токены начала assistant turn в конец
    char* out_buf,
    int32_t out_buf_size
) {
    if (model == NULL || user_contents == NULL || n_user <= 0 || out_buf == NULL || out_buf_size <= 0) {
        return -1;
    }
    InternalModel *im = (InternalModel *)model;
    if (im->model == NULL) {
        return -1;
    }

    // Достаём chat template из метаданных GGUF (tokenizer.chat_template)
    // Типичные размеры template: 200-2000 байт (для gemma: ~600, для llama3: ~300)
    char tmpl_buf[8192];
    int32_t tmpl_len = llama_model_meta_val_str(
        im->model,
        "tokenizer.chat_template",
        tmpl_buf,
        sizeof(tmpl_buf) - 1
    );
    if (tmpl_len <= 0) {
        // Fallback: попробуем chat_template (без tokenizer.)
        tmpl_len = llama_model_meta_val_str(
            im->model, "chat_template", tmpl_buf, sizeof(tmpl_buf) - 1);
    }
    if (tmpl_len <= 0) {
        return -2; // template не найден в GGUF
    }
    tmpl_buf[tmpl_len] = '\0';

    // Собираем массив llama_chat_message
    int32_t total = n_user + (system != NULL ? 1 : 0) + n_assistant;
    if (total > 32) {
        return -1; // слишком много сообщений
    }
    struct llama_chat_message msgs[32];
    int32_t idx = 0;
    if (system != NULL) {
        msgs[idx].role = "system";
        msgs[idx].content = system;
        idx++;
    }
    for (int32_t i = 0; i < n_user; i++) {
        msgs[idx].role = "user";
        msgs[idx].content = user_contents[i];
        idx++;
    }
    for (int32_t i = 0; i < n_assistant; i++) {
        msgs[idx].role = "assistant";
        msgs[idx].content = assistant_contents[i];
        idx++;
    }

    int32_t written = llama_chat_apply_template(
        tmpl_buf,
        msgs, (size_t)idx,
        add_ass,
        out_buf, out_buf_size
    );
    return written;
}

// bridge_get_chat_template — возвращает raw chat template из GGUF.
// out_buf заполняется template-ом (null-terminated), возвращает длину или -1.
int32_t bridge_get_chat_template(
    ModelHandle model,
    char* out_buf,
    int32_t out_buf_size
) {
    if (model == NULL || out_buf == NULL || out_buf_size <= 0) {
        return -1;
    }
    InternalModel *im = (InternalModel *)model;
    if (im->model == NULL) {
        return -1;
    }
    int32_t tmpl_len = llama_model_meta_val_str(
        im->model, "tokenizer.chat_template", out_buf, (size_t)(out_buf_size - 1));
    if (tmpl_len <= 0) {
        tmpl_len = llama_model_meta_val_str(
            im->model, "chat_template", out_buf, (size_t)(out_buf_size - 1));
    }
    if (tmpl_len <= 0) {
        return -2;
    }
    out_buf[tmpl_len] = '\0';
    return tmpl_len;
}

void bridge_free_model(ModelHandle model) {
    if (model == NULL) return;
    InternalModel *im = (InternalModel *)model;

    if (im->sampler) {
        llama_sampler_free(im->sampler);
        im->sampler = NULL;
    }
    if (im->context) {
        llama_free(im->context);
        im->context = NULL;
    }
    if (im->model) {
        llama_model_free(im->model);
        im->model = NULL;
    }

    free(im);
    printf("[bridge] model freed\n");
}

// ============================================================
// Инференс — РЕАЛЬНАЯ через llama_decode()
// ============================================================

InferenceResult bridge_infer(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params
) {
    InferenceResult result = {0};

    if (model == NULL || prompt == NULL) {
        result.status = 1;
        result.error_msg = strdup("model or prompt is NULL");
        return result;
    }

    InternalModel *im = (InternalModel *)model;

    // Round 13 (2026-07-28): извлекаем seq_id для multi-slot support.
    // seq_id=0 — legacy single-slot (clear all), seq_id>0 — multi-slot
    // (clear only this slot's region).
    llama_seq_id seq_id = (llama_seq_id)(params->seq_id > 0 ? params->seq_id : 0);

    // Сбрасываем KV-cache и сэмплер перед новым запросом.
    // Без этого prompt-токены и сгенерированные токены предыдущего запроса
    // остаются в KV-cache, и через 1-2 запроса контекст переполняется
    // с ошибкой "failed to find a memory slot".
    reset_inference_state(im, seq_id);

    // Токенизируем prompt
    int n_tokens = -llama_tokenize(im->vocab, prompt, (int)strlen(prompt), NULL, 0, true, true);
    if (n_tokens <= 0) {
        result.status = 1;
        result.error_msg = strdup("tokenization failed");
        return result;
    }

    llama_token *tokens = (llama_token *)malloc((size_t)(n_tokens + 1) * sizeof(llama_token));
    if (tokens == NULL) {
        result.status = 1;
        result.error_msg = strdup("out of memory");
        return result;
    }

    int actual_tokens = llama_tokenize(im->vocab, prompt, (int)strlen(prompt), tokens, n_tokens, true, true);
    if (actual_tokens < 0) {
        free(tokens);
        result.status = 1;
        result.error_msg = strdup("tokenization failed");
        return result;
    }

    // Количество для генерации
    int n_predict = params->n_predict > 0 ? params->n_predict : 512;
    int total_capacity = n_tokens + n_predict + 1;

// Если в bridge_check_ctx_capacity проставлен структурированный код
// (например, BRIDGE_ERR_N_CTX_NEEDS_RELOAD = 2 или BRIDGE_ERR_PROMPT_TOO_LONG = 3),
// result.status мапится на этот код. Иначе status=1 (generic).
// Это позволяет Go-стороне через bridge_get_last_error_info() и сам result.status
// понять причину и решить: дёргать auto-reload на бэкенде, либо возвращать
// клиенту 400/413 с понятным JSON-описанием.
    if (bridge_check_ctx_capacity(im, params, actual_tokens, n_predict) != 0) {
        free(tokens);
        err_mutex_lock();
        int code = g_last_error_info.code;
        err_mutex_unlock();
        result.status = (code != BRIDGE_OK) ? code : 1;
        result.error_msg = strdup(last_error);
        return result;
    }

    // Подготавливаем output buffer
    size_t output_capacity = 8192 * 16; // 128 KB initial
    char *output = (char *)malloc(output_capacity);
    if (output == NULL) {
        free(tokens);
        result.status = 1;
        result.error_msg = strdup("out of memory");
        return result;
    }
    output[0] = '\0';
    size_t output_len = 0;

    // Процессим токены промпта через llama_decode батчами
    int n_batch = params->n_batch > 0 ? params->n_batch : 512;
    int n_processed = 0;

    while (n_processed < actual_tokens) {
        int batch_size = (actual_tokens - n_processed) > n_batch ? n_batch : (actual_tokens - n_processed);

        // Round 13: build batch with explicit seq_id (multi-slot support).
        // start_pos = n_processed — позиции в prompt последовательны с 0.
        struct llama_batch batch = build_batch_with_seq(
            tokens + n_processed, batch_size, seq_id, (llama_pos)n_processed
        );

        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            free(output);
            result.error_msg = strdup("llama_decode failed for prompt");
            result.status = 1;
            llama_batch_free(batch);
            return result;
        }

        llama_batch_free(batch);
        n_processed += batch_size;
    }

    free(tokens);

    // Генерируем новые токены
    for (int i = 0; i < n_predict; i++) {
        // Сэмплируем следующий токен
        llama_token new_token = llama_sampler_sample(im->sampler, im->context, -1);

        // Проверяем конец генерации
        if (llama_vocab_is_eog(im->vocab, new_token)) {
            break;
        }

        // Преобразуем токен в текст
        char token_text[256];
        int token_len = llama_token_to_piece(im->vocab, new_token, token_text, sizeof(token_text) - 1, 0, true);
        if (token_len < 0) {
            continue;
        }
        token_text[token_len] = '\0';

        // Добавляем в output
        size_t needed = output_len + (size_t)token_len + 1;
        if (needed > output_capacity) {
            output_capacity = needed * 2;
            char *new_output = (char *)realloc(output, output_capacity);
            if (new_output == NULL) {
                free(output);
                result.status = 1;
                result.error_msg = strdup("out of memory");
                return result;
            }
            output = new_output;
        }
        strcpy(output + output_len, token_text);
        output_len += token_len;

        // Проверяем antiprompts ПОСЛЕ добавления токена: если хвост output
        // совпадает с одним из antiprompts — обрезаем хвост и выходим.
        int ap_idx = check_antiprompts(output, output_len, params);
        if (ap_idx >= 0) {
            output_len = trim_antiprompt_suffix(output, output_len, ap_idx, params);
            break;
        }

        // Декодируем следующий шаг
        // Round 13: build batch with seq_id, pos = n_processed + i (absolute position).
        struct llama_batch gen_batch = build_batch_with_seq(
            &new_token, 1, seq_id, (llama_pos)(n_processed + i)
        );
        if (llama_decode(im->context, gen_batch) != 0) {
            free(output);
            result.status = 1;
            result.error_msg = strdup("llama_decode failed during generation");
            llama_batch_free(gen_batch);
            return result;
        }
        llama_batch_free(gen_batch);
    }

    // Сбрасываем сэмплер для следующего запроса
    llama_sampler_reset(im->sampler);

    result.output = output;
    result.output_len = (int)output_len;
    result.status = 0;

    return result;
}

// ============================================================
// Стриминг инференс — РЕАЛЬНАЯ через llama_decode() + callback
// ============================================================

int bridge_infer_stream(
    ModelHandle model,
    const char* prompt,
    const GenerationParams* params,
    StreamCallback callback,
    void* user_data
) {
    if (model == NULL || prompt == NULL || callback == NULL) {
        set_error("invalid arguments to bridge_infer_stream (model/prompt/callback is NULL)");
        return 1;
    }

    InternalModel *im = (InternalModel *)model;

    // Round 13 (2026-07-28): extract seq_id for multi-slot support.
    llama_seq_id seq_id = (llama_seq_id)(params->seq_id > 0 ? params->seq_id : 0);

    // Сбрасываем KV-cache и сэмплер перед новым запросом — иначе
    // после 1-2 инференсов контекст забит и llama_decode падает
    // с "failed to find a memory slot".
    reset_inference_state(im, seq_id);

    // Токенизируем prompt
    int n_tokens = -llama_tokenize(im->vocab, prompt, (int)strlen(prompt), NULL, 0, true, true);
    if (n_tokens <= 0) {
        set_error("llama_tokenize returned non-positive token count (prompt may be empty or contain only BOS/EOS)");
        return 1;
    }

    llama_token *tokens = (llama_token *)malloc((size_t)(n_tokens + 1) * sizeof(llama_token));
    if (tokens == NULL) {
        set_error("out of memory: failed to allocate token buffer");
        return 1;
    }

    int actual_tokens = llama_tokenize(im->vocab, prompt, (int)strlen(prompt), tokens, n_tokens, true, true);
    if (actual_tokens < 0) {
        free(tokens);
        set_error("llama_tokenize failed for prompt (invalid UTF-8 or unsupported characters)");
        return 1;
    }

    // Количество для генерации
    int n_predict = params->n_predict > 0 ? params->n_predict : 512;
    int n_batch = params->n_batch > 0 ? params->n_batch : 512;

// bridge_infer_stream тоже пробрасывает структурированный код ошибки из
// bridge_check_ctx_capacity. Раньше всегда возвращал 1, что не позволяло
// Go-стороне отличить BRIDGE_ERR_N_CTX_NEEDS_RELOAD (=2) от generic
// ошибки. Теперь возвращаем код напрямую; Go-маппер обработает и прокинет
// 413/503 клиенту.
    if (bridge_check_ctx_capacity(im, params, actual_tokens, n_predict) != 0) {
        free(tokens);
        err_mutex_lock();
        int code = g_last_error_info.code;
        err_mutex_unlock();
        return (code != BRIDGE_OK) ? code : 1;
    }

    // Процессим токены промпта
    int n_processed = 0;
    while (n_processed < actual_tokens) {
        int batch_size = (actual_tokens - n_processed) > n_batch ? n_batch : (actual_tokens - n_processed);

        // Round 13: build batch with explicit seq_id (multi-slot support).
        // start_pos = n_processed — позиции в prompt последовательны с 0.
        struct llama_batch batch = build_batch_with_seq(
            tokens + n_processed, batch_size, seq_id, (llama_pos)n_processed
        );

        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            // Самая частая причина: n_ctx переполнен (kv-cache overflow),
            // либо n_batch > n_ctx, либо модель не помещается в VRAM.
            set_error("llama_decode failed for prompt batch (likely n_ctx overflow, prompt too long, or n_batch > n_ctx)");
            llama_batch_free(batch);
            return 1;
        }

        llama_batch_free(batch);
        n_processed += batch_size;
    }

    free(tokens);

    // Генерируем токены и отправляем через callback
    // Локальный буфер накапливает последние токены для antiprompt-проверки
    // (чтобы корректно ловить маркеры длиной в несколько токенов, вроде
    // "<end_of_turn>"). Размер: max длина antiprompt + запас.
    char streamed_acc[1024];
    size_t streamed_acc_len = 0;
    streamed_acc[0] = '\0';

    for (int i = 0; i < n_predict; i++) {
        llama_token new_token = llama_sampler_sample(im->sampler, im->context, -1);

        if (llama_vocab_is_eog(im->vocab, new_token)) {
            break;
        }

        char token_text[256];
        int token_len = llama_token_to_piece(im->vocab, new_token, token_text, sizeof(token_text) - 1, 0, true);
        if (token_len < 0) {
            continue;
        }
        token_text[token_len] = '\0';

        // Накапливаем токен в локальный буфер для antiprompt-проверки
        if (streamed_acc_len + (size_t)token_len + 1 < sizeof(streamed_acc)) {
            memcpy(streamed_acc + streamed_acc_len, token_text, (size_t)token_len);
            streamed_acc_len += (size_t)token_len;
            streamed_acc[streamed_acc_len] = '\0';
        } else {
            // буфер переполнен — сбрасываем (anti-prompts мы уже видели)
            streamed_acc_len = 0;
            streamed_acc[0] = '\0';
        }

        // Проверка antiprompt по хвосту буфера. Если совпало — обрезаем хвост
        // до antiprompt и НЕ отправляем его в callback.
        int ap_idx = check_antiprompts(streamed_acc, streamed_acc_len, params);
        if (ap_idx >= 0) {
            // обрезаем streamed_acc до antiprompt-суффикса
            const char* ap = params->antiprompts[ap_idx];
            size_t ap_len = strlen(ap);
            streamed_acc_len -= ap_len;
            streamed_acc[streamed_acc_len] = '\0';
            // отправляем то, что осталось (без antiprompt-хвоста)
            if (streamed_acc_len > 0) {
                if (callback(streamed_acc, (int)streamed_acc_len, user_data) == 0) {
                    llama_sampler_reset(im->sampler);
                    return 0;
                }
            }
            break; // antiprompt сработал, стрим завершён
        }

        // Конвенция callback: возврат !=0 (true) — продолжить стриминг,
        // возврат 0 (false) — остановить стриминг (клиент отвалился / ctx.Done).
        if (callback(token_text, token_len, user_data) == 0) {
            llama_sampler_reset(im->sampler);
            return 0; // клиент остановил стриминг
        }

        // Декодируем следующий шаг
        // Round 13: build batch with seq_id, pos = n_processed + i (absolute position).
        struct llama_batch gen_batch = build_batch_with_seq(
            &new_token, 1, seq_id, (llama_pos)(n_processed + i)
        );
        if (llama_decode(im->context, gen_batch) != 0) {
            // Обычно это переполнение KV-cache при длинной выдаче
            // (n_predict > n_ctx - prompt_len), либо OOM на GPU.
            set_error("llama_decode failed during generation step (likely KV-cache overflow: n_predict + prompt_len > n_ctx, or GPU OOM)");
            llama_batch_free(gen_batch);
            return 1;
        }
        llama_batch_free(gen_batch);
    }

    llama_sampler_reset(im->sampler);
    return 0;
}

// ============================================================
// Эмбеддинги — РЕАЛЬНАЯ через llama_get_embeddings()
// ============================================================

InferenceResult bridge_get_embeddings(
    ModelHandle model,
    const char* text
) {
    InferenceResult result = {0};

    if (model == NULL || text == NULL) {
        result.status = 1;
        result.error_msg = strdup("model or text is NULL");
        return result;
    }

    InternalModel *im = (InternalModel *)model;

    // Токенизируем текст
    int n_tokens = -llama_tokenize(im->vocab, text, (int)strlen(text), NULL, 0, true, true);
    if (n_tokens <= 0) {
        result.status = 1;
        result.error_msg = strdup("tokenization failed");
        return result;
    }

    llama_token *tokens = (llama_token *)malloc((size_t)(n_tokens + 1) * sizeof(llama_token));
    if (tokens == NULL) {
        result.status = 1;
        result.error_msg = strdup("out of memory");
        return result;
    }

    int actual_tokens = llama_tokenize(im->vocab, text, (int)strlen(text), tokens, n_tokens, true, true);
    if (actual_tokens < 0) {
        free(tokens);
        result.status = 1;
        result.error_msg = strdup("tokenization failed");
        return result;
    }

    // Создаём батч с запросом эмбеддингов
    struct llama_batch batch = llama_batch_get_one(tokens, actual_tokens);
    // Устанавливаем логиты для всех токенов чтобы получить эмбеддинги
    batch.logits = (int8_t *)malloc((size_t)actual_tokens);
    if (batch.logits) {
        for (int i = 0; i < actual_tokens; i++) {
            batch.logits[i] = 1;
        }
    }

    if (llama_decode(im->context, batch) != 0) {
        free(batch.logits);
        free(tokens);
        result.status = 1;
        result.error_msg = strdup("llama_decode failed for embeddings");
        return result;
    }

    free(batch.logits);
    free(tokens);

    // Получаем эмбеддинги (усреднённые по всем токенам)
    const float *embeddings = llama_get_embeddings(im->context);
    if (embeddings == NULL) {
        result.status = 1;
        result.error_msg = strdup("failed to get embeddings");
        return result;
    }

    int n_embd = llama_model_n_embd(im->model);

    // Сериализуем эмбеддинги как JSON-массив float'ов
    // Формат: [0.123,-0.456,...]
    size_t buf_size = (size_t)n_embd * 32 + 32; // запас на числа + скобки + запятые
    char *json_buf = (char *)malloc(buf_size);
    if (json_buf == NULL) {
        result.status = 1;
        result.error_msg = strdup("out of memory");
        return result;
    }

    int offset = snprintf(json_buf, buf_size, "[");
    for (int i = 0; i < n_embd && offset < (int)buf_size - 16; i++) {
        offset += snprintf(json_buf + offset, buf_size - offset, "%s%f",
                          i > 0 ? "," : "",
                          embeddings[i]);
    }
    snprintf(json_buf + offset, buf_size - offset, "]");

    result.output = json_buf;
    result.output_len = (int)strlen(json_buf);
    result.status = 0;

    return result;
}

// ============================================================
// Метаданные модели — РЕАЛЬНАЯ через llama_model_*()
// ============================================================

ModelMetadata bridge_get_model_metadata(ModelHandle model) {
    ModelMetadata metadata = {0};

    if (model == NULL) return metadata;

    InternalModel *im = (InternalModel *)model;

    // Получаем название архитектуры из метаданных
    char arch_buffer[256];
    int arch_len = llama_model_meta_val_str(im->model, "general.architecture", arch_buffer, sizeof(arch_buffer) - 1);
    if (arch_len > 0) {
        metadata.architecture = strdup(arch_buffer);
    } else {
        metadata.architecture = strdup("unknown");
    }

    // Получаем описание
    char desc_buffer[512];
    int desc_len = llama_model_meta_val_str(im->model, "general.description", desc_buffer, sizeof(desc_buffer) - 1);
    if (desc_len > 0) {
        metadata.description = strdup(desc_buffer);
    } else {
        metadata.description = strdup("");
    }

    // Параметры модели
    metadata.context_length = (int)llama_model_n_ctx_train(im->model);
    metadata.n_layers = (int)llama_model_n_layer(im->model);
    metadata.n_heads = (int)llama_model_n_head(im->model);
    metadata.n_head_kv = (int)llama_model_n_head_kv(im->model);  // public API

    // head_dim computed inline (n_embd_head_k/v are NOT in public llama.h header)

    int _n_embd = (int)llama_model_n_embd(im->model); int _n_heads = (int)llama_model_n_head(im->model);

    metadata.n_embd_head_k = (_n_heads > 0) ? _n_embd / _n_heads : 0;

    metadata.n_embd_head_v = (_n_heads > 0) ? _n_embd / _n_heads : 0;

    metadata.n_embd = _n_embd;
    metadata.n_vocab = (int)llama_vocab_n_tokens(im->vocab);
    metadata.size_total = 0; // будет заполнено из Go-уровня через stat()

    return metadata;
}

// ============================================================
// Освобождение памяти
// ============================================================

void bridge_free_inference_result(InferenceResult* result) {
    if (result == NULL) return;
    if (result->output) free(result->output);
    if (result->error_msg) free(result->error_msg);
    memset(result, 0, sizeof(InferenceResult));
}

void bridge_free_model_metadata(ModelMetadata* metadata) {
    if (metadata == NULL) return;
    if (metadata->description) free(metadata->description);
    if (metadata->architecture) free(metadata->architecture);
    memset(metadata, 0, sizeof(ModelMetadata));
}

void bridge_free_string(char* str) {
    if (str) free(str);
}

const char* bridge_last_error(void) {
    return last_error;
}

// ============================================================
// Tokenization utilities — реальный подсчёт токенов
// ============================================================

int32_t bridge_count_tokens(ModelHandle model, const char* text) {
    if (model == NULL || text == NULL) return -1;

    InternalModel *im = (InternalModel *)model;
    if (im->vocab == NULL) return -1;

    int n_tokens = -llama_tokenize(im->vocab, text, (int)strlen(text), NULL, 0, true, true);
    if (n_tokens < 0) return -1;
    return n_tokens;
}

const char* bridge_version(void) {
    return "0.2.0 (real llama.cpp linked)";
}

#endif // GO_BRIDGE_LLAMA_STUB guard
