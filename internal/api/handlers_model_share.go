// handlers_model_share.go — R89 (2026-10-08): HTTP-плоскость переноса моделей.
//
//	POST /api/v1/models/share              — создать задание (202) и вернуть его;
//	GET  /api/v1/models/share              — список заданий (новые сверху);
//	GET  /api/v1/models/share/{id}         — состояние одного задания (прогресс);
//	POST /api/v1/models/share/{id}/cancel   — отменить перенос.
//
// ПОЧЕМУ 202 И ОТДЕЛЬНЫЙ GET: перенос image-bundle — это 14 ГБ и десятки минут.
// Держать на этом HTTP-соединение клиента нельзя: браузер уйдёт со страницы, а
// задание обязано доехать. Клиент (панель) опрашивает прогресс по {id} — ровно
// так же, как страница моделей опрашивает прогресс загрузки.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleModelShare — POST (создать) / GET (список).
func (s *Server) handleModelShare(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		jobs := s.modelShare.list()
		out := make([]map[string]interface{}, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, j.snapshot())
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": out, "total": len(out)})
		return
	case http.MethodPost:
		var req shareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": "invalid JSON body: " + err.Error(),
				"hint":  "ожидается {\"source\":\"<backendId>\",\"model\":\"<имя>\",\"targets\":[\"<backendId>\",...],\"overwrite\":false}",
			})
			return
		}
		job, err := s.startModelShare(req)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"error": err.Error(),
				"hint": "source — бэкенд, где модель ЕСТЬ; targets — бэкенды того же типа, куда её нужно положить; " +
					"типы: llama_cpp (файл .gguf) и image_cpp (каталог bundle)",
			})
			return
		}
		status := http.StatusAccepted
		if job.State == shareStateFailed {
			status = http.StatusConflict
		}
		s.writeJSON(w, status, job.snapshot())
		return
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET or POST"})
	}
}

// handleModelShareByID — GET /{id} и POST /{id}/cancel.
func (s *Server) handleModelShareByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/models/share/")
	if rest == "" {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": "job id is required", "hint": "GET /api/v1/models/share — список заданий",
		})
		return
	}
	id := rest
	action := ""
	if i := strings.Index(rest, "/"); i >= 0 {
		id, action = rest[:i], rest[i+1:]
	}
	job := s.modelShare.get(id)
	if job == nil {
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": "share job not found", "job": id,
			"hint": "задания живут до перезапуска балансера; сам файл либо доехал до приёмника, либо нет — это видно в списке моделей бэкенда",
		})
		return
	}
	switch {
	case action == "cancel" && r.Method == http.MethodPost:
		job.mu.Lock()
		running := job.State == shareStateRunning
		cancel := job.cancel
		job.mu.Unlock()
		if running && cancel != nil {
			cancel()
			job.mu.Lock()
			job.State = shareStateCanceled
			for i := range job.Targets {
				if job.Targets[i].State == shareTargetPending {
					job.Targets[i].State = shareTargetSkipped
					job.Targets[i].Note = "отменено оператором"
				}
			}
			job.mu.Unlock()
		}
		s.writeJSON(w, http.StatusOK, job.snapshot())
	case action != "":
		s.writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"error": "unknown action", "action": action, "hint": "поддерживается только /cancel",
		})
	case r.Method != http.MethodGet:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
	default:
		s.writeJSON(w, http.StatusOK, job.snapshot())
	}
}
