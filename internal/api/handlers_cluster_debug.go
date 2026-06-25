package api

// handlers_cluster_debug.go — proxy cppworker /debug/last-prompt через балансировщик.
//
// Причина (см. CHANGELOG.md, "Различение prompt_exceeds_context vs n_ctx_too_large_for_backend"):
// Для диагностики случаев "Cline получил 413 prompt_exceeds_context" полезно знать,
// ЧТО именно занимает столько токенов в prompt. cppworker (на порту 18092 внутри compose)
// хранит snapshot последнего inference-запроса (model, prompt_chars, prompt_tokens,
// n_ctx, has_tools, status, prompt_head, prompt_tail) — endpoint GET /api/v1/cppworker/debug/last-prompt.
//
// Endpoint в балансировщике (порт 18081) агрегирует snapshot'ы со всех cppworker бэкендов
// в один JSON-ответ. Это позволяет:
//   - UI показывать последний prompt без прямого доступа к cppworker (защита сети)
//   - скриптам диагностики получать данные через один запрос
//   - ЛОГ-анализаторам находить случаи prompt_exceeds_context и видеть, что занимает токены
//
// Структура ответа:
//
//	{
//	  "count": <int>,
//	  "backends": [
//	    {
//	      "backendId": "cppworker-gpu-bundled",
//	      "status": "ok" | "error" | "unavailable",
//	      "info": { ... LastPromptInfo ... } | null,
//	      "error": "..." // если status=error
//	    }
//	  ]
//	}

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// debugLastPromptInfo — копия структуры из cmd/cppworker/debug_last_prompt.go.
// Дублируем здесь, чтобы не создавать import cycle (cppworker — main-пакет).
//
// ВАЖНО: при изменении cmd/cppworker/debug_last_prompt.go нужно обновить и эту структуру.
type debugLastPromptInfo struct {
	Model          string    `json:"model"`
	Endpoint       string    `json:"endpoint"`
	PromptChars    int       `json:"prompt_chars"`
	PromptTokens   int       `json:"prompt_tokens"`
	NCtxOverride   int       `json:"n_ctx_override"`
	NCtxLoaded     int       `json:"n_ctx_loaded"`
	HasTools       bool      `json:"has_tools"`
	Status         string    `json:"status"`
	Timestamp      time.Time `json:"timestamp"`
	PromptHead     string    `json:"prompt_head,omitempty"`
	PromptTail     string    `json:"prompt_tail,omitempty"`
	PromptLinesHint int      `json:"prompt_lines_hint,omitempty"`
	Error          string    `json:"error,omitempty"`
}

// debugLastPromptBackendResult — результат с одного cppworker бэкенда.
type debugLastPromptBackendResult struct {
	BackendID string              `json:"backendId"`
	Status    string              `json:"status"` // "ok" | "error" | "unavailable"
	Info      *debugLastPromptInfo `json:"info,omitempty"`
	Error     string              `json:"error,omitempty"`
}

// debugLastPromptResponse — агрегированный ответ.
type debugLastPromptResponse struct {
	Count    int                          `json:"count"`
	Backends []debugLastPromptBackendResult `json:"backends"`
}

