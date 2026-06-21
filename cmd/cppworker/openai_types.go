// openai_types.go — Shared types for OpenAI-compatible tool calling (function calling).
package main

import "encoding/json"

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
