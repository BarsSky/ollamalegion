package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
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
	// Round 7: per-tensor override (parallel arrays) для MoE.
	// Если заданы и согласованы по длине — load через /api/models/load-with-params.
	OverrideTensors     []string `json:"overrideTensors,omitempty"`
	OverrideTensorBufts []string `json:"overrideTensorBufts,omitempty"`
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

	// Round 18 (2026-07-10): llama.cpp runtime fields — populated from cppworker
	// /api/models. cppworker tracks these per-model: context length (n_ctx),
	// GPU layers (n_gpu_layers), quantization, file path, batch size, etc.
	// Используются WebUI Models tab для корректных метрик в карточках
	// (раньше показывались нули/прочерки потому что aggregator endpoint
	// не пробрасывал эти поля из cppworker).
	ContextLength  int    `json:"contextLength,omitempty"` // n_ctx
	NumGPULayers   int    `json:"numGpuLayers,omitempty"`  // -1 = all, 0 = cpu only
	BatchSize      int    `json:"batchSize,omitempty"`
	Quantization   string `json:"quantization,omitempty"`
	GGUFPath       string `json:"ggufPath,omitempty"`
	State          string `json:"state,omitempty"` // loaded | loading | unloaded | error
	Architecture   string `json:"architecture,omitempty"`
	VRAMUsage      uint64 `json:"vramUsage,omitempty"` // MB (cppworker returns 0 — not tracked)
	RAMUsage       uint64 `json:"ramUsage,omitempty"`  // MB (cppworker returns 0 — not tracked)
	LoadedAt       string `json:"loadedAt,omitempty"`
	// Architecture metadata (для VRAM/RAM split estimation на frontend).
	// cppworker /api/models reports эти поля — пробрасываем в API.
	NLayers    int `json:"nLayers,omitempty"`
	NKvHeads   int `json:"nKvHeads,omitempty"`
	NEmbd      int `json:"nEmbd,omitempty"`
	HeadDimK   int `json:"headDimK,omitempty"`
	HeadDimV   int `json:"headDimV,omitempty"`
	MaxContext int `json:"maxContext,omitempty"` // ggufContextLength — макс n_ctx для модели
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

	// Берём request_id из глобального fallback-context (ensureModelLoadedOnBackend
	// вызывает ExecuteOperation синхронно из HTTP-хендлера, и middleware ServeHTTP
	// уже положил request_id в lr_recentCtx). Если ctx пустой (вызов вне HTTP,
	// например из background-reload) — ridLog() вернёт обычный logger без полей.
	ridLog(lr_recentCtx()).Infow("executing model operation",
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
// + /api/models (что в памяти через state="loaded"). Round 17 (2026-07-10).
// Round 18 (2026-07-10): пробрасывает runtime поля (contextLength, numGpuLayers,
// quantization, ggufPath, batchSize, state, architecture) из /api/models response.
// Также использует loadingSizeBytes как fallback для size (cppworker reports sizeBytes=0
// для загруженных моделей).
func (mm *ModelManager) listLlamaCppModels(backend *types.Backend) ([]ModelInfo, error) {
	port := mm.resolveBackendPort(backend)
	base := fmt.Sprintf("http://%s:%d", backend.Host, port)

	filesURL := base + "/api/models/files"
	filesResp, err := mm.client.Get(filesURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch models from cppworker %s: %w", backend.ID, err)
	}
	defer filesResp.Body.Close()

	// Round 17: cppworker returns 'name' (filename with .gguf), NOT 'path' + 'quantization'.
	// The previous schema caused name="" in response (Models tab bug — loaded models invisible).
	var filesData struct {
		Files []struct {
			Name       string `json:"name"`
			SizeBytes  int64  `json:"sizeBytes"`
			ModifiedAt string `json:"modifiedAt"`
		} `json:"files"`
	}
	if err := json.NewDecoder(filesResp.Body).Decode(&filesData); err != nil {
		return nil, fmt.Errorf("failed to decode models from cppworker %s: %w", backend.ID, err)
	}

	// Round 17+18: читаем /api/models (НЕ /api/models/loaded — он не реализован в cppworker).
	// Используем для определения loaded state И для пробрасывания runtime полей.
	type runtimeModel struct {
		Name              string `json:"name"`
		State             string `json:"state"`
		Path              string `json:"path,omitempty"`
		SizeBytes         int64  `json:"sizeBytes,omitempty"`
		LoadingSizeBytes  int64  `json:"loadingSizeBytes,omitempty"` // cppworker: real file size (sizeBytes=0 для loaded)
		ContextSize       int    `json:"contextSize,omitempty"`
		GPULayers         int    `json:"gpuLayers,omitempty"`
		BatchSize         int    `json:"batchSize,omitempty"`
		Quantization      string `json:"quantization,omitempty"`
		Architecture      string `json:"architecture,omitempty"`
		VRAMUsage         uint64 `json:"vramUsage,omitempty"`
		RAMUsage          uint64 `json:"ramUsage,omitempty"`
		LoadedAt          string `json:"loadedAt,omitempty"`
		// Round 18+: architecture metadata для оценки VRAM/RAM на frontend.
		NLayers           int    `json:"nLayers,omitempty"`
		NHeads            int    `json:"nHeads,omitempty"`
		NKvHeads          int    `json:"nKvHeads,omitempty"`
		HeadDimK          int    `json:"headDimK,omitempty"`
		HeadDimV          int    `json:"headDimV,omitempty"`
		NEmbd             int    `json:"nEmbd,omitempty"`
		GGUFContextLength int    `json:"ggufContextLength,omitempty"`
	}
	loadedSet := make(map[string]bool)
	// runtimeMap: ключ — basename (с .gguf и без), значение — runtime данные.
	// Используется для обогащения ModelInfo из /api/models/files.
	runtimeMap := make(map[string]runtimeModel)
	allURL := base + "/api/models"
	if lr, lerr := mm.client.Get(allURL); lerr == nil {
		defer lr.Body.Close()
		var ld struct {
			Models []runtimeModel `json:"models"`
		}
		if err := json.NewDecoder(lr.Body).Decode(&ld); err == nil {
			for _, m := range ld.Models {
				// Сохраняем runtime данные по трём ключам (name, basename, basename без .gguf).
				runtimeMap[m.Name] = m
				runtimeMap[mm.basename(m.Name)] = m
				runtimeMap[strings.TrimSuffix(m.Name, ".gguf")] = m
				if m.State == "loaded" && m.Name != "" {
					loadedSet[m.Name] = true
					loadedSet[mm.basename(m.Name)] = true
					loadedSet[strings.TrimSuffix(m.Name, ".gguf")] = true
				}
			}
		}
	}

	models := make([]ModelInfo, 0, len(filesData.Files))
	for _, f := range filesData.Files {
		// f.Name это полное имя файла (Qwen3...Q4_K_M.gguf).
		// Для UI используем basename БЕЗ .gguf чтобы совпадало с тем что
		// отдаёт /api/tags у Ollama.
		name := strings.TrimSuffix(f.Name, ".gguf")
		// Берём runtime данные (по трём возможным ключам).
		rt := runtimeMap[name]
		if rt.Name == "" {
			rt = runtimeMap[f.Name]
		}
		if rt.Name == "" {
			rt = runtimeMap[mm.basename(f.Name)]
		}

		// cppworker reports sizeBytes=0 для загруженных моделей — fallback на loadingSizeBytes.
		size := f.SizeBytes
		if size == 0 {
			size = rt.LoadingSizeBytes
		}
		if size == 0 {
			size = rt.SizeBytes
		}

		mi := ModelInfo{
			Name:       name,
			Model:      name,
			Size:       size,
			Digest:     "",
			ModifiedAt: f.ModifiedAt,
			Loaded:     loadedSet[name] || loadedSet[f.Name],
		}
		// Round 18: проброс runtime полей в ModelInfo (omitempty — пустые поля не уходят в JSON).
		if rt.Name != "" {
			mi.ContextLength = rt.ContextSize
			mi.NumGPULayers = rt.GPULayers
			mi.BatchSize = rt.BatchSize
			mi.Quantization = rt.Quantization
			mi.GGUFPath = rt.Path
			mi.State = rt.State
			mi.Architecture = rt.Architecture
			mi.VRAMUsage = rt.VRAMUsage
			mi.RAMUsage = rt.RAMUsage
			mi.LoadedAt = rt.LoadedAt
			// Round 18+: architecture metadata для оценки VRAM/RAM split на frontend.
			mi.NLayers = rt.NLayers
			mi.NKvHeads = rt.NKvHeads
			mi.NEmbd = rt.NEmbd
			mi.HeadDimK = rt.HeadDimK
			mi.HeadDimV = rt.HeadDimV
			mi.MaxContext = rt.GGUFContextLength
		}
		// Round 18b (2026-07-10): cppworker reports quantization="" (баг в cppworker).
		// GGUF quantization всегда в имени файла (Q4_K_M, Q5_K_S, Q8_0, F16, IQ4_XS, ...).
		// Парсим из имени файла если cppworker не вернул.
		if mi.Quantization == "" {
			mi.Quantization = extractQuantizationFromName(f.Name)
		}
		models = append(models, mi)
	}
	return models, nil
}

// extractQuantizationFromName — парсит GGUF quantization из имени файла.
//
// cppworker не сообщает quantization в /api/models (баг cppworker), но
// стандарт GGUF-квантизации всегда в имени файла после последнего "-"
// и до ".gguf": Qwen3-...-Q4_K_M.gguf → Q4_K_M.
//
// Поддерживает все стандартные квантизации llama.cpp:
//   - Legacy k-quants:    Q2_K, Q3_K_S/M/L, Q4_0, Q4_1, Q4_K_S/M, Q5_0, Q5_1, Q5_K_S/M, Q6_K, Q8_0
//   - I-quants (I-quant): IQ1_S/M, IQ2_XXS/XS/S/M, IQ3_XXS/XS/S/M, IQ4_XS/NL
//   - Trellis quants:     TQ1_0, TQ2_0
//   - F-types (precise):  F16, F32, BF16
//   - fpW types:          FP16, BF16
//
// Возвращает "" если не нашёл.
func extractQuantizationFromName(filename string) string {
	if filename == "" {
		return ""
	}
	// Убираем .gguf (case-insensitive).
	base := filename
	if i := strings.LastIndex(strings.ToLower(base), ".gguf"); i >= 0 {
		base = base[:i]
	}
	// Ищем последний "-<QUANT>" в имени.
	// Паттерн: -(IQ?|TQ?|Q|F|BF|FP)?<digits>(_[A-Z]+)?(_[A-Z]+)?
	// Например: -Q4_K_M, -IQ4_XS, -F16, -BF16, -Q5_K
	re := regexp.MustCompile(`-(IQ[1-4]_[A-Z]+|TQ[12]_[0-9]|Q[0-8]_[0-9K]|Q[0-8]_[A-Z]|F1[26]|F32|BF16|FP16)$`)
	m := re.FindStringSubmatch(base)
	if len(m) >= 2 {
		return m[1]
	}
	// Fallback: ищем "Q\d_K_[A-Z]" или "Q\d_\d" в любом месте.
	re2 := regexp.MustCompile(`(Q[0-8]_[K0-9](_[A-Z])?|IQ[1-4]_[A-Z]+|TQ[12]_[0-9])`)
	if m2 := re2.FindStringSubmatch(base); len(m2) >= 2 {
		return m2[1]
	}
	return ""
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
	// Round 8 (2026-07-10): bumped default to 10 minutes. 21GB Qwen3-A3B takes
	// ~7 min на A10, плюс пользовательские модели могут быть больше.
	// Если клиент отвалится по timeout, graceful poll завершения
	// (pollLoadCompletionUntilLoaded) подхватит успех.
	llamaCppLoadDefaultTimeout = 600 * time.Second // 10 минут — для больших MoE/Qwen моделей
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

// resolveOverrideTensors — резолвит per-tensor override для load.
// Приоритет: явные поля req.OverrideTensors > сохранённый LlamaCppModelProfile.
// Возвращает согласованные по длине parallel arrays или пустые slices.
//
// Round 7 (2026-07-09): сохраняем override-tensors между перезагрузками через
// /api/profiles endpoint. Если у модели есть сохранённый профиль с override-tensors,
// каждая автоматическая загрузка применяет их (Qwen3-A3B → CPU offload экспертов).
func (mm *ModelManager) resolveOverrideTensors(req ModelOpRequest) ([]string, []string) {
	// 1) явное override в запросе — наивысший приоритет.
	if len(req.OverrideTensors) > 0 && len(req.OverrideTensors) == len(req.OverrideTensorBufts) {
		return req.OverrideTensors, req.OverrideTensorBufts
	}
	// 2) сохранённый профиль.
	if mm.proxy != nil {
		if prof, ok := mm.proxy.GetModelProfile(req.ModelName); ok {
			if len(prof.OverrideTensors) > 0 && len(prof.OverrideTensors) == len(prof.OverrideTensorBufts) {
				return prof.OverrideTensors, prof.OverrideTensorBufts
			}
		}
	}
	return nil, nil
}

// isTimeoutError — проверяет, является ли ошибка HTTP-клиента таймаутом
// (Client.Timeout exceeded while awaiting headers). Используется в Round 8
// graceful load timeout path.
func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "Client.Timeout exceeded") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "i/o timeout")
}

