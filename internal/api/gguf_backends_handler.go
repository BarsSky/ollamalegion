package api

import (
	"net/http"
	"strings"

	"ollama-loadbalancer/pkg/types"
)

// ggufBgInfo — JSON-сериализуемая структура одного llama.cpp бэкенда для
// ответа /api/v1/gguf/backends. Объявлена на уровне пакета, чтобы и
// handler, и buildGgufBgInfo использовали один именованный тип.
type ggufBgInfo struct {
	ID            string              `json:"id"`
	Type          types.BackendType   `json:"type"`
	Status        types.BackendStatus `json:"status"`
	Host          string              `json:"host"`
	CppWorkerPort int                 `json:"cppWorkerPort"`
	OllamaPort    int                 `json:"ollamaPort"`
	URL           string              `json:"url"`
	Models        []ggufBgModelInfo   `json:"models"`
	GPUMemory     ggufBgGpuMemInfo    `json:"gpuMemory"`
	VRAMUsage     float64             `json:"vramUsagePercent"`
	ActiveReqs    int                 `json:"activeRequests"`
	MaxReqs       int                 `json:"maxConcurrentReqs"`
	HasAgent      bool                `json:"hasAgent"`
}

type ggufBgModelInfo struct {
	Name        string `json:"name"`
	Status      string `json:"status"` // "loaded", "loading", "error"
	ContextSize int    `json:"contextSize,omitempty"`
}

type ggufBgGpuMemInfo struct {
	TotalMB int64 `json:"totalMB"`
	UsedMB  int64 `json:"usedMB"`
	FreeMB  int64 `json:"freeMB"`
}

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

	// Множество «нерабочих» статусов, которые по умолчанию скрываем на странице GGUF.
	unhealthyStatuses := map[types.BackendStatus]bool{
		types.StatusUnhealthy:         true,
		types.StatusOffline:           true,
		types.StatusDraining:          true,
		types.StatusOllamaUnavailable: true,
	}

	// 2026-06-30: de-dup. До фикса CPPWORKER_REGISTER_DISABLE=true в bundled-compose
	// могла быть ситуация, когда в кластере присутствуют два бэкенда с одинаковым
	// физическим адресом (host, port), но разными id (например, "cppworker-gpu-bundled"
	// от shell-script register-with-balancer.sh и "cppworker-gpu" от Go-side
	// cppworker'а). Оба указывают на один и тот же физический контейнер — race
	// в selectBackend и визуальный дубль «один бэкенд под двумя именами» на
	// странице GGUF WebUI.
	//
	// Логика де-дупликации (на один физический endpoint — host:port — оставляем
	// только один бэкенд):
	//   1. Сначала фильтруем только llama.cpp бэкенды.
	//   2. Скрываем нерабочие (если !includeUnhealthy).
	//   3. Группируем по ключу host:port. Для каждой группы оставляем
	//      кандидата с наивысшим приоритетом:
	//      (a) бэкенд с hasAgent=true (proxy получает реальные GPU/VRAM метрики);
	//      (b) иначе первый встретившийся (state.Backends отсортирован лексикографически
	//          в proxy при сохранении state → детерминированный порядок).
	//
	// Используем общий helper dedupBackendsByHostPort (internal/api/dedup.go),
	// чтобы одинаковая логика работала и в listBackends/agentStatsHandler.
	filtered := make([]types.BackendMetrics, 0, len(state.Backends))
	for _, bm := range state.Backends {
		bt := bm.BackendType
		if bt == "" {
			bt = types.BackendTypeOllama
		}
		if bt != types.BackendTypeLlamaCpp {
			continue
		}
		if !includeUnhealthy && unhealthyStatuses[bm.Status] {
			continue
		}
		filtered = append(filtered, bm)
	}

	deduped := dedupBackendsByHostPort(
		filtered,
		func(bm types.BackendMetrics) string { return bm.Host },
		func(bm types.BackendMetrics) int { return bm.CppWorkerPort },
		func(bm types.BackendMetrics) bool { return bm.HasAgent },
		true, // preferAgent: бэкенд с агентом предпочтительнее для отображения GPU/VRAM
	)

	result := make([]ggufBgInfo, 0, len(deduped))
	for _, bm := range deduped {
		bt := bm.BackendType
		if bt == "" {
			bt = types.BackendTypeLlamaCpp
		}
		result = append(result, buildGgufBgInfo(bm, bt))
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"backends":      result,
		"total":         len(result),
		"backendType":   types.BackendTypeLlamaCpp,
		"operatingMode": state.OperatingMode,
	})
}

// buildGgufBgInfo — собирает ggufBgInfo для одного llama.cpp бэкенда.
// Выделено из основного цикла, чтобы переиспользоваться в de-dup сравнениях.
func buildGgufBgInfo(bm types.BackendMetrics, bt types.BackendType) ggufBgInfo {
	models := make([]ggufBgModelInfo, 0, len(bm.LlamaCpp.LoadedModels))
	for _, m := range bm.LlamaCpp.LoadedModels {
		models = append(models, ggufBgModelInfo{
			Name:        m.Name,
			Status:      "loaded",
			ContextSize: m.ContextLength,
		})
	}
	for _, wm := range bm.WarmingUpModels {
		found := false
		for _, lm := range models {
			if lm.Name == wm {
				found = true
				break
			}
		}
		if !found {
			models = append(models, ggufBgModelInfo{Name: wm, Status: "loading"})
		}
	}

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

	return ggufBgInfo{
		ID:            bm.ID,
		Type:          bt,
		Status:        bm.Status,
		Host:          bm.Host,
		CppWorkerPort: bm.CppWorkerPort,
		OllamaPort:    bm.OllamaPort,
		URL:           url,
		Models:        models,
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