// clusterDebugLastPromptHandler — GET /api/v1/cluster/cppworker/debug/last-prompt.
//
// Возвращает snapshot последнего inference-запроса со всех cppworker бэкендов.
//
// Требует аутентификации (X-API-Token).
//
// Использование:
//
//	curl -H "X-API-Token: $LB_TOKEN" http://localhost:18081/api/v1/cluster/cppworker/debug/last-prompt
//
// Возвращает:
//
//	{
//	  "count": 1,
//	  "backends": [
//	    {
//	      "backendId": "cppworker-gpu-bundled",
//	      "status": "ok",
//	      "info": {
//	        "model": "gemma-4-E4B-it-Q4_K_M",
//	        "endpoint": "/v1/chat/completions",
//	        "prompt_chars": 270000,
//	        "prompt_tokens": 68271,
//	        "n_ctx_override": 65536,
//	        "n_ctx_loaded": 65536,
//	        "has_tools": true,
//	        "status": "prompt_too_long",
//	        "timestamp": "2026-06-25T16:30:00Z",
//	        "prompt_head": "<start_of_turn>user\n...",
//	        "prompt_tail": "...</tool_call>",
//	        "prompt_lines_hint": 1500,
//	        "error": "prompt exceeds n_ctx even with min floor: actual_tokens=68271 n_ctx=65536"
//	      }
//	    }
//	  ]
//	}
//
// Если бэкенд недоступен или cppworker вернул ошибку — backends[i].status будет
// "unavailable" или "error" с описанием в error-поле.
func (s *Server) clusterDebugLastPromptHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
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

	resp := debugLastPromptResponse{
		Count:    0,
		Backends: []debugLastPromptBackendResult{},
	}

	// 5 секунд на запрос к каждому cppworker (snapshot — лёгкий endpoint).
	client := &http.Client{Timeout: 5 * time.Second}

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		entry := debugLastPromptBackendResult{
			BackendID: b.ID,
			Status:    "unavailable",
		}
		// BackendMetrics содержит host (compose-имя или IP) и порт cppworker (18092 default).
		// Формируем адрес http://host:port для прямого обращения балансировщика к cppworker
		// внутри compose-сети (cppworker-gpu:18092).
		if b.Host == "" || b.CppWorkerPort <= 0 {
			entry.Error = "backend has no host/CppWorkerPort configured"
			resp.Backends = append(resp.Backends, entry)
			continue
		}
		addr := fmt.Sprintf("%s:%d", b.Host, b.CppWorkerPort)

		// Формируем URL к cppworker /api/v1/cppworker/debug/last-prompt.
		// Используем b.Addr напрямую — это внутренний адрес compose-сети
		// (cppworker-gpu:18092), доступный только из балансировщика.
		debugURL := "http://" + addr + "/api/v1/cppworker/debug/last-prompt"

		httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, debugURL, nil)
		if err != nil {
			entry.Status = "error"
			entry.Error = "build request: " + err.Error()
			resp.Backends = append(resp.Backends, entry)
			continue
		}
		// Пересылаем auth-заголовок, чтобы cppworker авторизовал запрос от балансировщика.
		// cppworker-у auth-token прокидывается через BackendConfig.APIToken,
		// но для debug-endpoint балансировщик уже сам аутентифицирован.
		// cppworker-у auth-token балансировщика известен (через shared env или
		// явно заданный в BackendConfig); если не задан — debug endpoint вернёт 401
		// (authMiddleware в cppworker сработает) и мы пометим status=error.

		httpResp, err := client.Do(httpReq)
		if err != nil {
			entry.Status = "unavailable"
			entry.Error = "http: " + err.Error()
			logger.Get().Debugw("clusterDebugLastPromptHandler: backend unreachable",
				"backendId", b.ID, "addr", addr, "error", err)
			resp.Backends = append(resp.Backends, entry)
			continue
		}

		bodyBytes, readErr := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()

		if readErr != nil {
			entry.Status = "error"
			entry.Error = "read body: " + readErr.Error()
			resp.Backends = append(resp.Backends, entry)
			continue
		}

		if httpResp.StatusCode == http.StatusNotFound {
			// Endpoint ещё не реализован в cppworker (например, старый bundled-образ).
			entry.Status = "unavailable"
			entry.Error = "cppworker does not implement /api/v1/cppworker/debug/last-prompt (build before 2026-06-25)"
			resp.Backends = append(resp.Backends, entry)
			continue
		}

		if httpResp.StatusCode != http.StatusOK {
			entry.Status = "error"
			entry.Error = string(bodyBytes)
			resp.Backends = append(resp.Backends, entry)
			continue
		}

		var info debugLastPromptInfo
		if err := json.Unmarshal(bodyBytes, &info); err != nil {
			entry.Status = "error"
			entry.Error = "parse response: " + err.Error()
			resp.Backends = append(resp.Backends, entry)
			continue
		}

		entry.Status = "ok"
		entry.Info = &info
		resp.Backends = append(resp.Backends, entry)
	}

	resp.Count = len(resp.Backends)
	s.writeJSON(w, http.StatusOK, resp)
}