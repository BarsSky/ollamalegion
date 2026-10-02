// handlers_image_profiles.go — R-Image (2026-09-27): CRUD профилей
// image-моделей (bundle) + каталог пресетов.
//
// ПО ОБРАЗЦУ handlers_cppworker_profiles.go (CRUD + merge + validate + persist,
// MaxBytesReader 64KB, DisallowUnknownFields), но хранилище ДРУГОЕ:
// профили llama.cpp лежат в types.LoadBalancerConfig.LlamaCppModelProfiles и
// пишутся configSaver()'ом, а pkg/types/config.go — замороженный контракт
// Phase 1, поэтому image-профили живут в отдельном файле
// (internal/config.ImageModelProfileStore → config/image-model-profiles.json).
// Правила валидации НЕ дублируются: единственный источник —
// types.ValidateImageModelProfile (движок sd-server свои границы не проверяет).
//
// API:
//
//	GET    /api/v1/image/model-catalog                        — пресеты каталога
//	GET    /api/v1/image/model-profiles                       — список профилей
//	GET    /api/v1/image/model-profiles/{name}                — профиль
//	PUT    /api/v1/image/model-profiles/{name}                — upsert (merge + save)
//	DELETE /api/v1/image/model-profiles/{name}                — удалить
//	POST   /api/v1/image/model-profiles/{name}/apply          — save + применить
//	GET    /api/v1/image/model-profiles/{name}/apply/progress — прогресс apply
//	GET    /api/v1/image/model-profiles/{name}/apply/status/{applyId} — статус apply
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/internal/config"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Хранилище профилей (ленивая инициализация)
// ============================================================

// imageProfileStores — кэш хранилищ по абсолютному пути файла.
//
// ЗАЧЕМ Кэш, А НЕ ПОЛЕ В Server: поле потребовало бы правки
// internal/api/handlers.go и связывания в cmd/balancer (вне зоны Phase 2).
// Ключ — путь, потому что путь определяется env-переменной и в тестах каждая
// проверка ставит свой временный файл: так «свой сервер» = «свой файл».
var (
	imageProfileStoresMu sync.Mutex
	imageProfileStores   = map[string]*config.ImageModelProfileStore{}
)

// imageProfileStore — хранилище профилей image-моделей этого сервера.
func (s *Server) imageProfileStore() *config.ImageModelProfileStore {
	path := config.ResolveImageModelProfilesPath()
	imageProfileStoresMu.Lock()
	defer imageProfileStoresMu.Unlock()
	if st, ok := imageProfileStores[path]; ok {
		return st
	}
	st := config.NewImageModelProfileStore(path)
	imageProfileStores[path] = st
	return st
}

// ============================================================
// Каталог пресетов
// ============================================================

// handleImageModelCatalog — GET /api/v1/image/model-catalog.
//
// Отдаёт пресеты из config/image-model-catalog.json. Файл читается на каждый
// запрос (он маленький, а оператор правит его без рестарта балансера). Битый
// каталог — это 500 с ПРИЧИНОЙ (какой пресет не прошёл валидацию), а не 404:
// «каталог не найден» и «каталог невалиден» требуют разных действий.
func (s *Server) handleImageModelCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	catalog, err := config.LoadImageModelCatalog("")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "catalog not found",
				"message": err.Error(),
				"path":    config.ResolveImageModelCatalogPath(),
			})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "invalid catalog",
			"message": err.Error(),
			"path":    config.ResolveImageModelCatalogPath(),
		})
		return
	}

	// Копии: ответ API не должен отдавать указатели на закэшированные структуры.
	presets := make([]types.ImageModelProfile, 0, len(catalog.Presets))
	for _, p := range catalog.Presets {
		presets = append(presets, config.CloneImageModelProfile(p))
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"version":   catalog.Version,
		"updatedAt": catalog.UpdatedAt,
		"notices":   catalog.Notices,
		"presets":   presets,
		"total":     len(presets),
	})
}

// ============================================================
// CRUD профилей
// ============================================================

// imageModelProfileResponse — DTO списка профилей.
type imageModelProfileResponse struct {
	Models map[string]types.ImageModelProfile `json:"models"`
	Total  int                                `json:"total"`
}

