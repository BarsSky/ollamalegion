# Round 14 — Native C-bridge `enable_thinking` via `common::chat` migration

**Status**: PARTIAL (Round 14a — C-bridge foundation, complete; Round 14b — Go integration, deferred)
**Date**: 2026-07-28
**Builds on**: v0.4.11 (Round 13 — n_parallel foundation)
**Author**: Mavis (Session 18)

## Goal

Replace Round 11's soft prompt injection (which prepends a "thinking instruction" to the
system prompt) with **native** `enable_thinking` support via the proper llama.cpp API:
`common::chat::common_chat_templates_apply` with `enable_thinking: bool` parameter.

## Why Round 11 wasn't enough

Round 11 added `EnableReasoning` in cppworker config + a soft prompt injection in
`injectThinkingInstruction()` / `injectThinkingIntoMessages()`. This works for
instruction-tuned models that don't have native thinking support (gemma-4-it, llama-3-it).

But for models that **DO** have native thinking (Qwen3-thinking, DeepSeek-R1, GLM-Z1,
etc.), the soft prompt is:
- A kludge that doesn't match the model's training
- Sometimes ignored or causes unexpected output
- May be redundant with the model's own reasoning

The native API (`common_chat_templates_apply` with `enable_thinking=true`) tells the
model's own Jinja chat template to emit the correct thinking tag (e.g. `<think>...</think>`)
and the parser (`common_chat_parser`) extracts the reasoning into a separate field.

## Scope split

**Round 14a (this PR — C-bridge foundation)**:
- C++ wrapper exposing `common_chat_templates_apply` as C-callable function
- CMakeLists.txt changes: enable C++ compilation, link with `common` library
- New C API in `bridge.h`: `bridge_chat_templates_apply_with_thinking()`
- Build verifies (CUDA compile + Go build + stub test)
- Stub compat for tests

**Round 14b (next session — Go integration)**:
- `cmd/cppworker/handlers_chat.go` passes `EnableReasoning` → `EnableThinking` to C-bridge
- `cmd/cppworker/handlers_generate.go` same
- `cmd/cppworker/inference.go` — replace `injectThinkingInstruction()` with native call
- Live test: gemma-4 + Qwen3-thinking, verify reasoning extraction works
- Build + deploy + tag v0.4.12

## Design (Round 14a)

### C++ wrapper (`c/bridge/chat_thinking.cpp`)

```cpp
// Round 14a (2026-07-28): C-callable wrapper around common_chat_templates_apply.
// Exposes native enable_thinking parameter that the low-level
// llama_chat_apply_template API does NOT have (see llama.h:1162).

#include "chat.h"           // c/llama.cpp/common/chat.h
#include "llama.h"          // c/llama.cpp/include/llama.h
#include "bridge.h"         // c/bridge/bridge.h
#include <string>
#include <vector>
#include <cstring>

// C struct for chat messages (flat, no std::vector).
// Caller owns the arrays; we copy strings into std::string.
struct CBridgeChatMessage {
    const char* role;
    const char* content;
};

extern "C" int32_t bridge_chat_templates_apply_with_thinking(
    ModelHandle model,
    const char* chat_template_override,   // NULL = use GGUF default
    const CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,                  // NEW: native thinking toggle
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking            // OUT: does template support thinking?
) {
    // ... build std::vector<common_chat_msg> from C arrays
    // ... call common_chat_templates_init(model, override, ...)
    // ... call common_chat_templates_apply(tmpls, inputs{enable_thinking:...})
    // ... copy result.prompt to out_buf
    // ... set out_supports_thinking
    // ... return number of bytes written
}
```

### C API (`c/bridge/bridge.h`)

```c
// Round 14a: native enable_thinking via common_chat_templates_apply.
// Replaces bridge_apply_chat_template for thinking-aware models.
// Currently additive — legacy bridge_apply_chat_template stays for backward compat.

struct CBridgeChatMessage {
    const char* role;
    const char* content;
};

int32_t bridge_chat_templates_apply_with_thinking(
    ModelHandle model,
    const char* chat_template_override,    // NULL = use GGUF default
    const struct CBridgeChatMessage* messages,
    int32_t n_messages,
    bool enable_thinking,
    bool add_generation_prompt,
    char* out_buf,
    int32_t out_buf_size,
    bool* out_supports_thinking
);
```

