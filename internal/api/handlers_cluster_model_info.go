package api

// handlers_cluster_model_info.go — proxy Ollama /api/show на каждый бэкенд в кластере
// и агрегация результатов в один JSON-ответ.
//
// Endpoint: GET /api/v1/cluster/models/{name}/info
//
// Позволяет UI и пользовательским скриптам получить детали модели
// (details.family, details.format, details.parameter_size, details.quantization_level,
// model_info.architecture, model_info.n_layers, model_info.n_embd, model_info.context_size,
// model_info.gpu_layers, model_info.state и т.д.) через балансировщик (порт 18081),
// не обращаясь к каждому cppworker/Ollama бэкенду напрямую.
//
// Семантика аналогична /api/v1/cluster/cppworker/debug/last-prompt (см. handlers_cluster_debug.go):
//   - Проксирует Ollama POST /api/show {"name": "<model>"} на каждый llama_cpp/ollama бэкенд.
//   - Per-backend ошибки (unavailable/404/5xx) не ломают общий ответ — backend помечается
//     status=error/unavailable с описанием в error-поле.
//   - Для ollama-бэкендов идём на host:OllamaPort; для llama_cpp — host:CppWorkerPort.
//
// Структура ответа:
//
//	{
//	  "model": "gemma-4-E4B-it-Q4_K_M",
//	  "count": <int>,                          // кол-во бэкендов, к которым обращались
//	  "okCount": <int>,                        // кол-во успешных ответов
//	  "backends": [
//	    {
//	      "backendId": "cppworker-gpu-bundled",
//	      "backendType": "llama_cpp" | "ollama",
//	      "status": "ok" | "not_found" | "error" | "unavailable",
//	      "info": { ...OllamaShowResponse... } | null,
//	      "error": "..."
//	    }
//	  ]
//	}
//
// Endpoint полезен для UI:
//   - Models tab — иконка "info" в model-card → открывает модальное окно с деталями
//     (architecture, parameter_size, quantization_level, context_size, gpu_layers).
//   - Вкладка Dashboard — счётчик "Loaded models" из clusterLoadedModelsHandler.
//
// Требует аутентификации (X-API-Token).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// modelInfoBackendResult — результат /api/show с одного бэкенда.
type modelInfoBackendResult struct {
	BackendID   string                 `json:"backendId"`
	BackendType string                 `json:"backendType"`
	Status      string                 `json:"status"` // "ok" | "not_found" | "error" | "unavailable"
	Info        map[string]interface{} `json:"info,omitempty"`
	Error       string                 `json:"error,omitempty"`
	HTTPStatus  int                    `json:"httpStatus,omitempty"`
}

// modelInfoResponse — агрегированный ответ по всем бэкендам кластера.
type modelInfoResponse struct {
	Model    string                   `json:"model"`
	Count    int                      `json:"count"`
	OKCount  int                      `json:"okCount"`
	Backends []modelInfoBackendResult `json:"backends"`
}

// clusterModelInfoHandler — GET /api/v1/cluster/models/{name}/info.
//
// Проксирует Ollama /api/show на каждый бэкенд в кластере и возвращает агрегированный
// результат. Семантически это «зеркало» /api/v1/cluster/models/loaded, но с подробностями.
//
// URL: /api/v1/cluster/models/{name}/info
//
// Использование:
//
//	curl -H "X-API-Token: $LB_TOKEN" \
//	  http://localhost:18081/api/v1/cluster/models/gemma-4-E4B-it-Q4_K_M/info
//
// Per-backend ошибки не ломают общий ответ — UI получает частичный успех
// (например, cppworker ответил, ollama не ответил — обе записи в массиве backends).
func (s *Server) clusterModelInfoHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	// Извлекаем имя модели из URL: /api/v1/cluster/models/{name}/info
	// Тот же парсер splitClusterModelPath, что и для /reload — он уже умеет
	// отделять {name} от суффикса (/info, /reload, /что-угодно).
	name, ok := splitClusterModelPath(r.URL.Path)
	if !ok || name == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing model name in URL (expected /api/v1/cluster/models/{name}/info)",
		})
		return
	}

	cs := s.getClusterState()
	if cs == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	resp := modelInfoResponse{
		Model:    name,
		Count:    0,
		OKCount:  0,
		Backends: []modelInfoBackendResult{},
	}

	if len(cs.Backends) == 0 {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   "no backends in cluster",
			"message": "register at least one backend (ollama or llama_cpp) before requesting model info",
		})
		return
	}

	// 10 секунд на запрос к каждому бэкенду (/api/show лёгкий, но бывает
	// ленивая загрузка на cppworker, которая может занять несколько секунд).
	client := &http.Client{Timeout: 10 * time.Second}

	for _, b := range cs.Backends {
		entry := s.fetchModelInfoFromBackend(r.Context(), client, b, name)
		resp.Backends = append(resp.Backends, entry)
		resp.Count++
		if entry.Status == "ok" {
			resp.OKCount++
		}
	}

	logger.Get().Infow("cluster model info requested",
		"model", name,
		"backends", resp.Count,
		"ok", resp.OKCount,
	)

	s.writeJSON(w, http.StatusOK, resp)
}