// imageModelProfileApplyResponse — результат apply профиля.
type imageModelProfileApplyResponse struct {
	Model    string                                `json:"model"`
	Profile  types.ImageModelProfile               `json:"profile"`
	ApplyID  string                                `json:"applyId"`
	Status   string                                `json:"status"` // running|completed|failed
	Backends []imageModelProfileApplyBackendResult `json:"backends"`
}

// imageModelProfileApplyBackendResult — результат применения на одном image-бэкенде.
type imageModelProfileApplyBackendResult struct {
	BackendID string `json:"backendId"`
	Status    string `json:"status"` // persisted|unreachable|not_an_image_backend|error
	Message   string `json:"message,omitempty"`
}

// handleImageModelProfiles — GET /api/v1/image/model-profiles (список).
func (s *Server) handleImageModelProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	store := s.imageProfileStore()
	if err := store.EnsureLoaded(); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "failed to load image model profiles",
			"message": err.Error(),
			"path":    store.Path(),
		})
		return
	}
	models := store.List()
	s.writeJSON(w, http.StatusOK, imageModelProfileResponse{Models: models, Total: len(models)})
}

// handleImageModelProfile — диспетчер /api/v1/image/model-profiles/{name}[/...].
func (s *Server) handleImageModelProfile(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/v1/image/model-profiles/")
	if trimmed == "" || trimmed == r.URL.Path {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}
	parts := strings.SplitN(trimmed, "/", 2)
	name := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "model name required"})
		return
	}

	switch {
	case sub == "" && r.Method == http.MethodGet:
		s.getImageModelProfile(w, name)
	case sub == "" && r.Method == http.MethodPut:
		s.upsertImageModelProfile(w, r, name)
	case sub == "" && r.Method == http.MethodDelete:
		s.deleteImageModelProfile(w, name)
	case sub == "apply" && r.Method == http.MethodPost:
		s.applyImageModelProfile(w, r, name)
	case strings.HasPrefix(sub, "apply/") && r.Method == http.MethodGet:
		subPath := sub
		if q := strings.Index(subPath, "?"); q >= 0 {
			subPath = subPath[:q]
		}
		rest := strings.TrimPrefix(subPath, "apply/")
		restParts := strings.SplitN(rest, "/", 2)
		switch {
		case restParts[0] == "progress":
			s.handleImageApplyProgress(w, r, name)
		case restParts[0] == "status" && len(restParts) == 2:
			s.handleImageApplyStatus(w, name, restParts[1])
		default:
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "unknown sub-path"})
		}
	default:
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// getImageModelProfile — GET профиля; 404 если нет.
func (s *Server) getImageModelProfile(w http.ResponseWriter, name string) {
	store := s.imageProfileStore()
	profile, ok := store.Get(name)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "profile not found",
			"message": fmt.Sprintf("no image model profile %q", name),
			"model":   name,
		})
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"model":   name,
		"profile": profile,
		// serverArgs — то, что реально уйдёт в sd-server. Полезно и оператору,
		// и contract-тестам: «профиль сохранён» != «движок поймёт флаги».
		"serverArgs": profile.ServerArgs(),
	})
}

// upsertImageModelProfile — PUT /api/v1/image/model-profiles/{name}.
//
// Мерж с существующим профилем (PATCH-like): поля, которых нет в теле,
// сохраняются. Явные false для булевых полей передаются через presence.
func (s *Server) upsertImageModelProfile(w http.ResponseWriter, r *http.Request, name string) {
	log := logger.Get()

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "failed to read request body",
			"message": err.Error(),
		})
		return
	}

	var upd types.ImageModelProfile
	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&upd); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid JSON body",
			"message": err.Error(),
		})
		return
	}
	var presence imageProfilePresence
	_ = json.Unmarshal(bodyBytes, &presence)

	store := s.imageProfileStore()
	existing, hasExisting := store.Get(name)
	incoming := mergeImageProfileUpdate(existing, upd, presence)
	// Имя берём из URL, а не из тела: путь /model-profiles/{name} и есть
	// идентификатор. Без этого PUT без поля "name" падал на «name is required»,
	// хотя имя уже пришло в пути.
	incoming.Name = name
	defaultImageProfileSeed(&incoming, presence.Defaults.Seed != nil, hasExisting)
	if err := applyImageProfileDefaults(&incoming, presence); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}
	if err := store.Set(name, incoming); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}

	log.Infow("image model profile upserted",
		"model", name, "family", incoming.Family,
		"files", len(incoming.Files), "hadExisting", hasExisting)

	saved, _ := store.Get(name)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"model":   name,
		"profile": saved,
	})
}

