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
//
// R346 (2026-10-30): теги — snake_case, как их реально печатает воркер.
//
// ЗАЧЕМ ПРАВКА. Раньше здесь стояли camelCase-теги (`sizeBytes`, `contextSize`,
// `nLayers`, `gpuLayers`, `loadedAt`), а `cmd/cppworker/handlers_model.go` печатает
// snake_case (`size_bytes`, `context_size`, `n_layers`, `gpu_layers_actual`,
// `loaded_at`). Из-за этого handleModels — «список моделей в НАТИВНОМ формате
// cppworker'а» — декодировал нули: в ответе оставались только name/path/state, а
// размер, окно, слои и время загрузки молча выпадали (omitempty). Ту же ошибку
// раньше поймали в поллере (llamacpp_metrics_poller.go, комментарий R45) и
// задокументировали как F-15 в docs/archive/audits/AUDIT-balancer-cppworker-wire.md,
// но структура этого пути осталась непоправленной.
//
// n_heads и total_queries из этого ответа УБРАНЫ: воркер их в /api/models не
// печатает вовсе (n_heads — только /api/show и /config/runtime, total_queries —
// только /config/runtime, см. docs/archive/audits/AUDIT-balancer-cppworker-wire.md
// F-05), поэтому поля были вечным нулём и только выглядели частью контракта.
type cppWorkerModelsNative struct {
	Count  int `json:"count"`
	Models []struct {
		Name        string `json:"name"`
		Path        string `json:"path,omitempty"`
		State       string `json:"state,omitempty"`
		SizeBytes   int64  `json:"size_bytes,omitempty"`
		NLayers     int    `json:"n_layers,omitempty"`
		NEmbd       int    `json:"n_embd,omitempty"`
		NVocab      int    `json:"n_vocab,omitempty"`
		ContextSize int    `json:"context_size,omitempty"`
		// GPULayersActual — фактический offload (R248). Старое имя `gpu_layers`
		// поддерживаем ради заглушек и воркеров до R248: в /api/models воркер
		// печатает именно gpu_layers_actual.
		GPULayersActual int    `json:"gpu_layers_actual,omitempty"`
		GPULayers       int    `json:"gpu_layers,omitempty"`
		ActiveQueries   int    `json:"active_queries,omitempty"`
		LoadedAt        string `json:"loaded_at,omitempty"`
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
			Content   string            `json:"content,omitempty"`
			Role      string            `json:"role,omitempty"`
			ToolCalls []json.RawMessage `json:"tool_calls,omitempty"`
		} `json:"delta,omitempty"`
		Message struct {
			Content   string            `json:"content,omitempty"`
			Role      string            `json:"role,omitempty"`
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
