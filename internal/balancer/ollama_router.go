package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// OllamaTag — модель из ответа /api/tags
type OllamaTag struct {
	Name       string            `json:"name"`
	Model      string            `json:"model,omitempty"`
	ModifiedAt time.Time         `json:"modified_at"`
	Size       int64             `json:"size"`
	Digest     string            `json:"digest"`
	Details    map[string]interface{} `json:"details"`
}

// OllamaTagsResponse — ответ /api/tags
type OllamaTagsResponse struct {
	Models []OllamaTag `json:"models"`
}

// OllamaProcess — запущенная модель из /api/ps
type OllamaProcess struct {
	Name      string            `json:"name"`
	Model     string            `json:"model,omitempty"`
	Size      int64             `json:"size"`
	Digest    string            `json:"digest"`
	Details   map[string]interface{} `json:"details"`
	ExpiresAt time.Time         `json:"expires_at"`
	SizeVRAM  int64             `json:"size_vram"`
}

// OllamaPSResponse — ответ /api/ps
type OllamaPSResponse struct {
	Models []OllamaProcess `json:"models"`
}

// OllamaVersionResponse — ответ /api/version
type OllamaVersionResponse struct {
	Version string `json:"version"`
}

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

// ---------- Read-only aggregation endpoints ----------

// handleTags — агрегация списка моделей со всех бэкендов
func (or *OllamaRouter) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := or.getHealthyBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
		return
	}

	// Параллельные запросы ко всем бэкендам
	var wg sync.WaitGroup
	results := make(chan []OllamaTag, len(backends))
	errors := make(chan error, len(backends))

	for _, backend := range backends {
		wg.Add(1)
		go func(b backendInfo) {
			defer wg.Done()
			tags, err := or.fetchTags(b.host, b.port)
			if err != nil {
				errors <- fmt.Errorf("backend %s: %v", b.id, err)
				return
			}
			results <- tags
		}(backend)
	}

	go func() {
		wg.Wait()
		close(results)
		close(errors)
	}()

	// Собираем модели в map по имени (убираем дубликаты)
	uniqueModels := make(map[string]OllamaTag)
	for tags := range results {
		for _, t := range tags {
			uniqueModels[t.Name] = t
		}
	}

	// Конвертируем в слайс
	models := make([]OllamaTag, 0, len(uniqueModels))
	for _, m := range uniqueModels {
		models = append(models, m)
	}

	// Сортируем по имени для стабильного порядка (детерминированно для тестов)
	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})

	writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: models})
}

// handlePS — агрегация запущенных процессов со всех бэкендов
func (or *OllamaRouter) handlePS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := or.getHealthyBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaPSResponse{Models: []OllamaProcess{}})
		return
	}

	var wg sync.WaitGroup
	results := make(chan []OllamaProcess, len(backends))

	for _, backend := range backends {
		wg.Add(1)
		go func(b backendInfo) {
			defer wg.Done()
			procs, err := or.fetchPS(b.host, b.port)
			if err != nil {
				return
			}
			results <- procs
		}(backend)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var allProcesses []OllamaProcess
	for procs := range results {
		allProcesses = append(allProcesses, procs...)
	}

	writeJSON(w, http.StatusOK, OllamaPSResponse{Models: allProcesses})
}

// handleVersion — версия балансировщика + версии всех бэкендов
func (or *OllamaRouter) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Версия балансировщика
	response := map[string]interface{}{
		"version":        "ollamalegion-1.0.0",
		"ollamaVersions": make(map[string]string),
	}

	backends := or.getHealthyBackends()
	if len(backends) > 0 {
		var wg sync.WaitGroup
		versions := make(map[string]string)
		var mu sync.Mutex

		for _, backend := range backends {
			wg.Add(1)
			go func(b backendInfo) {
				defer wg.Done()
				ver, err := or.fetchVersion(b.host, b.port)
				if err != nil {
					return
				}
				mu.Lock()
				versions[b.id] = ver
				mu.Unlock()
			}(backend)
		}
		wg.Wait()

		response["ollamaVersions"] = versions
	}

	writeJSON(w, http.StatusOK, response)
}

// ---------- Targeted routing endpoints ----------

// handleShow — найти бэкенд с моделью и проксировать запрос туда
func (or *OllamaRouter) handleShow(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	// Ищем бэкенд, где модель загружена (RunningModels)
	backendID := or.findBackendWithModel(model)
	if backendID == "" {
		// Fallback: проксируем на любой healthy бэкенд
		backendID = or.selectAnyHealthy()
	}

	or.proxy.proxyRequest(w, r, backendID)
}

// handleCreate — маршрутизация на бэкенд с максимальными свободными ресурсами
func (or *OllamaRouter) handleCreate(w http.ResponseWriter, r *http.Request) {
	backendID := or.selectBackendByResources()
	or.proxy.proxyRequest(w, r, backendID)
}

// handlePull — аналогично create
func (or *OllamaRouter) handlePull(w http.ResponseWriter, r *http.Request) {
	backendID := or.selectBackendByResources()
	or.proxy.proxyRequest(w, r, backendID)
}

