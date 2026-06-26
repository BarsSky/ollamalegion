package rpcworker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// =====================================================================
// B8 — TP (tensor parallelism) HTTP handlers
// =====================================================================
//
// Новые endpoints на rpcworker'е для тензорного параллелизма:
//
//   POST /rpc/tp/infer           — выполнить partial inference на rank'е
//   POST /rpc/tp/kv_sync         — сохранить KV-shard для rank'а
//   GET  /rpc/tp/kv_fetch        — получить KV-shard для rank'а
//
// Контракт совместим с internal/rptensor.TPShardInferRequest/Response
// (см. internal/rptensor/coordinator.go).
//
// В stub-режиме (без реального llama.cpp) /rpc/tp/infer возвращает
// deterministic partial output длиной len(input) / world_size с
// rank-маркированными байтами. Реальная интеграция (ggml bridge +
// NCCL all-reduce) — отдельная фаза B8.7 (post-1.0).

// TPInferRequest — запрос на partial inference для одного rank'а.
//
// Input принимается как RawMessage и декодируется вручную: допустимы
// три формата: base64-string (от Go encoding/json для []byte), plain
// string, или массив байтов (от других клиентов).
type TPInferRequest struct {
	ModelName   string          `json:"model_name"`
	Rank        int             `json:"rank"`
	WorldSize   int             `json:"world_size"`
	Layer       int             `json:"layer"`
	Input       json.RawMessage `json:"input"`
	KVShard     []byte          `json:"kv_shard,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Temperature float32         `json:"temperature,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
}

// decodeRawInput — декодирует RawMessage в []byte.
//
// Поддерживает:
//   - JSON string (plain text) → []byte(string).
//   - JSON string (base64 с padding) → base64.StdEncoding.DecodeString.
//   - JSON array of numbers → []byte.
//
// NB: base64 без padding (RawStdEncoding) НЕ используется, потому что
// мы не можем надёжно отличить plain string "1234567890" (10 chars)
// от base64 "1234567890" (которое декодируется в 9 байт с потерями).
// Поэтому base64 detection требует padding или длину кратную 4.
func decodeRawInput(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Попытка 1: array of numbers.
	var arr []int
	if err := json.Unmarshal(raw, &arr); err == nil {
		out := make([]byte, len(arr))
		for i, v := range arr {
			out[i] = byte(v)
		}
		return out, nil
	}
	// Попытка 2: string.
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("input must be string or array of numbers: %v", err)
	}
	// Base64 detection: либо padding (=), либо длина кратна 4 И только
	// base64-charset (A-Z a-z 0-9 + /).
	if isBase64String(s) {
		if decoded, decErr := base64.StdEncoding.DecodeString(s); decErr == nil {
			return decoded, nil
		}
		// Fallback на plain text даже если похоже на base64.
	}
	return []byte(s), nil
}

// isBase64String — true если строка выглядит как base64 (с padding или
// длиной кратной 4 и только из base64-charset).
func isBase64String(s string) bool {
	if len(s) == 0 {
		return false
	}
	if len(s)%4 != 0 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'A' && c <= 'Z':
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9':
		case c == '+' || c == '/' || c == '=':
		default:
			return false
		}
	}
	return true
}

// TPInferResponse — ответ с partial output'ом.
type TPInferResponse struct {
	Rank        int    `json:"rank"`
	Output      []byte `json:"output"`
	KVShard     []byte `json:"kv_shard,omitempty"`
	NeedsReduce bool   `json:"needs_reduce"`
	LatencyMs   int64  `json:"latency_ms"`
	WorkerID    string `json:"worker_id"`
}

// tpPartialLen — длина partial output для rank'а.
//
// Возвращает ceil(inputLen / worldSize) для всех rank'ов кроме последнего,
// и remainder для последнего. Дублирует rptensor.PartialLenForRank
// (избегаем circular import между rpcworker ↔ rptensor).
//
// NB: при изменении PartialLenForRank в rptensor синхронизировать.
func tpPartialLen(inputLen, worldSize, rank int) int {
	if worldSize <= 0 || rank < 0 || rank >= worldSize {
		return 0
	}
	if inputLen == 0 {
		return 0
	}
	base := inputLen / worldSize
	rem := inputLen % worldSize
	if rank < rem {
		return base + 1
	}
	return base
}