// fetchModelInfoFromBackend — POST /api/show на конкретный бэкенд и парсинг ответа.
//
// Возвращает modelInfoBackendResult со статусом:
//   - "ok"          — получили валидный JSON-ответ от бэкенда.
//   - "not_found"   — бэкенд вернул HTTP 404 (модель не обслуживается здесь).
//   - "unavailable" — сетевая ошибка или бэкенд не имеет host/port.
//   - "error"       — бэкенд вернул HTTP !200/!404, или невалидный JSON.
func (s *Server) fetchModelInfoFromBackend(
	ctx context.Context,
	client *http.Client,
	b types.BackendMetrics,
	modelName string,
) modelInfoBackendResult {
	entry := modelInfoBackendResult{
		BackendID:   b.ID,
		BackendType: string(b.BackendType),
		Status:      "unavailable",
	}

	if b.Host == "" {
		entry.Error = "backend has no host configured"
		return entry
	}

	port := backendPortForInfo(b)
	if port <= 0 {
		entry.Error = fmt.Sprintf("backend has no usable port (type=%s, ollamaPort=%d, cppWorkerPort=%d)",
			b.BackendType, b.OllamaPort, b.CppWorkerPort)
		return entry
	}

	addr := fmt.Sprintf("%s:%d", b.Host, port)
	showURL := "http://" + addr + "/api/show"

	// Тело запроса — Ollama-формат: {"name": "model"}.
	bodyBytes, err := json.Marshal(map[string]string{"name": modelName})
	if err != nil {
		entry.Status = "error"
		entry.Error = "marshal request: " + err.Error()
		return entry
	}

	httpReq, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		showURL,
		bytes.NewReader(bodyBytes),
	)
	if err != nil {
		entry.Status = "error"
		entry.Error = "build request: " + err.Error()
		return entry
	}
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := client.Do(httpReq)
	if err != nil {
		entry.Status = "unavailable"
		entry.Error = "http: " + err.Error()
		logger.Get().Debugw("clusterModelInfoHandler: backend unreachable",
			"backendId", b.ID, "addr", addr, "error", err)
		return entry
	}
	defer httpResp.Body.Close()

	respBody, readErr := io.ReadAll(httpResp.Body)
	entry.HTTPStatus = httpResp.StatusCode
	if readErr != nil {
		entry.Status = "error"
		entry.Error = "read body: " + readErr.Error()
		return entry
	}

	if httpResp.StatusCode == http.StatusNotFound {
		// Модель не найдена на этом бэкенде — это нормальная ситуация для
		// бэкенда, который её не обслуживает. Помечаем как "not_found" — UI
		// может скрыть этот бэкенд или показать "not served here".
		entry.Status = "not_found"
		entry.Error = fmt.Sprintf("model %q not found on backend %q (HTTP 404)", modelName, b.ID)
		return entry
	}

	if httpResp.StatusCode != http.StatusOK {
		entry.Status = "error"
		entry.Error = fmt.Sprintf("backend returned HTTP %d: %s",
			httpResp.StatusCode, truncateForError(string(respBody)))
		return entry
	}

	// /api/show может возвращать JSON разной формы (Ollama native, cppworker
	// наш формат, кастомные прокси). Парсим как map[string]interface{},
	// чтобы не зависеть от конкретной структуры.
	var info map[string]interface{}
	if err := json.Unmarshal(respBody, &info); err != nil {
		entry.Status = "error"
		entry.Error = "parse response: " + err.Error()
		return entry
	}

	// Ollama возвращает ошибку как 200 OK с {"error": "..."} — проверим.
	if errStr, ok := info["error"].(string); ok && errStr != "" {
		entry.Status = "error"
		entry.Error = "backend error: " + errStr
		return entry
	}

	entry.Status = "ok"
	entry.Info = info
	return entry
}

// backendPortForInfo — выбирает порт бэкенда для запроса /api/show.
//
// Для llama_cpp — CppWorkerPort (default 18091, в bundled compose EXPOSED как 18092).
// Для ollama — OllamaPort (default 11434).
//
// Возвращает 0 если порт не задан.
func backendPortForInfo(b types.BackendMetrics) int {
	if b.BackendType == types.BackendTypeLlamaCpp {
		if b.CppWorkerPort > 0 {
			return b.CppWorkerPort
		}
		// Fallback на дефолт cppworker, если в metrics не задан.
		return 18091
	}
	// Ollama (или пустой тип — legacy).
	if b.OllamaPort > 0 {
		return b.OllamaPort
	}
	return 11434
}

// truncateForError — обрезает большое тело ошибки до 512 символов для логов/JSON.
func truncateForError(s string) string {
	const max = 512
	if len(s) <= max {
		return s
	}
	return s[:max] + "... (truncated)"
}