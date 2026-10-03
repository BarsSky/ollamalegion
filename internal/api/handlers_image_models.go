// handlers_image_models.go — R-Image (2026-09-27): список image-бэкендов и
// прокси к image-воркеру (sd-server, stable-diffusion.cpp).
//
// АНАЛОГ internal/api/gguf_backends_handler.go (там фильтр Type==LlamaCpp) и
// internal/api/gguf_backend_proxy_handlers.go (прозрачный прокси WebUI → воркер).
//
// ЧТО ЗДЕСЬ ЕСТЬ:
//
//	GET  /api/v1/image/backends                     — список image-бэкендов (публичный)
//	GET  /api/v1/image/backends/{id}                — один бэкенд
//	GET  /api/v1/image/models                       — «сырые» списки моделей со всех
//	                                                  image-воркеров (без разбора схемы)
//	ANY  /api/v1/image/backends/{id}/proxy/{path}   — прозрачный прокси (зеркало gguf)
//	GET  /api/v1/image/backends/{id}/models         — удобный прокси GET /api/image/models
//	GET  /api/v1/image/backends/{id}/capabilities   — GET /api/image/capabilities
//	GET  /api/v1/image/backends/{id}/models/load/progress — GET /api/image/models/load/progress (в т.ч. SSE)
//	POST /api/v1/image/backends/{id}/models/load    — POST /api/image/models/load
//	POST /api/v1/image/backends/{id}/models/unload  — POST /api/image/models/unload
//	POST /api/v1/image/backends/{id}/generate       — POST /api/image/generate
//	GET  /api/v1/image/backends/{id}/jobs/{jobId}   — GET /api/image/jobs/{jobId}
//	POST /api/v1/image/backends/{id}/pull           — POST /api/hf/bundle (bundle-pull на воркере)
//
// ПРИНЦИП: ответы воркера НЕ хардкодятся и не «нормализуются». Воркер (Phase 3)
// владеет схемой своих ответов; балансер только доставляет байты (status/headers/
// SSE как есть). Единственное исключение — список бэкендов: он собирается из
// состояния кластера балансера, а не из воркера.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// imageBgInfo — JSON-представление одного image-бэкенда для /api/v1/image/backends.
type imageBgInfo struct {
	ID         string              `json:"id"`
	Type       types.BackendType   `json:"type"`
	Status     types.BackendStatus `json:"status"`
	Host       string              `json:"host"`
	ImagePort  int                 `json:"imagePort"`
	URL        string              `json:"url"`
	Models     []string            `json:"models"`
	WarmingUp  []string            `json:"warmingUpModels,omitempty"`
	GPUMemory  ggufBgGpuMemInfo    `json:"gpuMemory"`
	VRAMUsage  float64             `json:"vramUsagePercent"`
	ActiveReqs int                 `json:"activeRequests"`
	MaxReqs    int                 `json:"maxConcurrentReqs"`
	HasAgent   bool                `json:"hasAgent"`
}

// handleImageBackends — GET /api/v1/image/backends.
//
// Публичный (как /api/v1/gguf/backends): отдаёт только метаданные, нужен
// странице Image в WebUI для первичной отрисовки. Тяжёлые операции (load/pull/
// generate) идут через прокси, который закрыт AuthMiddleware.
func (s *Server) handleImageBackends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"

	state := s.proxy.GetClusterState()
	if state == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cluster state unavailable"})
		return
	}

	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	filtered := make([]types.BackendMetrics, 0, len(state.Backends))
	for _, bm := range state.Backends {
		if bm.BackendType != types.BackendTypeImage {
			continue
		}
		if !includeUnhealthy && unhealthyStatuses[bm.Status] {
			continue
		}
		filtered = append(filtered, bm)
	}

	// Де-дупликация по физическому эндпоинту — та же логика, что на странице
	// GGUF: один контейнер может быть зарегистрирован дважды (shell-script +
	// авторегистрация воркера), и в UI появлялись бы две одинаковые записи.
	deduped := dedupBackendsByHostPort(
		filtered,
		func(bm types.BackendMetrics) string { return bm.Host },
		func(bm types.BackendMetrics) int { return s.imagePortForBackend(bm.ID) },
		func(bm types.BackendMetrics) bool { return bm.HasAgent },
		true,
	)

	result := make([]imageBgInfo, 0, len(deduped))
	for _, bm := range deduped {
		result = append(result, s.buildImageBgInfo(bm))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends":      result,
		"total":         len(result),
		"backendType":   types.BackendTypeImage,
		"operatingMode": state.OperatingMode,
	})
}

