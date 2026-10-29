package api

// handlers_settings_r278.go — R278 (2026-10-29): API реестра «горячих» настроек.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ API, А НЕ PUT /api/v1/cluster/config. Настройки реестра — это
// НЕ config.json: они применяются на лету, живут в отдельном override-файле в
// записываемом томе и имеют собственный источник значения (override → env →
// дефолт). Смешивать их с полным конфигом значило бы снова получить «сохранил
// конфиг, а применилось не то» — ровно та жалоба, из-за которой делался аудит.
//
// КОНТРАКТ.
//
//	GET  /api/v1/settings        — список: name/type/group/description/hotReload/
//	                               default/effective/source (секретов нет);
//	PUT  /api/v1/settings        — {"name":"LB_...","value":123} → пишет override
//	                               и применяет на лету; не-hot → 400 с причиной;
//	POST /api/v1/settings/reset  — {"name":"LB_..."} или {} (все) → возврат к
//	                               env/дефолту.
//
// Почему не «частичное обновление массивом»: у каждой настройки есть валидация
// и источник, и оператор должен видеть результат по каждой. Один name за запрос
// = понятная ошибка и понятный лог.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"ollama-loadbalancer/internal/settings"
)

// settingsEntryJSON — одна настройка в ответе API.
type settingsEntryJSON struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Group       string `json:"group"`
	Description string `json:"description"`
	HotReload   bool   `json:"hotReload"`
	Default     int64  `json:"default"`
	Effective   int64  `json:"effective"`
	Source      string `json:"source"`
}

func settingsEntryFromSnapshot(e settings.SnapshotEntry) settingsEntryJSON {
	return settingsEntryJSON{
		Name:        e.Name,
		Type:        string(e.Type),
		Group:       e.Group,
		Description: e.Description,
		HotReload:   e.HotReload,
		Default:     e.Default,
		Effective:   e.Effective,
		Source:      string(e.Source),
	}
}

// settingsHandler — GET (список) и PUT (запись значения).
func (s *Server) settingsHandler(w http.ResponseWriter, r *http.Request) {
	reg := settings.Default()
	if reg == nil {
		// Реестр не инициализирован (лёгкая сборка/тесты без Init). Честный 503:
		// пустой список выглядел бы как «настроек нет», а не как «фича выключена».
		http.Error(w, `{"error":"settings registry is not initialised"}`, http.StatusServiceUnavailable)
		return
	}

	switch r.Method {
	case http.MethodGet:
		snapshot := reg.Snapshot()
		out := make([]settingsEntryJSON, 0, len(snapshot))
		for _, e := range snapshot {
			out = append(out, settingsEntryFromSnapshot(e))
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"settings":  out,
			"count":     len(out),
			"overrides": reg.OverrideCount(),
			"sourcePriority": []string{
				string(settings.SourceOverride),
				string(settings.SourceEnv),
				string(settings.SourceDefault),
			},
		})

	case http.MethodPut, http.MethodPost:
		var req struct {
			Name  string `json:"name"`
			Value *int64 `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" || req.Value == nil {
			http.Error(w, `{"error":"name and value are required"}`, http.StatusBadRequest)
			return
		}
		src, err := reg.Set(req.Name, *req.Value)
		if err != nil {
			s.writeJSON(w, settingsErrorStatus(err), map[string]interface{}{
				"error":  err.Error(),
				"name":   req.Name,
				"value":  *req.Value,
				"reason": settingsErrorReason(err),
			})
			return
		}
		eff, srcNow, _ := reg.Effective(req.Name)
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"status":    "updated",
			"name":      req.Name,
			"value":     *req.Value,
			"effective": eff,
			"source":    string(srcNow),
			"applied":   string(src),
			"hotReload": true,
		})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// settingsResetHandler — возврат к env/дефолту (по имени или всё).
func (s *Server) settingsResetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reg := settings.Default()
	if reg == nil {
		http.Error(w, `{"error":"settings registry is not initialised"}`, http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	// Пустое тело допустимо: это «сбросить всё».
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
			return
		}
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		if err := reg.ResetAll(); err != nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{"status": "reset", "scope": "all"})
		return
	}
	if err := reg.Reset(req.Name); err != nil {
		status := settingsErrorStatus(err)
		s.writeJSON(w, status, map[string]interface{}{
			"error":  err.Error(),
			"name":   req.Name,
			"reason": settingsErrorReason(err),
		})
		return
	}
	eff, srcNow, _ := reg.Effective(req.Name)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "reset",
		"name":      req.Name,
		"effective": eff,
		"source":    string(srcNow),
	})
}

// settingsErrorStatus — HTTP-код для ошибки реестра.
func settingsErrorStatus(err error) int {
	switch {
	case errors.Is(err, settings.ErrUnknown):
		return http.StatusNotFound
	case errors.Is(err, settings.ErrNotHot):
		return http.StatusBadRequest
	default:
		return http.StatusBadRequest
	}
}

// settingsErrorReason — человеческая причина для оператора.
func settingsErrorReason(err error) string {
	switch {
	case errors.Is(err, settings.ErrUnknown):
		return "настройка не зарегистрирована: примените реальное имя LB_* из GET /api/v1/settings"
	case errors.Is(err, settings.ErrNotHot):
		return "настройка требует рестарта балансера: на лету применяются только поля с hotReload=true"
	default:
		return "значение отклонено валидацией: " + err.Error()
	}
}
