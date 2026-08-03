package main

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// ============================================================
// Embeddings handlers
// ============================================================

// handleEmbeddings — внутренний endpoint /api/embeddings.
func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req embeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" || req.Input == "" {
		writeError(w, http.StatusBadRequest, "model and input are required")
		return
	}
	embeddings, err := backend.GetEmbeddings(req.Model, req.Input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":      req.Model,
		"embeddings": embeddings,
	})
}

// handleOllamaEmbeddings — Ollama /api/embeddings.
// Принимает "prompt" или "input", маппит на backend.GetEmbeddings.
func handleOllamaEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model  string `json:"model"`
		Input  string `json:"input"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	text := req.Input
	if text == "" {
		text = req.Prompt
	}
	if text == "" {
		writeError(w, http.StatusBadRequest, "input or prompt is required")
		return
	}

	// Ленивая загрузка модели для embeddings
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		logger.Get().Errorw("embeddings: model load failed", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	embeddings, err := backend.GetEmbeddings(req.Model, text)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"embedding": embeddings,
	})
}

// handleOllamaEmbed — Ollama v0.1.14+ /api/embed.
//
// Принимает `model` + `input` (string ИЛИ []string). Возвращает {"embeddings": [[...]]}.
// Round 21 hotfix (2026-08-03): раньше не было, OpenWebUI 0.4+ использует
// именно /api/embed вместо legacy /api/embeddings.
//
// Отличия от /api/embeddings:
//   - input: string | []string (batch поддержка)
//   - response: {"model":..., "embeddings":[[...], [...]]} (массив массивов)
//   - поддержка `truncate` и `options` (пока игнорируем — truncation в cppworker
//     не реализован, options для embeddings не нужны)
func handleOllamaEmbed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model    string        `json:"model"`
		Input    interface{}   `json:"input"`    // string OR []string
		Truncate *bool         `json:"truncate,omitempty"`
		Options  *map[string]interface{} `json:"options,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Input == nil {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}

	// Нормализуем input → []string
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				inputs = append(inputs, s)
			} else {
				writeError(w, http.StatusBadRequest, "input array must contain only strings")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "input must be string or []string")
		return
	}
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "input must contain at least one string")
		return
	}

	// Ленивая загрузка модели для embeddings
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		logger.Get().Errorw("embed: model load failed", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// GetEmbeddings поддерживает только 1 input за раз. Для batch — вызываем
	// в цикле. (В будущем можно оптимизировать через batched API cppworker'а.)
	embeddings := make([][]float32, 0, len(inputs))
	totalTokens := 0
	for _, text := range inputs {
		vec, err := backend.GetEmbeddings(req.Model, text)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
			return
		}
		embeddings = append(embeddings, vec)
		totalTokens += buildPromptTokens(req.Model, text)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":      req.Model,
		"embeddings": embeddings,
		"prompt_eval_count": totalTokens,
	})
}

// handleOpenAIEmbeddings — OpenAI /v1/embeddings.
//
// Принимает `model` + `input` (string ИЛИ []string). Возвращает OpenAI-формат:
//   {"object":"list",
//    "data":[{"object":"embedding","embedding":[...],"index":0}, ...],
//    "model":"...",
//    "usage":{"prompt_tokens":N,"total_tokens":N}}
//
// Round 22 (2026-08-03): раньше cppworker не имел /v1/embeddings, balancer
// проксировал напрямую → cppworker 404 → 30s timeout. Теперь cppworker сам
// реализует OpenAI-стиль, balancer'у достаточно проксировать as-is.
func handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		Model          string      `json:"model"`
		Input          interface{} `json:"input"` // string OR []string
		EncodingFormat string      `json:"encoding_format,omitempty"` // "float" (default) or "base64"
		User           string      `json:"user,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Input == nil {
		writeError(w, http.StatusBadRequest, "input is required")
		return
	}

	// Нормализуем input → []string
	var inputs []string
	switch v := req.Input.(type) {
	case string:
		inputs = []string{v}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				inputs = append(inputs, s)
			} else {
				writeError(w, http.StatusBadRequest, "input array must contain only strings")
				return
			}
		}
	default:
		writeError(w, http.StatusBadRequest, "input must be string or []string")
		return
	}
	if len(inputs) == 0 {
		writeError(w, http.StatusBadRequest, "input must contain at least one string")
		return
	}

	// Ленивая загрузка модели для embeddings
	if err := ensureModelLoaded(req.Model); err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		logger.Get().Errorw("openai-embeddings: model load failed", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// GetEmbeddings поддерживает только 1 input за раз. Для batch — вызываем
	// в цикле.
	data := make([]map[string]interface{}, 0, len(inputs))
	totalTokens := 0
	for idx, text := range inputs {
		vec, err := backend.GetEmbeddings(req.Model, text)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "embeddings failed: "+err.Error())
			return
		}
		data = append(data, map[string]interface{}{
			"object":    "embedding",
			"embedding": vec,
			"index":     idx,
		})
		totalTokens += buildPromptTokens(req.Model, text)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
		"model":  req.Model,
		"usage": map[string]interface{}{
			"prompt_tokens": totalTokens,
			"total_tokens":  totalTokens,
		},
	})
}
