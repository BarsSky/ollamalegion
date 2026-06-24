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
	"ollama-loadbalancer/pkg/types"
)

// ModelManager — менеджер операций с моделями на бэкендах.
// Отправляет HTTP-запросы напрямую к Ollama API или к CppWorker (для llama.cpp),
// в зависимости от типа бэкенда.
type ModelManager struct {
	proxy  *Proxy
	client *http.Client
	mu     sync.RWMutex
	// Активные операции (modelName -> backendID -> startedAt)
	activeOps map[string]map[string]time.Time
}

// modelOpTTL — таймаут жизни «залипших» операций (после которого запись автоматически
// считается устаревшей и может быть перезаписана). Защищает от ситуации, когда
// предыдущая операция упала по сети и оставила запись, блокирующую повторные попытки.
const modelOpTTL = 10 * time.Minute

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
	Operation  string `json:"operation"` // pull, push, delete, load, unload
	ModelName  string `json:"modelName"`
	ContextSize *int  `json:"contextSize,omitempty"` // optional: override n_ctx для загрузки
	GPULayers   *int  `json:"gpuLayers,omitempty"`   // optional: override gpu_layers для загрузки
	Insecure   bool   `json:"insecure,omitempty"`
	Stream     bool   `json:"stream,omitempty"`
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

// resolveBackendPort — возвращает порт инференса в зависимости от движка бэкенда.
// Для llama.cpp — CppWorkerPort (с fallback 18092), для ollama — OllamaPort.
func (mm *ModelManager) resolveBackendPort(backend *types.Backend) int {
	engine := types.ResolveEngine(backend.Engine, backend.Type)
	if engine == types.EngineLlamaCPP {
		if backend.CppWorkerPort > 0 {
			return backend.CppWorkerPort
		}
		return 18092 // актуальный default для современных cppworker
	}
	return backend.OllamaPort
}

// isLlamaCppBackend — true, если бэкенд работает через cppworker, а не Ollama.
func (mm *ModelManager) isLlamaCppBackend(backend *types.Backend) bool {
	engine := types.ResolveEngine(backend.Engine, backend.Type)
	return engine == types.EngineLlamaCPP
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
	port := mm.resolveBackendPort(backend)
	isLlamaCpp := mm.isLlamaCppBackend(backend)

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

	// Доступ к request_id из глобального fallback-context (ensureModelLoadedOnBackend
	// пока не прокидывает ctx сюда явно — TODO). Если ctx есть, используем
	// ridLogWith() для автоматического добавления request_id в каждую log-запись.
	logFn := logger.Get()
	_ = logFn

	logger.Get().Infow("executing model operation",
		"operation", req.Operation,
		"model", req.ModelName,
		"backend", backendID,
		"engine", string(types.ResolveEngine(backend.Engine, backend.Type)),
		"host", host,
		"port", port)

	var result *ModelOpResult
	op := strings.ToLower(req.Operation)

	// pull/push — только для ollama-бэкендов
	if op == "pull" || op == "push" {
		if isLlamaCpp {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("operation '%s' is not supported for llama.cpp backends; use HF download instead", req.Operation),
			}
		}
	}

	switch op {
	case "pull":
		result = mm.executePull(host, port, backendID, req)
	case "push":
		result = mm.executePush(host, port, backendID, req)
	case "delete":
		if isLlamaCpp {
			result = mm.executeLlamaCppDelete(host, port, backendID, req)
		} else {
			result = mm.executeDelete(host, port, backendID, req)
		}
	case "load":
		if isLlamaCpp {
			result = mm.executeLlamaCppLoad(host, port, backendID, req)
		} else {
			result = mm.executeLoad(host, port, backendID, req)
		}
	case "unload":
		if isLlamaCpp {
			result = mm.executeLlamaCppUnload(host, port, backendID, req)
		} else {
			result = mm.executeUnload(host, port, backendID, req)
		}
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

// ListModels — получение списка моделей на бэкенде.
// Для ollama-бэкендов: /api/tags + /api/ps. Для llama.cpp-бэкендов:
// /api/models/files (список на диске) + /api/models/loaded (что в памяти).
func (mm *ModelManager) ListModels(backendID string) ([]ModelInfo, error) {
	backend := mm.proxy.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend '%s' not found", backendID)
	}

	if mm.isLlamaCppBackend(backend) {
		return mm.listLlamaCppModels(backend)
	}
	return mm.listOllamaModels(backend)
}

