// handlers_stats.go — Round 31 #7 (2026-08-09): token usage stats endpoint.
//
// /api/v1/stats/tokens — per-model aggregate of prompt/completion tokens.
// Атомарно обновляется при каждом успешном response через recordTokenUsage
// (вызывается в proxyRequestLlamaCppNonStream).
package api

import (
	"encoding/json"
	"net/http"
)

// HandleStatsTokens — GET /api/v1/stats/tokens
//
// Response:
//   [
//     {
//       "model": "gemma-4-E4B-it-Q4_K_M",
//       "prompt_tokens": 1234567,
//       "completion_tokens": 789012,
//       "total_tokens": 2023579,
//       "request_count": 543,
//       "last_updated": "2026-08-09T10:30:00Z"
//     },
//     ...
//   ]
func (s *Server) HandleStatsTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusInternalServerError)
		return
	}
	snapshot := s.proxy.GetTokenUsageSnapshot()
	w.Header().Set("Content-Type", "application/json")
	// Всегда возвращаем [] даже если пусто (не null)
	if len(snapshot) == 0 {
		_, _ = w.Write([]byte("[]"))
		return
	}
	_ = json.NewEncoder(w).Encode(snapshot)
}