// deleteImageModelProfile — DELETE /api/v1/image/model-profiles/{name}.
func (s *Server) deleteImageModelProfile(w http.ResponseWriter, name string) {
	store := s.imageProfileStore()
	existed, err := store.Delete(name)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":   "failed to delete image model profile",
			"message": err.Error(),
		})
		return
	}
	if !existed {
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error":   "profile not found",
			"message": fmt.Sprintf("no image model profile %q", name),
			"model":   name,
		})
		return
	}
	logger.Get().Infow("image model profile deleted", "model", name)
	s.writeJSON(w, http.StatusOK, map[string]interface{}{"status": "ok", "model": name})
}

// ============================================================
// Apply
// ============================================================

// imageApplyRegistry — последние результаты apply (in-memory, per-process).
//
// ЗАЧЕМ: WebUI после POST apply опрашивает /apply/progress. Для image-моделей
// «применение» — это сохранение профиля + проверка доступности воркера (сам
// sd-server поднимается воркером при load), то есть операция короткая. Поэтому
// отдельный async-движок не нужен: храним последний результат по модели (и по
// applyId) и отдаём его как «completed». Это честнее, чем имитировать прогресс.
type imageApplyRegistry struct {
	mu      sync.Mutex
	byModel map[string]*imageModelProfileApplyResponse
	byID    map[string]*imageModelProfileApplyResponse
}

var imageApplies = &imageApplyRegistry{
	byModel: map[string]*imageModelProfileApplyResponse{},
	byID:    map[string]*imageModelProfileApplyResponse{},
}

func (reg *imageApplyRegistry) put(resp *imageModelProfileApplyResponse) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.byModel[resp.Model] = resp
	reg.byID[resp.ApplyID] = resp
}

func (reg *imageApplyRegistry) getByModel(model string) (*imageModelProfileApplyResponse, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	r, ok := reg.byModel[model]
	return r, ok
}

func (reg *imageApplyRegistry) getByID(id string) (*imageModelProfileApplyResponse, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	r, ok := reg.byID[id]
	return r, ok
}

// applyImageModelProfile — POST /api/v1/image/model-profiles/{name}/apply.
//
// Семантика:
//  1. Опциональное тело — merge поверх сохранённого профиля (как в PUT).
//  2. Валидация (types.ValidateImageModelProfile) и сохранение в
//     config/image-model-profiles.json.
//  3. Проверка каждого image-бэкенда: доступен ли воркер (GET
//     /api/image/capabilities). Профиль применяет ВОРКЕР при загрузке модели
//     (pull-синк, план §5.4), push-эндпоинта профилей в контракте воркера нет —
//     поэтому статус честный: persisted / unreachable, а не «reloaded».
func (s *Server) applyImageModelProfile(w http.ResponseWriter, r *http.Request, name string) {
	extendWriteDeadline(w)
	log := logger.Get()

	var bodyUpdate types.ImageModelProfile
	var presence imageProfilePresence
	hasBody := false

	if r.ContentLength != 0 {
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "failed to read request body",
				"message": err.Error(),
			})
			return
		}
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(bodyBytes))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&bodyUpdate); err != nil {
				s.writeJSON(w, http.StatusBadRequest, map[string]string{
					"error":   "invalid JSON body",
					"message": err.Error(),
				})
				return
			}
			_ = json.Unmarshal(bodyBytes, &presence)
			hasBody = true
		}
	}

	store := s.imageProfileStore()
	current, hasCurrent := store.Get(name)
	if !hasCurrent && !hasBody {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "no profile to apply",
			"message": "profile does not exist and no body provided to create one",
			"model":   name,
		})
		return
	}

	merged := current
	if hasBody {
		merged = mergeImageProfileUpdate(current, bodyUpdate, presence)
	}
	merged.Name = name
	defaultImageProfileSeed(&merged, presence.Defaults.Seed != nil, hasCurrent)
	if err := applyImageProfileDefaults(&merged, presence); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}
	if err := store.Set(name, merged); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid profile",
			"message": err.Error(),
		})
		return
	}

	saved, _ := store.Get(name)
	resp := &imageModelProfileApplyResponse{
		Model:    name,
		Profile:  saved,
		ApplyID:  fmt.Sprintf("imgapply-%d", time.Now().UnixNano()),
		Status:   "completed",
		Backends: s.probeImageBackends(name),
	}
	imageApplies.put(resp)

	log.Infow("image model profile applied",
		"model", name, "applyId", resp.ApplyID, "backends", len(resp.Backends))

	s.writeJSON(w, http.StatusOK, resp)
}

