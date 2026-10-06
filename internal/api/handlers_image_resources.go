// handlers_image_resources.go — R84 (2026-10-03): настройка политики
// сосуществования image-генерации с текстовым инференсом из WebUI.
//
// ЧТО ЗДЕСЬ:
//
//	GET    /api/v1/image/resources — действующие значения + встроенные (config.json)
//	                                 + признак «есть переопределение» и путь файла
//	PUT    /api/v1/image/resources — провалидировать, сохранить переопределение,
//	                                 применить БЕЗ рестарта
//	DELETE /api/v1/image/resources — сбросить к встроенным значениям
//
// ПОЧЕМУ ОТДЕЛЬНЫЙ ФАЙЛ-ПЕРЕОПРЕДЕЛЕНИЕ, А НЕ ПРАВКА config.json: каталог
// /app/config в едином стенде смонтирован read-only, а env-переменных для этих
// полей нет — см. подробный разбор в internal/config/image_resources.go.
//
// ПОЧЕМУ ИЗМЕНЕНИЕ ВИДНО СРАЗУ: гейт читает настройки на каждый запрос
// (imageResources.settings() → proxy.config.Balancing.Image), а proxy и API-сервер
// держат ОДИН указатель на конфиг (cmd/balancer/main.go: NewProxy(conf) и
// NewServer(proxy, conf, ...)). Поэтому достаточно записать значение в конфиг —
// ни перезапуска, ни перечитки файла не требуется.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// imageResourcesRequest — тело PUT: те же поля, что и в config.json
// (balancing.image), плюс признак «сохранить» для совместимости с UI-формой.
type imageResourcesRequest struct {
	Coexistence                types.ImageCoexistencePolicy `json:"coexistence"`
	VramHeadroomMB             int                          `json:"vramHeadroomMb"`
	BlockOnUnknownVRAMEstimate bool                         `json:"blockOnUnknownVramEstimate"`
	QueueWaitTimeoutSec        int                          `json:"queueWaitTimeoutSec"`
	ExclusiveLockTimeoutSec    int                          `json:"exclusiveLockTimeoutSec"`
	GateDisabled               bool                         `json:"gateDisabled"`
	// AllowToolLoad — R85: разрешать ли инструменту поднимать модель. Указатель,
	// чтобы отличить «оператор снял галочку» (false) от «поля в теле нет» (nil):
	// во втором случае уже сохранённое значение не трогаем.
	AllowToolLoad *bool `json:"allowToolLoad"`
	// ToolLoadTimeoutSec — R86-follow-up: сколько секунд ждать загрузку модели,
	// которую поднимает вызов инструмента. Тот же принцип: nil = не менять.
	ToolLoadTimeoutSec *int `json:"toolLoadTimeoutSec"`
}

func (r imageResourcesRequest) toSettings(current types.ImageResourceSettings) types.ImageResourceSettings {
	allow := current.AllowToolLoad
	if r.AllowToolLoad != nil {
		allow = r.AllowToolLoad
	}
	loadTimeout := current.ToolLoadTimeoutSec
	if r.ToolLoadTimeoutSec != nil {
		loadTimeout = r.ToolLoadTimeoutSec
	}
	return types.ImageResourceSettings{
		Coexistence:                r.Coexistence,
		VramHeadroomMB:             r.VramHeadroomMB,
		BlockOnUnknownVRAMEstimate: r.BlockOnUnknownVRAMEstimate,
		QueueWaitTimeoutSec:        r.QueueWaitTimeoutSec,
		ExclusiveLockTimeoutSec:    r.ExclusiveLockTimeoutSec,
		GateDisabled:               r.GateDisabled,
		AllowToolLoad:              allow,
		ToolLoadTimeoutSec:         loadTimeout,
	}
}

// SetImageResourcesStore — внедрение хранилища переопределения из main.go.
//
// bundled — значения ИЗ config.json (до применения переопределения): они нужны,
// чтобы кнопка «Сбросить к встроенным» возвращала именно их, а не пустую
// структуру (иначе после сброса политика стала бы дефолтной exclusive, даже если
// в config.json задано иное).
func (s *Server) SetImageResourcesStore(store *config.ImageResourcesStore, bundled types.ImageResourceSettings) {
	s.imageResources = store
	s.imageResourcesDefaults = bundled
}

