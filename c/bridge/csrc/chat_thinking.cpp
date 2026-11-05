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

// R434 (2026-11-05): общая реализация рендера шаблона.
// (forward declaration — C++ linkage, поэтому ВНЕ extern "C" блока ниже.)
//
// ПОЧЕМУ ПОЯВИЛАСЬ. До R434 мы НИКОГДА не передавали tools[] в
// common_chat_templates_apply (inputs.tools оставался пустым): модель не видела
// нативную секцию инструментов своего шаблона, и формат вызова задавала наша
// текстовая преамбула. Живой дефект — Cline в VS Code + gemma-4: модель
// копировала придуманный нами гибридный формат и теряла name/arguments, вызов не
// распознавался, клиент закрывал ход как «COMPLETED» без созданного файла.
//
// Теперь tools_json (OpenAI-массив из запроса клиента) разбирается
// common_chat_tools_parse_oaicompat и уходит в inputs.tools — шаблон сам рисует
// ровно тот синтаксис вызова, которому модель обучена (gemma — <|tool_call>
// call:…, Qwen — Hermes XML, Mistral — [TOOL_CALLS]).
//
// «Шаблон отрисовал инструменты» определяем ЧЕСТНО: рендерим второй раз БЕЗ
// tools и сравниваем prompt'ы. Равны → шаблон инструменты игнорирует (или их
// нет в нём), и вызывающий обязан включить текстовую преамбулу как fallback.
// Так решение не зависит от имён моделей и от внутренних эвристик llama.cpp.
static int32_t bridge_chat_templates_apply_common(
    void* model,
    const char* chat_template_override,
    const char* tools_json,
    const struct CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking,
    bool* out_tools_rendered,
    int32_t* out_format);

// bridge_chat_templates_apply_common — общая реализация (см. forward-declaration).
static int32_t bridge_chat_templates_apply_common(
    void* model,
    const char* chat_template_override,
    const char* tools_json,
    const struct CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking,
    bool* out_tools_rendered,
    int32_t* out_format
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
        // R246: structured tool history. Templates that render tool calls or tool
        // results only from these fields (gemma-4) see nothing without them.
        if (messages[i].tool_call_id != NULL) {
            m.tool_call_id = messages[i].tool_call_id;
        }
        if (messages[i].tool_calls_json != NULL && messages[i].tool_calls_json[0] != '\0') {
            common_json arr = common_json::parse_no_throw(messages[i].tool_calls_json);
            if (arr.is_array()) {
                for (const auto & item : arr) {
                    if (!item.is_object()) {
                        continue;
                    }
                    common_chat_tool_call tc;
                    if (item.contains("id") && item.at("id").is_string()) {
                        tc.id = item.at("id").get<std::string>();
                    }
                    if (item.contains("function") && item.at("function").is_object()) {
                        const common_json & fn = item.at("function");
                        if (fn.contains("name") && fn.at("name").is_string()) {
                            tc.name = fn.at("name").get<std::string>();
                        }
                        if (fn.contains("arguments")) {
                            const common_json & args = fn.at("arguments");
                            if (args.is_string()) {
                                tc.arguments = args.get<std::string>();
                            } else if (!args.is_null()) {
                                tc.arguments = args.dump();
                            }
                        }
                    }
                    if (!tc.name.empty()) {
                        m.tool_calls.push_back(tc);
                    }
                }
            }
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

    // Собираем inputs. messages копируем: ниже нужен ВТОРОЙ рендер (без tools)
    // для честного ответа «шаблон отрисовал инструменты или проигнорировал».
    common_chat_templates_inputs inputs;
    inputs.messages = msgs;
    inputs.add_generation_prompt = add_generation_prompt;
    inputs.use_jinja = true;  // required for enable_thinking
    inputs.enable_thinking = enable_thinking;
    // defaults: reasoning_format=NONE, tools=empty, parallel_tool_calls=false

    bool have_tools = (tools_json != NULL && tools_json[0] != '\0');
    if (have_tools) {
        // OpenAI-массив tools[] из запроса клиента → common_chat_tool.
        // R434: именно этого не хватало — шаблон рисовал инструменты «ни с чем».
        try {
            common_json tools_arr = common_json::parse_no_throw(tools_json);
            if (tools_arr.is_array() && !tools_arr.empty()) {
                inputs.tools = common_chat_tools_parse_oaicompat(tools_arr);
            }
        } catch (...) {
            // Битый tools[] не должен рушить запрос: рендерим без инструментов,
            // вызывающий включит текстовую преамбулу (fallback).
            inputs.tools.clear();
        }
    }

    // Вызываем native API.
    common_chat_params params = common_chat_templates_apply(tmpls.get(), inputs);

    // «Отрисовал ли шаблон инструменты» — сравнением с рендером БЕЗ tools.
    bool tools_rendered = false;
    if (have_tools && !inputs.tools.empty()) {
        try {
            common_chat_templates_inputs plain = inputs;
            plain.tools.clear();
            common_chat_params plain_params = common_chat_templates_apply(tmpls.get(), plain);
            tools_rendered = (plain_params.prompt != params.prompt);
        } catch (...) {
            tools_rendered = false;
        }
    }

    // Возвращаем supports_thinking / tools_rendered / format caller'у.
    if (out_supports_thinking != NULL) {
        *out_supports_thinking = params.supports_thinking;
    }
    if (out_tools_rendered != NULL) {
        *out_tools_rendered = tools_rendered;
    }
    if (out_format != NULL) {
        *out_format = static_cast<int32_t>(params.format);
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
    return bridge_chat_templates_apply_common(
        model, chat_template_override, /*tools_json=*/NULL,
        messages, n_messages, enable_thinking, add_generation_prompt,
        out_buf, out_buf_size, out_supports_thinking,
        /*out_tools_rendered=*/NULL, /*out_format=*/NULL);
}

// bridge_chat_templates_apply_with_tools — R434 (2026-11-05): рендер шаблона
// ВМЕСТЕ с инструментами запроса.
//
// Параметры те же, что у ..._with_thinking, плюс:
//   tools_json          — OpenAI-массив tools[] (NULL/"" = без инструментов)
//   out_tools_rendered  — [OUT] true, если шаблон нарисовал секцию инструментов
//                         (сравнение рендеров с tools и без). false → вызывающий
//                         обязан включить текстовую преамбулу (fallback).
//   out_format          — [OUT] common_chat_format (0 = CONTENT_ONLY): каким
//                         парсером llama.cpp видит вызовы этой модели.
//
// Возвращает те же коды, что ..._with_thinking.
int32_t bridge_chat_templates_apply_with_tools(
    void* model,
    const char* chat_template_override,
    const char* tools_json,
    const struct CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking,
    bool* out_tools_rendered,
    int32_t* out_format
) {
    return bridge_chat_templates_apply_common(
        model, chat_template_override, tools_json,
        messages, n_messages, enable_thinking, add_generation_prompt,
        out_buf, out_buf_size, out_supports_thinking,
        out_tools_rendered, out_format);
}

} // extern "C"

