package api

// handlers_window_align_r494.go — R494 (2026-11-12): предложение выровнять окна
// копий одной модели.
//
// ЗАЧЕМ ОТДЕЛЬНАЯ РУЧКА. Монитор уже видел «две копии, разные окна», но не мог
// сказать, ДО КАКОГО окна поднимать и МОЖНО ЛИ трогать конкретную копию: для
// этого нужны сессии, активные запросы и потолок оператора
// (LB_WINDOW_ALIGN_MAX_N_CTX). Считать это в JS значило бы завести вторую
// арифметику рядом с R459 и разойтись с ней на первом изменении правил.
//
// КОНТРАКТ (только чтение — никаких перезагрузок):
//
//	GET /api/v1/window/align[?model=<имя>]
//	  -> {suggestions:[{model,cap,targetWindow,copies:[{backendId,window,
//	      sessions,active,free}],alignableBackends:[...],busyBackends:[...]}],
//	      count, cap, setting}
//
// ПОДТВЕРЖДЕНИЕ. Сама перезагрузка выполняется существующим
// POST /api/v1/cluster/models/{name}/reload с {"operation":"reload",
// "backendId":"<узел>","contextSize":<targetWindow>}: не заводим вторую дорогу
// загрузки моделей, у которой был бы свой набор проверок состояния узла
// (R263). Балансер только предлагает — оператор подтверждает.

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/balancer"
)

// windowAlignResponseR494 — ответ ручки.
type windowAlignResponseR494 struct {
	Suggestions []balancer.WindowAlignSuggestionR494 `json:"suggestions"`
	Count       int                                  `json:"count"`
	Cap         int                                  `json:"cap"`
	// Setting — имя настройки, которой оператор управляет потолком: чтобы в
	// ответе было видно, ГДЕ это менять, а не только число.
	Setting string `json:"setting"`
}

// handleWindowAlignR494 — GET /api/v1/window/align.
func (s *Server) handleWindowAlignR494(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error":   "method not allowed",
			"message": "GET /api/v1/window/align — только чтение; подтверждение делается через POST /api/v1/cluster/models/{name}/reload",
		})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "proxy is not initialized",
		})
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	sugg := s.proxy.WindowAlignSuggestionsR494(model)
	if sugg == nil {
		sugg = []balancer.WindowAlignSuggestionR494{}
	}
	s.writeJSON(w, http.StatusOK, windowAlignResponseR494{
		Suggestions: sugg,
		Count:       len(sugg),
		Cap:         balancer.WindowAlignCapR494(),
		Setting:     "LB_WINDOW_ALIGN_MAX_N_CTX",
	})
}