### CMakeLists.txt

```cmake
# Change project type from C to CXX (common.h is C++).
project(ollamalegion_bridge CXX)

# Add C++ standard (common.h uses C++17 std::string_view etc.)
set(CMAKE_CXX_STANDARD 17)
set(CMAKE_CXX_STANDARD_REQUIRED ON)

# Sources (now mixed C and C++)
set(BRIDGE_SOURCES
    bridge.c
    chat_thinking.cpp
)

# Link with common library (built by llama.cpp build)
find_library(COMMON_LIB common PATHS "${LLAMA_BUILD_DIR}/common")
# Append to LINK_LIBS
```

### Go bridge (`c/bridge/bridge.go`)

```go
// BridgeChatMessage — C struct mirror.
type BridgeChatMessage struct {
    Role    *C.char
    Content *C.char
}

// ApplyChatTemplateWithThinking — native thinking-aware template apply.
func (m *ModelHandle) ApplyChatTemplateWithThinking(
    chatTemplateOverride string,
    messages []ChatMessage,
    enableThinking bool,
    addGenerationPrompt bool,
) (prompt string, supportsThinking bool, err error) {
    // ... build C array, call C function, convert result
}
```

## Backward compat

- `bridge_apply_chat_template` (legacy) остаётся — для моделей, которые НЕ поддерживают
  thinking, можно использовать как раньше.
- Новый `bridge_chat_templates_apply_with_thinking` используется ТОЛЬКО когда
  `EnableReasoning=true` (config или per-request).
- Если модель НЕ поддерживает thinking (template не имеет `enable_thinking` Jinja
  variable), результат = без thinking blocks, parser их не извлечёт, behavior
  идентичен Round 11 (просто без soft prompt).

## Trade-offs

**Pros**:
- Поддержка native thinking для Qwen3-thinking, DeepSeek-R1, GLM-Z1
- Не конфликтует с обучением модели (template знает как правильно)
- Reasoning parser (`common_chat_parser`) уже есть в `c/llama.cpp/common/`
- Потенциально лучшее качество reasoning output

**Cons**:
- Требует C++ compilation в bridge (медленнее, сложнее build)
- Зависимость от `common` library (дополнительная linking)
- Все еще partial — Round 14b нужен для реальной интеграции

## Success criteria (Round 14a)

1. `go build -tags llama_stub` — компилируется без изменений (stub не использует C++ wrapper)
2. `docker build` — CUDA compile + C++ compile + Go cgo build, image создаётся
3. Контейнер запускается, `GET /api/v1/cppworker/config` возвращает тот же JSON (no behavior change)
4. Smoke test 14/14 — все existing checks проходят (no regression)
5. `chat_thinking.cpp` компилируется без warnings (g++ -Wall)

## Success criteria (Round 14b — next session)

1. Live test: gemma-4 + `enableReasoning=true` → НЕТ soft prompt injection, но модель
   всё равно генерирует reasoning (через native template + parser)
2. Live test: Qwen3-thinking (если есть) + `enableReasoning=true` → корректное извлечение
   `<think>...</think>` в `message.reasoning`
3. Live test: gemma-4 + `enableReasoning=false` → behavior IDENTICAL to v0.4.11
   (no reasoning, no soft prompt)
4. Все 14 smoke tests pass

## Effort estimate

- Round 14a (C-bridge foundation): 1.5-2 часа (C++ wrapper + CMake + build verify)
- Round 14b (Go integration): 1.5-2 часа (handlers + live test + deploy)
- Total: 3-4 часа (1 working day)

Round 14a выполнена в этой сессии, Round 14b — в следующей.

## Files changed (Round 14a)

- `c/bridge/chat_thinking.cpp` (новый, ~80 строк)
- `c/bridge/bridge.h` (+30 строк, C API declaration)
- `c/bridge/CMakeLists.txt` (project type → CXX, link common lib)
- `c/bridge/bridge_stub.go` (+15 строк, stub compat для тестов)
- `c/bridge/bridge.go` (+30 строк, Go wrapper)
- `docs/round-14-native-enable-thinking-design.md` (новый, этот файл)