// handleImageResources — GET/PUT/DELETE /api/v1/image/resources.
func (s *Server) handleImageResources(w http.ResponseWriter, r *http.Request) {
	if s.config == nil {
		writeJSONResponse(w, http.StatusServiceUnavailable, map[string]string{"error": "config not available"})
		return
	}
	if s.imageResources == nil {
		// Балансер собран/запущен без хранилища (например, старый main) — не
		// притворяемся, что настройка есть.
		writeJSONResponse(w, http.StatusServiceUnavailable, map[string]string{
			"error": "image resources store is not configured on this balancer",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.writeImageResources(w)
	case http.MethodPut, http.MethodPost:
		var req imageResourcesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body: " + err.Error()})
			return
		}
		settings := req.toSettings(s.effectiveImageResources())
		if err := config.ValidateImageResourceSettings(settings); err != nil {
			writeJSONResponse(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := s.imageResources.Save(settings); err != nil {
			logger.Get().Errorw("image resources: save failed", "error", err)
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "save failed: " + err.Error()})
			return
		}
		s.applyImageResources(settings)
		logger.Get().Infow("image resources updated via API",
			"coexistence", settings.EffectiveCoexistencePolicy(),
			"headroomMB", settings.VramHeadroomMB,
			"queueWaitSec", settings.QueueWaitTimeoutSec,
			"lockFuseSec", settings.ExclusiveLockTimeoutSec,
			"blockOnUnknown", settings.BlockOnUnknownVRAMEstimate,
			"gateDisabled", settings.GateDisabled,
			"allowToolLoad", allowToolLoadLabel(settings.AllowToolLoad))
		s.writeImageResources(w)
	case http.MethodDelete:
		if err := s.imageResources.Remove(); err != nil {
			writeJSONResponse(w, http.StatusInternalServerError, map[string]string{"error": "reset failed: " + err.Error()})
			return
		}
		s.applyImageResources(s.imageResourcesDefaults)
		logger.Get().Infow("image resources reset to bundled defaults",
			"coexistence", s.imageResourcesDefaults.EffectiveCoexistencePolicy())
		s.writeImageResources(w)
	default:
		writeJSONResponse(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET, PUT or DELETE"})
	}
}

// applyImageResources — записать значения в действующий конфиг (гейт читает их
// на каждый запрос, поэтому применяется мгновенно).
func (s *Server) applyImageResources(settings types.ImageResourceSettings) {
	s.imageResourcesMu.Lock()
	s.config.Balancing.Image = settings
	s.imageResourcesMu.Unlock()
}

// effectiveImageResources — действующие настройки под тем же мьютексом, что и
// запись (нужно, чтобы PUT «без поля» сохранил уже действующее значение).
func (s *Server) effectiveImageResources() types.ImageResourceSettings {
	s.imageResourcesMu.RLock()
	defer s.imageResourcesMu.RUnlock()
	return s.config.Balancing.Image
}

// writeImageResources — ответ для UI: действующие, встроенные, источник.
func (s *Server) writeImageResources(w http.ResponseWriter) {
	s.imageResourcesMu.RLock()
	effective := s.config.Balancing.Image
	s.imageResourcesMu.RUnlock()

	overridden := s.imageResources.Present()
	source := "config"
	if overridden {
		source = "override"
	}

	writeJSONResponse(w, http.StatusOK, map[string]interface{}{
		"effective":  effective,
		"defaults":   s.imageResourcesDefaults,
		"overridden": overridden,
		"source":     source,
		"path":       s.imageResources.Path(),
		"limits": map[string]int{
			"maxVramHeadroomMb":       config.ImageResourceMaxHeadroomMB,
			"maxQueueWaitTimeoutSec":  config.ImageResourceMaxQueueWaitS,
			"maxExclusiveLockFuseSec": config.ImageResourceMaxLockFuseS,
			// Границы ожидания загрузки модели из вызова инструмента (R86-follow-up).
			"minToolLoadTimeoutSec": config.ImageResourceMinToolLoadTimeoutS,
			"maxToolLoadTimeoutSec": config.ImageResourceMaxToolLoadTimeoutS,
		},
		// Подсказки для формы: что означает каждая политика. Держим их рядом с
		// данными, чтобы UI не дублировал тексты и не расходился с движком.
		"policies": []map[string]string{
			{"value": string(types.ImageCoexistenceExclusive), "hint": "во время генерации image-бэкенд владеет картой: текстовый трафик ждёт (безопасный дефолт)"},
			{"value": string(types.ImageCoexistenceOffload), "hint": "совместная работа разрешена: применяйте, только если image-воркер запущен с offload в RAM (--offload-to-cpu / --backend te=cpu), иначе рискуете OOM"},
			{"value": string(types.ImageCoexistenceDedicated), "hint": "под image-бэкенд выделена отдельная GPU: ограничения сосуществования не применяются"},
		},
		// R85: автозагрузка модели из инструмента. Показываем и действующее
		// значение, и источник (env, если поле не задано) — иначе непонятно,
		// почему галочка стоит, хотя её никто не ставил.
		"allowToolLoad": map[string]interface{}{
			"effective":  types.EffectiveAllowToolLoad(effective.AllowToolLoad),
			"overridden": effective.AllowToolLoad != nil,
			"env":        balancer.ImageToolAllowLoadEnv(),
			"hint":       "разрешить текстовой модели поднимать image-модель, которой нет в VRAM (инструмент generate_image + list_image_models); при выключении инструмент объявляется только для уже загруженной модели",
		},
		// R86-follow-up: ожидание загрузки модели из вызова инструмента.
		"toolLoadTimeout": map[string]interface{}{
			"effectiveSec": types.EffectiveToolLoadTimeout(effective.ToolLoadTimeoutSec, balancer.DefaultImageToolLoadTimeoutSec),
			"overridden":   effective.ToolLoadTimeoutSec != nil,
			"env":          balancer.ImageToolLoadTimeoutEnv(),
			"hint":         "сколько секунд ждать загрузку image-модели, которую поднимает вызов инструмента (большие модели на медленном диске грузятся дольше); по истечении в tool-сообщение уходит причина",
		},
		"checkedAt": time.Now().UTC().Format(time.RFC3339),
	})
}

// allowToolLoadLabel — значение настройки для лога: «on»/«off»/«env».
func allowToolLoadLabel(v *bool) string {
	if v == nil {
		return "env"
	}
	if *v {
		return "on"
	}
	return "off"
}
