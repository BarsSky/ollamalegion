package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// backendInfo — информация о бэкенде для внутреннего использования.
type backendInfo struct {
	id   string
	host string
	port int
}

// ---------- Backend helpers (llama.cpp specific) ----------

// getLlamaCppBackends возвращает все healthy/degraded llama.cpp бэкенды
func (lr *LlamaCppRouter) getLlamaCppBackends() []backendInfo {
	allBackends := lr.proxy.GetAllBackends()
	result := make([]backendInfo, 0, len(allBackends))
	for _, b := range allBackends {
		if b.Status != types.StatusHealthy && string(b.Status) != "degraded" {
			continue
		}
		bt := normalizeBackendType(b.Type)
		if bt != types.BackendTypeLlamaCpp {
			continue
		}
		port := lr.proxy.getBackendPort(&b)
		result = append(result, backendInfo{
			id:   b.ID,
			host: b.Host,
			port: port,
		})
	}
	logger.Get().Debugw("getLlamaCppBackends",
		"total_backends", len(allBackends),
		"llamacpp_count", len(result),
	)
	return result
}

// findModelOnLlamaCppBackend ищет модель только среди llama.cpp бэкендов
func (lr *LlamaCppRouter) findModelOnLlamaCppBackend(model string) string {
	lr.proxy.metricsMgr.mu.RLock()
	defer lr.proxy.metricsMgr.mu.RUnlock()

	for id, metrics := range lr.proxy.metricsMgr.metrics {
		lr.proxy.mu.RLock()
		state, ok := lr.proxy.backends[id]
		lr.proxy.mu.RUnlock()
		if !ok || (state.Backend.Status != types.StatusHealthy && string(state.Backend.Status) != "degraded") {
			continue
		}
		if normalizeBackendType(state.Backend.Type) != types.BackendTypeLlamaCpp {
			continue
		}
		for _, m := range metrics.LlamaCpp.LoadedModels {
			if m.Name == model {
				return id
			}
		}
	}
	return ""
}

// selectAnyLlamaCppHealthy выбирает любой healthy llama.cpp бэкенд
func (lr *LlamaCppRouter) selectAnyLlamaCppHealthy() string {
	backends := lr.proxy.GetAllBackends()
	for _, b := range backends {
		if b.Status == types.StatusHealthy && normalizeBackendType(b.Type) == types.BackendTypeLlamaCpp {
			return b.ID
		}
	}
	return ""
}

// selectLlamaCppBackend выбирает llama.cpp бэкенд по ресурсам
func (lr *LlamaCppRouter) selectLlamaCppBackendByResources(r *http.Request) string {
	model := lr.proxy.parseRequestBody(r).Model
	return lr.proxy.selectBackend(model, types.BackendTypeLlamaCpp)
}

