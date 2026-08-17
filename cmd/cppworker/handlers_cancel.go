package main

import (
	"errors"
	"net/http"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// handleCancel — отменяет активную генерацию (Round 18 P0.2).
//
// POST /api/cancel
// Body: {"request_id": "..."}  (опционально: {"model": "...", "user_id": "..."})
//
// Returns: 200 {"cancelled": N, "by": "id|user|model"}
//
// Auth: X-API-Token required (round 18 user decision #2).
//
// Semantics:
//   - Если указан request_id — отменяет только этот запрос
//   - Если указан user_id без model — отменяет ВСЕ запросы пользователя
//   - Если указан user_id + model — отменяет запросы пользователя для этой модели
//   - Если ничего не указано — отменяет ВСЕ (admin override)
func handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}

	var req struct {
		RequestID string `json:"request_id"`
		UserID    string `json:"user_id"`
		Model     string `json:"model"`
	}
	// Round 36 Phase 2: strict JSON decoder.
	if err := types.DecodeJSONRequest(r.Body, types.MaxRequestBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}

	tracker := backend.ActiveGenerations()
	if tracker == nil {
		writeError(w, http.StatusServiceUnavailable, "active generations tracker not initialized")
		return
	}

	var cancelled int
	var by string

	switch {
	case req.RequestID != "":
		if tracker.CancelByID(req.RequestID) {
			cancelled = 1
			by = "id"
		} else {
			writeError(w, http.StatusNotFound, "request_id not found: "+req.RequestID)
			return
		}
	case req.UserID != "" && req.Model != "":
		cancelled = tracker.CancelByModel(req.UserID, req.Model)
		by = "model"
	case req.UserID != "":
		cancelled = tracker.CancelByUser(req.UserID)
		by = "user"
	default:
		// No filter — cancel ALL (admin override)
		snap := tracker.Snapshot()
		for _, g := range snap {
			if tracker.CancelByID(g.RequestID) {
				cancelled++
			}
		}
		by = "all"
	}

	logger.Get().Infow("handleCancel: completed",
		"by", by, "cancelled", cancelled, "request_id", req.RequestID,
		"user_id", req.UserID, "model", req.Model)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cancelled": cancelled,
		"by":        by,
	})
}

// handleInferActive — список активных генераций (Round 18 P1.4).
//
// GET /api/infer/active
// Returns: 200 {"count": N, "generations": [...]}
//
// Auth: X-API-Token required.
func handleInferActive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	tracker := backend.ActiveGenerations()
	if tracker == nil {
		writeError(w, http.StatusServiceUnavailable, "active generations tracker not initialized")
		return
	}

	snap := tracker.Snapshot()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count":       len(snap),
		"generations": snap,
	})
}

// Compile-time check: we use cppbackend in handler_inferActive return.
var _ = cppbackend.GenerationInfo{}
