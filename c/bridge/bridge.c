// ============================================================
// bridge.c — C-обёртка над llama.cpp для Go-вызова (CGo)
// Версия: 0.2.0 — реальный инференс через llama.cpp
// ============================================================

#include "bridge.h"
#include "llama.h"
#include <stdlib.h>
#include <string.h>
#include <stdio.h>

// ============================================================
// Внутренние структуры
// ============================================================

// InternalModel — внутреннее представление загруженной модели
typedef struct {
    struct llama_model *model;
    struct llama_context *context;
    struct llama_sampler *sampler;
    struct llama_vocab *vocab;
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
static void reset_inference_state(InternalModel *im) {
    if (im == NULL) return;
    if (im->context != NULL) {
        llama_memory_t mem = llama_get_memory(im->context);
        if (mem != NULL) {
            // data=true: очищаем и метаданные seq, и сами KV-буферы в VRAM
            llama_memory_clear(mem, true);
        }
    }
    if (im->sampler != NULL) {
        llama_sampler_reset(im->sampler);
    }
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
    ctx_params.flash_attn_type = config->flash_attn_type >= 0 ? 
        (config->flash_attn_type == 0 ? LLAMA_FLASH_ATTN_TYPE_DISABLED : LLAMA_FLASH_ATTN_TYPE_ENABLED) :
        LLAMA_FLASH_ATTN_TYPE_AUTO;
    ctx_params.rope_freq_base = config->rope_freq_base > 0 ? config->rope_freq_base : 0.0f;
    ctx_params.rope_freq_scale = config->rope_freq_scale > 0 ? config->rope_freq_scale : 0.0f;

    // Создаём контекст
    struct llama_context *context = llama_new_context_with_model(model, ctx_params);
    if (context == NULL) {
        char buf[512];
        snprintf(buf, sizeof(buf), "failed to create context for %s", config->model_path);
        set_error(buf);
        llama_model_free(model);
        if (error_msg) *error_msg = strdup(buf);
        return NULL;
    }

    // Получаем vocabulary
    struct llama_vocab *vocab = llama_model_get_vocab(model);

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
    im->vocab = vocab;

    printf("[bridge] model loaded successfully: %s\n", config->model_path);
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

    // Сбрасываем KV-cache и сэмплер перед новым запросом.
    // Без этого prompt-токены и сгенерированные токены предыдущего запроса
    // остаются в KV-cache, и через 1-2 запроса контекст переполняется
    // с ошибкой "failed to find a memory slot".
    reset_inference_state(im);

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

        struct llama_batch batch = llama_batch_get_one(
            tokens + n_processed,
            batch_size
        );

        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            free(output);
            result.status = 1;
            result.error_msg = strdup("llama_decode failed for prompt");
            return result;
        }

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
        struct llama_batch gen_batch = llama_batch_get_one(&new_token, 1);
        if (llama_decode(im->context, gen_batch) != 0) {
            free(output);
            result.status = 1;
            result.error_msg = strdup("llama_decode failed during generation");
            return result;
        }
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
        return 1;
    }

    InternalModel *im = (InternalModel *)model;

    // Сбрасываем KV-cache и сэмплер перед новым запросом — иначе
    // после 1-2 инференсов контекст забит и llama_decode падает
    // с "failed to find a memory slot".
    reset_inference_state(im);

    // Токенизируем prompt
    int n_tokens = -llama_tokenize(im->vocab, prompt, (int)strlen(prompt), NULL, 0, true, true);
    if (n_tokens <= 0) {
        return 1;
    }

    llama_token *tokens = (llama_token *)malloc((size_t)(n_tokens + 1) * sizeof(llama_token));
    if (tokens == NULL) {
        return 1;
    }

    int actual_tokens = llama_tokenize(im->vocab, prompt, (int)strlen(prompt), tokens, n_tokens, true, true);
    if (actual_tokens < 0) {
        free(tokens);
        return 1;
    }

    // Количество для генерации
    int n_predict = params->n_predict > 0 ? params->n_predict : 512;
    int n_batch = params->n_batch > 0 ? params->n_batch : 512;

    // Процессим токены промпта
    int n_processed = 0;
    while (n_processed < actual_tokens) {
        int batch_size = (actual_tokens - n_processed) > n_batch ? n_batch : (actual_tokens - n_processed);

        struct llama_batch batch = llama_batch_get_one(
            tokens + n_processed,
            batch_size
        );

        if (llama_decode(im->context, batch) != 0) {
            free(tokens);
            return 1;
        }

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
        struct llama_batch gen_batch = llama_batch_get_one(&new_token, 1);
        if (llama_decode(im->context, gen_batch) != 0) {
            return 1;
        }
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
    metadata.n_embd = (int)llama_model_n_embd(im->model);
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

const char* bridge_version(void) {
    return "0.2.0 (real llama.cpp linked)";
}