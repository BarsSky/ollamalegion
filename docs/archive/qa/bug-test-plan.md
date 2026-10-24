# Bug Test Plan: Balancer proxy request/response correctness

Дата: 2026-08-09
Автор: Mavis
Контекст: юзер сообщил о проблемах в балансере при работе с reasoning-моделями (gemma-4, qwen3.6) через OpenWebUI и Cline. Также: долгие сессии падают с "balancer finishes response while backend still working".

## Live-диагностика (уже проведена, 2026-08-09)

**gemma-4 (5GB Q4_K_M, RTX 3070 8GB, n_ctx=65536):**

| Endpoint                              | Status         | Latency  | Content keys                                  |
|---------------------------------------|----------------|----------|-----------------------------------------------|
| cppworker :18092 /v1/chat             | HTTP 200       | 89.4s    | `content`, `reasoning_content`, `role` ✓       |
| balancer :18080 /v1/chat              | **TIMEOUT 180s** (header await) | - | - |
| balancer :18080 /api/chat             | **TIMEOUT 180s** (header await) | - | - |

**Лог балансера** (proxy-openai 180s timeout):
```
"caller":"balancer/llamacpp_handlers_inference.go:145","msg":"handleOpenAIChatCompletions: upstream request failed",
"error":"Post ...: net/http: timeout awaiting response headers","error_type":"timeout"
```

→ Подтверждён баг #4: cppworker для non-streaming запросов буферизирует всю генерацию и
отдаёт HTTP-заголовки ТОЛЬКО после завершения. Балансер с `ResponseHeaderTimeout`
(даже 300s в bundled) убивает соединение, не дождавшись headers.

---

## BUG SURFACE — что нужно покрыть тестами

### BUG #1: `reasoning_content` теряется в SSE→NDJSON трансляции (OpenWebUI streaming)

**Файл:** `internal/balancer/llamacpp_translate_resp.go::translateSSEChatToOllama` (line 295+)
**Симптом:** OpenWebUI вызывает `/api/chat` стримингом, получает только `content`, но НЕ получает `reasoning_content` в `message.thinking`. У пользователя reasoning-секция не отображается.
**Причина:** cppworker эмитит SSE-чанк `{"choices":[{"delta":{"reasoning_content":"..."}}]}`. Транслятор `translateSSEChatToOllama` не извлекает `delta.reasoning_content`.

**Тесты:**
- `TestTranslateSSEChatToOllama_ReasoningContent` — extract `delta.reasoning_content` → `message.thinking`
- `TestTranslateSSEChatToOllama_ReasoningContent_MultipleChunks` — accumulate multiple reasoning chunks
- `TestTranslateSSEChatToOllama_ReasoningAndContent` — mixed: reasoning + content in same/different chunks
- `TestTranslateSSEChatToOllama_ReasoningWithToolCalls` — reasoning + tool_calls coexistence

### BUG #2: `reasoning_content` теряется в non-streaming /api/chat (OpenWebUI non-stream)

**Файл:** `internal/balancer/llamacpp_translate_resp.go::translateOpenAIChatToOllama` (line 116+)
**Симптом:** OpenWebUI в non-stream режиме получает `content` + `role` без `reasoning` поля.
**Причина:** Транслятор `translateOpenAIChatToOllama` не маппит `message.reasoning_content` → `message.reasoning`.

**Тесты:**
- `TestTranslateOpenAIChatToOllama_ReasoningContent` — extract `message.reasoning_content` → `message.reasoning`
- `TestTranslateOpenAIChatToOllama_ReasoningContentWithTools` — reasoning + tool_calls

### BUG #3: `reasoning_content` не пробрасывается в /v1/chat/completions SSE passthrough (Cline)

**Файл:** `internal/balancer/llamacpp_transport.go::proxyRequestLlamaCpp` (line 535-588)
**Симптом:** Cline получает reasoning_content непоследовательно — иногда эмитится, иногда теряется.
**Причина:** passthrough фильтрует chunks через `filterOpenAIStreamingLine` и `extractToolCallsFromSSEContent`, но нет явного сохранения reasoning_content. Нужен тест.

**Тесты:**
- `TestProxyRequestLlamaCpp_OpenAIChatSSE_PreservesReasoningContent` — full SSE stream passthrough

### BUG #4: Non-streaming /v1/chat requests падают по ResponseHeaderTimeout

**Файл:** `internal/balancer/llamacpp_handlers_inference.go::handleOpenAIChatCompletions` (line 142)
**Симптом:** balancer таймаутит non-streaming запросы к reasoning-моделям, которые генерируют 60-90+ секунд.
**Причина:** cppworker буферизирует non-streaming ответ и не отправляет HTTP headers пока не закончит. Balancer's `firstByteTimeout` (300s) для gemma-4 в большинстве случаев хватает, но для qwen3.6 35B на cold load (90s) с reasoning (60-120s) не хватает. Также: retry attempt идёт на тот же backend без backoff → ложный "backend unhealthy".

**Тесты:**
- `TestHandleOpenAIChatCompletions_LongGenerationPassesThrough` — mock backend that takes 30s to send headers, should succeed
- `TestHandleOpenAIChatCompletions_HeaderTimeoutDoesntMarkBackendUnhealthy` — после timeout backend остаётся healthy
- `TestHandleOpenAIChatCompletions_RetryAfterTimeout` — retry с другим backend

### BUG #5: qwen3.6 specific (35B-A3B MoE, 22GB, soft-thinking)