// handleDelete — удаление модели со всех бэкендов, где она есть
func (or *OllamaRouter) handleDelete(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	// Ищем все бэкенды с моделью
	targets := or.findBackendsWithModel(model)
	if len(targets) == 0 {
		http.Error(w, `{"error":"model not found on any backend"}`, http.StatusNotFound)
		return
	}

	// Отправляем запрос на все бэкенды параллельно
	var wg sync.WaitGroup
	var firstResp *http.Response
	var firstErr error
	var mu sync.Mutex
	var gotResult bool

	for _, backendID := range targets {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			resp, err := or.proxyHTTP(r, id)
			mu.Lock()
			defer mu.Unlock()
			if !gotResult && err == nil && resp != nil && resp.StatusCode == http.StatusOK {
				firstResp = resp
				gotResult = true
			} else if resp != nil {
				// Закрываем Body для ненужных ответов, чтобы избежать утечки
				resp.Body.Close()
			}
			if firstErr == nil && err != nil {
				firstErr = err
			}
		}(backendID)
	}
	wg.Wait()

	// Возвращаем успешный ответ от первого бэкенда
	if firstResp != nil {
		copyResponse(w, firstResp)
		return
	}

	http.Error(w, `{"error":"failed to delete model"}`, http.StatusInternalServerError)
}

// handleCopy — на бэкенд с исходной моделью
func (or *OllamaRouter) handleCopy(w http.ResponseWriter, r *http.Request) {
	// Извлекаем source и destination из тела
	body := or.readBody(r)
	var req struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	json.Unmarshal(body, &req)

	sourceBackend := or.findBackendWithModel(req.Source)
	if sourceBackend == "" {
		http.Error(w, `{"error":"source model not found"}`, http.StatusNotFound)
		return
	}

	// Восстанавливаем тело для проксирования
	r.Body = io.NopCloser(bytes.NewBuffer(body))
	or.proxy.proxyRequest(w, r, sourceBackend)
}

// handlePush — на бэкенд с моделью
func (or *OllamaRouter) handlePush(w http.ResponseWriter, r *http.Request) {
	model := or.extractModelFromBody(r)
	if model == "" {
		http.Error(w, `{"error":"model name required"}`, http.StatusBadRequest)
		return
	}

	backendID := or.findBackendWithModel(model)
	if backendID == "" {
		http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
		return
	}

	or.proxy.proxyRequest(w, r, backendID)
}

// ---------- Вспомогательные функции ----------

type backendInfo struct {
	id   string
	host string
	port int
}

func (or *OllamaRouter) getHealthyBackends() []backendInfo {
	backends := or.proxy.GetAllBackends()
	result := make([]backendInfo, 0, len(backends))
	for _, b := range backends {
		if b.Status == "healthy" {
			result = append(result, backendInfo{
				id:   b.ID,
				host: b.Host,
				port: b.OllamaPort,
			})
		}
	}
	return result
}

func (or *OllamaRouter) fetchTags(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaTagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Models, nil
}

func (or *OllamaRouter) fetchPS(host string, port int) ([]OllamaProcess, error) {
	url := fmt.Sprintf("http://%s:%d/api/ps", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaPSResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data.Models, nil
}

func (or *OllamaRouter) fetchVersion(host string, port int) (string, error) {
	url := fmt.Sprintf("http://%s:%d/api/version", host, port)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}

	var data OllamaVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", err
	}
	return data.Version, nil
}

func (or *OllamaRouter) findBackendWithModel(model string) string {
	backends := or.proxy.GetAllBackends()
	state := or.proxy.GetClusterState()

	// Строим мапу ID → RunningModels для O(1) поиска
	runningModelsMap := make(map[string][]types.RunningModel, len(state.Backends))
	for _, metrics := range state.Backends {
		runningModelsMap[metrics.ID] = metrics.Ollama.RunningModels
	}

	for _, b := range backends {
		if b.Status != "healthy" {
			continue
		}
		if models, ok := runningModelsMap[b.ID]; ok {
			for _, m := range models {
				if m.Name == model {
					return b.ID
				}
			}
		}
	}
	return ""
}

func (or *OllamaRouter) findBackendsWithModel(model string) []string {
	var result []string
	backends := or.proxy.GetAllBackends()
	state := or.proxy.GetClusterState()

	// Строим мапу ID → RunningModels для O(1) поиска
	runningModelsMap := make(map[string][]types.RunningModel, len(state.Backends))
	for _, metrics := range state.Backends {
		runningModelsMap[metrics.ID] = metrics.Ollama.RunningModels
	}

	for _, b := range backends {
		if b.Status != "healthy" {
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

func (or *OllamaRouter) selectBackendByResources() string {
	// Используем существующую логику выбора бэкенда
	model := or.extractModelFromBody(nil)
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
	// Восстанавливаем тело для повторного чтения
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return ""
	}

	if model, ok := req["model"].(string); ok {
		return model
	}
	// Для /api/copy
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