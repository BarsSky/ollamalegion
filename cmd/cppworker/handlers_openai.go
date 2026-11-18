package main

import (
	"encoding/json"
	"fmt"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
	"strings"
	"sync"
)

// ============================================================
// OpenAI-compatible /v1/ handlers
// ============================================================

// openAICompletionRequest — структура запроса OpenAI /v1/completions
//
// IMPORTANT: Temperature, TopP, PresencePenalty, FrequencyPenalty используют
// *float64 — см. комментарий к openAIChatCompletionRequest (Round 16 follow-up
// fix). nil = использовать дефолт cppworker; *0.0 = explicit 0 от клиента.
type openAICompletionRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	Suffix    string `json:"suffix,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
	// MaxOutputTokens — R69 (2026-09-23): алиас max_tokens (Cline и другие
	// клиенты, использующие один код для разных API).
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// Sampling params — *float64 для различения "unset" vs "explicit 0".
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	N                int      `json:"n,omitempty"`
	Stream           bool     `json:"stream,omitempty"`
	Echo             bool     `json:"echo,omitempty"`
	Stop             []string `json:"stop,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	Seed             int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	// > effective n_ctx → C-bridge вернёт informative ошибку с предложением
	// перезагрузить модель через /api/models/reload.
	NumCtx int `json:"num_ctx,omitempty"`
	// StreamOptions — параметры стриминга (Round 15, 2026-07-29: теперь и
	// для legacy /v1/completions endpoint). include_usage добавляет usage chunk
	// в финальный SSE чанк — требуется Cline/прочим IDE-агентам для трекинга
	// контекста. По дефолту ВКЛЮЧЕНО (см. includeUsageEffective ниже).
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`

	// ===== R65d (2026-09-20): те же расширения, что и в chat-структуре =====
	// См. комментарий к openAIChatCompletionRequest: строгий декодер отвечал
	// 400 на валидные поля OpenAI/Ollama-клиентов.
	TopK           *float64           `json:"top_k,omitempty"`
	NumPredict     *int               `json:"num_predict,omitempty"`
	Logprobs       *int               `json:"logprobs,omitempty"` // int у completions (не bool)
	User           string             `json:"user,omitempty"`
	LogitBias      map[string]float64 `json:"logit_bias,omitempty"`
	BestOf         *int               `json:"best_of,omitempty"`
	ResponseFormat json.RawMessage    `json:"response_format,omitempty"`
	KeepAlive      json.RawMessage    `json:"keep_alive,omitempty"`
	Format         json.RawMessage    `json:"format,omitempty"`
	Think          json.RawMessage    `json:"think,omitempty"`
	Truncate       *bool              `json:"truncate,omitempty"`
}

// logitBiasWarnOnce — R66b: logit_bias принимается, но НЕ применяется
// (C-bridge не поддерживает). Предупреждаем один раз за жизнь процесса, чтобы
// не спамить на каждый запрос клиента, который всегда шлёт это поле.
var logitBiasWarnOnce sync.Once

// warnLogitBiasOnce — однократное честное предупреждение о том, что
// logit_bias проигнорирован.
func warnLogitBiasOnce(field string, count int) {
	logitBiasWarnOnce.Do(func() {
		logger.Get().Warnw(i18n.T("cppworker.handlers_openai.logit_bias_ignored"),
			"field", field, "entries", count,
			"hint", i18n.T("cppworker.handlers_openai.logit_bias_hint"))
	})
}

// openAIChatCompletionRequest — структура запроса OpenAI /v1/chat/completions
//
// IMPORTANT (Round 16 follow-up fix, 2026-07-30): поля с optional zero values
// (Temperature, TopP) используют *float64 чтобы отличить "не задано клиентом"
// (nil → использовать дефолт cppworker 0.7/0.9) от "клиент явно задал 0"
// (*0.0 → greedy/no-top_p). Раньше использовался float64 + `if > 0` — это
// ИГНОРИРОВАЛО temperature=0 от клиента (Cline/Aider/Continue все шлют
// temperature=0 для tool calls) и подставляло 0.7 по умолчанию → не-greedy
// sampling, недетерминированные tool calls. См. _audit_test_temp0_2.py для repro.
type openAIChatCompletionRequest struct {
	Model     string              `json:"model"`
	Messages  []openAIChatMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
	// Temperature / TopP — pointer types: nil = не задано, *0.0 = explicit 0.
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	N           int      `json:"n,omitempty"`
	Stream      bool     `json:"stream,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Seed        int      `json:"seed,omitempty"`
	// NumCtx — per-request переопределение n_ctx (OpenAI-совместимый).
	// 0 = использовать n_ctx модели. > 0 → C-bridge pre-flight check.
	NumCtx int `json:"num_ctx,omitempty"`
	// Tools — определения инструментов для function calling (OpenAI / Ollama совместимый формат).
	Tools []openAITool `json:"tools,omitempty"`
	// ToolChoice — управление выбором инструмента ("auto", "none", "required", или {"type":"function","function":{"name":"..."}}).
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// StreamOptions — параметры стриминга (include_usage добавляет usage в финальный SSE чанк).
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`

	// ===== R65d (2026-09-20): поля, которые реально шлют OpenAI-клиенты =====
	//
	// Аудит 2026-09-20 (находка 1.1): OpenAI SDK, OpenWebUI (OpenAI mode) и ряд
	// инструментов отправляют эти поля, а строгий декодер
	// (types.DecodeJSONRequest → DisallowUnknownFields) отвечал
	// HTTP 400 `unknown field "..."` — то есть отказ на валидном запросе.
	// Поля добавлены и проброшены в GenerationParams (см. buildOpenAIChatParams).
	//
	// Pointer-типы там, где важно отличить "не задано" от "explicit 0"
	// (frequency_penalty=0 — валидное значение).
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	// TopK / RepeatPenalty / MinP — Ollama-расширения, которые часть клиентов
	// (и балансер в mixed-режиме) кладёт в top-level.
	TopK          *float64 `json:"top_k,omitempty"`
	RepeatPenalty *float64 `json:"repeat_penalty,omitempty"`
	MinP          *float64 `json:"min_p,omitempty"`
	// NumPredict — Ollama-алиас max_tokens на верхнем уровне.
	NumPredict *int `json:"num_predict,omitempty"`
	// MaxOutputTokens — R69 (2026-09-23): ещё один алиас max_tokens, который
	// шлёт реальный клиент Cline (наблюдалось на Ollama-провайдере; в
	// OpenAI-совместимом режиме поле тоже встречается). Без него строгий
	// декодер отвечал 400 `invalid JSON: json: unknown field "max_output_tokens"`.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// ResponseFormat — {"type":"json_object"} / {"type":"json_schema",...}.
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
	// ParallelToolCalls — OpenWebUI/OpenAI SDK шлют его вместе с tools.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// User — OpenAI-идентификатор конечного пользователя (трекинг/лимиты).
	User string `json:"user,omitempty"`
	// LogitBias — принимается для совместимости с OpenAI-клиентами и
	// ИГНОРИРУЕТСЯ. C-bridge (c/bridge/bridge.h) не экспонирует ни
	// llama_sample_logit_bias, ни грамматики: наружу доступен только
	// SampleToken(logits, temperature, seed). Поле не попадает ни в
	// bridge.GenerationParams, ни в params из buildGenerationParams.
	//
	// Прежний комментарий («применяется C-bridge») был ложным — оператор,
	// выставив logit_bias, молча не получал эффекта. Теперь при непустом
	// значении пишем однократное предупреждение в лог (warnLogitBiasOnce),
	// чтобы это не выглядело как работающая настройка.
	LogitBias map[string]float64 `json:"logit_bias,omitempty"`
	// KeepAlive / Format / Think / Truncate / Logprobs — Ollama-поля: клиенты,
	// использующие один код для обоих API (в т.ч. балансер), могут прислать их
	// и сюда. Принимаем, чтобы не отвечать 400.
	KeepAlive   json.RawMessage `json:"keep_alive,omitempty"`
	Format      json.RawMessage `json:"format,omitempty"`
	Think       json.RawMessage `json:"think,omitempty"`
	Truncate    *bool           `json:"truncate,omitempty"`
	Logprobs    *bool           `json:"logprobs,omitempty"`
	TopLogprobs *int            `json:"top_logprobs,omitempty"`
}

// openAIStreamOptions — подмножество OpenAI `stream_options`.
// Используется Cline для отслеживания контекстного окна (usage-чанк в SSE).
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	// R248/B3 (2026-10-27): картинки сообщения. Балансер поднимает image-части
	// из content[] (image_url / Anthropic image) в это поле, чтобы cppworker
	// получил их как нативные images — иначе картинка терялась по дороге.
	Images []string `json:"images,omitempty"`

	// ReasoningContent — R106 (2026-10-11): reasoning ПРЕДЫДУЩЕГО ответа
	// ассистента, который клиент присылает ОБРАТНО в истории.
	//
	// OpenAI-поверхность cppworker САМА отдаёт `reasoning_content`
	// (handlers_openai.go:630-690), а клиент (Cline/OpenWebUI/OpenAI SDK) при
	// продолжении сохранённой сессии отправляет историю целиком, включая это
	// поле. Строгий декодер отвечал
	//
	//	400 {"error":"invalid JSON: json: unknown field \"reasoning_content\""}
	//
	// Ровно тот же класс дефекта, что `thinking` на Ollama-поверхности
	// (handlers_chat.go, chatMessage.Thinking).
	//
	// Принимается и в промпт не попадает (openAIToChatMessage переносит только
	// role/content/tool_calls).
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// Thinking — тот же history-echo в Ollama-написании: клиент с одним кодом на
	// оба API (и балансер в mixed-режиме) может прислать его и сюда.
	Thinking string `json:"thinking,omitempty"`
}

// openAIToChatMessage converts an openAIChatMessage slice to chatMessage slice
// for use with the canonical buildChatPrompt from handlers_chat.go.
// Сохраняет tool_calls, tool_call_id, name для поддержки function calling.
func openAIToChatMessage(msgs []openAIChatMessage) []chatMessage {
	result := make([]chatMessage, 0, len(msgs))
	for _, m := range msgs {
		cm := chatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// R248/B3: картинки едут дальше в общий image-инференс.
		if len(m.Images) > 0 {
			cm.Images = m.Images
		}
		if len(m.ToolCalls) > 0 {
			cm.ToolCalls = ollamaToolCallsFromOpenAI(m.ToolCalls)
		}
		result = append(result, cm)
	}
	return result
}

// buildNaiveChatPrompt — fallback when GGUF has no chat template (or bridge_apply_chat_template fails).
// Строит простой chat-формат, совместимый с gemma-style instruction-tuned моделями.
// Если в имени модели встречается "gemma" — используется формат <start_of_turn>user/model<end_of_turn>,
// иначе — формат <|user|>...<|assistant|>, оба совместимы с большинством chat-моделей llama.cpp.
// Поддерживает tool-сообщения и assistant-сообщения с tool_calls.
func buildNaiveChatPrompt(msgs []openAIChatMessage, modelName string) string {
	isGemma := strings.Contains(strings.ToLower(modelName), "gemma")
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>system\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|system|>\n%s<|end|>\n", m.Content)
			}
		case "user":
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>user\n%s<end_of_turn>\n", m.Content)
			} else {
				fmt.Fprintf(&sb, "<|user|>\n%s<|end|>\n", m.Content)
			}
		case "assistant":
			content := m.Content
			// Если есть tool_calls, сериализуем их в content
			if len(m.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(m.ToolCalls)
				if err == nil {
					if content != "" {
						content += "\n" + string(tcJSON)
					} else {
						content = string(tcJSON)
					}
				}
			}
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>model\n%s<end_of_turn>\n", content)
			} else {
				fmt.Fprintf(&sb, "<|assistant|>\n%s<|end|>\n", content)
			}
		case "tool":
			content := m.Content
			if m.Name != "" {
				content = "tool_call_result(" + m.Name + "): " + content
			} else if m.ToolCallID != "" {
				content = "tool_call_result(" + m.ToolCallID + "): " + content
			}
			if isGemma {
				fmt.Fprintf(&sb, "<start_of_turn>tool\n%s<end_of_turn>\n", content)
			} else {
				fmt.Fprintf(&sb, "<|tool|>\n%s<|end|>\n", content)
			}
		}
	}
	if isGemma {
		sb.WriteString("<start_of_turn>model\n")
	} else {
		sb.WriteString("<|assistant|>\n")
	}
	return sb.String()
}

// augmentSystemWithTools injects tool definitions into the system message.
// Если system-сообщения нет — создаёт новое.
func augmentSystemWithTools(msgs []chatMessage, tools []openAITool) []chatMessage {
	return augmentSystemWithToolsForChoice(msgs, tools, nil)
}

// augmentSystemWithToolsForChoice — то же, но с учётом tool_choice клиента.
//
// R246 (2026-10-26): вынужденный выбор инструмента (`"required"` или
// {"type":"function","function":{"name":"…"}}) уходит в промпт требованием.
// Без этого клиент, который ОБЯЗЫВАЕТ модель вызвать инструмент
// (структурированный вывод LangChain, MCP-мосты, плагины), получал обычный
// текст: поле принималось, но применялось только `"none"`.
func augmentSystemWithToolsForChoice(msgs []chatMessage, tools []openAITool, choice json.RawMessage) []chatMessage {
	if len(tools) == 0 {
		return msgs
	}
	toolsPrompt := buildToolsSystemPrompt(tools)
	toolsPrompt = withToolChoiceInstruction(toolsPrompt, choice)
	if toolsPrompt == "" {
		return msgs
	}
	// Ищем существующее system-сообщение и дополняем его
	for i := range msgs {
		if msgs[i].Role == "system" {
			msgs[i].Content = msgs[i].Content + "\n\n" + toolsPrompt
			return msgs
		}
	}
	// Нет system-сообщения — создаём новое в начале
	result := make([]chatMessage, 0, len(msgs)+1)
	result = append(result, chatMessage{Role: "system", Content: toolsPrompt})
	result = append(result, msgs...)
	return result
}
