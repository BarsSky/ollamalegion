// handlers_hf.go — HuggingFace search, file listing, download, and cancel handlers.
package main

import (
	"context"
		"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// ============================================================
// HuggingFace Handlers
// ============================================================

func handleHFSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	query := r.URL.Query().Get("query")
	if query == "" {
		writeError(w, http.StatusBadRequest, "query parameter is required")
		return
	}
	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := parseInt(l, 20); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	results, err := backend.HFDownloader().SearchModels(ctx, query, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}
	if results == nil {
		results = []cppbackend.HFModelRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"results": results, "count": len(results), "query": query,
	})
}

func handleHFFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId parameter is required")
		return
	}
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	files, err := backend.HFDownloader().ListModelFiles(ctx, modelID, revision)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list files failed: "+err.Error())
		return
	}
	if files == nil {
		files = []cppbackend.HFFileInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": files, "count": len(files), "modelId": modelID, "revision": revision,
	})
}

func handleHFDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req cppbackend.HFDownloadRequest
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	hfToken := r.Header.Get("X-HF-Token")
	if hfToken == "" {
		hfToken = r.Header.Get("Authorization")
		if strings.HasPrefix(hfToken, "Bearer ") {
			hfToken = strings.TrimPrefix(hfToken, "Bearer ")
		}
	}
	if hfToken != "" {
		backend.HFDownloader().SetToken(hfToken)
	}
	progress, err := backend.HFDownloader().StartDownload(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "download failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status": "started", "progress": progress,
	})
}

func handleHFDownloadProgress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	filename := r.URL.Query().Get("filename")
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	progress, err := backend.HFDownloader().GetDownloadProgress(modelID, filename)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func handleHFDownloads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	active := backend.HFDownloader().ListActiveDownloads()
	history := backend.HFDownloader().ListDownloadHistory()
	if active == nil {
		active = []cppbackend.HFDownloadProgress{}
	}
	if history == nil {
		history = []cppbackend.HFDownloadProgress{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"active": active, "history": history})
}

func handleHFCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req struct {
		ModelID  string `json:"modelId"`
		Filename string `json:"filename"`
	}
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.ModelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required")
		return
	}
	if err := backend.HFDownloader().CancelDownload(req.ModelID, req.Filename); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// handleHFCleanup — Round 17.3 (2026-08-03): DELETE /api/hf/download
// Удаляет скачанный/частичный файл из контейнера, освобождая дисковое пространство.
// Раньше единственный способ освободить место — остановить контейнер.
//
// Поддерживает оба варианта: query params (для DELETE) и body (для POST).
func handleHFCleanup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use DELETE or POST")
		return
	}
	modelID := r.URL.Query().Get("modelId")
	filename := r.URL.Query().Get("filename")
	if r.Method == http.MethodPost || (modelID == "" && filename == "") {
		var req struct {
			ModelID  string `json:"modelId"`
			Filename string `json:"filename"`
		}
		if err := decodeJSONRequest(r, &req, 0); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if modelID == "" {
			modelID = req.ModelID
		}
		if filename == "" {
			filename = req.Filename
		}
	}
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "modelId is required (query param or body)")
		return
	}
	if filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required (query param or body)")
		return
	}
	result, err := backend.HFDownloader().DeleteDownload(modelID, filename)
	if err != nil {
		// 200 с noop — UI просто покажет toast "no file found"
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "noop",
			"message": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "deleted",
		"result": result,
	})
}

// handlePull — Ollama-compatible /api/pull для llama.cpp бэкендов.
// Поддерживает загрузку GGUF-моделей с HuggingFace по имени вида:
//
//	hf:<repo>/<filename.gguf>  или  <repo>/<filename.gguf>
//
// Ollama-реестр (имена без '/') не поддерживается — возвращается 501.
func handlePull(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		Name   string `json:"name"`
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := decodeJSONRequest(r, &req, 0); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	name := req.Name
	if name == "" {
		name = req.Model
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "name or model is required")
		return
	}

	hf := backend.HFDownloader()
	if hf == nil {
		writeError(w, http.StatusServiceUnavailable, "HF downloader not available")
		return
	}

	ref := strings.TrimPrefix(name, "hf:")
	if !strings.Contains(ref, "/") {
		writeError(w, http.StatusNotImplemented, "Ollama registry pull is not supported for llama.cpp backends; use hf:<repo>/<filename.gguf> or the /api/hf/download endpoint")
		return
	}

	parts := strings.SplitN(ref, "/", 2)
	modelID := parts[0]
	filename := parts[1]
	if filename == "" {
		writeError(w, http.StatusBadRequest, "filename is required (expected repo/filename.gguf)")
		return
	}

	// Если файл уже скачан — сразу возвращаем успех.
	if localPath, err := hf.GetLocalPath(ref); err == nil && localPath != "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":    "present",
			"name":      name,
			"modelId":   modelID,
			"filename":  filename,
			"localPath": localPath,
		})
		return
	}

	dlReq := cppbackend.HFDownloadRequest{
		ModelID:  modelID,
		Filename: filename,
		Revision: "main",
	}
	progress, err := hf.StartDownload(dlReq)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "pull failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"status":   "pulling",
		"name":     name,
		"modelId":  modelID,
		"filename": filename,
		"progress": progress,
	})
}
