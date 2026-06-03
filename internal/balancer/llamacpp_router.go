package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

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

	// Fallback: если метрики пустые — запрашиваем /v1/models у первого здорового бэкенда
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
				break // получили модели от первого доступного бэкенда
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
				Name:    m.Name,
				Model:   m.Name,
				Size:    0,
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
		"version":   "ollamalegion-1.0.0",
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
// Запрос приходит в OpenAI-формате, а не Ollama-формате, поэтому мы НЕ вызываем
// proxyRequestLlamaCpp (который делает Ollama→OpenAI трансляцию). Вместо этого
// проксируем как есть — llama.cpp ждёт именно OpenAI-формат.
// При streaming=true: проксируем SSE байт-в-байт (без трансляции, т.к. OpenWebUI
// понимает OpenAI-стрим).
// При streaming=false: отдаём ответ бэкенда как есть.
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

	// Выбираем бэкенд
	backendID := lr.proxy.selectBackend(model, types.BackendTypeLlamaCpp)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}

	state, ok := lr.proxy.backends[backendID]
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "backend state not found"})
		return
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
	defer upstreamResp.Body.Close()

	// Копируем статус-код и Content-Type ответа бэкенда
	for key, values := range upstreamResp.Header {
		keyLower := strings.ToLower(key)
		if keyLower == "transfer-encoding" || keyLower == "content-length" || keyLower == "connection" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(upstreamResp.StatusCode)

	// Стрим в обе стороны: копируем байты от бэкенда клиенту.
	// Это работает и для SSE (text/event-stream), и для JSON.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := upstreamResp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				logger.Get().Warnw("handleOpenAIChatCompletions: write to client failed",
					"backend", backendID, "error", writeErr)
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				logger.Get().Debugw("handleOpenAIChatCompletions: read from backend ended",
					"backend", backendID, "error", readErr)
			}
			return
		}
	}
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

	// Выбираем бэкенд по модели
	model := lr.proxy.parseRequestBody(r).Model
	backendID := lr.proxy.selectBackend(model, types.BackendTypeLlamaCpp)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

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

	// Выбираем бэкенд по модели из уже прочитанного тела
	model := lr.proxy.parseRequestBody(r).Model
	backendID := lr.proxy.selectBackend(model, types.BackendTypeLlamaCpp)
	if backendID == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no llama.cpp backend available"})
		return
	}
	// Восстанавливаем тело для proxyRequestLlamaCpp
	r.Body = io.NopCloser(bytes.NewReader(bodyBuf))

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