// queryCppWorkerModels возвращает список моделей на cppworker-бэкенде.
// Использует endpoint /api/models. Каждая модель имеет поле state
// ("unloaded" | "loading" | "loaded" | "error").
//
// Возвращает (nil, nil) если backend не найден или запрос упал.
func (lr *LlamaCppRouter) queryCppWorkerModels(backendID string) []cppWorkerModelState {
	rid := RequestIDFromContext(lr_recentCtx())
	backend := lr.proxy.GetBackend(backendID)
	if backend == nil {
		ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: backend not found",
			"backend", backendID)
		return nil
	}
	port := lr.proxy.getBackendPort(backend)
	if port <= 0 {
		ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: invalid port",
			"backend", backendID, "port", port)
		return nil
	}
	url := fmt.Sprintf("http://%s:%d/api/models", backend.Host, port)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        5,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
		},
	}
	req, _ := http.NewRequest("GET", url, nil)
	if rid != "" {
		req.Header.Set(requestIDHeader, rid)
	}
	start := time.Now()
	resp, err := client.Do(req)
	durationMs := time.Since(start).Milliseconds()
	if err != nil {
		ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: failed to query cppworker",
			"backend", backendID, "url", url, "duration_ms", durationMs, "error", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: bad status",
			"backend", backendID, "url", url, "status_code", resp.StatusCode, "duration_ms", durationMs)
		return nil
	}
	var data struct {
		Count  int `json:"count"`
		Models []struct {
			Name  string `json:"name"`
			Path  string `json:"path,omitempty"`
			State string `json:"state,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: decode error",
			"backend", backendID, "error", err)
		return nil
	}
	out := make([]cppWorkerModelState, 0, len(data.Models))
	for _, m := range data.Models {
		out = append(out, cppWorkerModelState{Name: m.Name, Path: m.Path, State: m.State})
	}
	ridLog(lr_recentCtx()).Debugw("queryCppWorkerModels: response",
		"backend", backendID, "url", url, "count", len(out),
		"status_code", resp.StatusCode, "duration_ms", durationMs)
	return out
}

// matchCppWorkerModel проверяет совпадение имени модели на cppworker.
// Поддерживает три варианта: точное имя, basename пути без .gguf, basename пути с .gguf.
func matchCppWorkerModel(modelName string, m cppWorkerModelState) bool {
	if m.Name == modelName {
		return true
	}
	if m.Path == "" {
		return false
	}
	modelNameLower := strings.ToLower(modelName)
	baseLower := strings.ToLower(basenameOfPath(m.Path))
	if modelNameLower == baseLower {
		return true
	}
	if strings.ToLower(modelNameLower+".gguf") == baseLower {
		return true
	}
	return false
}

// isModelLoadedOnBackend проверяет, загружена ли модель в VRAM на cppworker-бэкенде.
// Возвращает true если модель уже готова к inference (state == "loaded") ИЛИ
// уже в процессе загрузки (state == "loading").
func (lr *LlamaCppRouter) isModelLoadedOnBackend(backendID, modelName string) bool {
	models := lr.queryCppWorkerModels(backendID)
	if models == nil {
		ridLog(lr_recentCtx()).Debugw("isModelLoadedOnBackend: query returned nil",
			"backend", backendID, "model", modelName)
		return false
	}
	for _, m := range models {
		if m.State != "loaded" && m.State != "loading" {
			continue
		}
		if matchCppWorkerModel(modelName, m) {
			ridLog(lr_recentCtx()).Debugw("isModelLoadedOnBackend: found",
				"backend", backendID, "model", modelName,
				"matched_name", m.Name, "matched_state", m.State)
			return true
		}
	}
	ridLog(lr_recentCtx()).Debugw("isModelLoadedOnBackend: not found",
		"backend", backendID, "model", modelName, "cppworker_models_count", len(models))
	return false
}

// isModelReadyOnBackend проверяет, завершилась ли загрузка модели (state == "loaded").
func (lr *LlamaCppRouter) isModelReadyOnBackend(backendID, modelName string) bool {
	models := lr.queryCppWorkerModels(backendID)
	if models == nil {
		return false
	}
	for _, m := range models {
		if m.State != "loaded" {
			continue
		}
		if matchCppWorkerModel(modelName, m) {
			return true
		}
	}
	return false
}

// basenameOfPath — выделяет имя файла из пути (поддержка '/' и '\').
func basenameOfPath(p string) string {
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

// ensureModelLoadedOnBackend автоматически загружает модель в VRAM на cppworker-бэкенде,
// если она ещё не загружена. Нужно для обработки inference-запросов (chat/generate) на
// холодную. С auto-load балансер сам СИНХРОННО грузит модель (до 5 минут), дожидается
// готовности, и только потом проксирует inference.
//
// extraOpts — опциональные параметры num_ctx и gpu_layers для загрузки.
//
// Возвращает:
//   - loaded=true если модель уже загружена или успешно загружена
//   - loaded=false + err != nil если не удалось загрузить
func (lr *LlamaCppRouter) ensureModelLoadedOnBackend(backendID, modelName string, extraOpts ...warmupOptions) (bool, error) {

	ridLog(lr_recentCtx()).Debugw("ensureModelLoadedOnBackend: enter",
		"backend", backendID, "model", modelName, "step", "enter")
	if modelName == "" {
		ridLog(lr_recentCtx()).Warnw("ensureModelLoadedOnBackend: empty model name",
			"backend", backendID)
		return false, fmt.Errorf("empty model name")
	}
	// Быстрая проверка — может модель уже загружена
	if lr.isModelReadyOnBackend(backendID, modelName) {
		ridLog(lr_recentCtx()).Debugw("ensureModelLoadedOnBackend: model already loaded",
			"backend", backendID, "model", modelName, "step", "is_loaded=true")
		return true, nil
	}
	// Проверка: возможно другой запрос уже инициировал загрузку (state="loading").
	models := lr.queryCppWorkerModels(backendID)
	isAlreadyLoading := false
	for _, m := range models {
		if matchCppWorkerModel(modelName, m) && m.State == "loading" {
			isAlreadyLoading = true
			break
		}
	}
	if isAlreadyLoading {
		ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: concurrent load already in progress, waiting",
			"backend", backendID, "model", modelName, "step", "wait_concurrent_load")
		pollDeadline := time.Now().Add(5 * time.Minute)
		for {
			if lr.isModelReadyOnBackend(backendID, modelName) {
				ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: concurrent load completed",
					"backend", backendID, "model", modelName, "step", "concurrent_load_done")
				return true, nil
			}
			models := lr.queryCppWorkerModels(backendID)
			foundError := false
			for _, m := range models {
				if matchCppWorkerModel(modelName, m) {
					if m.State == "error" {
						foundError = true
					}
					break
				}
			}
			if foundError {
				return false, fmt.Errorf("cppworker reported error state for model %q while waiting for concurrent load", modelName)
			}
			if time.Now().After(pollDeadline) {
				return false, fmt.Errorf("timed out waiting for concurrent model load to complete on backend %q", backendID)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Модель не загружена и не в процессе загрузки — выполняем auto-load через ModelManager
	mm := lr.proxy.GetModelManager()
	if mm == nil {
		ridLog(lr_recentCtx()).Errorw("ensureModelLoadedOnBackend: model manager not available",
			"backend", backendID, "model", modelName)
		return false, fmt.Errorf("model manager not available")
	}
	var ctxSize *int
	var gpuLayers *int
	if len(extraOpts) > 0 {
		opts := extraOpts[0]
		if opts.NumCtx > 0 {
			ctxSize = &opts.NumCtx
		}
		if opts.GPULayers > 0 {
			gpuLayers = &opts.GPULayers
		}
	}

	ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: auto-loading model",
		"backend", backendID, "model", modelName, "step", "execute_op=load",
		"numCtx", ctxSize, "gpuLayers", gpuLayers)
	result := mm.ExecuteOperation(backendID, ModelOpRequest{
		Operation:  "load",
		ModelName:  modelName,
		ContextSize: ctxSize,
		GPULayers:  gpuLayers,
	})

	if !result.Success {
		errStr := strings.ToLower(result.Error)
		if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "not implemented") || strings.Contains(errStr, "501") {
			ridLog(lr_recentCtx()).Warnw("ensureModelLoadedOnBackend: explicit load not supported by upstream, falling back to lazy-load",
				"backend", backendID, "model", modelName, "error", result.Error)
			return true, nil
		}

		// Страховка: если ExecuteOperation вернул ошибку вида "model is loading"
		// (cppworker уже загружает эту модель другой горутиной, наши ретраи
		// в executeLlamaCppLoad исчерпаны), попробуем дождаться завершения
		// загрузки через polling /api/models.
		if strings.Contains(errStr, "model is loading") || strings.Contains(errStr, "loading") {
			ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: load returned 'model is loading', polling for ready state",
				"backend", backendID, "model", modelName,
				"step", "wait_after_load_error",
				"op_error", result.Error)
			pollDeadline := time.Now().Add(5 * time.Minute)
			for {
				if lr.isModelReadyOnBackend(backendID, modelName) {
					ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: model became ready while polling after load error",
						"backend", backendID, "model", modelName,
						"step", "ready_after_load_error")
					return true, nil
				}
				if time.Now().After(pollDeadline) {
					ridLog(lr_recentCtx()).Errorw("ensureModelLoadedOnBackend: timeout polling for ready after load error",
						"backend", backendID, "model", modelName,
						"step", "poll_timeout_after_load_error")
					return false, fmt.Errorf("auto-load failed: %s", result.Error)
				}
				time.Sleep(500 * time.Millisecond)
			}
		}

		ridLog(lr_recentCtx()).Errorw("ensureModelLoadedOnBackend: ExecuteOperation failed",
			"backend", backendID, "model", modelName,
			"step", "execute_op_result",
			"op_success", result.Success,
			"op_error", result.Error,
			"op_message", result.Message)
		return false, fmt.Errorf("auto-load failed: %s", result.Error)
	}
	pollDeadline := time.Now().Add(5 * time.Second)
	for {
		if lr.isModelReadyOnBackend(backendID, modelName) {
			logger.Get().Infow("ensureModelLoadedOnBackend: auto-load successful",
				"backend", backendID, "model", modelName)
			return true, nil
		}
		models := lr.queryCppWorkerModels(backendID)
		foundLoading := false
		foundError := false
		for _, m := range models {
			if !matchCppWorkerModel(modelName, m) {
				continue
			}
			if m.State == "loading" {
				foundLoading = true
			}
			if m.State == "error" {
				foundError = true
			}
		}
		if foundError {
			return false, fmt.Errorf("cppworker reported error state for model %q", modelName)
		}
		if !foundLoading && time.Now().After(pollDeadline) {
			return false, fmt.Errorf("load reported success but model never reached state=loaded (deadline 5s)")
		}
		if time.Now().After(pollDeadline) {
			return false, fmt.Errorf("load reported success but model still in state=loading after 5s")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// proxyHTTP проксирует запрос к конкретному бэкенду
func (lr *LlamaCppRouter) proxyHTTP(r *http.Request, backendID string) (*http.Response, error) {
	backend := lr.proxy.GetBackend(backendID)
	if backend == nil {
		return nil, fmt.Errorf("backend not found")
	}

	port := lr.proxy.getBackendPort(backend)
	url := fmt.Sprintf("http://%s:%d%s", backend.Host, port, r.URL.String())
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
