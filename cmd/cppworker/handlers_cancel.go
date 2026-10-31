package main

import (
	"errors"
	"net/http"

	"ollama-loadbalancer/c/bridge"
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

	var req cancelRequestR363
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

	// R363 (2026-10-31, аудит abort A4): ЗАГРУЗКА ТОЖЕ ОТМЕНЯЕМА.
	//
	// До этого отмена во время загрузки модели не работала ПО ЗАМЫСЛУ: загрузка отвязана
	// от клиентского ctx, флаг отмены выставлялся только для генерации. Клиент нажимал
	// Stop, получал 200 «no_active_generation», а модель продолжала читаться минутами и
	// держать VRAM (на крупных моделях — до нескольких минут и без возможности отменить).
	//
	// Теперь при cancel_load=true и отсутствии активных генераций отменяем загрузку тем
	// же путём, что force-выгрузка (R215): bridge.RequestLoadAbort останавливает чтение
	// весов в progress-callback, llama.cpp сам освобождает частично загруженную модель,
	// включая буферы на узлах раскладки.
	if cancelled == 0 && req.CancelLoad && req.Model != "" {
		outcome, aborted := abortLoadingModelR363(req.Model)
		if outcome != "" {
			logger.Get().Infow("handleCancel: отмена загрузки модели",
				"model", req.Model, "outcome", outcome, "abort_requested", aborted)
			resp := map[string]interface{}{
				"cancelled": 0,
				"by":        "load",
				"status":    outcome,
				"model":     req.Model,
				"aborted":   aborted,
				"supported": bridge.InferAbortSupported(),
			}
			if outcome == cancelLoadOutcomeStillLoadingR363 {
				resp["hint"] = "флаг отмены выставлен, но загрузка ещё не остановилась: " +
					"C-мост проверяет флаг на каждом тензоре, повторите через несколько секунд"
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
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
		// R362 (2026-10-31, аудит abort A6): ЧЕСТНАЯ возможность сборки.
		//
		// В stub-сборке (-tags llama_stub) per-inference abort-флаг всегда nil: отмена
		// физически невозможна, generation идёт до n_predict. Но обработчик отвечал
		// «cancelled: N», и клиент (а вслед за ним балансер и тесты) считал, что Stop
		// сработал. Теперь supported=false и статус cancel_not_supported — вызывающий
		// видит разницу между «отменили» и «отменять нечем».
		"supported": bridge.InferAbortSupported(),
	}
	if !bridge.InferAbortSupported() {
		resp["status"] = "cancel_not_supported"
		resp["notice"] = "эта сборка воркера без C-bridge (stub): отмена генерации не поддерживается, " +
			"модель остановится сама по исчерпанию n_predict"
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
// cancelRequestR363 — тело приказа об отмене. Именованный тип, а не анонимная структура:
// иначе тесты не могут передать её в applyCancelScope без копирования полей, а добавление
// нового поля молча ломает вызовы (живой случай при введении cancel_load).
type cancelRequestR363 struct {
	RequestID string `json:"request_id"`
	UserID    string `json:"user_id"`
	Model     string `json:"model"`
	// CancelLoad — R363 (2026-10-31, аудит abort A4): прервать не только генерацию, но и
	// ИДУЩУЮ ЗАГРУЗКУ модели. Ставит его балансер ТОЛЬКО для явной клиентской отмены
	// (кнопка Stop): таймаут чужого стрима не должен обрывать загрузку, которую ждут другие.
	CancelLoad bool `json:"cancel_load"`
}

func applyCancelScope(tracker *cppbackend.ActiveGenerations, req cancelRequestR363) (cancelled int, by string, idMissing bool) {
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
