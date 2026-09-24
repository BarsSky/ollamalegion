package api

// R72 (P0): GET /api/v1/placement — наблюдаемость placement-политики.
//
// Отдаёт:
//   - конфигурацию политики (enabled / allowRequestOverride / fallback);
//   - ошибки валидации конфига (понятные сообщения, не блокируют запуск);
//   - решения по всем описанным моделям + глобальный дефолт ("*");
//   - решение для конкретной модели: ?model=...&sizeGB=...&strategy=...;
//   - счётчики принятых решений (стратегия/источник).
//
// Требует X-API-Token (как остальные admin-endpoint'ы).
//
// План: plans/2026-09-23-multi-backend-placement-policy.md (§5).

import (
	"net/http"
	"strconv"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

func (s *Server) placementHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"success": false,
			"error":   "method not allowed (только GET)",
		})
		return
	}

	q := r.URL.Query()
	cfg := s.proxy.PlacementSettings()

	globalMode := ""
	if s.proxy != nil {
		globalMode = s.proxy.OperatingMode()
	}

	resp := map[string]interface{}{
		"success":              true,
		"enabled":              cfg.Enabled,
		"allowRequestOverride": cfg.AllowRequestOverride,
		"fallback":             fallbackName(cfg.Fallback),
		"operatingMode":        globalMode,
		"strategies":           placementStrategyNames(),
		"executable":           executableStrategies(),
		"warnings":             warningsOrEmpty(s.proxy.PlacementWarnings()),
		"replication":          s.proxy.PlacementReplicationStatus(),
		"plan":                 "plans/2026-09-23-multi-backend-placement-policy.md",
		"stage": "P1 (исполняются single/pool/replicated; auto — детерминированное подмножество §4;" +
			" sharded/rpc — этап P2, уточнение auto по VRAM — P1.5)",
	}

	// Решение для конкретной модели (для отладки конфига и тестов).
	if model := strings.TrimSpace(q.Get("model")); model != "" {
		sizeGB, _ := strconv.ParseFloat(strings.TrimSpace(q.Get("sizeGB")), 64)
		override := strings.TrimSpace(q.Get("strategy"))
		resp["decision"] = s.proxy.ResolvePlacement(model, sizeGB, override)
		s.writeJSON(w, http.StatusOK, resp)
		return
	}

	resp["decisions"] = s.proxy.PlacementDecisions()
	s.writeJSON(w, http.StatusOK, resp)
}

func fallbackName(f string) string {
	if strings.TrimSpace(f) == "" {
		return types.PlacementFallbackError
	}
	return f
}

func warningsOrEmpty(w []string) []string {
	if w == nil {
		return []string{}
	}
	return w
}

func placementStrategyNames() []string {
	strategies := types.PlacementStrategies()
	out := make([]string, 0, len(strategies))
	for _, st := range strategies {
		out = append(out, string(st))
	}
	return out
}

func executableStrategies() []string {
	strategies := types.PlacementStrategies()
	out := make([]string, 0, len(strategies))
	for _, st := range strategies {
		if types.IsExecutablePlacementStrategy(st) {
			out = append(out, string(st))
		}
	}
	return out
}
