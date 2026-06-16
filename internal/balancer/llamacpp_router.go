package balancer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// LlamaCppRouter — маршрутизатор для llama.cpp backend endpoint'ов.
// Аналогичен OllamaRouter, но использует BackendTypeLlamaCpp для выбора бэкендов
// и ищет модели в метриках llama.cpp вместо Ollama.
type LlamaCppRouter struct {
	proxy *Proxy
}

// NewLlamaCppRouter — создание маршрутизатора для llama.cpp
func NewLlamaCppRouter(proxy *Proxy) *LlamaCppRouter {
	return &LlamaCppRouter{proxy: proxy}
}

// Route — диспетчеризация запроса по URL.Path.
// Возвращает true если запрос был обработан.
//
// Поддерживает префиксы /ollama/* и /openai/* (используются OpenWebUI):
//   - /ollama/api/version  → strip → /api/version   → handleVersion
//   - /ollama/api/chat     → strip → /api/chat      → handleChat
//   - /ollama/api/tags     → strip → /api/tags      → handleTags
//   - /openai/v1/models    → strip → /v1/models     → handleOpenAIModels
//   - /openai/v1/chat/completions → strip → /v1/chat/completions → handleOpenAIChatCompletions
//
// Без этой нормализации OpenWebUI получает 500 на любой запрос к балансеру
// (потому что в switch нет case'ов /ollama/* и /openai/*, и proxy flow падает
// с пустым model / неизвестным путём).
func (lr *LlamaCppRouter) Route(w http.ResponseWriter, r *http.Request) bool {
	// Нормализация префиксов OpenWebUI: /ollama/* и /openai/*
	// делаем r.URL.Path копию чтобы не мутировать входящий *http.Request
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, "/ollama/"):
		path = strings.TrimPrefix(path, "/ollama")
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r = r2
	case strings.HasPrefix(path, "/openai/"):
		path = strings.TrimPrefix(path, "/openai")
		r2 := r.Clone(r.Context())
		r2.URL.Path = path
		r = r2
	}

	switch r.URL.Path {
	case "/api/tags":
		lr.handleTags(w, r)
		return true
	case "/api/version":
		lr.handleVersion(w, r)
		return true
	case "/api/ps":
		lr.handlePS(w, r)
		return true
	case "/api/show":
		lr.handleShow(w, r)
		return true
	case "/api/create":
		lr.handleCreate(w, r)
		return true
	case "/api/pull":
		lr.handlePull(w, r)
		return true
	case "/api/delete":
		lr.handleDelete(w, r)
		return true
	case "/api/copy":
		lr.handleCopy(w, r)
		return true
	case "/api/push":
		lr.handlePush(w, r)
		return true
	case "/api/chat":
		lr.handleChat(w, r)
		return true
	case "/api/generate":
		lr.handleGenerate(w, r)
		return true
	case "/api/models":
		// Нативный cppworker endpoint: {count, models:[{name, path, state, sizeBytes, ...}]}.
		// Агрегируем по всем llama.cpp бэкендам.
		lr.handleModels(w, r)
		return true
	// OpenAI-совместимые пути (для OpenWebUI, который вызывает /openai/v1/*)
	case "/v1/models":
		lr.handleOpenAIModels(w, r)
		return true
	case "/v1/chat/completions":
		lr.handleOpenAIChatCompletions(w, r)
		return true
	}
	return false
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
// Использует endpoint /api/models (НЕ /api/models/loaded — последний в текущей
// версии cppworker не реализован и возвращает 404). В /api/models каждая модель
// имеет поле state ("unloaded" | "loading" | "loaded" | "error").
//
// Возвращает (nil, nil) если backend не найден или запрос упал — в этом случае
// вызывающий код сам решает, грузить модель или нет.
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
	// Пробрасываем request_id в cppworker для сквозной корреляции логов.
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

