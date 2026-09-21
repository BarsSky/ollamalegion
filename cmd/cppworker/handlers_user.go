package main

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// maxParallelPerUser — безопасный доступ к currentConfig.MaxParallelPerUser.
//
// R65d (2026-09-20): в нескольких обработчиках (handleV1ChatCompletions,
// handleV1Completions, handleChat, handleGenerate) это поле читалось напрямую
// через currentConfig, из-за чего при неинициализированном конфиге
// (unit-тесты, ранний старт) происходил nil-pointer panic → HTTP 500.
// Правильный образец защиты уже был в handleInferUsers (строки ниже);
// хелпер делает его единым для всех вызовов.
func maxParallelPerUser() int {
	if currentConfig == nil {
		return 0
	}
	return currentConfig.MaxParallelPerUser
}

// handleInferUsers — список per-user parallel counters.
//
// Round 18 P0.3 (2026-08-04). Admin endpoint для мониторинга.
//
// GET /api/infer/users
// → {"users": [{"user_id": "alice", "current": 2}, ...], "max_per_user": 4, "enabled": true}
//
// Auth: X-API-Token обязателен.
//
// enabled = (currentConfig.MaxParallelPerUser > 0). Если false — admission
// не выполняется и users будет пустым (или содержит только бакеты от других
// механизмов).
func handleInferUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	maxPerUser := maxParallelPerUser()

	snap := backend.UserTracker().Snapshot()
	users := make([]map[string]interface{}, 0, len(snap))
	for userID, current := range snap {
		users = append(users, map[string]interface{}{
			"user_id": userID,
			"current": current,
		})
	}

	logger.Get().Debugw("handleInferUsers: snapshot served",
		"users_count", len(users), "max_per_user", maxPerUser)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"users":        users,
		"max_per_user": maxPerUser,
		"enabled":      maxPerUser > 0,
	})
}