// handleTPInfer — POST /rpc/tp/infer.
//
// Stub-mode: возвращает partial output длиной len(input)/worldSize с
// rank-маркированными байтами (rank 0 → byte 1, rank 1 → byte 2, ...).
// В production с реальным bridge — вызывает slice.Handle.Infer() для
// шарда модели этого rank'а (отложено в B8.7).
func (s *WorkerServer) handleTPInfer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}

	var req TPInferRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.ModelName == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "model_name is required")
		return
	}
	if req.Rank < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_rank", "rank must be >= 0")
		return
	}
	if req.WorldSize <= 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_world_size", "world_size must be > 0")
		return
	}
	if req.Rank >= req.WorldSize {
		writeJSONError(w, http.StatusBadRequest, "invalid_rank",
			"rank must be < world_size")
		return
	}
	if req.Layer < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_layer", "layer must be >= 0")
		return
	}

	// Проверяем что модель загружена. Разрешаем как "name", так и "name.gguf".
	slice := s.manager.GetSlice(req.ModelName)
	if slice == nil && !strings.HasSuffix(req.ModelName, ".gguf") {
		slice = s.manager.GetSlice(req.ModelName + ".gguf")
	}
	if slice == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "model_not_loaded",
			"model not loaded on this worker; call /rpc/load first")
		return
	}
	if slice.Handle == nil {
		writeJSONError(w, http.StatusInternalServerError, "no_handle",
			"loaded slice has no bridge handle")
		return
	}

	// Декодируем input (может быть base64-string, plain string, или array).
	inputBytes, decErr := decodeRawInput(req.Input)
	if decErr != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_input", decErr.Error())
		return
	}
	logger.Get().Debugw("tp_infer decoded",
		"rank", req.Rank, "world_size", req.WorldSize,
		"layer", req.Layer, "input_len", len(inputBytes))

	// Stub-mode: partial output = rank-marked bytes.
	plen := tpPartialLen(len(inputBytes), req.WorldSize, req.Rank)
	out := make([]byte, plen)
	for i := range out {
		out[i] = byte(req.Rank + 1) // rank 0 → 1, rank 1 → 2, ...
	}

	// Stub KV-shard: rank+layer marker.
	kvShard := []byte{byte(req.Rank), byte(req.Layer)}

	// Production path (если включён не-stub) — будет реализовано в B8.7.
	if !s.cfg.StubMode {
		logger.Get().Debugw("tp_infer non-stub path not implemented yet (B8.7)",
			"rank", req.Rank, "layer", req.Layer)
	}

	resp := TPInferResponse{
		Rank:        req.Rank,
		Output:      out,
		KVShard:     kvShard,
		NeedsReduce: true,
		LatencyMs:   0, // stub — мгновенно
		WorkerID:    s.cfg.WorkerID,
	}

	writeJSON(w, http.StatusOK, resp)
}

// tpKvSyncRequest — POST /rpc/tp/kv_sync body.
type tpKvSyncRequest struct {
	SessionID string `json:"session_id"`
	Rank      int    `json:"rank"`
	Shard     []byte `json:"shard,omitempty"`
	Encoded   string `json:"encoded,omitempty"` // base64(shard)
}

// tpKvSyncResponse — ответ с подтверждением сохранения.
type tpKvSyncResponse struct {
	Status    string `json:"status"`
	SessionID string `json:"session_id"`
	Rank      int    `json:"rank"`
	WorkerID  string `json:"worker_id"`
}

// handleTPKvSync — POST /rpc/tp/kv_sync. Сохраняет KV-shard для конкретного rank'а.
func (s *WorkerServer) handleTPKvSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return
	}
	var req tpKvSyncRequest
	if err := readJSON(r, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error())
		return
	}
	if req.SessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "session_id is required")
		return
	}
	if req.Rank < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_rank", "rank must be >= 0")
		return
	}

	shard := req.Shard
	if req.Encoded != "" {
		decoded, decErr := base64.StdEncoding.DecodeString(req.Encoded)
		if decErr != nil {
			writeJSONError(w, http.StatusBadRequest, "bad_encoding",
				"encoded field must be base64: "+decErr.Error())
			return
		}
		shard = decoded
	}

	if err := s.kvStore.SaveShard(req.SessionID, req.Rank, shard); err != nil {
		if IsKVStoreFullError(err) {
			writeJSONError(w, http.StatusInsufficientStorage, "kv_full",
				"kv_store max entries reached")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, tpKvSyncResponse{
		Status:    "saved",
		SessionID: req.SessionID,
		Rank:      req.Rank,
		WorkerID:  s.cfg.WorkerID,
	})
}

// handleTPKvFetch — GET /rpc/tp/kv_fetch?session_id=X&rank=N.
func (s *WorkerServer) handleTPKvFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "session_id query param required")
		return
	}
	rankStr := r.URL.Query().Get("rank")
	if rankStr == "" {
		writeJSONError(w, http.StatusBadRequest, "missing_field", "rank query param required")
		return
	}
	rank, err := strconv.Atoi(rankStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_rank",
			"rank must be integer: "+err.Error())
		return
	}
	if rank < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_rank", "rank must be >= 0")
		return
	}

	shard := s.kvStore.LoadShard(sessionID, rank)
	if shard == nil {
		writeJSONError(w, http.StatusNotFound, "not_found",
			"no KV shard for session="+sessionID+" rank="+rankStr)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"session_id": sessionID,
		"rank":       rank,
		"worker_id":  s.cfg.WorkerID,
		"shard":      shard,
		"encoded":    base64.StdEncoding.EncodeToString(shard),
		"size_bytes": len(shard),
	})
}