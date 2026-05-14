package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// ModelManager — менеджер операций с моделями на бэкендах.
// Отправляет HTTP-запросы напрямую к Ollama API на указанном бэкенде.
type ModelManager struct {
	proxy  *Proxy
	client *http.Client
	mu     sync.RWMutex
	// Активные операции (modelName -> backendID -> startedAt)
	activeOps map[string]map[string]time.Time
}

// NewModelManager — создание менеджера моделей
func NewModelManager(proxy *Proxy) *ModelManager {
	return &ModelManager{
		proxy:     proxy,
		client: &http.Client{
			Timeout: 10 * time.Minute, // pull/push могут быть долгими
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		activeOps: make(map[string]map[string]time.Time),
	}
}

// ModelOpRequest — запрос на выполнение операции с моделью
type ModelOpRequest struct {
	Operation string `json:"operation"` // pull, push, delete, load, unload
	ModelName string `json:"modelName"`
	Insecure  bool   `json:"insecure,omitempty"`
	Stream    bool   `json:"stream,omitempty"`
}

// ModelOpResult — результат операции с моделью
type ModelOpResult struct {
	Success   bool   `json:"success"`
	Operation string `json:"operation"`
	ModelName string `json:"modelName"`
	BackendID string `json:"backendId"`
	Message   string `json:"message,omitempty"`
	Error     string `json:"error,omitempty"`
}

// ModelInfo — информация о модели на бэкенде
type ModelInfo struct {
	Name       string `json:"name"`
	Model      string `json:"model,omitempty"`
	Size       int64  `json:"size"`
	Digest     string `json:"digest"`
	ModifiedAt string `json:"modifiedAt,omitempty"`
	Loaded     bool   `json:"loaded"` // загружена ли в память
}

// ExecuteOperation — выполнение операции с моделью на указанном бэкенде
func (mm *ModelManager) ExecuteOperation(backendID string, req ModelOpRequest) *ModelOpResult {
	backend := mm.proxy.GetBackend(backendID)
	if backend == nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("backend '%s' not found", backendID),
		}
	}

	host := backend.Host
	port := backend.OllamaPort

	// Проверяем, не выполняется ли уже такая операция
	if !mm.tryAcquireOp(req.Operation, req.ModelName, backendID) {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("operation '%s' for model '%s' is already in progress on backend '%s'", req.Operation, req.ModelName, backendID),
		}
	}
	defer mm.releaseOp(req.Operation, req.ModelName, backendID)

	logger.Get().Infow("executing model operation",
		"operation", req.Operation,
		"model", req.ModelName,
		"backend", backendID,
		"host", host,
		"port", port)

	var result *ModelOpResult
	switch strings.ToLower(req.Operation) {
	case "pull":
		result = mm.executePull(host, port, backendID, req)
	case "push":
		result = mm.executePush(host, port, backendID, req)
	case "delete":
		result = mm.executeDelete(host, port, backendID, req)
	case "load":
		result = mm.executeLoad(host, port, backendID, req)
	case "unload":
		result = mm.executeUnload(host, port, backendID, req)
	default:
		result = &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("unsupported operation: %s", req.Operation),
		}
	}

	return result
}

