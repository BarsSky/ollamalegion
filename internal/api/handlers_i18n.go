package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"ollama-loadbalancer/internal/i18n"
)

// handlers_i18n.go — R234 (2026-10-24): язык логов и ошибок.
//
// КОНТРАКТ.
//
//	GET /api/v1/i18n  -> {"language":"ru|en","resolved":"ru|en","supported":["ru","en"],
//	                      "source":"env|config|webui|locale"}
//	PUT /api/v1/i18n  <- {"language":"ru|en|auto"}   (только администратор)
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ENDPOINT, А НЕ ТОЛЬКО СЕКЦИЯ В /api/v1/cluster/config.
// Язык — общая настройка стенда: его читают воркеры (cppworker, agent), которые
// не разбирают весь cluster config, а знают ровно один адрес — этот. Через него
// же WebUI переключает язык без перезапуска контейнеров: i18n.Set меняет язык
// немедленно, последующие лог-строки и API-ошибки идут уже на новом языке.
//
// «auto» в PUT означает «взять из локали (LANG/LC_ALL), при неясности — ru»;
// в конфиг сохраняется именно auto, чтобы при следующем старте поведение
// не зависело от того, что было выбрано вручную.
func (s *Server) handleI18n(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"language":  i18n.Get(),
			"resolved":  i18n.Get(),
			"supported": i18n.Supported(),
			"source":    s.logLanguageSource(),
		})
	case http.MethodPut:
		if !requestIsAdmin(r, s.authSessions) {
			s.writeJSON(w, http.StatusForbidden, map[string]interface{}{
				"success": false,
				"error":   i18n.T("api.i18n.read_only"),
			})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   i18n.T("api.i18n.bad_body", err),
			})
			return
		}
		var req struct {
			Language string `json:"language"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   i18n.T("api.i18n.bad_body", err),
			})
			return
		}
		raw := strings.ToLower(strings.TrimSpace(req.Language))
		lang, auto, ok := i18n.Parse(raw)
		if !ok {
			s.writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"error":   i18n.T("api.i18n.unsupported", req.Language, strings.Join(i18n.Supported(), ", ")),
			})
			return
		}
		if auto {
			lang = i18n.FromLocale()
		}
		changed := lang != i18n.Get()
		i18n.Set(lang)
		if changed {
			log.Print(i18n.T("i18n.changed", lang))
		}
		// В конфиг пишем исходное значение ("auto" — это настройка, а не язык),
		// чтобы перезапуск контейнера не «залипал» на вычисленном когда-то языке.
		if s.config != nil {
			s.config.Logging.Language = raw
		}
		saveErr := error(nil)
		if s.configSaver != nil {
			saveErr = s.configSaver()
		}
		if saveErr != nil {
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"success":  true,
				"language": lang,
				"warning":  i18n.T("api.i18n.save_failed", saveErr),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":  true,
			"language": lang,
			"message":  i18n.T("api.i18n.saved", lang),
		})
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{
			"error": "method not allowed: use GET or PUT",
		})
	}
}

// logLanguageSource — откуда сейчас взят язык (для GET /api/v1/i18n).
// Порядок совпадает с приоритетом в main: env > config > локаль.
func (s *Server) logLanguageSource() string {
	for _, name := range []string{"OLLAMALEGION_LOG_LANG", "LB_LOG_LANG"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return "env"
		}
	}
	if s.config != nil && strings.TrimSpace(s.config.Logging.Language) != "" {
		return "config"
	}
	return "locale"
}
