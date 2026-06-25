// Package api — handler для /api/v1/cppworker/reset-reload-counter.
//
// Проксирует POST-запрос к cppworker-бэкенду для сброса ramFallbackAttempts
// (cycle counter, который блокирует reload после превышения лимита).
//
// Использование:
//
//	curl -X POST -H 'X-API-Token: <token>' \
//	  'http://localhost:18081/api/v1/cppworker/reset-reload-counter?backend=cppworker-gpu'
//
// Без ?backend=... — сбрасывает ВСЕ бэкенды (round-robin по всем
// llama.cpp бэкендам).
//
// До этой версии единственный способ сбросить счётчик — `docker restart
// deployments-cppworker-gpu-1`. Сейчас это можно сделать горячо через
// management API.
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// resetResult — тип для записи результата reset-reload-counter по одному бэкенду.
type resetResult struct {
	BackendID string `json:"backend_id"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Body      string `json:"body,omitempty"`
}

// handleResetCppWorkerReloadCounter — POST /api/v1/cppworker/reset-reload-counter[?backend=X]
//
// Проксирует запрос к указанному (или всем) cppworker-бэкенду. Каждый
// бэкенд получает POST /api/v1/cppworker/reset-reload-counter с тем же токеном.
//
// Защищён authMiddleware (если задан API_TOKEN в balancer config).
func (s *Server) handleResetCppWorkerReloadCounter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}

	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusServiceUnavailable)
		return
	}

	backendID := r.URL.Query().Get("backend")
	if backendID == "" {
		s.resetAllCppWorkerReloadCounters(w, r)
		return
	}
	s.resetSingleCppWorkerReloadCounter(w, r, backendID)
}

// resetSingleCppWorkerReloadCounter проксирует POST к одному cppworker-бэкенду.
func (s *Server) resetSingleCppWorkerReloadCounter(w http.ResponseWriter, r *http.Request, backendID string) {
	backend := s.proxy.GetBackend(backendID)
	if backend == nil {
		http.Error(w, "backend not found: "+backendID, http.StatusNotFound)
		return
	}
	if !isCppWorkerBackend(backend.Type) {
		http.Error(w, "backend "+backendID+" is not a llama_cpp backend (type="+
			string(backend.Type)+")", http.StatusBadRequest)
		return
	}

	port := backend.CppWorkerPort
	if port <= 0 {
		http.Error(w, "invalid port for backend "+backendID, http.StatusInternalServerError)
		return
	}
	url := fmt.Sprintf("http://%s:%d/api/v1/cppworker/reset-reload-counter", backend.Host, port)

	respBody, statusCode, err := s.proxyCppWorkerReset(r, url, backendID)
	if err != nil {
		logger.Get().Errorw("reset-reload-counter: proxy request failed",
			"backend", backendID, "url", url, "error", err)
		http.Error(w, "proxy request failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(respBody)))
	w.WriteHeader(statusCode)
	_, _ = w.Write(respBody)
}

// resetAllCppWorkerReloadCounters проксирует POST ко всем llama.cpp бэкендам.
func (s *Server) resetAllCppWorkerReloadCounters(w http.ResponseWriter, r *http.Request) {
	results := make([]resetResult, 0)

	all := s.proxy.GetAllBackends()
	for _, b := range all {
		if !isCppWorkerBackend(b.Type) {
			continue
		}
		port := b.CppWorkerPort
		if port <= 0 {
			results = append(results, resetResult{BackendID: b.ID, Status: "skipped", Error: "invalid port"})
			continue
		}
		url := fmt.Sprintf("http://%s:%d/api/v1/cppworker/reset-reload-counter", b.Host, port)
		respBody, statusCode, err := s.proxyCppWorkerReset(r, url, b.ID)
		if err != nil {
			results = append(results, resetResult{BackendID: b.ID, Status: "error", Error: err.Error()})
			continue
		}
		if statusCode >= 200 && statusCode < 300 {
			results = append(results, resetResult{BackendID: b.ID, Status: "reset", Body: string(respBody)})
		} else {
			results = append(results, resetResult{BackendID: b.ID, Status: "error",
				Error: fmt.Sprintf("status %d", statusCode), Body: string(respBody)})
		}
	}

	out, _ := json.Marshal(map[string]interface{}{
		"results":      results,
		"total":        len(results),
		"successCount": countResetSuccess(results),
	})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// proxyCppWorkerReset — проксирует POST к одному cppworker endpoint.
// Возвращает body, status code и error.
func (s *Server) proxyCppWorkerReset(r *http.Request, url, backendID string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if s.config != nil && len(s.config.Auth.Tokens) > 0 {
		req.Header.Set("Authorization", "Bearer "+s.config.Auth.Tokens[0])
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// isCppWorkerBackend — true если backend — llama_cpp (cppworker).
func isCppWorkerBackend(t interface{}) bool {
	s := strings.ToLower(fmt.Sprintf("%v", t))
	return s == "llama_cpp" || s == "llamacpp" || s == "llama.cpp"
}

func countResetSuccess(results []resetResult) int {
	n := 0
	for _, r := range results {
		if r.Status == "reset" {
			n++
		}
	}
	return n
}