// listOllamaModels — список моделей Ollama-бэкенда через /api/tags + /api/ps.
func (mm *ModelManager) listOllamaModels(backend *types.Backend) ([]ModelInfo, error) {
	port := backend.OllamaPort
	tagsURL := fmt.Sprintf("http://%s:%d/api/tags", backend.Host, port)
	tagsResp, err := mm.client.Get(tagsURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch tags from backend %s: %w", backend.ID, err)
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
		return nil, fmt.Errorf("failed to decode tags from backend %s: %w", backend.ID, err)
	}

	psURL := fmt.Sprintf("http://%s:%d/api/ps", backend.Host, port)
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

// listLlamaCppModels — список моделей cppworker-бэкенда: /api/models/files (на диске)
// + /api/models/loaded (что в памяти). Метка Loaded выставляется по handle.
func (mm *ModelManager) listLlamaCppModels(backend *types.Backend) ([]ModelInfo, error) {
	port := mm.resolveBackendPort(backend)
	base := fmt.Sprintf("http://%s:%d", backend.Host, port)

	filesURL := base + "/api/models/files"
	filesResp, err := mm.client.Get(filesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models from cppworker %s: %w", backend.ID, err)
	}
	defer filesResp.Body.Close()

	var filesData struct {
		Files []struct {
			Path         string `json:"path"`
			SizeBytes    int64  `json:"sizeBytes"`
			Quantization string `json:"quantization"`
		} `json:"files"`
	}
	if err := json.NewDecoder(filesResp.Body).Decode(&filesData); err != nil {
		return nil, fmt.Errorf("failed to decode models from cppworker %s: %w", backend.ID, err)
	}

	loadedSet := make(map[string]bool)
	loadedURL := base + "/api/models/loaded"
	if lr, lerr := mm.client.Get(loadedURL); lerr == nil {
		defer lr.Body.Close()
		var ld struct {
			Models []struct {
				Handle string `json:"handle"`
				Name   string `json:"name,omitempty"`
				Path   string `json:"path,omitempty"`
			} `json:"models"`
		}
		if err := json.NewDecoder(lr.Body).Decode(&ld); err == nil {
			for _, m := range ld.Models {
				if m.Handle != "" {
					loadedSet[m.Handle] = true
				}
				if m.Name != "" {
					loadedSet[m.Name] = true
				}
				if m.Path != "" {
					loadedSet[mm.basename(m.Path)] = true
				}
			}
		}
	}

	models := make([]ModelInfo, 0, len(filesData.Files))
	for _, f := range filesData.Files {
		name := mm.basename(f.Path)
		models = append(models, ModelInfo{
			Name:   name,
			Model:  name,
			Size:   f.SizeBytes,
			Loaded: loadedSet[name] || loadedSet[f.Path],
		})
	}
	return models, nil
}

// basename — выделяет имя файла из пути (поддержка '/' и '\').
func (mm *ModelManager) basename(p string) string {
	if p == "" {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if i := strings.LastIndex(p, `\`); i >= 0 {
		p = p[i+1:]
	}
	return p
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

// Параметры retry при 503 "model is loading" от cppworker — чтобы не отдавать
// клиенту ошибку, если параллельный запрос уже инициировал загрузку.
const (
	llamaCppLoadMaxRetries     = 10               // ~30 секунд при retryInterval=3s
	llamaCppLoadRetryInterval  = 3 * time.Second
	llamaCppLoadDefaultTimeout = 120 * time.Second // дефолтный таймаут load-запроса (если конфиг не задан)
)

// getLoadTimeout — возвращает таймаут для POST /api/models/load из конфигурации.
// Приоритет: Balancing.ModelLoadTimeout → llamaCppLoadDefaultTimeout (120s).
// Раньше был хардкод 5s, что недостаточно для cold-start больших GGUF моделей.
func (mm *ModelManager) getLoadTimeout() time.Duration {
	if mm.proxy != nil && mm.proxy.config != nil {
		if mm.proxy.config.Balancing.ModelLoadTimeout > 0 {
			return time.Duration(mm.proxy.config.Balancing.ModelLoadTimeout) * time.Second
		}
	}
	return llamaCppLoadDefaultTimeout
}

// executeLlamaCppLoad — загрузка модели в память на cppworker-бэкенде.
// cppworker принимает POST /api/models/load с JSON {"name": "...", ...}.
// Если в req заданы ContextSize или GPULayers — передаёт их в теле.
//
// При получении HTTP 503 с body содержащим "model is loading" — повторяет запрос
// через llamaCppLoadRetryInterval, до llamaCppLoadMaxRetries попыток. Это
// закрывает race condition, когда параллельные запросы приходят на cppworker
// в момент, когда другая горутина уже грузит эту же модель (другая запрос
// получил `TryLockLoad=false`, а handleLoadModel ещё не завершил WaitForLoad).
func (mm *ModelManager) executeLlamaCppLoad(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/models/load", host, port)
	body := map[string]interface{}{
		"name": req.ModelName,
	}
	if req.ContextSize != nil {
		body["contextSize"] = *req.ContextSize
	}
	if req.GPULayers != nil {
		body["gpuLayers"] = *req.GPULayers
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("failed to marshal request body: %v", err),
		}
	}

	// Используем отдельный клиент с таймаутом из конфигурации (Balancing.ModelLoadTimeout,
	// default 120s). Раньше был хардкод 5s — этого недостаточно для cold-start
	// больших GGUF моделей (чтение файла + загрузка весов в VRAM).
	// Общий mm.client имеет Timeout 10 минут (для pull/push), что слишком много
	// для load-запроса к потенциально неотвечающему cppworker.
	loadTimeout := mm.getLoadTimeout()
	loadClient := &http.Client{
		Timeout: loadTimeout,
	}

	for attempt := 0; attempt < llamaCppLoadMaxRetries; attempt++ {
		httpReq, reqErr := http.NewRequest("POST", url, bytes.NewReader(jsonBody))
		if reqErr != nil {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("failed to create request: %v", reqErr),
			}
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if rid := RequestIDFromContext(httpReq.Context()); rid != "" {
			httpReq.Header.Set(requestIDHeader, rid)
		}

		start := time.Now()
		resp, err := loadClient.Do(httpReq)
		durationMs := time.Since(start).Milliseconds()
		if err != nil {
			// Сетевая ошибка или таймаут — НЕ делаем retry.
			// Если cppworker недоступен (refused, timeout, DNS), повтор не поможет —
			// нужно сразу вернуть ошибку, чтобы клиент получил осмысленный ответ.
			logger.Get().Debugw("executeLlamaCppLoad: HTTP error (no retry)",
				"backend", backendID, "model", req.ModelName,
				"attempt", attempt+1, "duration_ms", durationMs, "error", err)
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("request failed: %v", err),
			}
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error:     fmt.Sprintf("failed to read response: %v", readErr),
			}
		}

		// Успех.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return &ModelOpResult{
				Success:   true,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Message:   fmt.Sprintf("Model '%s' loaded into memory on backend '%s'", req.ModelName, backendID),
			}
		}

		// Извлекаем сообщение об ошибке из JSON-ответа cppworker.
		errMsg := string(respBody)
		var cwErr struct {
			Error   string `json:"error"`
			Loading bool   `json:"loading"`
		}
		if json.Unmarshal(respBody, &cwErr) == nil && cwErr.Error != "" {
			errMsg = cwErr.Error
		}

		// Специальный случай: 503 с признаком параллельной загрузки — retry.
		if resp.StatusCode == http.StatusServiceUnavailable &&
			(cwErr.Loading || strings.Contains(strings.ToLower(errMsg), "model is loading")) {
			logger.Get().Infow("executeLlamaCppLoad: model is loading on cppworker, retrying",
				"backend", backendID, "model", req.ModelName,
				"attempt", attempt+1, "max_attempts", llamaCppLoadMaxRetries,
				"retry_in_sec", llamaCppLoadRetryInterval.Seconds(),
				"status_code", resp.StatusCode)
			if attempt < llamaCppLoadMaxRetries-1 {
				time.Sleep(llamaCppLoadRetryInterval)
				continue
			}
			// Последняя попытка исчерпана — возвращаем ошибку.
			return &ModelOpResult{
				Success:   false,
				Operation: req.Operation,
				ModelName: req.ModelName,
				BackendID: backendID,
				Error: fmt.Sprintf("cppworker still loading model '%s' after %d attempts (%v)",
					req.ModelName, llamaCppLoadMaxRetries, llamaCppLoadMaxRetries*int(llamaCppLoadRetryInterval.Seconds())),
			}
		}

		// Другая ошибка — не повторяем.
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, errMsg),
		}
	}

	// Сюда не должны попасть (цикл всегда выходит через return), но на всякий случай:
	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("cppworker load retries exhausted for model '%s'", req.ModelName),
	}
}

// executeLlamaCppUnload — выгрузка модели из памяти на cppworker-бэкенде.
// cppworker принимает POST /api/models/unload с ?name=... в query string.
func (mm *ModelManager) executeLlamaCppUnload(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	safeName := urlPathEscape(req.ModelName)
	url := fmt.Sprintf("http://%s:%d/api/models/unload?name=%s", host, port, safeName)
	resp, err := mm.sendRawRequest("POST", url, nil)
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
			Message:   fmt.Sprintf("Model '%s' unloaded from memory on backend '%s'", req.ModelName, backendID),
		}
	}
	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, string(respBody)),
	}
}

// executeLlamaCppDelete — удаление модели с диска на cppworker-бэкенде.
// cppworker принимает POST /api/models/delete (или DELETE) с JSON {"name": "..."} или ?name=...
func (mm *ModelManager) executeLlamaCppDelete(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	url := fmt.Sprintf("http://%s:%d/api/models/delete", host, port)
	body := map[string]interface{}{
		"name": req.ModelName,
	}
	return mm.sendCppWorkerRequest("POST", url, backendID, req, body)
}

// sendCppWorkerRequest — отправка запроса к cppworker (load/unload и пр.).
func (mm *ModelManager) sendCppWorkerRequest(method, url, backendID string, req ModelOpRequest, body map[string]interface{}) *ModelOpResult {
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

	errMsg := string(respBody)
	var cwErr struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(respBody, &cwErr) == nil && cwErr.Error != "" {
		errMsg = cwErr.Error
	}
	return &ModelOpResult{
		Success:   false,
		Operation: req.Operation,
		ModelName: req.ModelName,
		BackendID: backendID,
		Error:     fmt.Sprintf("cppworker error (HTTP %d): %s", resp.StatusCode, errMsg),
	}
}

// urlPathEscape — экранирование пути для подстановки в URL-сегмент (а не в query).
func urlPathEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '-' || r == '_' || r == '.' || r == '~' {
			b.WriteRune(r)
		} else {
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
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

// sendRawRequest — отправка HTTP запроса к Ollama.
// Дополнительно: пробрасывает X-Request-ID из контекста (если он был
// установлен в ServeHTTP) — чтобы cppworker мог логировать тот же
// request_id и можно было проследить всю цепочку по одному grep'у.
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

	// Пробрасываем request_id из ctx, если он есть.
	if rid := RequestIDFromContext(req.Context()); rid != "" {
		req.Header.Set(requestIDHeader, rid)
	}

	// Подробное логирование: до запроса и после, с duration и status.
	start := time.Now()
	resp, err := mm.client.Do(req)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		logger.Get().Debugw("sendRawRequest: HTTP error",
			"method", method, "url", url,
			"duration_ms", durationMs, "error", err)
		return nil, err
	}
	logger.Get().Debugw("sendRawRequest: HTTP response",
		"method", method, "url", url,
		"status_code", resp.StatusCode,
		"duration_ms", durationMs)
	return resp, nil
}

// ===== Вспомогательные методы =====

// tryAcquireOp — пытается зарегистрировать операцию. Возвращает false, если для той же
// пары (op, modelName, backendID) уже есть активная запись младше modelOpTTL.
// Записи старше modelOpTTL считаются «залипшими» и автоматически перезаписываются —
// это защищает от блокировки при network-fail, когда releaseOp() не был вызван.
func (mm *ModelManager) tryAcquireOp(operation, modelName, backendID string) bool {
	mm.mu.Lock()
	defer mm.mu.Unlock()

	now := time.Now()
	key := operation + ":" + modelName
	if backends, exists := mm.activeOps[key]; exists {
		if startedAt, running := backends[backendID]; running {
			if now.Sub(startedAt) < modelOpTTL {
				return false
			}
			// Залипшая запись — перезаписываем.
		}
		backends[backendID] = now
	} else {
		mm.activeOps[key] = map[string]time.Time{backendID: now}
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
