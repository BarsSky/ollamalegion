package api

import (
	"net/http"
	"strconv"

	"ollama-loadbalancer/pkg/logger"
)

// proxyLogsHandler — GET /api/v1/proxy/logs?limit=50
// Возвращает последние записи лога прокси-запросов.
func (s *Server) proxyLogsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	proxyLogger := s.proxy.GetProxyLogger()
	if proxyLogger == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"entries": []interface{}{},
			"count":   0,
		})
		return
	}

	// Парсим limit из query параметра (по умолчанию 50, максимум 1000)
	limit := 50
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if parsed, err := strconv.Atoi(limitStr); err == nil && parsed > 0 {
			limit = parsed
			if limit > 1000 {
				limit = 1000
			}
		}
	}

	entries := proxyLogger.GetLast(limit)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries,
		"count":   len(entries),
		"total":   proxyLogger.Count(),
	})

	logger.Get().Debugw("proxy logs requested",
		"limit", limit,
		"returned", len(entries),
		"total", proxyLogger.Count())
}
