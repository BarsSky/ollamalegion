package api

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// handleGgufBackends — GET /api/v1/gguf/backends
// Возвращает список зарегистрированных llama.cpp бэкендов с информацией о загруженных моделях.
// Используется страницей GGUF в WebUI для отображения состояния llama.cpp бэкендов.
func (s *Server) handleGgufBackends(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// По умолчанию скрываем нерабочие бэкенды на странице GGUF.
	// Query-параметр includeUnhealthy=true позволяет показать их для отладки.
	includeUnhealthy := r.URL.Query().Get("includeUnhealthy") == "true"

	// Получаем состояние кластера с полной информацией о бэкендах
	state := s.proxy.GetClusterState()
	if state == nil {
		s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cluster state unavailable"})
		return
	}

	type bgModelInfo struct {
		Name        string `json:"name"`
		Status      string `json:"status"` // "loaded", "loading", "error"
		ContextSize int    `json:"contextSize,omitempty"`
	}

	type bgGpuMemInfo struct {
		TotalMB int64 `json:"totalMB"`
		UsedMB  int64 `json:"usedMB"`
		FreeMB  int64 `json:"freeMB"`
	}

	type bgInfo struct {
		ID            string              `json:"id"`
		Type          types.BackendType   `json:"type"`
		Status        types.BackendStatus `json:"status"`
		Host          string              `json:"host"`
		CppWorkerPort int                 `json:"cppWorkerPort"`
		OllamaPort    int                 `json:"ollamaPort"`
		URL           string              `json:"url"`
		Models        []bgModelInfo       `json:"models"`
		GPUMemory     bgGpuMemInfo        `json:"gpuMemory"`
		VRAMUsage     float64             `json:"vramUsagePercent"`
		ActiveReqs    int                 `json:"activeRequests"`
		MaxReqs       int                 `json:"maxConcurrentReqs"`
	}

	result := make([]bgInfo, 0)

	// Множество «нерабочих» статусов, которые по умолчанию скрываем на странице GGUF.
	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	for _, bm := range state.Backends {
		// Фильтруем только llama.cpp бэкенды
		bt := bm.BackendType
		if bt == "" {
			bt = types.BackendTypeOllama
		}
		if bt != types.BackendTypeLlamaCpp {
			continue
		}

		// По умолчанию не показываем нерабочие бэкенды на странице GGUF,
		// чтобы WebUI не отображал заглушки/недоступные ноды.
		if !includeUnhealthy && unhealthyStatuses[bm.Status] {
			continue
		}

		// Сборка URL с учётом типа бэкенда и правильного порта по умолчанию.
		// Поскольку выше уже отфильтровано всё кроме llama.cpp, дефолтный порт 18092.
		// OllamaPort сюда не подставляется — для llama.cpp он не имеет смысла.
		// ВАЖНО: если бэкенд использует нестандартный порт (например, 18091 legacy),
		// WebUI всё равно попробует runtime auto-detect через probe /info.
		//
		// Если хост — это Docker-специфичный алиас (host.docker.internal),
		// заменяем его на localhost: из браузера host.docker.internal не резолвится,
		// а CppWorker в типичной конфигурации слушает на том же хосте, что и WebUI.
		hostForURL := bm.Host
		if hostForURL == "host.docker.internal" || strings.HasSuffix(hostForURL, ".docker.internal") {
			hostForURL = "localhost"
		}
		url := "http://" + hostForURL
		if bm.CppWorkerPort > 0 {
			url += ":" + itoa(bm.CppWorkerPort)
		} else {
			// llama.cpp бэкенд без явного CppWorkerPort — используем актуальный default 18092
			// (раньше был 18091, но это legacy-порт; современные инсталляции слушают 18092).
			url += ":18092"
		}

		models := make([]bgModelInfo, 0, len(bm.LlamaCpp.LoadedModels))
		for _, m := range bm.LlamaCpp.LoadedModels {
			models = append(models, bgModelInfo{
				Name:        m.Name,
				Status:      "loaded",
				ContextSize: m.ContextLength,
			})
		}
		// Добавляем warming up модели
		for _, wm := range bm.WarmingUpModels {
			found := false
			for _, lm := range models {
				if lm.Name == wm {
					found = true
					break
				}
			}
			if !found {
				models = append(models, bgModelInfo{
					Name:   wm,
					Status: "loading",
				})
			}
		}

		result = append(result, bgInfo{
			ID:            bm.ID,
			Type:          bt,
			Status:        bm.Status,
			Host:          bm.Host,
			CppWorkerPort: bm.CppWorkerPort,
			OllamaPort:    bm.OllamaPort,
			URL:           url,
			Models:        models,
			GPUMemory: bgGpuMemInfo{
				TotalMB: int64(bm.GPU.MemoryTotal),
				UsedMB:  int64(bm.GPU.MemoryUsed),
				FreeMB:  int64(bm.GPU.MemoryTotal - bm.GPU.MemoryUsed),
			},
			VRAMUsage:  bm.VRAMUsagePercent,
			ActiveReqs: bm.Ollama.ActiveRequests,
			MaxReqs:    bm.MaxConcurrentRequests,
		})
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends":      result,
		"total":         len(result),
		"backendType":   types.BackendTypeLlamaCpp,
		"operatingMode": state.OperatingMode,
	})
}

// itoa — простая конвертация int в string без fmt.Sprintf
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
