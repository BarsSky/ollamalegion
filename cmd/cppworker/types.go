package main

import "time"

// ============================================================
// Request/Response types
// ============================================================

// generateOptions — Ollama-style nested options (options.*).
// Полный набор опций, маппится на bridge.GenerationParams.
type generateOptions struct {
	Temperature     float64     `json:"temperature"`
	TopP            float64     `json:"top_p"`
	TopK            int         `json:"top_k"`
	MinP            float64     `json:"min_p"`
	TypicalP        float64     `json:"typical_p"`
	TfsZ            float64     `json:"tfs_z"`
	NumPredict      int         `json:"num_predict"`
	NumKeep         int         `json:"num_keep"`
	RepeatPenalty   float64     `json:"repeat_penalty"`
	FrequencyPenalty float64   `json:"frequency_penalty"`
	PresencePenalty float64    `json:"presence_penalty"`
	RepeatLastN     int         `json:"repeat_last_n"`
	Mirostat        int         `json:"mirostat"`
	MirostatTau     float64     `json:"mirostat_tau"`
	MirostatEta     float64     `json:"mirostat_eta"`
	Seed            int         `json:"seed"`
	NumCtx          int         `json:"num_ctx"`
	Stop            interface{} `json:"stop"` // string или []string
}

type generateRequest struct {
	Model            string          `json:"model"`
	Prompt           string          `json:"prompt"`
	System           string          `json:"system,omitempty"`
	Template         string          `json:"template,omitempty"`
	Raw              bool            `json:"raw"`
	Format           string          `json:"format,omitempty"`
	KeepAlive        string          `json:"keep_alive,omitempty"`
	Context          []int           `json:"context,omitempty"`
	Images           []string        `json:"images,omitempty"` // not supported yet, accepted for compatibility
	Options          generateOptions `json:"options"`
	Temperature      float64         `json:"temperature,omitempty"`
	TopP             float64         `json:"topP,omitempty"`
	TopK             int             `json:"topK,omitempty"`
	MinP             float64         `json:"minP,omitempty"`
	TypicalP         float64         `json:"typicalP,omitempty"`
	TfsZ             float64         `json:"tfsZ,omitempty"`
	MaxTokens        int             `json:"maxTokens,omitempty"`
	RepeatPenalty    float64         `json:"repeatPenalty,omitempty"`
	FrequencyPenalty float64         `json:"frequencyPenalty,omitempty"`
	PresencePenalty  float64         `json:"presencePenalty,omitempty"`
	Seed             int             `json:"seed,omitempty"`
	NumCtx           int             `json:"numCtx,omitempty"`
	Stream           bool            `json:"stream"`
	// _keepAliveDuration — парсится из req.KeepAlive в normalizeGenerateRequest.
	// Используется в handleGenerate/handleOllamaGenerate для продления lastUsedAt
	// модели после успешного ответа. 0 = дефолт (30 минут).
	// Не экспортируется в JSON (unexported field).
	_keepAliveDuration time.Duration `json:"-"`
}

type generateResponse struct {
	Model            string  `json:"model"`
	Response         string  `json:"response"`
	Done             bool    `json:"done"`
	Tokens           int     `json:"tokens"`
	DurationMs       int64   `json:"durationMs"`
	TokensPerSec     float64 `json:"tokensPerSec,omitempty"`
	TotalDuration    int64   `json:"total_duration,omitempty"`
	LoadDuration     int64   `json:"load_duration,omitempty"`
	PromptEvalCount  int     `json:"prompt_eval_count,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalCount        int     `json:"eval_count,omitempty"`
	EvalDuration     int64   `json:"eval_duration,omitempty"`
}

type streamChunk struct {
	Model    string `json:"model"`
	Token    string `json:"token"`
	Response string `json:"response,omitempty"`
	Done     bool   `json:"done"`
}

type loadModelRequest struct {
	Name          string    `json:"name"`
	Path          string    `json:"path,omitempty"`
	GPULayers     *int      `json:"gpuLayers,omitempty"`
	ContextSize   *int      `json:"contextSize,omitempty"`
	BatchSize     *int      `json:"batchSize,omitempty"`
	TensorSplit   []float32 `json:"tensorSplit,omitempty"`
	FlashAttnType *int      `json:"flashAttn,omitempty"`
	NUMA          *bool     `json:"numa,omitempty"`
	UseMmap       *bool     `json:"useMmap,omitempty"`
}

type embeddingsRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type reloadModelRequest struct {
	Name        string `json:"name"`
	ContextSize *int   `json:"contextSize,omitempty"`
	BatchSize   *int   `json:"batchSize,omitempty"`
	GPULayers   *int   `json:"gpuLayers,omitempty"`
	FlashAttn   *int   `json:"flashAttn,omitempty"`
	NUMA        *bool  `json:"numa,omitempty"`
	UseMmap     *bool  `json:"useMmap,omitempty"`
	Force       *bool  `json:"force,omitempty"`
}