// cppWorkerModelState — минимальная проекция состояния модели на cppworker.
type cppWorkerModelState struct {
	Name  string
	Path  string
	State string // "unloaded" | "loading" | "loaded" | "error"
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
// Используется в auto-load перед inference (handleChat/handleGenerate).
// Возвращает true если модель уже готова к inference (state == "loaded") ИЛИ
// уже в процессе загрузки (state == "loading" — в этом случае balancer не должен
// пытаться вызвать /api/models/load повторно, иначе cppworker вернёт "already loaded"
// и сразу отдаст 500).
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
// Отличие от isModelLoadedOnBackend: "loading" → false, ждём полной готовности.
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
// холодную — без auto-load cppworker начал бы lazy-load при первом inference, что:
//  1. Блокирует Go-воркер cppworker на 10-30+ секунд (чтение gguf с диска).
//  2. Превышает таймаут Go-клиента балансера (streamTimeout), и OpenWebUI получает
//     "timeout awaiting response headers" → 500 Internal Server Error.
//
// С auto-load балансер сам СИНХРОННО грузит модель (до 5 минут), дожидается готовности,
// и только потом проксирует inference. OpenWebUI получает либо 503 (load упал), либо
// нормальный ответ.
//
// Возвращает:
//   - loaded=true если модель уже загружена или успешно загружена
//   - loaded=false + err != nil если не удалось загрузить (нужно вернуть клиенту 503)
func (lr *LlamaCppRouter) ensureModelLoadedOnBackend(backendID, modelName string) (bool, error) {
	ridLog(lr_recentCtx()).Debugw("ensureModelLoadedOnBackend: enter",
		"backend", backendID, "model", modelName, "step", "enter")
	if modelName == "" {
		ridLog(lr_recentCtx()).Warnw("ensureModelLoadedOnBackend: empty model name",
			"backend", backendID)
		return false, fmt.Errorf("empty model name")
	}
	// Быстрая проверка — может модель уже загружена
	if lr.isModelLoadedOnBackend(backendID, modelName) {
		ridLog(lr_recentCtx()).Debugw("ensureModelLoadedOnBackend: model already loaded",
			"backend", backendID, "model", modelName, "step", "is_loaded=true")
		return true, nil
	}
	// Модель не загружена — выполняем auto-load через ModelManager
	mm := lr.proxy.GetModelManager()
	if mm == nil {
		ridLog(lr_recentCtx()).Errorw("ensureModelLoadedOnBackend: model manager not available",
			"backend", backendID, "model", modelName)
		return false, fmt.Errorf("model manager not available")
	}
	ridLog(lr_recentCtx()).Infow("ensureModelLoadedOnBackend: auto-loading model",
		"backend", backendID, "model", modelName, "step", "execute_op=load")
	result := mm.ExecuteOperation(backendID, ModelOpRequest{
		Operation: "load",
		ModelName: modelName,
	})
	if !result.Success {
		// Graceful fallback: если upstream не реализует /api/models/load (404/501),
		// не прерываем запрос — пусть CppWorker выполнит lazy-load самостоятельно.
		// Это нужно для совместимости с mock/stub-серверами и старыми сборками.
		errStr := strings.ToLower(result.Error)
		if strings.Contains(errStr, "404") || strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "not implemented") || strings.Contains(errStr, "501") {
			ridLog(lr_recentCtx()).Warnw("ensureModelLoadedOnBackend: explicit load not supported by upstream, falling back to lazy-load",
				"backend", backendID, "model", modelName, "error", result.Error)
			return true, nil
		}
		ridLog(lr_recentCtx()).Errorw("ensureModelLoadedOnBackend: ExecuteOperation failed",
			"backend", backendID, "model", modelName,
			"step", "execute_op_result",
			"op_success", result.Success,
			"op_error", result.Error,
			"op_message", result.Message)
		return false, fmt.Errorf("auto-load failed: %s", result.Error)
	}
	// Дополнительно: cppworker в handleLoadModel возвращает 200 только после полного
	// LoadModelWithOpts (он синхронный и блокирует до окончания bridge.LoadModel).
	// Но на всякий случай — короткое poll-ожидание state=="loaded" с таймаутом 5 сек.
	// Это страховка от рейс-кондишенов, если в будущем load станет асинхронным.
	pollDeadline := time.Now().Add(5 * time.Second)
	for {
		if lr.isModelReadyOnBackend(backendID, modelName) {
			logger.Get().Infow("ensureModelLoadedOnBackend: auto-load successful",
				"backend", backendID, "model", modelName)
			return true, nil
		}
		// Если модель в состоянии "loading" — другой запрос уже грузит, продолжаем ждать.
		// Если "error" — cppworker сообщил об ошибке, дальше ждать нет смысла.
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
			// Даже если loading — выходим по таймауту. Это очень странно.
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

// ---------- Read-only endpoints ----------

// handleTags — агрегирует список моделей со всех llama.cpp бэкендов.
// Сначала собирает из метрик (быстро). Если метрики пустые — делает fallback-запрос
// к /v1/models здорового llama.cpp бэкенда для получения актуального списка.
func (lr *LlamaCppRouter) handleTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: []OllamaTag{}})
		return
	}

	// Собираем модели из метрик всех llama.cpp бэкендов
	lr.proxy.metricsMgr.mu.RLock()
	uniqueModels := make(map[string]OllamaTag)
	for _, b := range backends {
		metrics, ok := lr.proxy.metricsMgr.metrics[b.id]
		if !ok {
			continue
		}
		for _, m := range metrics.LlamaCpp.LoadedModels {
			if _, exists := uniqueModels[m.Name]; !exists {
				uniqueModels[m.Name] = OllamaTag{
					Name:  m.Name,
					Model: m.Name,
					Size:  0,
				}
			}
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// Fallback 1: если метрики пустые — запрашиваем /v1/models (OpenAI) у бэкендов.
	// /v1/models на cppworker отдаёт только загруженные в VRAM модели.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			models, err := lr.fetchLlamaCppModels(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleTags: fallback /v1/models failed",
					"backend", b.id, "host", b.host, "port", b.port, "error", err)
				continue
			}
			for _, m := range models {
				if _, exists := uniqueModels[m.Name]; !exists {
					uniqueModels[m.Name] = OllamaTag{
						Name:  m.Name,
						Model: m.Name,
						Size:  0,
					}
				}
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	// Fallback 2: если и /v1/models пустой — запрашиваем cppworker's Ollama-совместимый
	// /api/tags. В отличие от /v1/models, этот endpoint отдаёт ВСЕ .gguf файлы на диске
	// (включая выгруженные), с size/digest/modified_at/details. Это правильный источник
	// для Ollama-клиентов, ожидающих увидеть все доступные модели, а не только
	// загруженные в VRAM.
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			tags, err := lr.fetchLlamaCppTags(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleTags: fallback cppworker /api/tags failed",
					"backend", b.id, "host", b.host, "port", b.port, "error", err)
				continue
			}
			for _, t := range tags {
				if _, exists := uniqueModels[t.Name]; !exists {
					uniqueModels[t.Name] = t
				}
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	models := make([]OllamaTag, 0, len(uniqueModels))
	for _, m := range uniqueModels {
		models = append(models, m)
	}

	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})

	logger.Get().Infow("handleTags: returning models",
		"count", len(models),
		"from_metrics", len(uniqueModels) > 0,
	)

	writeJSON(w, http.StatusOK, OllamaTagsResponse{Models: models})
}