// ListModels — получение списка моделей на бэкенде (из /api/tags и /api/ps)
func (mm *ModelManager) ListModels(backendID string) ([]ModelInfo, error) {
	backend := mm.proxy.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend '%s' not found", backendID)
	}

	// Получаем все модели из /api/tags
	tagsURL := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, backend.OllamaPort)
	tagsResp, err := mm.client.Get(tagsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tags from backend %s: %w", backendID, err)
	}
	defer tagsResp.Body.Close()

	var tagsData struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model,omitempty"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest"`
			ModifiedAt string `json:"modified_at"`
		} `json:"models"`
	}
	if err := json.NewDecoder(tagsResp.Body).Decode(&tagsData); err != nil {
		return nil, fmt.Errorf("failed to decode tags from backend %s: %w", backendID, err)
	}

	// Получаем загруженные модели из /api/ps
	psURL := fmt.Sprintf("http://%s:%d/api/ps", backend.Host, backend.OllamaPort)
	loadedModels := make(map[string]bool)

	psResp, psErr := mm.client.Get(psURL)
	if psErr == nil {
		defer psResp.Body.Close()
		var psData struct {
			Models []struct {
				Name string `json:"name"`
			} `json:"models"`
		}
		if err := json.NewDecoder(psResp.Body).Decode(&psData); err == nil {
			for _, m := range psData.Models {
				loadedModels[m.Name] = true
			}
		}
	}

	// Собираем результат
	models := make([]ModelInfo, 0, len(tagsData.Models))
	for _, t := range tagsData.Models {
		models = append(models, ModelInfo{
			Name:       t.Name,
			Model:      t.Model,
			Size:       t.Size,
			Digest:     t.Digest,
			ModifiedAt: t.ModifiedAt,
			Loaded:     loadedModels[t.Name],
		})
	}

	return models, nil
}

// GetActiveOps — возвращает список активных операций
func (mm *ModelManager) GetActiveOps() []map[string]interface{} {
	mm.mu.RLock()
	defer mm.mu.RUnlock()

	var result []map[string]interface{}
	for key, backends := range mm.activeOps {
		// key = operation + ":" + modelName
		op := ""
		modelName := key
		if idx := strings.Index(key, ":"); idx > 0 {
			op = key[:idx]
			modelName = key[idx+1:]
		}
		for backendID, startedAt := range backends {
			result = append(result, map[string]interface{}{
				"operation": op,
				"modelName": modelName,
				"backendId": backendID,
				"startedAt": startedAt.Format(time.RFC3339),
				"duration":  time.Since(startedAt).String(),
				"status":    "running",
			})
		}
	}
	return result
}

// ===== Внутренние реализации =====

// executePull — загрузка модели на бэкенд
func (mm *ModelManager) executePull(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/pull", host, port)

	body := map[string]interface{}{
		"name":   req.ModelName,
		"stream": req.Stream,
	}
	if req.Insecure {
		body["insecure"] = true
	}

	return mm.sendOllamaRequest("POST", url, backendID, req, body)
}

// executePush — отправка модели с бэкенда
func (mm *ModelManager) executePush(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/push", host, port)

	body := map[string]interface{}{
		"name":   req.ModelName,
		"stream": req.Stream,
	}
	if req.Insecure {
		body["insecure"] = true
	}

	return mm.sendOllamaRequest("POST", url, backendID, req, body)
}

// executeDelete — удаление модели с бэкенда
func (mm *ModelManager) executeDelete(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/delete", host, port)

	body := map[string]interface{}{
		"name": req.ModelName,
	}

	return mm.sendOllamaRequest("DELETE", url, backendID, req, body)
}

// executeLoad — загрузка модели в память на бэкенде (через /api/generate с keep_alive)
func (mm *ModelManager) executeLoad(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/generate", host, port)

	body := map[string]interface{}{
		"model":     req.ModelName,
		"keep_alive": "5m",
		"prompt":    "", // пустой промпт для загрузки без генерации
		"stream":    false,
	}

	resp, err := mm.sendRawRequest("POST", url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to load model: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' loaded successfully on backend '%s'", req.ModelName, backendID),
		}
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// executeUnload — выгрузка модели из памяти (через /api/generate с keep_alive=0)
func (mm *ModelManager) executeUnload(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/generate", host, port)

	body := map[string]interface{}{
		"model":      req.ModelName,
		"keep_alive": "0s",
		"prompt":     "",
		"stream":     false,
	}

	resp, err := mm.sendRawRequest("POST", url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to unload model: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' unloaded successfully on backend '%s'", req.ModelName, backendID),
		}
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// sendOllamaRequest — общая отправка запроса к Ollama API
func (mm *ModelManager) sendOllamaRequest(method, url, backendID string, req ModelOpRequest, body map[string]interface{}) *ModelOpResult {
	resp, err := mm.sendRawRequest(method, url, body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("request failed: %v", err),
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		opName := operationDisplayName(req.Operation)
		return &ModelOpResult{
			Success:   true,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Message:   fmt.Sprintf("Model '%s' %s on backend '%s'", req.ModelName, opName, backendID),
		}
	}

	// Парсим ошибку Ollama
	errMsg := string(respBody)
	var ollamaErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(respBody, &ollamaErr) == nil && ollamaErr.Error != "" {
		errMsg = ollamaErr.Error
	}

	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("ollama error (HTTP %d): %s", resp.StatusCode, errMsg),
	}
}

// sendRawRequest — отправка HTTP запроса к Ollama
func (mm *ModelManager) sendRawRequest(method, url string, body interface{}) (*http.Response, error) {
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return mm.client.Do(req)
}

// ===== Вспомогательные методы =====

func (mm *ModelManager) tryAcquireOp(operation, modelName, backendID string) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	key := operation + ":" + modelName
	if backends, exists := mm.activeOps[key]; exists {
		if _, running := backends[backendID]; running {
			return false
		}
		backends[backendID] = time.Now()
	} else {
		mm.activeOps[key] = map[string]time.Time{backendID: time.Now()}
	}
	return true
}

func (mm *ModelManager) releaseOp(operation, modelName, backendID string) {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	key := operation + ":" + modelName
	if backends, exists := mm.activeOps[key]; exists {
		delete(backends, backendID)
		if len(backends) == 0 {
			delete(mm.activeOps, key)
		}
	}
}

func operationDisplayName(op string) string {
	switch op {
	case "pull":
		return "pulled"
	case "push":
		return "pushed"
	case "delete":
		return "deleted"
	case "load":
		return "loaded into memory"
	case "unload":
		return "unloaded from memory"
	default:
		return op + "ed"
	}
}

// GetBackendModelManager — возвращает ModelManager из Proxy
func (p *Proxy) GetModelManager() *ModelManager {
	return p.modelManager
}

// SetModelManager — устанавливает ModelManager в Proxy (вызывается при инициализации)
func (p *Proxy) SetModelManager(mm *ModelManager) {
	p.modelManager = mm
}