// probeImageBackends — доступность image-воркеров, к которым относится профиль.
//
// Имя модели в image-бэкендах — это имя bundle'а, поэтому «относится» = любой
// image-бэкенд: модель может быть загружена на любом из них. Проверяем
// capability-эндпоинт (он же подтверждает, что sd-server поднят и отвечает).
func (s *Server) probeImageBackends(model string) []imageModelProfileApplyBackendResult {
	results := make([]imageModelProfileApplyBackendResult, 0)
	if s.proxy == nil {
		return results
	}

	client := &http.Client{Timeout: 5 * time.Second}
	for _, backend := range s.proxy.GetAllBackends() {
		if backend.Type != types.BackendTypeImage {
			continue
		}
		port := backend.EffectiveImagePort()
		url := fmt.Sprintf("http://%s:%d/api/image/capabilities", backend.Host, port)
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			results = append(results, imageModelProfileApplyBackendResult{
				BackendID: backend.ID, Status: "error", Message: err.Error(),
			})
			continue
		}
		if token := s.imageWorkerAPIToken(backend.ID); token != "" {
			req.Header.Set(types.HeaderXAPIToken, token)
		}
		resp, err := client.Do(req)
		if err != nil {
			results = append(results, imageModelProfileApplyBackendResult{
				BackendID: backend.ID,
				Status:    "unreachable",
				Message:   fmt.Sprintf("image worker %s:%d: %v", backend.Host, port, err),
			})
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			results = append(results, imageModelProfileApplyBackendResult{
				BackendID: backend.ID,
				Status:    "persisted",
				Message:   fmt.Sprintf("профиль %q сохранён; воркер доступен и применит его при загрузке модели", model),
			})
			continue
		}
		results = append(results, imageModelProfileApplyBackendResult{
			BackendID: backend.ID,
			Status:    "error",
			Message:   fmt.Sprintf("image worker %s:%d returned HTTP %d", backend.Host, port, resp.StatusCode),
		})
	}
	return results
}

