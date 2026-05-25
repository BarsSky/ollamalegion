package api

import (
	"encoding/json"
	"net/http"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// validateBackendTypeForMode проверяет совместимость нового/изменяемого бэкенда с текущим режимом.
func (s *Server) validateBackendTypeForMode(backend types.Backend) error {
	return config.ValidateModeBackendCompatibility(s.config.Balancing.OperatingMode, []types.Backend{backend})
}

// validateBackendTypeConsistencyForAdd проверяет, что добавляемый бэкенд совместим с существующими.
func (s *Server) validateBackendTypeConsistencyForAdd(newBackend types.Backend) error {
	newType := normalizeBackendAPIType(newBackend.Type)
	if newType == "" {
		newType = types.BackendTypeOllama
	}

	// Проверяем совместимость с режимом
	if err := s.validateBackendTypeForMode(types.Backend{Type: newType, Status: types.StatusStarting}); err != nil {
		return err
	}

	// Проверяем консистентность с существующими активными бэкендами
	for _, b := range s.config.Backends {
		if b.Status == types.StatusOffline {
			continue
		}
		existingType := normalizeBackendAPIType(b.Type)
		if existingType == "" {
			existingType = types.BackendTypeOllama
		}
		if existingType != newType {
			return &BackendTypeMismatchError{
				NewType:      newType,
				ExistingType: existingType,
				ExistingID:   b.ID,
			}
		}
	}
	return nil
}

// BackendTypeMismatchError — ошибка несовместимости типов бэкендов
type BackendTypeMismatchError struct {
	NewType      types.BackendType
	ExistingType types.BackendType
	ExistingID   string
}

func (e *BackendTypeMismatchError) Error() string {
	return "cannot add backend of type " + e.NewType.Label() +
		": existing backend " + e.ExistingID + " is of type " + e.ExistingType.Label() +
		". All backends must be of the same type (ollama or llama_cpp)"
}

// handleBackendTypeFilter — GET /api/v1/backends?type=ollama|llama_cpp
func (s *Server) handleBackendTypeFilter(w http.ResponseWriter, r *http.Request) {
	typeFilter := r.URL.Query().Get("type")
	if typeFilter == "" {
		s.listBackendsAll(w, r)
		return
	}

	bt := types.BackendType(typeFilter)
	if bt != types.BackendTypeOllama && bt != types.BackendTypeLlamaCpp {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid backend type: must be 'ollama' or 'llama_cpp'",
		})
		return
	}

	filtered := make([]types.Backend, 0)
	for _, b := range s.config.Backends {
		bType := normalizeBackendAPIType(b.Type)
		if bType == bt {
			filtered = append(filtered, b)
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": filtered,
		"total":    len(filtered),
		"type":     bt,
	})
}

// listBackendsAll возвращает все бэкенды (без фильтрации)
func (s *Server) listBackendsAll(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": s.config.Backends,
		"total":    len(s.config.Backends),
	})
}

// handleBackendTypeInfo — GET /api/v1/backends/types — возвращает информацию о доступных типах
func (s *Server) handleBackendTypeInfo(w http.ResponseWriter, r *http.Request) {
	typeInfos := make([]map[string]interface{}, 0, len(types.AllBackendTypes()))
	for _, bt := range types.AllBackendTypes() {
		compatibleModes := make([]string, 0)
		for mode, allowed := range types.ModeBackendTypes {
			for _, t := range allowed {
				if t == bt {
					compatibleModes = append(compatibleModes, mode)
					break
				}
			}
		}
		typeInfos = append(typeInfos, map[string]interface{}{
			"type":             bt,
			"label":            bt.Label(),
			"emoji":            bt.Emoji(),
			"compatibleModes":  compatibleModes,
			"isCurrent":        normalizeBackendAPIType(types.BackendType(s.config.BackendEngine)) == bt || bt == types.BackendTypeOllama && s.config.BackendEngine == types.EngineOllamaAPI || bt == types.BackendTypeLlamaCpp && s.config.BackendEngine == types.EngineLlamaCPP,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"types": typeInfos,
	})
}

// handleModeSwitch — PUT /api/v1/config/mode — переключение режима с валидацией
func (s *Server) handleModeSwitch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req struct {
		OperatingMode string `json:"operatingMode"`
		Force         bool   `json:"force"` // принудительная деактивация несовместимых бэкендов
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}

	if req.OperatingMode == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "operatingMode is required"})
		return
	}

	// Проверяем валидность режима
	if _, ok := types.ModeBackendTypes[req.OperatingMode]; !ok {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid operatingMode: " + req.OperatingMode,
		})
		return
	}

	// Проверяем совместимость с существующими бэкендами
	if err := config.ValidateModeBackendCompatibility(req.OperatingMode, s.config.Backends); err != nil {
		if !req.Force {
			s.writeJSON(w, http.StatusConflict, map[string]interface{}{
				"error":   err.Error(),
				"message": "Use force=true to deactivate incompatible backends",
				"currentMode": s.config.Balancing.OperatingMode,
			})
			return
		}
		// Принудительная деактивация
		deactivated := config.DeactivateIncompatibleBackends(s.config, req.OperatingMode)
		logger.Get().Warnw("force mode switch: deactivated incompatible backends",
			"newMode", req.OperatingMode,
			"deactivatedCount", deactivated)
	}

	// Очищаем нерелевантные настройки
	config.SanitizeConfigOnModeSwitch(s.config, req.OperatingMode)
	s.config.Balancing.OperatingMode = req.OperatingMode

	// Сохраняем конфигурацию
	if s.configSaver != nil {
		if err := s.configSaver(); err != nil {
			logger.Get().Errorw("failed to save config after mode switch", "error", err)
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "ok",
		"operatingMode": req.OperatingMode,
		"backendEngine": s.config.BackendEngine,
		"message":       "Mode switched successfully",
	})
}

// normalizeBackendAPIType нормализует тип бэкенда в API-контексте
func normalizeBackendAPIType(bt types.BackendType) types.BackendType {
	if bt == "" {
		return types.BackendTypeOllama
	}
	switch bt {
	case types.BackendTypeOllama, types.BackendTypeLlamaCpp:
		return bt
	default:
		return types.BackendTypeOllama
	}
}