// chat_thinking.cpp — Round 14a (2026-07-28): C-callable wrapper around
// common::chat::common_chat_templates_apply для native enable_thinking.
//
// ЗАЧЕМ: low-level llama_chat_apply_template (c/llama.cpp/include/llama.h:1162)
// НЕ имеет параметра enable_thinking — только role/content/add_ass.
// Для native thinking support (Qwen3-thinking, DeepSeek-R1, GLM-Z1) нужно
// использовать C++ API common_chat_templates_apply из c/llama.cpp/common/chat.h,
// который парсит Jinja template с enable_thinking=true|false и эмитит правильные
// thinking tags (например, <think>...</think>).
//
// Round 11 (v0.4.9) использовал soft prompt injection для instruction-tuned
// моделей. Round 14a добавляет NATIVE путь для моделей, которые в своём
// chat template поддерживают enable_thinking Jinja variable.
//
// Round 14b (next session): интеграция в cmd/cppworker/handlers_chat.go
// + замена soft prompt на native apply.

#include "bridge.h"            // c/bridge/bridge.h — ModelHandle, CBridgeChatMessage (extern "C")
#include "chat.h"              // c/llama.cpp/common/chat.h — common_chat_templates_*
#include "llama.h"             // c/llama.cpp/include/llama.h — для llama_model

#include <string>
#include <vector>
#include <cstring>
#include <cstdio>

// Все C-bridge функции обёрнуты в extern "C" для совместимости с C-вызовами
// из Go (cgo). C++ имеет name mangling, без extern "C" имена будут
// искажены и Go-сторона не сможет их найти.
extern "C" {

// InternalModel определён в c/bridge/bridge.c. Мы не можем включить bridge.c
// напрямую (multiple definition), но нам нужен доступ к model pointer.
//
// Решение: forward-declare struct InternalModel (как opaque) и используем
// тот же layout, что в bridge.c. Поскольку оба файла компилируются в одну
// статическую библиотеку ollamalegion_bridge, линкер увидит только ОДНО
// определение InternalModel (из bridge.c).
//
// ВАЖНО: Если layout поменяется в bridge.c — нужно обновить mirror здесь.
struct InternalModel;
typedef struct llama_model llama_model_t;

// C struct CBridgeChatMessage определён в bridge.h (line ~42) и
// переиспользуется здесь через #include "bridge.h". НЕ определять
// повторно — это даёт "error: redefinition of 'struct CBridgeChatMessage'".

// bridge_chat_templates_apply_with_thinking — C-обёртка над common_chat_templates_apply.
//
// Вызывает common::chat::common_chat_templates_apply с enable_thinking=true|false
// и возвращает formatted prompt в out_buf.
//
// Параметры:
//   model                    — загруженная модель (ModelHandle из bridge_load_model)
//   chat_template_override   — кастомный template (NULL = использовать GGUF default)
//   messages                 — массив {role, content} пар
//   n_messages               — количество сообщений
//   enable_thinking          — true = native thinking mode (Qwen3-thinking etc.)
//   add_generation_prompt    — true = добавить assistant turn tokens в конец
//   out_buf                  — выходной буфер (null-terminated строка)
//   out_buf_size             — размер выходного буфера
//   out_supports_thinking    — [OUT] true если template поддерживает thinking
//
// Возвращает: количество записанных байт (без \0), или:
//   -1  invalid args
//   -2  template not found
//   -3  common_chat_templates_init failed
//   -4  output buffer too small (для retry с большим буфером)
int32_t bridge_chat_templates_apply_with_thinking(
    void* model,                                      // ModelHandle
    const char* chat_template_override,                // NULL = use GGUF default
    const struct CBridgeChatMessage* messages,         // массив сообщений
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking
) {
    if (model == NULL || messages == NULL || n_messages <= 0 || out_buf == NULL || out_buf_size <= 0) {
        return -1;
    }

    // Получаем llama_model* из InternalModel. Layout mirror:
    //   struct InternalModel {
    //       struct llama_model *model;     // <- первое поле
    //       struct llama_context *context;
    //       struct llama_sampler *sampler;
    //       ...
    //   };
    // См. c/bridge/bridge.c:35-47.
    const struct llama_model* lmodel = *(const struct llama_model**)model;
    if (lmodel == NULL) {
        return -1;
    }

    // Строим std::vector<common_chat_msg> из C-массива.
    std::vector<common_chat_msg> msgs;
    msgs.reserve(static_cast<size_t>(n_messages));
    for (int32_t i = 0; i < n_messages; i++) {
        common_chat_msg m;
        if (messages[i].role != NULL) {
            m.role = messages[i].role;
        }
        if (messages[i].content != NULL) {
            m.content = messages[i].content;
        }
        msgs.push_back(std::move(m));
    }

    // Инициализируем common_chat_templates из GGUF metadata.
    // Пустой override означает "использовать template из GGUF".
    std::string tmpl_override = (chat_template_override != NULL) ? std::string(chat_template_override) : std::string();

    common_chat_templates_ptr tmpls = common_chat_templates_init(
        lmodel,
        tmpl_override,
        "",  // bos_token_override
        ""   // eos_token_override
    );
    if (tmpls == nullptr) {
        return -3;  // init failed (no template in GGUF, or invalid override)
    }

    // Собираем inputs.
    common_chat_templates_inputs inputs;
    inputs.messages = std::move(msgs);
    inputs.add_generation_prompt = add_generation_prompt;
    inputs.use_jinja = true;  // required for enable_thinking
    inputs.enable_thinking = enable_thinking;
    // defaults: reasoning_format=NONE, tools=empty, parallel_tool_calls=false

    // Вызываем native API.
    common_chat_params params = common_chat_templates_apply(tmpls.get(), inputs);

    // Возвращаем supports_thinking caller'у.
    if (out_supports_thinking != NULL) {
        *out_supports_thinking = params.supports_thinking;
    }

    // Копируем prompt в out_buf.
    const std::string& prompt = params.prompt;
    int32_t prompt_len = static_cast<int32_t>(prompt.size());

    if (prompt_len >= out_buf_size) {
        // Output buffer too small. Caller может перевыделить и retry.
        return -4;
    }

    std::memcpy(out_buf, prompt.c_str(), static_cast<size_t>(prompt_len));
    out_buf[prompt_len] = '\0';
    return prompt_len;
}

} // extern "C"
