package main

import (
	"net/http"
	"strings"
)

// handleGetActiveQueries — список активных запросов по моделям (Round 26 v0.5.13).
//
// GET /api/models/active-queries[?model=<name>]
//
// Назначение (Bug #1+#2 v0.5.13): WebUI/balancer могут проверить, есть ли
// в полёте активные генерации для конкретной модели ПЕРЕД тем как
// инициировать reload/apply profile. Без этого WebUI показывает
// «loading...» в течение всего времени, пока runAsyncReload ждёт
// inflight.WaitZero(req.Name, 0) — и пользователь думает, что
// «настройки заблокированы» при генерации.
//
// Параметры:
//   - ?model=<name>  → возвращает {"model": <name>, "activeQueries": N}.
//     Если модель не загружена или неизвестна — N=0.
//   - без параметра  → возвращает {"queries": {"modelA": N, "modelB": M}}
//     для всех моделей с ненулевыми счётчиками.
//
// Это lightweight endpoint, не дёргает heavy блокировки — безопасно
// вызывать каждые 2-3s из WebUI busy badge.
//
// Auth: НЕ требуется. Endpoint не возвращает чувствительных данных
// (только счётчики). authMiddleware в роутере не ставим чтобы не
// ломать WebUI polling при истёкшем токене (как с /api/models/load).
func handleGetActiveQueries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	inflight := backend.InFlight()
	if inflight == nil {
		writeError(w, http.StatusServiceUnavailable, "inflight counter not initialized")
		return
	}

	modelName := strings.TrimSpace(r.URL.Query().Get("model"))

	// Per-model запрос
	if modelName != "" {
		n := inflight.Get(modelName)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"model":         modelName,
			"activeQueries": n,
		})
		return
	}

	// Snapshot всех моделей с ненулевыми счётчиками
	snap := inflight.Snapshot()
	// Snapshot() уже фильтрует нулевые значения (см. inflight.go:135),
	// но на всякий случай ещё раз
	filtered := make(map[string]int64, len(snap))
	for name, n := range snap {
		if n > 0 {
			filtered[name] = n
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"queries": filtered,
		"count":   len(filtered),
		"total":   sumInt64Values(filtered),
	})
}

// sumInt64Values — helper для total-active.
func sumInt64Values(m map[string]int64) int64 {
	var total int64
	for _, v := range m {
		total += v
	}
	return total
}