// imagePortForBackend — порт image-воркера: берём из ЖИВОЙ записи бэкенда
// (BackendMetrics не несёт ImagePort), с фолбэком EffectiveImagePort → 18093.
func (s *Server) imagePortForBackend(backendID string) int {
	if s.proxy != nil {
		if b := s.proxy.GetBackend(backendID); b != nil {
			return b.EffectiveImagePort()
		}
	}
	return types.DefaultImageWorkerPort
}

// buildImageBgInfo — сборка ответа по одному бэкенду.
func (s *Server) buildImageBgInfo(bm types.BackendMetrics) imageBgInfo {
	port := s.imagePortForBackend(bm.ID)

	hostForURL := bm.Host
	if hostForURL == "host.docker.internal" || strings.HasSuffix(hostForURL, ".docker.internal") {
		hostForURL = "localhost"
	}

	models := make([]string, 0, len(bm.Models))
	models = append(models, bm.Models...)

	return imageBgInfo{
		ID:        bm.ID,
		Type:      types.BackendTypeImage,
		Status:    bm.Status,
		Host:      bm.Host,
		ImagePort: port,
		URL:       fmt.Sprintf("http://%s:%d", hostForURL, port),
		Models:    models,
		WarmingUp: bm.WarmingUpModels,
		GPUMemory: ggufBgGpuMemInfo{
			TotalMB: int64(bm.GPU.MemoryTotal),
			UsedMB:  int64(bm.GPU.MemoryUsed),
			FreeMB:  int64(bm.GPU.MemoryTotal - bm.GPU.MemoryUsed),
		},
		VRAMUsage:  bm.VRAMUsagePercent,
		ActiveReqs: bm.Ollama.ActiveRequests,
		MaxReqs:    bm.MaxConcurrentRequests,
		HasAgent:   bm.HasAgent,
	}
}

// handleImageBackendRoutes — диспетчер /api/v1/image/backends/{id}[/...].
func (s *Server) handleImageBackendRoutes(w http.ResponseWriter, r *http.Request) {
	id, rest, ok := splitBackendScopedPath(r.URL.Path, "/api/v1/image/backends/")
	if !ok || id == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid image backend path"})
		return
	}

	// Прозрачный прокси — зеркало /api/v1/gguf/backends/{id}/proxy/.
	if strings.HasPrefix(rest, "proxy/") {
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete:
			s.proxyToImageWorker(w, r, id, "/"+strings.TrimPrefix(rest, "proxy/"))
			return
		default:
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
	}

	if rest == "" {
		if r.Method != http.MethodGet {
			s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		state := s.proxy.GetClusterState()
		if state == nil {
			s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cluster state unavailable"})
			return
		}
		for _, bm := range state.Backends {
			if bm.ID == id && bm.BackendType == types.BackendTypeImage {
				s.writeJSON(w, http.StatusOK, s.buildImageBgInfo(bm))
				return
			}
		}
		s.writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "image backend not found", "backend": id,
		})
		return
	}

	// Таблица удобных прокси-алиасов: путь на балансере → путь в воркере.
	// Алиасы дают WebUI один стабильный контракт на балансере (пути воркера
	// могут меняться в Phase 3, а страница Image уже написана).
	type alias struct {
		method string
		suffix string
		worker string
	}
	aliases := []alias{
		{http.MethodGet, "models", "/api/image/models"},
		{http.MethodGet, "capabilities", "/api/image/capabilities"},
		{http.MethodGet, "models/load/progress", "/api/image/models/load/progress"},
		{http.MethodPost, "models/load", "/api/image/models/load"},
		{http.MethodPost, "models/unload", "/api/image/models/unload"},
		// Phase 9: удаление bundle с диска — удобный алиас к ручке воркера.
		{http.MethodPost, "models/delete", "/api/image/models/delete"},
		{http.MethodPost, "generate", "/api/image/generate"},
		// pull = bundle-загрузка НА ВОРКЕРЕ (файлы должны лежать рядом с
		// sd-server, а не на балансере), поэтому это прозрачный прокси на
		// HF-эндпоинт воркера; балансер ничего не «дорисовывает» в ответ.
		{http.MethodPost, "pull", "/api/hf/bundle"},
	}

	for _, a := range aliases {
		if rest == a.suffix && r.Method == a.method {
			s.proxyToImageWorker(w, r, id, a.worker)
			return
		}
	}

	// jobs/{jobId} — идентификатор в пути, поэтому отдельной веткой.
	if r.Method == http.MethodGet && strings.HasPrefix(rest, "jobs/") {
		jobID := strings.TrimPrefix(rest, "jobs/")
		if jobID == "" || strings.Contains(jobID, "/") {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job id required"})
			return
		}
		s.proxyToImageWorker(w, r, id, "/api/image/jobs/"+jobID)
		return
	}

	// Любые HF-пути (/api/hf/search, /files, /download, /progress, /downloads,
	// /cancel, /orphans) нужны странице Image так же, как странице GGUF.
	if strings.HasPrefix(rest, "hf/") {
		s.proxyToImageWorker(w, r, id, "/api/"+rest)
		return
	}

	s.writeJSON(w, http.StatusNotFound, map[string]string{
		"error":   "unknown image backend path",
		"path":    rest,
		"backend": id,
	})
}

