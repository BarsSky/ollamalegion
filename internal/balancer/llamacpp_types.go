package balancer

import (
	"encoding/json"
)

// cppWorkerModelState — минимальная проекция состояния модели на cppworker.
type cppWorkerModelState struct {
	Name  string
	Path  string
	State string // "unloaded" | "loading" | "loaded" | "error"
}

// cppWorkerModelsNative — нативный ответ cppworker /api/models.
type cppWorkerModelsNative struct {
	Count  int `json:"count"`
	Models []struct {
		Name          string `json:"name"`
		Path          string `json:"path,omitempty"`
		State         string `json:"state,omitempty"`
		SizeBytes     int64  `json:"sizeBytes,omitempty"`
		NLayers       int    `json:"nLayers,omitempty"`
		NHeads        int    `json:"nHeads,omitempty"`
		NEmbd         int    `json:"nEmbd,omitempty"`
		NVocab        int    `json:"nVocab,omitempty"`
		ContextSize   int    `json:"contextSize,omitempty"`
		GPULayers     int    `json:"gpuLayers,omitempty"`
		ActiveQueries int    `json:"activeQueries,omitempty"`
		TotalQueries  int    `json:"totalQueries,omitempty"`
		LoadedAt      string `json:"loadedAt,omitempty"`
	} `json:"models"`
}

// LlamaCppChatResponse — структура для парсинга ответа llama.cpp /v1/chat/completions
type LlamaCppChatResponse struct {
	ID      string `json:"id,omitempty"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string             `json:"content,omitempty"`
			Role      string             `json:"role,omitempty"`
			ToolCalls []json.RawMessage `json:"tool_calls,omitempty"`
		} `json:"delta,omitempty"`
		Message struct {
			Content   string             `json:"content,omitempty"`
			Role      string             `json:"role,omitempty"`
			ToolCalls []json.RawMessage `json:"tool_calls,omitempty"`
		} `json:"message,omitempty"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens,omitempty"`
		CompletionTokens int `json:"completion_tokens,omitempty"`
		TotalTokens      int `json:"total_tokens,omitempty"`
	} `json:"usage,omitempty"`
}