// fetchLlamaCppModels — запрашивает /v1/models у llama.cpp бэкенда
// и парсит OpenAI-совместимый ответ в список OllamaTag.
func (lr *LlamaCppRouter) fetchLlamaCppModels(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/v1/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// OpenAI /v1/models формат: {"object":"list","data":[{"id":"model-name","object":"model",...}]}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Data))
	for _, d := range result.Data {
		tags = append(tags, OllamaTag{
			Name:  d.ID,
			Model: d.ID,
			Size:  0,
		})
	}
	return tags, nil
}

// fetchLlamaCppTags — запрашивает Ollama-совместимый /api/tags у cppworker.
// В отличие от /v1/models, этот endpoint отдаёт ВСЕ .gguf файлы на диске
// (включая выгруженные), с полным Ollama-форматом: name, model, size, digest,
// modified_at, details (family, format, parameter_size, quantization_level).
func (lr *LlamaCppRouter) fetchLlamaCppTags(host string, port int) ([]OllamaTag, error) {
	url := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	// Ollama-совместимый формат: {"models":[{"name":"...","model":"...","size":N,"digest":"...","modified_at":"...","details":{...}}]}
	var result struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model,omitempty"`
			Size       int64  `json:"size"`
			Digest     string `json:"digest,omitempty"`
			ModifiedAt string `json:"modified_at,omitempty"`
			Details    struct {
				Format          string `json:"format,omitempty"`
				Family          string `json:"family,omitempty"`
				ParameterSize   string `json:"parameter_size,omitempty"`
				QuantizationLvl string `json:"quantization_level,omitempty"`
			} `json:"details,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	tags := make([]OllamaTag, 0, len(result.Models))
	for _, m := range result.Models {
		name := m.Name
		if name == "" {
			name = m.Model
		}
		// Убираем расширение .gguf для консистентности с Ollama-форматом
		// (cppworker может отдавать "model.gguf", Ollama — "model:tag").
		displayName := strings.TrimSuffix(name, ".gguf")

		// Details: собираем map[string]interface{} как в OllamaTagsResponse.
		details := map[string]interface{}{}
		if m.Details.Format != "" {
			details["format"] = m.Details.Format
		}
		if m.Details.Family != "" {
			details["family"] = m.Details.Family
		}
		if m.Details.ParameterSize != "" {
			details["parameter_size"] = m.Details.ParameterSize
		}
		if m.Details.QuantizationLvl != "" {
			details["quantization_level"] = m.Details.QuantizationLvl
		}

		// Парсим modified_at в time.Time (OllamaTag.ModifiedAt имеет тип time.Time).
		var modTime time.Time
		if m.ModifiedAt != "" {
			if t, err := time.Parse(time.RFC3339, m.ModifiedAt); err == nil {
				modTime = t
			} else {
				logger.Get().Debugw("fetchLlamaCppTags: failed to parse modified_at",
					"value", m.ModifiedAt, "error", err)
			}
		}

		tags = append(tags, OllamaTag{
			Name:       displayName,
			Model:      displayName,
			Size:       m.Size,
			Digest:     m.Digest,
			ModifiedAt: modTime,
			Details:    details,
		})
	}
	return tags, nil
}

// handleModels — возвращает список моделей в нативном формате cppworker'а.
// cppworker отдаёт {count, models:[{name, path, state, sizeBytes, nLayers, ...}]}.
// Агрегируем по всем llama.cpp бэкендам (в данный момент берём первый, как в handleTags;
// полная агрегация возможна, но требует дедупликации по path).
func (lr *LlamaCppRouter) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"count":  0,
			"models": []interface{}{},
		})
		return
	}

	// Запрашиваем /api/models у каждого llama.cpp бэкенда и агрегируем.
	// Добавляем поле "backend" к каждой модели, чтобы клиент знал источник.
	type cppModel struct {
		Name          string `json:"name"`
		Path          string `json:"path,omitempty"`
		State         string `json:"state,omitempty"`
		SizeBytes     int64  `json:"sizeBytes,omitempty"`
		NLayers       int    `json:"nLayers,omitempty"`
		NHeads        int    `json:"nHeads,omitempty"`
		NEmbd         int    `json:"nEmbd,omitempty"`
		NVocab        int    `json:"nVocab,omitempty"`
		ContextSize   int    `json:"contextSize,omitempty"`
		GPULayers     int    `json:"gpuLayers,omitempty"`
		ActiveQueries int    `json:"activeQueries,omitempty"`
		TotalQueries  int    `json:"totalQueries,omitempty"`
		LoadedAt      string `json:"loadedAt,omitempty"`
		Backend       string `json:"backend,omitempty"`
	}
	type cppResponse struct {
		Count  int        `json:"count"`
		Models []cppModel `json:"models"`
	}

	merged := cppResponse{Models: []cppModel{}}
	seen := make(map[string]bool)

	for _, b := range backends {
		resp, err := lr.fetchLlamaCppModelsNative(b.host, b.port)
		if err != nil {
			logger.Get().Warnw("handleModels: fetch failed",
				"backend", b.id, "host", b.host, "port", b.port, "error", err)
			continue
		}
		for _, m := range resp.Models {
			key := m.Path
			if key == "" {
				key = m.Name
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			merged.Models = append(merged.Models, cppModel{
				Name:          m.Name,
				Path:          m.Path,
				State:         m.State,
				SizeBytes:     m.SizeBytes,
				NLayers:       m.NLayers,
				NHeads:        m.NHeads,
				NEmbd:         m.NEmbd,
				NVocab:        m.NVocab,
				ContextSize:   m.ContextSize,
				GPULayers:     m.GPULayers,
				ActiveQueries: m.ActiveQueries,
				TotalQueries:  m.TotalQueries,
				LoadedAt:      m.LoadedAt,
				Backend:       b.id,
			})
		}
	}
	merged.Count = len(merged.Models)

	logger.Get().Infow("handleModels: returning models",
		"count", merged.Count,
		"backends_queried", len(backends),
	)

	writeJSON(w, http.StatusOK, merged)
}

// fetchLlamaCppModelsNative — запрашивает нативный /api/models у cppworker.
// Возвращает список {name, path, state, sizeBytes, ...} без агрегации.
type cppWorkerModelsNative struct {
	Count  int `json:"count"`
	Models []struct {
		Name          string `json:"name"`
		Path          string `json:"path,omitempty"`
		State         string `json:"state,omitempty"`
		SizeBytes     int64  `json:"sizeBytes,omitempty"`
		NLayers       int    `json:"nLayers,omitempty"`
		NHeads        int    `json:"nHeads,omitempty"`
		NEmbd         int    `json:"nEmbd,omitempty"`
		NVocab        int    `json:"nVocab,omitempty"`
		ContextSize   int    `json:"contextSize,omitempty"`
		GPULayers     int    `json:"gpuLayers,omitempty"`
		ActiveQueries int    `json:"activeQueries,omitempty"`
		TotalQueries  int    `json:"totalQueries,omitempty"`
		LoadedAt      string `json:"loadedAt,omitempty"`
	} `json:"models"`
}

func (lr *LlamaCppRouter) fetchLlamaCppModelsNative(host string, port int) (*cppWorkerModelsNative, error) {
	url := fmt.Sprintf("http://%s:%d/api/models", host, port)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result cppWorkerModelsNative
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// handlePS — возвращает список запущенных моделей на llama.cpp бэкендах (из метрик)
func (lr *LlamaCppRouter) handlePS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	if len(backends) == 0 {
		writeJSON(w, http.StatusOK, OllamaPSResponse{Models: []OllamaProcess{}})
		return
	}

	lr.proxy.metricsMgr.mu.RLock()
	var allProcesses []OllamaProcess
	for _, b := range backends {
		metrics, ok := lr.proxy.metricsMgr.metrics[b.id]
		if !ok {
			continue
		}
		for _, m := range metrics.LlamaCpp.LoadedModels {
			allProcesses = append(allProcesses, OllamaProcess{
				Name:     m.Name,
				Model:    m.Name,
				Size:     0,
				SizeVRAM: 0,
			})
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	writeJSON(w, http.StatusOK, OllamaPSResponse{Models: allProcesses})
}

// handleVersion — возвращает версию балансировщика и версии llama.cpp бэкендов
func (lr *LlamaCppRouter) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	response := map[string]interface{}{
		"version":       "ollamalegion-1.0.0",
		"llamaVersions": make(map[string]string),
	}

	writeJSON(w, http.StatusOK, response)
}

// handleOpenAIModels — возвращает список моделей в OpenAI-формате
// {"object":"list","data":[{"id":"...","object":"model","created":...,"owned_by":"ollamalegion"}]}
// Используется OpenWebUI при обращении к /openai/v1/models.
func (lr *LlamaCppRouter) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	backends := lr.getLlamaCppBackends()
	uniqueModels := make(map[string]bool)

	// Собираем модели из метрик
	lr.proxy.metricsMgr.mu.RLock()
	for _, b := range backends {
		metrics, ok := lr.proxy.metricsMgr.metrics[b.id]
		if !ok {
			continue
		}
		for _, m := range metrics.LlamaCpp.LoadedModels {
			uniqueModels[m.Name] = true
		}
	}
	lr.proxy.metricsMgr.mu.RUnlock()

	// Fallback: если метрики пустые — запрашиваем /v1/models у бэкендов напрямую
	if len(uniqueModels) == 0 {
		for _, b := range backends {
			models, err := lr.fetchLlamaCppModels(b.host, b.port)
			if err != nil {
				logger.Get().Warnw("handleOpenAIModels: fallback /v1/models failed",
					"backend", b.id, "error", err)
				continue
			}
			for _, m := range models {
				uniqueModels[m.Name] = true
			}
			if len(uniqueModels) > 0 {
				break
			}
		}
	}

	now := time.Now().Unix()
	data := make([]map[string]interface{}, 0, len(uniqueModels))
	for name := range uniqueModels {
		data = append(data, map[string]interface{}{
			"id":       name,
			"object":   "model",
			"created":  now,
			"owned_by": "ollamalegion",
		})
	}
	sort.Slice(data, func(i, j int) bool {
		idI, _ := data[i]["id"].(string)
		idJ, _ := data[j]["id"].(string)
		return idI < idJ
	})

	logger.Get().Infow("handleOpenAIModels: returning models",
		"count", len(data),
		"from_metrics", len(uniqueModels) > 0,
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"object": "list",
		"data":   data,
	})
}

// handleOpenAIChatCompletions — проксирует /v1/chat/completions к llama.cpp бэкенду.
//
// Этот endpoint вызывают OpenAI-совместимые клиенты: Roo Code, Cline, Continue.dev,
// и иногда OpenWebUI при выборе OpenAI-совместимого режима. Клиент ожидает SSE
// в OpenAI-формате (data: {choices:[{delta:{content:"..."}}]}\n\n) и закрывает
// соединение по таймауту (~60s), если от бэкенда не приходит никаких данных.
//
// Ключевые отличия от handleChat:
//  1. Auto-load модели ПЕРЕД проксированием (без этого cppworker блокирует
//     чтение заголовков на 10-30s во время cold-load, и клиент таймаутится).
//  2. Проксирование через proxyRequestOpenAIStreaming — с SSE-heartbeat
//     каждые 15s (`: keepalive\n\n`), чтобы клиент не закрыл соединение
//     по idle-таймауту, пока бэкенд генерирует длинный ответ.
//  3. Выбор бэкенда: сначала ищем, на каком бэкенде модель уже загружена;
//     если нигде — берём любой healthy llama.cpp бэкенд и запускаем auto-load.
func (lr *LlamaCppRouter) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Читаем тело единожды
	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	// Извлекаем model из OpenAI-формата
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &req); err != nil {
		logger.Get().Errorw("handleOpenAIChatCompletions: failed to parse body", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := req["model"].(string)

	// Нормализуем messages[].content: современные OpenAI-клиенты (Cline, Roo Code,
	// Continue.dev, OpenWebUI) посылают multi-modal content в виде массива
	// (например, [{type:"text", text:"..."}, {type:"image_url", ...}]), а
	// cppworker ожидает строку. Без нормализации upstream возвращает
	// 400 "json: cannot unmarshal array into ... messages.content of type string".
	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	// Выбор бэкенда: сначала ищем бэкенд, где модель уже загружена
	// (по метрикам LoadedModels), чтобы избежать cold-start.
	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		// Нигде не загружена — берём любой healthy и сделаем auto-load.
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}

	state, ok := lr.proxy.backends[backendID]
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "backend state not found"})
		return
	}

	// Auto-load: если модель выгружена из VRAM — синхронно грузим перед проксированием.
	// Без этого cppworker блокирует чтение заголовков ответа на 10-30+ секунд,
	// клиент (Roo/Cline) таймаутится на ~60s и показывает «бесконечную загрузку».
	if model != "" {
		if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model); loadErr != nil {
			logger.Get().Errorw("handleOpenAIChatCompletions: auto-load failed",
				"backend", backendID, "model", model, "error", loadErr)
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
			})
			return
		}
	}

	// Восстанавливаем r.Body из bodyBuf для последующего использования.
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// 3-tier resolver: применяем X-Cpp-Ctx header (если body не задал num_ctx).
	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleOpenAIChatCompletions: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	targetURL := fmt.Sprintf("http://%s:%d/v1/chat/completions", state.Backend.Host, lr.proxy.getBackendPort(state.Backend))

	logger.Get().Infow("handleOpenAIChatCompletions: proxying to cppworker",
		"backend", backendID, "url", targetURL, "model", model)

	// Создаём upstream-запрос, передавая тело как есть
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, targetURL, bytes.NewReader(bodyBuf))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Пробрасываем Content-Type
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json, text/event-stream")

	// Только Authorization если есть (для прокси-аутентификации), и IP-заголовки
	clientRealIP := lr.proxy.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		upstreamReq.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		upstreamReq.Header.Set("X-Forwarded-For", clientRealIP)
	}
	upstreamReq.Header.Set("X-Real-IP", clientRealIP)

	upstreamResp, err := lr.proxy.streamingClient.Do(upstreamReq)
	if err != nil {
		logger.Get().Errorw("handleOpenAIChatCompletions: upstream request failed",
			"backend", backendID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	// ==== n_ctx auto-reload: перехват структурированной ошибки от cppworker ====
	// cppworker при n_ctx overflow отвечает HTTP 400 (Bad Request) с JSON
	// {error, code: 2 (N_CTX_NEEDS_RELOAD), bridge_info: {...}}. Если
	// auto-reload включён — перезагружаем модель с большим n_ctx и повторяем.
	if upstreamResp.StatusCode >= 400 {
		// Drain body для парсинга структурированной ошибки
		if peekBody, peekErr := io.ReadAll(upstreamResp.Body); peekErr == nil {
			_ = upstreamResp.Body.Close()
			if nctxErr := ParseCppWorkerError(peekBody, upstreamResp.StatusCode, backendID); nctxErr != nil {
				if errors.Is(nctxErr, bridge.ErrNCtxNeedsReload) || errors.Is(nctxErr, bridge.ErrPromptTooLong) {
					logger.Get().Infow("handleOpenAIChatCompletions: detected n_ctx error from cppworker, invoking auto-reload",
						"backend", backendID, "model", model,
						"status", upstreamResp.StatusCode)
					lr.proxy.handleNCtxReload(r.Context(), w, r, backendID, model, nctxErr, bodyBuf)
					return
				}
			}
			// Не nctx-ошибка — проксируем как обычно (с восстановленным body).
			upstreamResp.Body = io.NopCloser(bytes.NewReader(peekBody))
		}
	}

	// Проксируем через streaming-обёртку с heartbeat. Не делаем defer close —
	// proxyRequestOpenAIStreaming сам закроет upstreamResp.Body.
	_ = lr.proxy.proxyRequestOpenAIStreaming(w, r, upstreamResp, backendID)
}

// ---------- Model operation endpoints ----------

func (lr *LlamaCppRouter) handleShow(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleCreate(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handlePull(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleDelete(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handleCopy(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

func (lr *LlamaCppRouter) handlePush(w http.ResponseWriter, r *http.Request) {
	backendID := lr.selectLlamaCppBackendByResources(r)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	resp, err := lr.proxyHTTP(r, backendID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	copyResponse(w, resp)
}

// handleChat — проксирует /api/chat запросы к llama.cpp бэкендам с трансляцией форматов.
// При stream=true использует proxyRequestLlamaCpp (SSE→NDJSON).
// При stream=false использует proxyRequestLlamaCppNonStream.
//
// Auto-load: перед проксированием проверяет, загружена ли модель в VRAM на выбранном
// бэкенде. Если нет — вызывает POST /api/models/load и ждёт готовности (синхронно,
// до 5 минут). Без этого первый запрос после выгрузки модели получает 30-сек таймаут
// на streamingClient и OpenWebUI видит "timeout awaiting response headers" → 500.
func (lr *LlamaCppRouter) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Читаем тело единожды
	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleChat: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	// Парсим модель напрямую из bodyBuf — НЕ через parseRequestBody, потому что
	// parseRequestBody вызывает io.ReadAll(r.Body), а мы только что восстановили
	// r.Body из bodyBuf. Прямой json.Unmarshal — проще и надёжнее, плюс не зависит
	// от порядка восстановления body (раньше был баг с empty model name).
	var reqMap map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &reqMap); err != nil {
		logger.Get().Errorw("handleChat: failed to parse body JSON", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := reqMap["model"].(string)
	if model == "" {
		logger.Get().Warnw("handleChat: empty model in request body")
	}

	// Нормализуем multi-modal content[] (Cline/Roo/OpenWebUI могут слать массивный
	// content даже в Ollama-формате). Без этого cppworker вернёт 400.
	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	// Выбор бэкенда: используем простой и проверенный алгоритм (тот же, что и в
	// handleOpenAIChatCompletions), а не многоуровневый selectBackend() с P1-P4,
	// который может вернуть "" или зависнуть в sync-load цикле на нездоровом бэкенде.
	// Сначала ищем бэкенд, где модель уже загружена (горячий путь),
	// иначе берём любой healthy llama.cpp бэкенд (для auto-load).
	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		logger.Get().Errorw("handleChat: no llama.cpp backend available",
			"model", model)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	logger.Get().Debugw("handleChat: selected backend",
		"model", model, "backend", backendID)
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// Auto-load: если модель выгружена из VRAM — синхронно грузим перед проксированием.
	if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model); loadErr != nil {
		logger.Get().Errorw("handleChat: auto-load failed",
			"backend", backendID, "model", model, "error", loadErr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
		})
		return
	}

	// 3-tier resolver: применяем X-Cpp-Ctx header (если body не задал num_ctx).
	// Приоритет: body > profile > backend default. cppworker прочитает header и
	// применит к params.NCtxOverride (если body не задал явный num_ctx).
	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleChat: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	// Выбираем прокси по режиму streaming
	var err error
	if isStreamingFromBody(r.URL.Path, bodyBuf) {
		logger.Get().Debugw("handleChat: streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	} else {
		logger.Get().Debugw("handleChat: non-streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCppNonStream(w, r, backendID, bodyBuf)
	}
	if err != nil {
		logger.Get().Errorw("handleChat: proxy failed", "backend", backendID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

// handleGenerate — проксирует /api/generate запросы к llama.cpp бэкендам с трансляцией форматов.
// При stream=true использует proxyRequestLlamaCpp (SSE→NDJSON).
// При stream=false использует proxyRequestLlamaCppNonStream.
//
// Auto-load: см. комментарий к handleChat.
func (lr *LlamaCppRouter) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Читаем тело единожды
	var bodyBuf []byte
	if r.Body != nil {
		var err error
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("handleGenerate: failed to read body", "error", err)
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(bodyBuf))
	}

	// Парсим модель напрямую из bodyBuf (см. handleChat).
	var reqMap map[string]interface{}
	if err := json.Unmarshal(bodyBuf, &reqMap); err != nil {
		logger.Get().Errorw("handleGenerate: failed to parse body JSON", "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	model, _ := reqMap["model"].(string)
	logger.Get().Debugw("handleGenerate: parsed request",
		"model", model, "body_len", len(bodyBuf))

	// Нормализуем multi-modal content[] (Cline/Roo/OpenWebUI могут слать массивный
	// content даже в Ollama-формате). Без этого cppworker вернёт 400.
	if normalized := normalizeOpenAIBody(bodyBuf); len(normalized) > 0 {
		bodyBuf = normalized
	}

	// Выбор бэкенда: используем простой и проверенный алгоритм (тот же, что и в
	// handleOpenAIChatCompletions), а не многоуровневый selectBackend() с P1-P4,
	// который может вернуть "" или зависнуть в sync-load цикле на нездоровом бэкенде.
	// Сначала ищем бэкенд, где модель уже загружена (горячий путь),
	// иначе берём любой healthy llama.cpp бэкенд (для auto-load).
	backendID := lr.findModelOnLlamaCppBackend(model)
	if backendID == "" {
		backendID = lr.selectAnyLlamaCppHealthy()
	}
	if backendID == "" {
		logger.Get().Errorw("handleGenerate: no llama.cpp backend available",
			"model", model)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	logger.Get().Debugw("handleGenerate: selected backend",
		"model", model, "backend", backendID)
	// Восстанавливаем тело для proxyRequestLlamaCpp
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

	// Auto-load: если модель выгружена из VRAM — синхронно грузим перед проксированием.
	if _, loadErr := lr.ensureModelLoadedOnBackend(backendID, model); loadErr != nil {
		logger.Get().Errorw("handleGenerate: auto-load failed",
			"backend", backendID, "model", model, "error", loadErr)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("model '%s' is not loaded and auto-load failed: %v", model, loadErr),
		})
		return
	}

	// 3-tier resolver: применяем X-Cpp-Ctx header (если body не задал num_ctx).
	resolved := lr.proxy.ApplyCppCtxHeader(r, model, bodyBuf, backendID)
	if resolved.Value > 0 {
		logger.Get().Debugw("handleGenerate: 3-tier resolver applied num_ctx override",
			"model", model, "backend", backendID,
			"resolved_n_ctx", resolved.Value, "source", resolved.Source)
	}

	// Выбираем прокси по режиму streaming
	var err error
	if isStreamingFromBody(r.URL.Path, bodyBuf) {
		logger.Get().Debugw("handleGenerate: streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	} else {
		logger.Get().Debugw("handleGenerate: non-streaming mode", "backend", backendID)
		err = lr.proxy.proxyRequestLlamaCppNonStream(w, r, backendID, bodyBuf)
	}
	if err != nil {
		logger.Get().Errorw("handleGenerate: proxy failed", "backend", backendID, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
	}
}

// ---------- Ollama-compatible streaming endpoints ----------

// LlamaCppChatResponse — структура для парсинга ответа llama.cpp /v1/chat/completions
type LlamaCppChatResponse struct {
	ID      string `json:"id,omitempty"`
	Object  string `json:"object,omitempty"`
	Created int64  `json:"created,omitempty"`
	Model   string `json:"model,omitempty"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content string `json:"content,omitempty"`
			Role    string `json:"role,omitempty"`
		} `json:"delta,omitempty"`
		Message struct {
			Content string `json:"content,omitempty"`
			Role    string `json:"role,omitempty"`
		} `json:"message,omitempty"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens,omitempty"`
		CompletionTokens int `json:"completion_tokens,omitempty"`
		TotalTokens      int `json:"total_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

// ConvertOpenAIResponseToOllama конвертирует ответ llama.cpp (OpenAI-формат) в Ollama формат
func ConvertOpenAIResponseToOllama(lcppResp *LlamaCppChatResponse) map[string]interface{} {
	ollamaResp := map[string]interface{}{
		"model":      lcppResp.Model,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}

	if len(lcppResp.Choices) > 0 {
		choice := lcppResp.Choices[0]
		if choice.Message.Content != "" {
			ollamaResp["message"] = map[string]interface{}{
				"role":    choice.Message.Role,
				"content": choice.Message.Content,
			}
		}
		if choice.Delta.Content != "" {
			ollamaResp["message"] = map[string]interface{}{
				"role":    choice.Delta.Role,
				"content": choice.Delta.Content,
			}
		}
		ollamaResp["done"] = choice.FinishReason == "stop"
	}

	if lcppResp.Usage.TotalTokens > 0 {
		ollamaResp["eval_count"] = lcppResp.Usage.CompletionTokens
		ollamaResp["prompt_eval_count"] = lcppResp.Usage.PromptTokens
	}

	return ollamaResp
}

// formatLlamaCppModelsList строит список моделей из всех llama.cpp бэкендов
func (lr *LlamaCppRouter) formatLlamaCppModelsList() map[string]interface{} {
	var wg sync.WaitGroup
	type modelInfo struct {
		Name       string `json:"name"`
		ModifiedAt string `json:"modified_at"`
		Size       int64  `json:"size"`
	}

	backends := lr.getLlamaCppBackends()
	results := make(chan modelInfo, len(backends)*10)

	for _, b := range backends {
		wg.Add(1)
		go func(bi backendInfo) {
			defer wg.Done()
			lr.proxy.metricsMgr.mu.RLock()
			metrics, ok := lr.proxy.metricsMgr.metrics[bi.id]
			lr.proxy.metricsMgr.mu.RUnlock()
			if !ok {
				return
			}
			for _, m := range metrics.LlamaCpp.LoadedModels {
				results <- modelInfo{
					Name:       m.Name,
					ModifiedAt: time.Now().Format(time.RFC3339),
					Size:       0,
				}
			}
		}(b)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	unique := make(map[string]modelInfo)
	for mi := range results {
		if _, exists := unique[mi.Name]; !exists {
			unique[mi.Name] = mi
		}
	}

	models := make([]modelInfo, 0, len(unique))
	for _, m := range unique {
		models = append(models, m)
	}

	return map[string]interface{}{
		"models": models,
	}
}

// encodeJSON helper для encode
func encodeJSON(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}