// splitBackendScopedPath — разбор /api/v1/image/backends/{id}[/rest].
// Возвращает id и остаток БЕЗ ведущего слэша ("" если остатка нет).
func splitBackendScopedPath(fullPath, prefix string) (id, rest string, ok bool) {
	if !strings.HasPrefix(fullPath, prefix) {
		return "", "", false
	}
	tail := fullPath[len(prefix):]
	if tail == "" {
		return "", "", false
	}
	if slash := strings.Index(tail, "/"); slash >= 0 {
		id = tail[:slash]
		rest = strings.TrimPrefix(tail[slash+1:], "/")
		return id, rest, true
	}
	return tail, "", true
}

// handleImageModelsAggregate — GET /api/v1/image/models.
//
// Собирает «сырые» ответы image-воркеров (GET /api/image/models) со всех
// бэкендов. Схему ответа воркера НЕ разбираем и не переупаковываем: контракт
// воркера (Phase 3) — источник истины, а нормализованный агрегат появится в
// Phase 4 (/v1/models). Здесь отдаём payload как есть + статус опроса.
func (s *Server) handleImageModelsAggregate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if s.proxy == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "proxy unavailable"})
		return
	}

	type backendPayload struct {
		BackendID string          `json:"backendId"`
		Host      string          `json:"host"`
		Port      int             `json:"port"`
		OK        bool            `json:"ok"`
		Status    int             `json:"status,omitempty"`
		Error     string          `json:"error,omitempty"`
		Payload   json.RawMessage `json:"payload,omitempty"`
	}

	backends := make([]types.Backend, 0)
	for _, b := range s.proxy.GetAllBackends() {
		if b.Type == types.BackendTypeImage {
			backends = append(backends, b)
		}
	}
	sort.Slice(backends, func(i, j int) bool { return backends[i].ID < backends[j].ID })

	results := make([]backendPayload, len(backends))
	var wg sync.WaitGroup
	for i, b := range backends {
		wg.Add(1)
		go func(idx int, backend types.Backend) {
			defer wg.Done()
			port := backend.EffectiveImagePort()
			entry := backendPayload{BackendID: backend.ID, Host: backend.Host, Port: port}

			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			url := fmt.Sprintf("http://%s:%d/api/image/models", backend.Host, port)
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				entry.Error = err.Error()
				results[idx] = entry
				return
			}
			if token := s.imageWorkerAPIToken(backend.ID); token != "" {
				req.Header.Set(types.HeaderXAPIToken, token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				entry.Error = err.Error()
				results[idx] = entry
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			entry.Status = resp.StatusCode
			entry.OK = resp.StatusCode >= 200 && resp.StatusCode < 300
			if !entry.OK {
				entry.Error = fmt.Sprintf("worker returned HTTP %d", resp.StatusCode)
			}
			if json.Valid(body) {
				entry.Payload = json.RawMessage(body)
			} else if len(body) > 0 {
				entry.Error = "worker response is not valid JSON"
			}
			results[idx] = entry
		}(i, b)
	}
	wg.Wait()

	logger.Get().Debugw("image models aggregate collected", "backends", len(results))
	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends": results,
		"total":    len(results),
	})
}
