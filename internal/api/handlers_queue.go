package api

import (
	"net/http"
)

// ============================================================
// Queue API handlers
// ============================================================

// queueStatsHandler - статистика очереди
func (s *Server) queueStatsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	stats := s.proxy.GetQueueStats()
	s.writeJSON(w, http.StatusOK, stats)
}

// queueDetailsHandler - детали очереди (pending + processing requests)
func (s *Server) queueDetailsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	pending := s.proxy.GetQueuePendingRequests()
	processing := s.proxy.GetQueueProcessingRequests()
	stats := s.proxy.GetQueueStats()

	// Объединяем в единый список для отображения
	all := make([]map[string]interface{}, 0, len(pending)+len(processing))
	all = append(all, pending...)
	all = append(all, processing...)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"pending":          pending,
		"processing":       processing,
		"all":              all,
		"pending_count":    len(pending),
		"processing_count": len(processing),
		"total":            len(all),
		"current_size":     stats.CurrentSize,
		"processed_total":  stats.Processed,
	})
}

// queueHistoryHandler - история выполненных запросов
func (s *Server) queueHistoryHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	history := s.proxy.GetQueueHistory()
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"history": history,
	})
}