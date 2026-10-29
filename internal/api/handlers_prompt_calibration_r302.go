// handlers_prompt_calibration_r302.go — R302 (2026-10-29): видимость
// калибровки оценки промпта.
//
// Зачем endpoint. Дефект, из-за которого появилась калибровка, оператор видел
// как «балансер отказывает, хотя влезает»: estimated 66001 против фактических
// 17616 при окне 32768. Без наблюдаемого отношения это выглядит как случайный
// отказ, и единственный способ разобраться — читать логи. Здесь видны числа,
// по которым принимается решение.
//
// GET  /api/v1/stats/prompt-calibration — снимок по моделям.
// POST /api/v1/stats/prompt-calibration — сброс калибровки (смена модели или
// токенизатора делает старые отношения неверными; файл перезаписывается).
package api

import (
	"encoding/json"
	"net/http"
)

// HandleStatsPromptCalibration — GET/POST /api/v1/stats/prompt-calibration
//
// Response (GET):
//
//	[
//	  {
//	    "model": "Qwen3.8-27B",
//	    "ratio": 0.267,
//	    "max_ratio": 0.267,
//	    "samples": 12,
//	    "max_actual": 17616,
//	    "last_raw_estimated": 66001,
//	    "last_actual": 17616,
//	    "updated_at": "2026-10-29T12:00:00Z"
//	  }
//	]
//
// POST → {"status":"reset"}.
func (s *Server) HandleStatsPromptCalibration(w http.ResponseWriter, r *http.Request) {
	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		snapshot := s.proxy.PromptCalibrationSnapshot()
		w.Header().Set("Content-Type", "application/json")
		// Всегда массив: null в ответе ломает наивных клиентов WebUI.
		if len(snapshot) == 0 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(snapshot)
	case http.MethodPost:
		s.proxy.ResetPromptCalibration()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "reset"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