**Файл:** `cmd/cppworker/reasoning_content.go::SplitReasoningContent` (already exists)
**Симптом:** qwen3.6 может использовать `<reasoning>` (без атрибутов) или другие форматы тегов.
**Причина:** MoE-архитектура с другим chat template; reasoning может приходить с префиксом `\n\n` или специальным system-prompt.

**Тесты:**
- `TestSplitReasoningContent_Qwen36_Style` — qwen3.6 specific format
- `TestCppWorker_RoutingDecision_Qwen36` — cppworker распознаёт qwen3.6 как reasoning
- `TestBalancer_FirstByteTimeout_Qwen36` — 35B модель получает адекватный firstByteTimeout

### BUG #6: Long session → premature stream close

**Файл:** `internal/balancer/llamacpp_transport.go::proxyRequestLlamaCpp` (line 832+)
**Симптом:** Долгая сессия (Cline, много tool calls) → балансер внезапно закрывает стрим с `finish_reason: "truncated"`, хотя модель ещё генерирует.
**Причина:** При reload n_ctx (cppworker unload → reload) во время активного streaming — балансер видит TCP close → помечает как truncation.

**Тесты:**
- `TestProxyRequestLlamaCpp_StreamTruncationDuringReload` — mock backend that closes connection mid-stream (mimicking n_ctx reload), should NOT emit truncated if reload is in progress
- `TestProxyRequestLlamaCpp_StreamActiveDuringReload` — backend says "loading model" in middle of stream, should not abort

### BUG #7: OpenWebUI /api/chat heartbeat (NDJSON) конфликтует с reasoning

**Файл:** `internal/balancer/streaming.go::handleStreamingResponse` (line 152-158)
**Симптом:** OpenWebUI в режиме streaming получает периодический NDJSON-heartbeat `{"message":{"content":""}}`. Если reasoning content приходит сразу после — OpenWebUI может сбросить сессию или отобразить пустое сообщение.
**Причина:** Heartbeat не содержит `done:false` правильно эмитится, но reasoning секция приходит отдельным chunk'ом; UI интерпретирует пустой content как новый message.

**Тесты:**
- `TestStreaming_HeartbeatDuringReasoning` — heartbeat не прерывает reasoning секцию

### BUG #8: gemma-4 + Cline long session → gemma-4 reload

**Файл:** n/a (cppworker-side), но balancer должен gracefully handle
**Симптом:** Cline session достигает лимита n_ctx → cppworker reload → balancer обрывает стрим.
**Причина:** При reload cppworker убивает текущий контекст, balancer видит TCP close, шлёт "truncated".

**Тесты:**
- `TestProxyRequestLlamaCpp_NCtxReloadDuringStream` — stream переживает reload, клиент получает 503+Retry-After

---

## Live-тесты, которые нужно провести

| # | Сценарий                                                   | Endpoint                                | Ожидаемый результат                                       |
|---|------------------------------------------------------------|-----------------------------------------|-----------------------------------------------------------|
| 1 | gemma-4 reasoning через proxy OpenAI                       | :18080/v1/chat (non-stream)             | 200 + reasoning_content + content                          |
| 2 | gemma-4 reasoning через proxy OpenAI streaming            | :18080/v1/chat (stream)                 | stream содержит `delta.reasoning_content`                  |
| 3 | gemma-4 reasoning через proxy Ollama (OpenWebUI)           | :18080/api/chat (stream)                | NDJSON содержит `message.thinking`                        |
| 4 | qwen3.6 reasoning через proxy OpenAI                       | :18080/v1/chat (stream)                 | stream содержит `delta.reasoning_content`                  |
| 5 | qwen3.6 через proxy Ollama (OpenWebUI)                     | :18080/api/chat (stream)                | NDJSON содержит `message.thinking`                        |
| 6 | gemma-4 + tools через Cline                                | :18080/v1/chat (stream, tools=true)     | tool_calls + content parsed correctly                     |
| 7 | long session (Cline 20+ turns)                             | :18080/v1/chat (stream)                 | выживает n_ctx reload, не обрывается                     |

## Файлы для изменения

| Файл                                                                    | Что менять                                    |
|-------------------------------------------------------------------------|-----------------------------------------------|
| `internal/balancer/llamacpp_translate_resp.go::translateSSEChatToOllama` | extract `delta.reasoning_content`             |
| `internal/balancer/llamacpp_translate_resp.go::translateSSEGenerateToOllama` | extract `delta.reasoning_content` (top-level) |
| `internal/balancer/llamacpp_translate_resp.go::translateOpenAIChatToOllama` | map `message.reasoning_content` → `message.reasoning` |
| `internal/balancer/llamacpp_handlers_inference.go`                      | adjust non-stream timeout / retry logic      |
| `internal/balancer/llamacpp_transport.go`                               | preserve reasoning_content in SSE passthrough |

## Файлы тестов

| Файл                                                                                | Что покрывает                          |
|-------------------------------------------------------------------------------------|----------------------------------------|
| `internal/balancer/llamacpp_translate_resp_reasoning_test.go` (NEW)                  | Bugs #1, #2                            |
| `internal/balancer/llamacpp_transport_reasoning_passthrough_test.go` (NEW)           | Bug #3                                 |
| `internal/balancer/llamacpp_handlers_inference_long_gen_test.go` (NEW)               | Bug #4                                 |
| `internal/balancer/llamacpp_transport_reload_test.go` (NEW)                          | Bugs #6, #8                            |
| `internal/balancer/streaming_heartbeat_reasoning_test.go` (NEW)                      | Bug #7                                 |
| `cmd/cppworker/reasoning_qwen36_test.go` (NEW)                                      | Bug #5                                 |
