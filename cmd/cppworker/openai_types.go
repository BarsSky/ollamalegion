// openai_types.go — Shared types for OpenAI-compatible tool calling (function calling).
package main

import (
	"encoding/json"
	"strings"
)

// ============================================================
// Tool definitions (в запросе от клиента)
// ============================================================

// openAITool — определение инструмента (OpenAI-формат, совместимый с Ollama).
type openAITool struct {
	Type     string         `json:"type"`               // всегда "function"
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"` // JSON Schema object
}

// ============================================================
// Tool calls (в ответе от ассистента при finish_reason="tool_calls")
// ============================================================

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON-строка с аргументами
}

// ============================================================
// Ollama-native tool calls (R66b, 2026-09-22)
// ============================================================
//
// Ollama-нативный /api/chat использует ДРУГУЮ схему, чем OpenAI:
//
//	Ollama: {"function":{"name":"f","arguments":{"a":1}}}   // arguments — ОБЪЕКТ
//	OpenAI: {"function":{"name":"f","arguments":"{\"a\":1}"}} // arguments — СТРОКА
//
// До R66b cppworker и в ответе, и в запросе использовал OpenAI-форму
// (arguments-строка). Из-за этого:
//   * клиент Cline CLI (провайдер "ollama") получал объект там, где ждал
//     строку... точнее наоборот: он ждёт ОБЪЕКТ, а получал строку, повторно
//     её сериализовал и tool call «терял» аргументы → Cline сообщал
//     "Model returned empty response" и не вызывал инструменты;
//   * на втором шаге (assistant.tool_calls + tool-результат) cppworker падал
//     с 400 `cannot unmarshal object into Go struct field ... Arguments of
//     type string`, потому что клиент присылал arguments объектом.
//
// ollamaToolCall принимает ОБЕ формы на входе (UnmarshalJSON) и всегда отдаёт
// Ollama-форму (MarshalJSON: arguments — объект), как это делает настоящий
// Ollama-сервер.
type ollamaToolCall struct {
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function ollamaToolCallFunction `json:"function"`
}

type ollamaToolCallFunction struct {
	Name string `json:"name"`
	// Arguments хранится как raw JSON ЗНАЧЕНИЯ: объект/массив (Ollama) либо
	// строка с JSON внутри (OpenAI). Нормализуется в UnmarshalJSON.
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// UnmarshalJSON — принимает arguments как объект ИЛИ как строку (обе схемы).
func (f *ollamaToolCallFunction) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	f.Arguments = normalizeToolArgumentsRaw(raw.Arguments)
	return nil
}

// MarshalJSON — всегда отдаёт arguments как JSON-ОБЪЕКТ (Ollama-схема).
func (f ollamaToolCallFunction) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments,omitempty"`
	}{Name: f.Name, Arguments: f.Arguments})
}

// normalizeToolArgumentsRaw приводит arguments к raw JSON-значению:
// строка с JSON внутри → этот JSON; объект/массив → как есть; мусор → {}.
func normalizeToolArgumentsRaw(raw json.RawMessage) json.RawMessage {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return json.RawMessage("{}")
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		inner := strings.TrimSpace(asString)
		if json.Valid([]byte(inner)) {
			return json.RawMessage(inner)
		}
		// Строка, но не JSON — отдаём как JSON-строку.
		return raw
	}
	if json.Valid(raw) {
		return raw
	}
	return json.RawMessage("{}")
}

// openAIToolCallsFromOllama — конвертация в OpenAI-представление (arguments-строка)
// для внутренних путей (bridge/prompt/OpenAI-ответ).
func openAIToolCallsFromOllama(calls []ollamaToolCall) []openAIToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]openAIToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, openAIToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: openAIFunctionCall{
				Name:      c.Function.Name,
				Arguments: string(normalizeToolArgumentsRaw(c.Function.Arguments)),
			},
		})
	}
	return out
}

// ollamaToolCallsFromOpenAI — обратная конвертация (для нативного ответа /api/chat).
func ollamaToolCallsFromOpenAI(calls []openAIToolCall) []ollamaToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]ollamaToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, ollamaToolCall{
			ID:   c.ID,
			Type: c.Type,
			Function: ollamaToolCallFunction{
				Name:      c.Function.Name,
				Arguments: normalizeToolArgumentsRaw(json.RawMessage(c.Function.Arguments)),
			},
		})
	}
	return out
}

// ============================================================
// Bridge-side tool types
// ============================================================

// BridgeToolCall — используется при конвертации messages в bridge.ChatMessage.
type BridgeToolCall struct {
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function BridgeFunctionCall `json:"function,omitempty"`
}

type BridgeFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}
