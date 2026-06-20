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
