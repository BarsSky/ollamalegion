// handlers_llama_cpp_overrides.go — HTTP endpoint'ы для persistent runtime-overrides
// llamaCpp-секции (Session 17, 2026-07-27).
//
// Endpoints:
//   GET    /api/v1/cluster/llama-cpp/overrides  — есть ли активный override?
//                                              (200 + body / 404 not found)
//   DELETE /api/v1/cluster/llama-cpp/overrides  — сбросить override (Reset to bundled)
//
// Эти endpoint'ы обслуживают кнопку "Reset to bundled defaults" в WebUI
// (раздел "Настройки llama.cpp / GGUF"). Пользователь может:
//   1. Изменить параметры через WebUI → PUT /api/v1/cluster/config
//      сохраняет в /app/data/runtime-overrides/llama-cpp.json.
//   2. Перезагрузить стек → balancer на старте применяет override
//      поверх config.json (host = source of truth).
//   3. Решить откатиться → DELETE этот endpoint стирает override,
//      in-memory конфиг возвращается к base значениям из config.json.
package api

import (
	"net/http"

	"ollama-loadbalancer/pkg/logger"
)

// handleGetLlamaCppOverride — GET /api/v1/cluster/llama-cpp/overrides
//
// Ответ:
//   200 + {"exists": true, "config": {...}}  — override есть
//   200 + {"exists": false}                  — override нет (нормальная ситуация)
//   503                                          — overrides отключены (store == nil)
func (s *Server) handleGetLlamaCppOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed; use GET", http.StatusMethodNotAllowed)
		return
	}
	if s.overridesStore == nil || !s.overridesStore.IsEnabled() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "runtime overrides not enabled in this build (dataDir not configured)",
			"enabled": false,
		})
		return
	}
	ll, exists, err := s.overridesStore.LoadLlamaCpp()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": err.Error(),
		})
		return
	}
	resp := map[string]interface{}{
		"exists": exists,
		"enabled": true,
	}
	if exists && ll != nil {
		resp["config"] = *ll
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleDeleteLlamaCppOverride — DELETE /api/v1/cluster/llama-cpp/overrides
//
// Стирает override-файл. In-memory state немедленно возвращается к base config
// (см. handleDeleteLlamaCppOverrideAfter: после удаления нужно сбросить
// s.config.LlamaCpp к base значениям).
//
// Ответ:
//   200 + {"status": "reset", "existed": true|false}  — успех
//   503                                                  — overrides отключены
func (s *Server) handleDeleteLlamaCppOverride(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed; use DELETE", http.StatusMethodNotAllowed)
		return
	}
	if s.overridesStore == nil || !s.overridesStore.IsEnabled() {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "runtime overrides not enabled in this build (dataDir not configured)",
			"enabled": false,
		})
		return
	}

	existedBefore := s.overridesStore.HasLlamaCppOverride()
	if err := s.overridesStore.ClearLlamaCpp(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error": err.Error(),
		})
		return
	}

	// Восстанавливаем in-memory LlamaCpp к base значениям (snapshot при старте).
	// Без этого пользователь увидит reset в UI, но runtime-конфиг останется
	// прежним до перезапуска контейнера.
	if s.baseLlamaCpp != nil {
		s.config.LlamaCpp = *s.baseLlamaCpp
		logger.Get().Infow("clusterConfigHandler: llamaCpp restored to base (bundled defaults)")
	}

	logger.Get().Infow("clusterConfigHandler: llamaCpp override cleared via API",
		"existed_before", existedBefore)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "reset",
		"existed_before": existedBefore,
		"message":        "Runtime override removed and in-memory config restored to bundled defaults. No restart needed.",
	})
}