// handleImageApplyProgress — GET /apply/progress[?applyId=...].
func (s *Server) handleImageApplyProgress(w http.ResponseWriter, r *http.Request, name string) {
	if applyID := r.URL.Query().Get("applyId"); applyID != "" {
		if resp, ok := imageApplies.getByID(applyID); ok {
			s.writeJSON(w, http.StatusOK, resp)
			return
		}
	}
	if resp, ok := imageApplies.getByModel(name); ok {
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	s.writeJSON(w, http.StatusNotFound, map[string]string{
		"error":   "no apply in progress",
		"message": fmt.Sprintf("no apply result recorded for image model %q", name),
		"model":   name,
	})
}

// handleImageApplyStatus — GET /apply/status/{applyId}.
func (s *Server) handleImageApplyStatus(w http.ResponseWriter, name, applyID string) {
	if resp, ok := imageApplies.getByID(applyID); ok {
		s.writeJSON(w, http.StatusOK, resp)
		return
	}
	s.writeJSON(w, http.StatusNotFound, map[string]string{
		"error":   "apply not found",
		"message": fmt.Sprintf("no apply %q for image model %q", applyID, name),
		"model":   name,
		"applyId": applyID,
	})
}

// ============================================================
// Мерж и дефолты
// ============================================================

// imageProfilePresence — какие поля тела были ПЕРЕДАНЫ ЯВНО.
//
// Профиль image-модели почти весь состоит из bool/числовых полей без
// omitempty-указателей: `false`, `0` и «поле не передали» неотличимы. Без
// presence нельзя ни выключить флаг (`runtime.offloadToCpu: false`), ни задать
// `defaults.seed: 0` — а это валидный ФИКСИРОВАННЫЙ seed (в отличие от -1 = random).
//
// R-Image (2026-10-02): presence расширен на ЧИСЛОВЫЕ и СТРОКОВЫЕ скаляры.
// Раньше `steps: 0`, `timeoutSec: 0`, `vramEstimateMb: 0` и `sampler: ""`
// трактовались как «не менять», поэтому из WebUI нельзя было ни обнулить
// поле, ни очистить строку — редактор об этом честно предупреждал подсказкой.
// Теперь «0/пусто» — это ЗНАЧЕНИЕ, а «поле отсутствует в теле» — не менять
// (PATCH-семантика сохраняется: поля, которых нет в JSON, остаются как были).
//
// ВАЖНО: структура повторяет ВЛОЖЕННОСТЬ типа ImageModelProfile
// (presence.runtime.… / presence.defaults.…), иначе JSON-теги указывали бы на
// верхний уровень, и presence молча ничего не находил — ровно этот баг был
// пойман тестом «explicit false must clear the flag».
type imageProfilePresence struct {
	Family            *string               `json:"family"`
	Disabled          *bool                 `json:"disabled"`
	Notes             *string               `json:"notes"`
	VramEstimateMB    *int                  `json:"vramEstimateMb"`
	TimeoutSec        *int                  `json:"timeoutSec"`
	IdleUnloadMinutes *int                  `json:"idleUnloadMinutes"`
	Defaults          imageDefaultsPresence `json:"defaults"`
	Runtime           imageRuntimePresence  `json:"runtime"`
}

// imageDefaultsPresence — presence для ImageGenDefaults.
type imageDefaultsPresence struct {
	Steps          *int     `json:"steps"`
	CFGScale       *float64 `json:"cfgScale"`
	Sampler        *string  `json:"sampler"`
	Scheduler      *string  `json:"scheduler"`
	Width          *int     `json:"width"`
	Height         *int     `json:"height"`
	BatchCount     *int     `json:"batchCount"`
	ClipSkip       *int     `json:"clipSkip"`
	Seed           *int64   `json:"seed"`
	NegativePrompt *string  `json:"negativePrompt"`
}

// imageRuntimePresence — presence для ImageRuntime (булевы флаги + скаляры).
type imageRuntimePresence struct {
	Backend       *string `json:"backend"`
	ParamsBackend *string `json:"paramsBackend"`
	MaxVRAM       *string `json:"maxVram"`
	AutoFit       *string `json:"autoFit"`
	SplitMode     *string `json:"splitMode"`
	SeedMode      *string `json:"seedMode"`
	NGpuLayers    *int    `json:"nGpuLayers"`
	Threads       *int    `json:"threads"`
	VaeTileSize   *int    `json:"vaeTileSize"`
	OffloadToCPU  *bool   `json:"offloadToCpu"`
	DiffusionFA   *bool   `json:"diffusionFa"`
	VaeTiling     *bool   `json:"vaeTiling"`
	VaeConvDirect *bool   `json:"vaeConvDirect"`
	Taesd         *bool   `json:"taesd"`
}

// mergeImageProfileUpdate — мерж обновления поверх существующего профиля.
//
// Семантика (R-Image, 2026-10-02): поле, ОТСУТСТВУЮЩЕЕ в теле, не меняется;
// поле, переданное явно (presence != nil), записывается КАК ЕСТЬ — включая
// `0`, `false` и `""`. Zero-value fallback ниже оставлен для обратной
// совместимости с клиентами, которые не шлют полный набор ключей.
// Files: nil = «не менять», непустой список = «заменить целиком» (роли
// привязаны к файлам, частичный мерж состава дал бы bundle без VAE/TE).
func mergeImageProfileUpdate(existing, update types.ImageModelProfile, presence imageProfilePresence) types.ImageModelProfile {
	out := existing

	if update.Name != "" {
		out.Name = update.Name
	}
	if presence.Family != nil {
		out.Family = *presence.Family
	} else if update.Family != "" {
		out.Family = update.Family
	}
	if update.Files != nil {
		out.Files = update.Files
	}

	d, u := &out.Defaults, update.Defaults
	if presence.Defaults.Steps != nil {
		d.Steps = *presence.Defaults.Steps
	} else if u.Steps != 0 {
		d.Steps = u.Steps
	}
	if presence.Defaults.CFGScale != nil {
		d.CFGScale = *presence.Defaults.CFGScale
	} else if u.CFGScale != 0 {
		d.CFGScale = u.CFGScale
	}
	if presence.Defaults.Sampler != nil {
		d.Sampler = *presence.Defaults.Sampler
	} else if u.Sampler != "" {
		d.Sampler = u.Sampler
	}
	if presence.Defaults.Scheduler != nil {
		d.Scheduler = *presence.Defaults.Scheduler
	} else if u.Scheduler != "" {
		d.Scheduler = u.Scheduler
	}
	if presence.Defaults.Width != nil {
		d.Width = *presence.Defaults.Width
	} else if u.Width != 0 {
		d.Width = u.Width
	}
	if presence.Defaults.Height != nil {
		d.Height = *presence.Defaults.Height
	} else if u.Height != 0 {
		d.Height = u.Height
	}
	if presence.Defaults.BatchCount != nil {
		d.BatchCount = *presence.Defaults.BatchCount
	} else if u.BatchCount != 0 {
		d.BatchCount = u.BatchCount
	}
	if presence.Defaults.ClipSkip != nil {
		d.ClipSkip = *presence.Defaults.ClipSkip
	} else if u.ClipSkip != 0 {
		d.ClipSkip = u.ClipSkip
	}
	if presence.Defaults.Seed != nil {
		d.Seed = *presence.Defaults.Seed
	} else if u.Seed != 0 {
		d.Seed = u.Seed
	}
	if presence.Defaults.NegativePrompt != nil {
		d.NegativePrompt = *presence.Defaults.NegativePrompt
	} else if u.NegativePrompt != "" {
		d.NegativePrompt = u.NegativePrompt
	}

	ro, ru := &out.Runtime, update.Runtime
	if presence.Runtime.Backend != nil {
		ro.Backend = *presence.Runtime.Backend
	} else if ru.Backend != "" {
		ro.Backend = ru.Backend
	}
	if presence.Runtime.ParamsBackend != nil {
		ro.ParamsBackend = *presence.Runtime.ParamsBackend
	} else if ru.ParamsBackend != "" {
		ro.ParamsBackend = ru.ParamsBackend
	}
	if presence.Runtime.MaxVRAM != nil {
		ro.MaxVRAM = *presence.Runtime.MaxVRAM
	} else if ru.MaxVRAM != "" {
		ro.MaxVRAM = ru.MaxVRAM
	}
	if presence.Runtime.AutoFit != nil {
		ro.AutoFit = *presence.Runtime.AutoFit
	} else if ru.AutoFit != "" {
		ro.AutoFit = ru.AutoFit
	}
	if presence.Runtime.SplitMode != nil {
		ro.SplitMode = *presence.Runtime.SplitMode
	} else if ru.SplitMode != "" {
		ro.SplitMode = ru.SplitMode
	}
	if presence.Runtime.SeedMode != nil {
		ro.SeedMode = *presence.Runtime.SeedMode
	} else if ru.SeedMode != "" {
		ro.SeedMode = ru.SeedMode
	}
	if presence.Runtime.NGpuLayers != nil {
		ro.NGpuLayers = *presence.Runtime.NGpuLayers
	} else if ru.NGpuLayers != 0 {
		ro.NGpuLayers = ru.NGpuLayers
	}
	if presence.Runtime.Threads != nil {
		ro.Threads = *presence.Runtime.Threads
	} else if ru.Threads != 0 {
		ro.Threads = ru.Threads
	}
	if presence.Runtime.VaeTileSize != nil {
		ro.VaeTileSize = *presence.Runtime.VaeTileSize
	} else if ru.VaeTileSize != 0 {
		ro.VaeTileSize = ru.VaeTileSize
	}
	if ru.ExtraArgs != nil {
		ro.ExtraArgs = ru.ExtraArgs
	}
	if presence.Runtime.OffloadToCPU != nil {
		ro.OffloadToCPU = *presence.Runtime.OffloadToCPU
	} else if ru.OffloadToCPU {
		ro.OffloadToCPU = true
	}
	if presence.Runtime.DiffusionFA != nil {
		ro.DiffusionFA = *presence.Runtime.DiffusionFA
	} else if ru.DiffusionFA {
		ro.DiffusionFA = true
	}
	if presence.Runtime.VaeTiling != nil {
		ro.VaeTiling = *presence.Runtime.VaeTiling
	} else if ru.VaeTiling {
		ro.VaeTiling = true
	}
	if presence.Runtime.VaeConvDirect != nil {
		ro.VaeConvDirect = *presence.Runtime.VaeConvDirect
	} else if ru.VaeConvDirect {
		ro.VaeConvDirect = true
	}
	if presence.Runtime.Taesd != nil {
		ro.Taesd = *presence.Runtime.Taesd
	} else if ru.Taesd {
		ro.Taesd = true
	}

	if presence.VramEstimateMB != nil {
		out.VramEstimateMB = *presence.VramEstimateMB
	} else if update.VramEstimateMB != 0 {
		out.VramEstimateMB = update.VramEstimateMB
	}
	if presence.TimeoutSec != nil {
		out.TimeoutSec = *presence.TimeoutSec
	} else if update.TimeoutSec != 0 {
		out.TimeoutSec = update.TimeoutSec
	}
	if presence.IdleUnloadMinutes != nil {
		out.IdleUnloadMinutes = *presence.IdleUnloadMinutes
	} else if update.IdleUnloadMinutes != 0 {
		out.IdleUnloadMinutes = update.IdleUnloadMinutes
	}
	if presence.Disabled != nil {
		out.Disabled = *presence.Disabled
	} else if update.Disabled {
		out.Disabled = true
	}
	if presence.Notes != nil {
		out.Notes = *presence.Notes
	} else if update.Notes != "" {
		out.Notes = update.Notes
	}
	return out
}

// applyImageProfileDefaults — то, что можно вывести из family, а не требовать
// от оператора: шаги/cfg/sampler/размер (types.DefaultImageGenDefaults).
//
// Заполняем только НЕЗАДАННЫЕ поля: иначе PUT «только files» требовал бы вручную
// прописать 25 полей, а пресеты каталога несли бы дублирующиеся константы.
//
// R-Image (2026-10-02): поля, ПЕРЕДАННЫЕ ЯВНО (presence), не заполняем.
// Без этого явный `clipSkip: 0` / `sampler: ""` / `vaeTileSize: 0` затирался
// дефолтом семейства — то есть «обнулить поле из UI» было невозможно, даже
// когда значение валидно. Если явное значение выходит за границы (например
// `steps: 0`), валидация ниже вернёт честный 400, а не тихую подмену дефолтом.
func applyImageProfileDefaults(p *types.ImageModelProfile, presence imageProfilePresence) error {
	if p == nil {
		return fmt.Errorf("profile is nil")
	}
	if strings.TrimSpace(p.Family) == "" {
		return fmt.Errorf("family is required (см. types.ImageModelFamilies)")
	}
	if !types.IsValidImageFamily(p.Family) {
		return fmt.Errorf("unknown family %q", p.Family)
	}

	d := types.DefaultImageGenDefaults(p.Family)
	if p.Defaults.Steps == 0 && presence.Defaults.Steps == nil {
		p.Defaults.Steps = d.Steps
	}
	if p.Defaults.CFGScale == 0 && presence.Defaults.CFGScale == nil {
		p.Defaults.CFGScale = d.CFGScale
	}
	if p.Defaults.Sampler == "" && presence.Defaults.Sampler == nil {
		p.Defaults.Sampler = d.Sampler
	}
	if p.Defaults.Scheduler == "" && presence.Defaults.Scheduler == nil {
		p.Defaults.Scheduler = d.Scheduler
	}
	if p.Defaults.Width == 0 && presence.Defaults.Width == nil {
		p.Defaults.Width = d.Width
	}
	if p.Defaults.Height == 0 && presence.Defaults.Height == nil {
		p.Defaults.Height = d.Height
	}
	if p.Defaults.BatchCount == 0 && presence.Defaults.BatchCount == nil {
		p.Defaults.BatchCount = 1
	}
	if p.Runtime.SeedMode == "" && presence.Runtime.SeedMode == nil {
		p.Runtime.SeedMode = "random"
	}
	return types.ValidateImageModelProfile(p)
}

// defaultImageProfileSeed — seed по умолчанию для НОВОГО профиля.
//
// Зачем отдельная функция: zero-value int64 (0) — это ВАЛИДНЫЙ фиксированный
// seed, поэтому «поле не передали» и «передали 0» различимы только через
// presence. Для нового профиля без явного seed ставим -1 (random) — иначе
// первая же генерация залипла бы на seed=0 при seedMode=fixed.
func defaultImageProfileSeed(p *types.ImageModelProfile, seedExplicit, hasExisting bool) {
	if p == nil || seedExplicit || hasExisting {
		return
	}
	p.Defaults.Seed = -1
}
