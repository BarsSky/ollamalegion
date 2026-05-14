package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// OllamaRouter — маршрутизатор для Ollama API endpoint'ов
type OllamaRouter struct {
	proxy *Proxy
}

// NewOllamaRouter — создание маршрутизатора
func NewOllamaRouter(proxy *Proxy) *OllamaRouter {
	return &OllamaRouter{proxy: proxy}
}

// Route — диспетчеризация запроса по URL.Path.
// Возвращает true если запрос был обработан.
func (or *OllamaRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/tags":
		or.handleTags(w, r)
		return true
	case "/api/version":
		or.handleVersion(w, r)
		return true
	case "/api/ps":
		or.handlePS(w, r)
		return true
	case "/api/show":
		or.handleShow(w, r)
		return true
	case "/api/create":
		or.handleCreate(w, r)
		return true
	case "/api/pull":
		or.handlePull(w, r)
		return true
	case "/api/delete":
		or.handleDelete(w, r)
		return true
	case "/api/copy":
		or.handleCopy(w, r)
		return true
	case "/api/push":
		or.handlePush(w, r)
		return true
	}
	return false
}

// ---------- Вспомогательные структуры и функции ----------

type backendInfo struct {
	id   string
	host string
	port int
}

// getHealthyBackends — возвращает все healthy и degraded бэкенды для агрегации.
func (or *OllamaRouter) getHealthyBackends() []backendInfo {
	backends := or.proxy.GetAllBackends()
	result := make([]backendInfo, 0, len(backends))
	for _, b := range backends {
		if b.Status == "healthy" || b.Status == "degraded" {
			result = append(result, backendInfo{
				id:   b.ID,
				host: b.Host,
				port: b.OllamaPort,
			})
		}
	}
	logger.Get().Debugw("getHealthyBackends for aggregation",
		"total_backends", len(backends),
		"included", len(result),
	)
	return result
}

func (or *OllamaRouter) findBackendWithModel(model string) string {
	or.proxy.metricsMgr.mu.RLock()
	defer or.proxy.metricsMgr.mu.RUnlock()

	for id, metrics := range or.proxy.metricsMgr.metrics {
		// Проверяем статус бэкенда через proxy
		or.proxy.mu.RLock()
		state, ok := or.proxy.backends[id]
		or.proxy.mu.RUnlock()
		if !ok || (state.Backend.Status != types.StatusHealthy && string(state.Backend.Status) != "degraded") {
			continue
		}
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == model {
				return id
			}
		}
	}
	return ""
}

func (or *OllamaRouter) findBackendsWithModel(model string) []string {
	or.proxy.metricsMgr.mu.RLock()
	runningModelsMap := make(map[string][]types.RunningModel)
	for id, metrics := range or.proxy.metricsMgr.metrics {
		runningModelsMap[id] = metrics.Ollama.RunningModels
	}
	or.proxy.metricsMgr.mu.RUnlock()

	var result []string
	for _, b := range or.proxy.GetAllBackends() {
		if b.Status != types.StatusHealthy && string(b.Status) != "degraded" {
			continue
		}
		if models, ok := runningModelsMap[b.ID]; ok {
			for _, m := range models {
				if m.Name == model {
					result = append(result, b.ID)
					break
				}
			}
		}
	}
	return result
}

func (or *OllamaRouter) selectAnyHealthy() string {
	backends := or.proxy.GetAllBackends()
	for _, b := range backends {
		if b.Status == "healthy" {
			return b.ID
		}
	}
	return ""
}

func (or *OllamaRouter) selectBackendByResources(r *http.Request) string {
	model := or.extractModelFromBody(r)
	return or.proxy.selectBackend(model)
}

func (or *OllamaRouter) extractModelFromBody(r *http.Request) string {
	if r == nil || r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}

	if model, ok := req["model"].(string); ok {
		return model
	}
	if name, ok := req["name"].(string); ok {
		return name
	}
	if source, ok := req["source"].(string); ok {
		return source
	}
	return ""
}

func (or *OllamaRouter) readBody(r *http.Request) []byte {
	if r == nil || r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	return body
}

func (or *OllamaRouter) proxyHTTP(r *http.Request, backendID string) (*http.Response, error) {
	backend := or.proxy.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend not found")
	}

	url := fmt.Sprintf("http://%s:%d%s", backend.Host, backend.OllamaPort, r.URL.String())
	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, r.Body)
	if err != nil {
		return nil, err
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	return client.Do(req)
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}