// pollLoadCompletionUntilLoaded — после HTTP timeout на /api/models/load
// проверяет cppworker (state="loaded" в /api/models) периодически до завершения
// загрузки или до истечения maxWait. Возвращает success если модель загружена,
// nil если polling превысил maxWait.
//
// Round 8 (2026-07-10): решает проблему "model loaded but client got timeout":
//   1. cppworker начинает загружать 21GB модель (5-10 мин).
//   2. balancer HTTP client timeout (default 120s) срабатывает.
//   3. НО cppworker всё ещё грузит — polling показывает state="loading".
//   4. По завершении cppworker state="loaded" — мы возвращаем success клиенту.
//
// Без этой логики клиент получал ошибку даже при успешной загрузке.
func (mm *ModelManager) pollLoadCompletionUntilLoaded(
	host string, port int, backendID string, modelName string, maxWait time.Duration,
) *ModelOpResult {
	if maxWait <= 0 {
		maxWait = 5 * time.Minute
	}
	deadline := time.Now().Add(maxWait)
	pollInterval := 2 * time.Second
	pollClient := &http.Client{
		Timeout: 10 * time.Second, // poll-запросы быстрые
	}

	logger.Get().Infow("executeLlamaCppLoad: polling for model load completion",
		"backend", backendID, "model", modelName,
		"max_wait", maxWait, "poll_interval", pollInterval)

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		// Проверяем /api/models — там state="loaded"/"loading"
		modelsURL := fmt.Sprintf("http://%s:%d/api/models", host, port)
		resp, err := pollClient.Get(modelsURL)
		if err != nil {
			logger.Get().Debugw("pollLoadCompletion: /api/models poll failed",
				"backend", backendID, "error", err)
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			continue
		}

		// Парсим models[].state — ищем нашу модель
		var modelsResp struct {
			Models []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"models"`
		}
		if json.Unmarshal(body, &modelsResp) != nil {
			continue
		}
		for _, m := range modelsResp.Models {
			if m.Name == modelName && m.State == "loaded" {
				logger.Get().Infow("executeLlamaCppLoad: model loaded successfully (recovered from HTTP timeout)",
					"backend", backendID, "model", modelName,
					"elapsed", maxWait-time.Until(deadline))
				return &ModelOpResult{
					Success:   true,
					Operation: "load",
					ModelName: modelName,
					BackendID: backendID,
					Message:   "model loaded (load request timed out but polling confirmed completion)",
				}
			}
		}
	}

	logger.Get().Warnw("executeLlamaCppLoad: poll for load completion exceeded max_wait",
		"backend", backendID, "model", modelName, "max_wait", maxWait)
	return nil
}

// executeLlamaCppLoad — загрузка модели в память на cppworker-бэкенде.
// cppworker принимает POST /api/models/load с JSON {"name": "...", ...}.
// Если в req заданы ContextSize или GPULayers — передаёт их в теле.
//
// Round 7: если заданы OverrideTensors/OverrideTensorBufts (явно в req или
// через сохранённый LlamaCppModelProfile) — роутим на /api/models/load-with-params
// чтобы cppworker применил per-tensor routing для MoE моделей.
//
// При получении HTTP 503 с body содержащим "model is loading" — повторяет запрос
// через llamaCppLoadRetryInterval, до llamaCppLoadMaxRetries попыток. Это
// закрывает race condition, когда параллельные запросы приходят на cppworker
// в момент, когда другая горутина уже грузит эту же модель (другая запрос
// получил `TryLockLoad=false`, а handleLoadModel ещё не завершил WaitForLoad).
func (mm *ModelManager) executeLlamaCppLoad(host string, port int, backendID string, req ModelOpRequest) *ModelOpResult {
	// Round 27 follow-up (v0.5.14 follow-up #2): если у модели стоит профиль
	// с disabled=true (например, gemma-4 с upstream GGML_ASSERT на любом n_ctx >= 8192)
	// — отказываем в авто-загрузке с понятной ошибкой. Без этого клиент (Cline/OpenWebUI)
	// уходит в crash-loop: cppworker SIGABRT → Docker restart → балансер снова
	// пытается загрузить → опять SIGABRT.
	if prof, ok := mm.proxy.GetModelProfile(req.ModelName); ok && prof.Disabled {
		msg := fmt.Sprintf("model %q is marked as disabled in profile (broken: see profile.notes). "+
			"Auto-load refused. Use a different model or remove the disabled flag from the profile.",
			req.ModelName)
		logger.Get().Warnw("executeLlamaCppLoad: model disabled in profile, refusing auto-load",
			"model", req.ModelName, "backend", backendID, "profileNotes", prof.Notes)
		return &ModelOpResult{
			Success:   false,
			Operation: req.Operation,
			ModelName: req.ModelName,
			BackendID: backendID,
			Error:     msg,
		}
	}

	// Round 7: resolve override-tensors (req override > profile > none).
	overrideTensors, overrideTensorBufts := mm.resolveOverrideTensors(req)
	useLoadWithParams := len(overrideTensors) > 0 && len(overrideTensors) == len(overrideTensorBufts)

	url := fmt.Sprintf("http://%s:%d/api/models/load", host, port)
	if useLoadWithParams {
		url = fmt.Sprintf("http://%s:%d/api/models/load-with-params", host, port)
	}
	body := map[string]interface{}{
		"name": req.ModelName,
	}
	if req.ContextSize != nil {
		body["contextSize"] = *req.ContextSize
	}
	if req.GPULayers != nil {
		body["gpuLayers"] = *req.GPULayers
	}
	if useLoadWithParams {
		body["overrideTensors"] = overrideTensors
		body["overrideTensorBufts"] = overrideTensorBufts
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
			// Сетевая ошибка или таймаут. Раньше сразу возвращали ошибку —
			// но для 21GB+ моделей load занимает 5-10 минут, что может превысить
			// даже увеличенный timeout. Поэтому проверяем, может модель уже загружена
			// на cppworker (load продолжается в фоне) — poll /api/models/load/progress
			// и /api/models пока не получим state="loaded" или timeout на poll.
			//
			// Round 8 (2026-07-10): graceful load timeout — если наш HTTP client
			// отвалился по таймауту, но cppworker всё ещё грузит, ждём завершения
			// через polling. Это решает проблему "model loaded but client got timeout".
			if isTimeoutError(err) {
				logger.Get().Warnw("executeLlamaCppLoad: HTTP timeout, polling for load completion",
					"backend", backendID, "model", req.ModelName,
					"timeout", loadTimeout, "duration_ms", durationMs)
				if pollResult := mm.pollLoadCompletionUntilLoaded(
					host, port, backendID, req.ModelName,
					loadTimeout); pollResult != nil {
					return pollResult
				}
				// poll тоже не дождался — возвращаем ошибку таймаута клиенту
			}
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
			// Round 24 (2026-08-04): detect 202 Accepted from cppworker.
			// cppworker теперь по умолчанию возвращает 202 + Location сразу,
			// реальная загрузка идёт в background goroutine. Без polling balancer
			// считал бы модель загруженной, но инференс-вызов получил бы 503
			// "model is loading". Поэтому при 202 — polling /api/models пока
			// state не станет "loaded" или maxWait не истечёт.
			//
			//   - status=loading      → ещё грузится
			//   - status=already_loaded → готово
			//   - status=loaded       → готово (sync ?wait=true path)
			if resp.StatusCode == http.StatusAccepted {
				var loadResp struct {
					Status              string `json:"status"`
					EstimatedLoadTimeMs int64  `json:"estimatedLoadTimeMs"`
				}
				_ = json.Unmarshal(respBody, &loadResp)
				if loadResp.Status == "loading" || loadResp.Status == "loading_after_timeout" {
					// Round 32 #15 (2026-08-11): user report "model not ready after
					// 1m26.4475s" — balancer сдался слишком рано, модель
					// догрузилась через несколько секунд после timeout.
					// Root cause: maxWait = est*1.5 + 10s — слишком оптимистично
					// для gemma-4 (5GB) на RTX 3070. Реальная загрузка занимает
					// 1m30s+ (VRAM alloc + llama.cpp init + warmup), а cppworker
					// estimated = ~50s → maxWait = 85s = 1m25s. Балансер polling
					// истёк за 3-5 секунд ДО завершения load.
					//
					// Fix: 2x estimated + 60s buffer, min 3min, max 15min.
					// Это даёт 1m40s + 60s = 2m40s для 50s estimated
					// (раньше было 85s). Достаточно для RTX 3070.
					//
					// На медленных GPU (ноутбук, A10) gemma-4 может грузиться
					// 3-5 минут. maxWait = 15min покрывает.
					//
					// Round 35c (2026-08-13): user report "model not ready after
					// 8m7.182s" для Qwen3.6-35B-A3B (22GB) на 3070 — `2*est+60s`
					// дало 8m6s, но фактическая загрузка заняла 12+ мин из-за
					// CUDA_Host pinned memory allocation для 19GB+ весов.
					// Multiplier/cap/buffer теперь env-конфигурируемые через
					// LB_NCTX_PREFLIGHT_MAX_WAIT_SEC / _WAIT_MULTIPLIER /
					// _WAIT_BUFFER_SEC (см. NCtxReloadConfig + applyNCtxReloadEnvOverrides).
					capWait, multiplier, bufDur := resolvePreflightWaitTuning(mm.proxy)
					maxWait := capWait
					if loadResp.EstimatedLoadTimeMs > 0 {
						est := time.Duration(loadResp.EstimatedLoadTimeMs) * time.Millisecond
						maxWait = time.Duration(multiplier)*est + bufDur
						// Round 35c: убрал hardcoded 3-min floor, заменил на dynamic
						// min(1s, capWait/10). Для capWait=900s → min=90s (1.5 min).
						// Для capWait=3s (тесты) → min=300ms. Это позволяет unit-тестам
						// использовать маленький cap через ENV override.
						if maxWait < capWait/10 {
							maxWait = capWait / 10
						}
						if maxWait < time.Second {
							maxWait = time.Second
						}
						if maxWait > capWait {
							maxWait = capWait
						}
					}
					logger.Get().Infow("executeLlamaCppLoad: 202 Accepted (async load), polling for completion",
						"backend", backendID, "model", req.ModelName,
						"estimated_ms", loadResp.EstimatedLoadTimeMs,
						"max_wait", maxWait, "cap_wait", capWait,
						"multiplier", multiplier, "buffer", bufDur)
					if pollResult := mm.pollLoadCompletionUntilLoaded(
						host, port, backendID, req.ModelName, maxWait); pollResult != nil {
						return pollResult
					}
					// Polling exhausted — return error so caller can retry.
					return &ModelOpResult{
						Success:   false,
						Operation: req.Operation,
						ModelName: req.ModelName,
						BackendID: backendID,
						Error: fmt.Sprintf("async load (202) on cppworker: model not ready after %v",
							maxWait),
					}
				}
				// status=already_loaded / loaded / loaded_by_other / unknown → fall through to success.
			}
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
