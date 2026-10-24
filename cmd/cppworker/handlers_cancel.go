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
	// R106c (2026-10-12): терпимое декодирование клиентского тела (/api/cancel
	// вызывают OpenWebUI/Cline — их расширения не должны давать 400).
	if err := decodeClientJSON(r, types.MaxRequestBodyBytes, &req, "handleCancel"); err != nil {
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

	cancelled, by, idMissing := applyCancelScope(tracker, req)
	if idMissing {
		writeError(w, http.StatusNotFound, "request_id not found: "+req.RequestID)
		return
	}

	logger.Get().Infow("handleCancel: completed",
		"by", by, "cancelled", cancelled, "request_id", req.RequestID,
		"user_id", req.UserID, "model", req.Model)

	// R229 (2026-10-22): диагностика, когда отменять было нечего.
	//
	// ЖИВОЙ СЦЕНАРИЙ: клиент нажал Stop, воркер отвечает «cancelled: 0», а клиент
	// продолжает ждать — и оператор не понимает почему. Частая причина — генерация
	// стоит в RPC-передаче на узел раскладки (порт 50052): keepalive-соединение
	// потеряно, реконнект не завершён, отмена по флагу не доходит до точки проверки.
	// Здесь отдаём ФАКТЫ (активные генерации всего, состав раскладки) и подсказку,
	// не делая дорогих проб узлов на пути отмены: проба — это /api/rpc-peers.
	resp := map[string]interface{}{
		"cancelled": cancelled,
		"by":        by,
		"status":    "cancelled",
	}
	if cancelled == 0 {
		resp["status"] = "no_active_generation"
		resp["active_generations"] = len(tracker.Snapshot())
		if currentConfig != nil {
			resp["rpc_enabled"] = currentConfig.RPCEnabled
			if currentConfig.RPCEnabled {
				resp["rpc_servers"] = currentConfig.RPCServers
				resp["hint"] = "нет активных генераций для отмены, раскладка RPC включена " +
					"(порт 50052): если клиент продолжает ждать — проверьте keepalive/реконнект " +
					"узла раскладки, GET /api/rpc-peers показывает доступность с воркера"
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyCancelScope — R229: выбрать ОБЛАСТЬ отмены по телу запроса.
//
// ДЕФЕКТ, который здесь закрыт (живой разбор). Балансер присылает отмену с ОДНИМ
// полем model (кнопка Stop в OpenWebUI). Прежний switch такой случай не разбирал
// и попадал в default «отменить ВСЕ»: Stop на модели A гасил и генерацию модели B
// на том же воркере. Теперь модель — отдельный случай; user_id остаётся фильтром
// внутри модели (CancelByModel с пустым userID отменяет всех пользователей модели).
func applyCancelScope(tracker *cppbackend.ActiveGenerations, req struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
}) (cancelled int, by string, idMissing bool) {
	switch {
	case req.RequestID != "":
		if !tracker.CancelByID(req.RequestID) {
			return 0, "id", true
		}
		return 1, "id", false
	case req.Model != "":
		return tracker.CancelByModel(req.UserID, req.Model), "model", false
	case req.UserID != "":
		return tracker.CancelByUser(req.UserID), "user", false
	default:
		// Без фильтров — отменить всё (админский сброс).
		snap := tracker.Snapshot()
		for _, g := range snap {
			if tracker.CancelByID(g.RequestID) {
				cancelled++
			}
		}
		return cancelled, "all", false
	}
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
