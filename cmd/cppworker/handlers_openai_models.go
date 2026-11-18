package main

import (
	"fmt"
	"net/http"
	"ollama-loadbalancer/internal/cppbackend"
	"strings"
	"time"
)

// handleV1Models — OpenAI-совместимый /v1/models endpoint.
//
// R248/B1: в каждой записи отдаётся `capabilities.vision`. Без этого клиент не
// может отличить модель, которая умеет картинки, от текстовой, и балансер не
// может решить: пропустить изображения (passthrough) или честно ответить 415.
func handleV1Models(w http.ResponseWriter, r *http.Request) {
	models := backend.ListModels()
	openaiModels := make([]map[string]interface{}, 0, len(models))
	for _, m := range models {
		// R248/B3: авторитетный источник — факт загрузки mmproj в бэкенде
		// (ModelInfo.Vision); карта visionCaps остаётся для операторских
		// переопределений. Ни при каких условиях capability не выводится
		// из имени модели.
		vision := m.Vision || modelVisionCapable(m.Name)
		openaiModels = append(openaiModels, map[string]interface{}{
			"id":       m.Name,
			"object":   "model",
			"created":  m.LoadedAt.Unix(),
			"owned_by": "ollamalegion",
			"capabilities": map[string]bool{
				"vision": vision,
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   openaiModels,
	})
}

// handleV1ModelByID — OpenAI-совместимый GET /v1/models/{model_id}.
//
// R60.8 (2026-09-07): retrieve specific model metadata per OpenAI API spec.
// Используется OpenWebUI и OpenAI python clients для отображения карточки
// модели по запросу. Раньше endpoint не реализован → 404 page not found.
//
// R60.16 (2026-09-08): disk-known unloaded models теперь возвращают 200 OK.
// Pre-R60.16: только loaded модели (через backend.GetModel); disk-known
// unloaded → 404, что ломает OpenWebUI model picker для скачанных, но
// ещё не загруженных моделей.
//
// Lookup path (NO file I/O on request goroutine):
//  1. backend.GetModel(modelID) — loaded models (in-memory map).
//  2. mm.ListModels() cache — disk-known .gguf files (populated by
//     ScanModels at startup; only os.Stat, no GGUF header read).
//  3. → 404.
//
// R60.15 Fix C (rolled back due to production hangs) tried path 2
// via mm.GetModelMeta which lazy-reads the GGUF header on the request
// goroutine — that blocked the bridge call queue under load. R60.16
// uses pure cache lookup instead, so disk-known lookups are O(N)
// cache iteration with no syscalls.
//
// Регистрируется в router.go как /v1/models/ (с trailing slash для
// subtree match). Handler извлекает {model_id} из r.URL.Path, ищет модель
// в ModelManager, возвращает OpenAI-формат или 404.
//
// Также обрабатывает /v1/models/ (empty ID после slash) → 400 "model id required".
func handleV1ModelByID(w http.ResponseWriter, r *http.Request) {
	// Extract model_id from path: strip "/v1/models/" prefix.
	const prefix = "/v1/models/"
	modelID := strings.TrimPrefix(r.URL.Path, prefix)
	if modelID == "" {
		writeError(w, http.StatusBadRequest, "model id is required in URL path /v1/models/{model_id}")
		return
	}
	// Reject nested paths (e.g. /v1/models/foo/bar) — OpenAI spec is single ID.
	if strings.Contains(modelID, "/") {
		writeError(w, http.StatusBadRequest, "model id must be a single segment, got nested path")
		return
	}

	// Path 1: loaded models (in-memory).
	if model, err := backend.GetModel(modelID); err == nil {
		created := model.LoadedAt.Unix()
		if created == 0 {
			created = time.Now().Unix()
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":       model.Name,
			"object":   "model",
			"created":  created,
			"owned_by": "ollamalegion",
		})
		return
	}

	// Path 2: disk-known unloaded models (cache only, no file I/O).
	// Matches by exact filename or stripped .gguf suffix.
	mm := backend.ModelManager()
	if mm == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelID))
		return
	}
	if meta := lookupDiskKnownNoIO(mm, modelID); meta != nil {
		// Return the bare model name (strip .gguf) — Ollama convention.
		// The user may have requested either "name" or "name.gguf";
		// the canonical response id is the bare name.
		canonicalID := strings.TrimSuffix(meta.Filename, ".gguf")
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":       canonicalID,
			"object":   "model",
			"created":  meta.ModifiedAt.Unix(),
			"owned_by": "ollamalegion",
		})
		return
	}

	writeError(w, http.StatusNotFound, fmt.Sprintf("model %q not found", modelID))
}

// lookupDiskKnownNoIO — pure cache lookup for a disk-known .gguf file
// by name. NO file I/O, NO GGUF header read. Returns nil if not found.
//
// Match priority:
//  1. exact match in ggufFiles (with or without .gguf suffix)
//  2. case-insensitive match on filename
//
// R60.16 (2026-09-08): replaces the original R60.15 Fix C path that
// called mm.GetModelMeta (which triggers ReadGGUFHeader) and caused
// production hangs under concurrent bridge usage.
func lookupDiskKnownNoIO(mm *cppbackend.ModelManager, modelID string) *cppbackend.GGUFModelMeta {
	if mm == nil || modelID == "" {
		return nil
	}
	models := mm.ListModels()

	// 1. exact match (with or without .gguf)
	if meta := findExactMatch(models, modelID); meta != nil {
		return meta
	}

	// 2. case-insensitive match on filename
	idLower := strings.ToLower(modelID)
	for i := range models {
		if strings.ToLower(models[i].Filename) == idLower {
			meta := models[i]
			return &meta
		}
		// also try stripped-suffix form
		baseLower := strings.ToLower(strings.TrimSuffix(models[i].Filename, ".gguf"))
		if baseLower == idLower || baseLower == strings.ToLower(strings.TrimSuffix(modelID, ".gguf")) {
			meta := models[i]
			return &meta
		}
	}
	return nil
}

// findExactMatch — try the modelID directly, then with .gguf appended.
// Returns pointer to the meta (stable for the test caller) or nil.
func findExactMatch(models []cppbackend.GGUFModelMeta, modelID string) *cppbackend.GGUFModelMeta {
	for i := range models {
		if models[i].Filename == modelID {
			meta := models[i]
			return &meta
		}
	}
	// also try with .gguf if the caller didn't include it
	if !strings.HasSuffix(modelID, ".gguf") {
		for i := range models {
			if models[i].Filename == modelID+".gguf" {
				meta := models[i]
				return &meta
			}
		}
	}
	return nil
}

// ============================================================
// Streaming helpers
// ============================================================

// indexedToolCalls конвертирует []openAIToolCall в []map с полем `index` per OpenAI
// streaming spec. Без `index` клиенты (Cline/Roo/openai-python) не могут правильно
// склеить инкрементальные deltas tool_calls и считают ответ невалидным.
// Каждый элемент получает уникальный индекс в порядке массива.
func indexedToolCalls(calls []openAIToolCall) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(calls))
	for i, c := range calls {
		out = append(out, map[string]interface{}{
			"index": i,
			"id":    c.ID,
			"type":  c.Type,
			"function": map[string]interface{}{
				"name":      c.Function.Name,
				"arguments": c.Function.Arguments,
			},
		})
	}
	return out
}
