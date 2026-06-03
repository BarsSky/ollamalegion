package balancer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// OllamaErrorResponse — структура ошибки от Ollama API
type OllamaErrorResponse struct {
	Error string `json:"error"`
}

// determineErrorType — классифицирует ошибку HTTP-запроса для логирования и retry-логики
func determineErrorType(err error, reqCtx context.Context) string {
	errDetail := err.Error()
	if reqCtx.Err() == context.DeadlineExceeded {
		return "context_deadline_exceeded"
	}
	if reqCtx.Err() == context.Canceled {
		return "context_canceled"
	}
	if strings.Contains(errDetail, "connection refused") {
		return "connection_refused"
	}
	if strings.Contains(errDetail, "connection reset") {
		return "connection_reset_by_peer"
	}
	if strings.Contains(errDetail, "no such host") {
		return "dns_resolution_failed"
	}
	if strings.Contains(errDetail, "timeout") || strings.Contains(errDetail, "deadline") {
		return "timeout"
	}
	if strings.Contains(errDetail, "broken pipe") {
		return "broken_pipe"
	}
	if strings.Contains(errDetail, "EOF") {
		return "unexpected_eof"
	}
	return "unknown"
}

// isModelNotFoundError — проверяет, является ли ошибка от Ollama "model not found"
func isModelNotFoundError(body []byte) bool {
	var errResp OllamaErrorResponse
	if err := json.Unmarshal(body, &errResp); err != nil {
		return false
	}
	errLower := strings.ToLower(errResp.Error)
	return strings.Contains(errLower, "model") &&
		(strings.Contains(errLower, "not found") ||
			strings.Contains(errLower, "does not exist") ||
			strings.Contains(errLower, "not exist") ||
			strings.Contains(errLower, "not supported"))
}

// proxyRequest - проксирование запроса к бэкенду с поддержкой streaming/SSE.
// Вызывающий код (ServeHTTP / processRequest) должен предварительно захватить слот
// через tryAcquireSlot и гарантировать вызов releaseSlot через defer после возврата.
func (p *Proxy) proxyRequest(w http.ResponseWriter, r *http.Request, backendID string) error {
	state, ok := p.backends[backendID]
	if !ok {
		err := fmt.Errorf("backend %s not found", backendID)
		logger.Get().Errorw("proxyRequest: backend not found", "backend", backendID)
		return err
	}

	// Проверка совместимости типа бэкенда с текущим OperatingMode
	if !types.IsModeCompatibleWithBackendType(p.config.Balancing.OperatingMode, state.Backend.Type) {
		bt := normalizeBackendType(state.Backend.Type)
		err := fmt.Errorf("backend type %s not compatible with operating mode %s", bt, p.config.Balancing.OperatingMode)
		logger.Get().Errorw("proxyRequest: backend type incompatible with operating mode",
			"backend", backendID, "backend_type", bt, "operating_mode", p.config.Balancing.OperatingMode)
		return err
	}

	p.recordRequest(backendID)
	startTime := time.Now()

	targetURL := p.getBackendBaseURL(state.Backend)

	// Для llama.cpp бэкендов — используем отдельный прокси с трансляцией форматов
	logger.Get().Infow("proxyRequest: checking backend type for llamacpp routing",
		"backend_id", backendID,
		"backend_type", state.Backend.Type,
		"normalized_type", normalizeBackendType(state.Backend.Type),
		"is_llamacpp", p.isLlamaCppBackend(state.Backend),
	)
	if p.isLlamaCppBackend(state.Backend) {
		// Читаем тело запроса для трансляции
		var bodyBuf []byte
		if r.Body != nil {
			var readErr error
			bodyBuf, readErr = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
			if readErr != nil {
				logger.Get().Errorw("proxyRequest: failed to read body for llamacpp translation",
					"backend", backendID, "error", readErr)
			}
		}
		return p.proxyRequestLlamaCpp(w, r, backendID, bodyBuf)
	}

	target, err := url.Parse(targetURL)
	if err != nil {
		err = fmt.Errorf("invalid backend URL: %v", err)
		logger.Get().Errorw("proxyRequest: invalid backend URL", "backend", backendID, "url", targetURL, "error", err)
		return err
	}

	isStreamingRequest := p.isStreamingRequest(r)
	modelFromCtx := ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		modelFromCtx = m
	}

	logger.Get().Debugw("proxyRequest: starting",
		"backend", backendID, "method", r.Method, "path", r.URL.Path,
		"model", modelFromCtx, "is_streaming", isStreamingRequest)

	// Вычисляем эффективный таймаут для этого бэкенда (адаптивный / per-backend / глобальный)
	effectiveTimeout := getEffectiveTimeout(state, p.config.Balancing.RequestTimeout)
	logger.Get().Debugw("proxyRequest: effective timeout",
		"backend", backendID, "timeout_sec", effectiveTimeout, "model", modelFromCtx)

	client := p.client
	if isStreamingRequest {
		client = p.streamingClient
		logger.Get().Debugw("proxyRequest: using streaming client (no timeout)",
			"backend", backendID, "model", modelFromCtx)
	}

	// Читаем тело запроса в буфер, чтобы можно было восстановить r.Body
	// если потребуется повторный запрос (например, при auto-pull retry).
	var bodyBuf []byte
	if r.Body != nil {
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("proxyRequest: failed to read request body",
				"backend", backendID, "error", err)
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
		}
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
	}

	// Создаём контекст с адаптивным таймаутом.
	// Для non-streaming: используем effectiveTimeout (замена глобальному client.Timeout).
	// Для streaming: используем StreamTimeout (если задан) как максимум.
	reqCtx := r.Context()
	if isStreamingRequest {
		streamTimeout := time.Duration(p.config.Balancing.StreamTimeout) * time.Second
		if streamTimeout <= 0 {
			// Без таймаута: полагаемся на heartbeat + proxy_read_timeout nginx.
			streamTimeout = 0
		}
		if streamTimeout > 0 {
			var streamCancel context.CancelFunc
			reqCtx, streamCancel = context.WithTimeout(r.Context(), streamTimeout)
			defer streamCancel()
		}
	} else if effectiveTimeout > 0 {
		// Non-streaming: контекст с адаптивным таймаутом
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(r.Context(), time.Duration(effectiveTimeout)*time.Second)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, r.Method, targetURL+r.URL.String(), r.Body)
	if err != nil {
		err = fmt.Errorf("failed to create request: %v", err)
		logger.Get().Errorw("proxyRequest: failed to create request",
			"backend", backendID, "url", targetURL+r.URL.String(), "error", err)
		return err
	}

	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Пробрасываем реальный IP клиента, а не адрес контейнера Docker
	clientRealIP := p.getClientRealIP(r)
	if existingXFF := r.Header.Get("X-Forwarded-For"); existingXFF != "" {
		req.Header.Set("X-Forwarded-For", existingXFF)
	} else {
		req.Header.Set("X-Forwarded-For", clientRealIP)
	}
	req.Header.Set("X-Real-IP", clientRealIP)
	req.Host = target.Host

	logger.Get().Debugw("proxyRequest: sending request to backend",
		"backend", backendID, "url", targetURL+r.URL.String(),
		"method", r.Method, "model", modelFromCtx,
		"timeout", client.Timeout)

	resp, err := client.Do(req)
	if err != nil {
		// Детальная классификация ошибки для диагностики TransferEncodingError
		errType := determineErrorType(err, reqCtx)

		logger.Get().Errorw("PROXY_BACKEND_REQUEST_FAILED",
			"backend", backendID,
			"url", targetURL+r.URL.String(),
			"model", modelFromCtx,
			"error", err,
			"error_type", errType,
			"is_streaming", isStreamingRequest,
			"context_error", reqCtx.Err(),
			"body_size", len(bodyBuf),
			"elapsed_ms", time.Since(startTime).Milliseconds(),
		)

		// STRATEGIC RETRY для streaming-запросов: одна повторная попытка к тому же бэкенду.
		if isStreamingRequest && errType != "context_deadline_exceeded" && errType != "context_canceled" {
			logger.Get().Warnw("PROXY_STREAMING_RETRY_SAME_BACKEND",
				"backend", backendID,
				"model", modelFromCtx,
				"error_type", errType,
				"retry_delay_ms", 500,
			)
			time.Sleep(p.getStreamingRetryDelay())

			if len(bodyBuf) > 0 {
				r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
			}
			retryCtx, retryCancel := context.WithTimeout(r.Context(), 5*time.Minute)
			defer retryCancel()
			retryReq, retryErr := http.NewRequestWithContext(retryCtx, r.Method, targetURL+r.URL.String(), r.Body)
			if retryErr == nil {
				for key, values := range r.Header {
					for _, value := range values {
						retryReq.Header.Add(key, value)
					}
				}
				retryReq.Host = target.Host

				retryResp, retryDoErr := client.Do(retryReq)
				if retryDoErr == nil {
					logger.Get().Infow("PROXY_STREAMING_RETRY_SUCCESS",
						"backend", backendID, "model", modelFromCtx)
					resp = retryResp
					goto retrySucceeded
				}
				logger.Get().Warnw("PROXY_STREAMING_RETRY_FAILED",
					"backend", backendID, "model", modelFromCtx,
					"retry_error", retryDoErr)
			}
		}

		p.logStreamingError(backendID, err)
		if len(bodyBuf) > 0 {
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
		}
		return fmt.Errorf("backend error [%s]: %v", errType, err)
	}
retrySucceeded:

	logger.Get().Debugw("proxyRequest: backend responded",
		"backend", backendID, "status", resp.StatusCode,
		"model", modelFromCtx, "content_type", resp.Header.Get("Content-Type"),
		"elapsed_ms", time.Since(startTime).Milliseconds())

	atomic.AddInt64(&p.totalRequests, 1)
	atomic.AddInt64(&state.TotalRequests, 1)

	// Копируем заголовки ответа бэкенда, исключаем:
	// - Transfer-Encoding, Content-Length, Connection (управляются Go/http)
	// - CORS-заголовки (уже установлены в ServeHTTP, дублирование ломает браузеры)
	// - Vary: Origin (связан с CORS, тоже исключаем)
	for key, values := range resp.Header {
		keyLower := strings.ToLower(key)
		if keyLower == "transfer-encoding" || keyLower == "content-length" || keyLower == "connection" {
			continue
		}
		if strings.HasPrefix(keyLower, "access-control-") {
			continue
		}
		if keyLower == "vary" {
			for _, value := range values {
				if strings.ToLower(strings.TrimSpace(value)) == "origin" {
					continue
				}
				w.Header().Add(key, value)
			}
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Сессии создаются только для реальных клиентских запросов chat/generate.
	path := r.URL.Path
	isClientRequest := (path == "/api/generate" || path == "/api/chat")
	if isClientRequest {
		clientNameForSession := p.getClientName(r)
		modelFromCtx := ""
		if m, ok := r.Context().Value(modelContextKey).(string); ok {
			modelFromCtx = m
		}
		sessionID := p.getSessionIDWithModel(r, clientNameForSession, modelFromCtx)
		if sessionID != "" {
			p.sessionMgr.Set(sessionID, backendID, modelFromCtx, clientNameForSession, p.getClientRealIP(r), r.UserAgent())
			w.Header().Set("X-Session-ID", sessionID)
		}
		w.Header().Set("X-Backend-ID", backendID)
	}

	isStreaming := p.isStreamingResponse(resp)

	// Обработка ошибки "model not found"
	if !isStreaming && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest) {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr == nil && isModelNotFoundError(bodyBytes) {
			modelFromCtx := ""
			if m, ok := r.Context().Value(modelContextKey).(string); ok {
				modelFromCtx = m
			}
			logger.Get().Warnw("model not found on backend, auto-pull may be triggered",
				"backend", backendID, "model", modelFromCtx,
				"status", resp.StatusCode, "ollama_error", string(bodyBytes))
			if len(bodyBuf) > 0 {
				r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
			}
			return &ModelNotFoundError{
				BackendID: backendID,
				Model:     modelFromCtx,
				Status:    resp.StatusCode,
				Body:      bodyBytes,
			}
		}
		if readErr == nil {
			w.WriteHeader(resp.StatusCode)
			w.Write(bodyBytes)
			return nil
		}
		return p.proxyNonStreamingResponse(w, r, resp, backendID)
	}

	// Обработка 503 Service Unavailable от Ollama
	if resp.StatusCode == http.StatusServiceUnavailable {
		modelFromCtx := ""
		if m, ok := r.Context().Value(modelContextKey).(string); ok {
			modelFromCtx = m
		}
		if isStreaming {
			logger.Get().Warnw("backend returned 503 for streaming request, will retry on alternate backend",
				"backend", backendID, "model", modelFromCtx)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return &BackendBusyError{
				BackendID: backendID,
				Model:     modelFromCtx,
			}
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusServiceUnavailable)
		if len(bodyBytes) > 0 {
			w.Write(bodyBytes)
		} else {
			w.Write([]byte(`{"error":"backend temporarily unavailable"}`))
		}
		return nil
	}

	if isStreaming {
		p.handleStreamingResponse(w, r, resp, backendID)
	} else {
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			logger.Get().Errorw("proxyRequest: failed to read non-streaming body",
				"backend", backendID, "error", readErr)
			if len(body) > 0 {
				w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
				w.WriteHeader(resp.StatusCode)
				w.Write(body)
			} else {
				w.WriteHeader(http.StatusBadGateway)
				w.Write([]byte(`{"error":"backend response incomplete"}`))
			}
			return fmt.Errorf("failed to read response body: %w", readErr)
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	}

	// Record for background controllers
	modelFromCtx = ""
	if m, ok := r.Context().Value(modelContextKey).(string); ok {
		modelFromCtx = m
	}
	latencyMs := time.Since(startTime).Milliseconds()
	success := resp.StatusCode < 500
	if p.unloadScheduler != nil {
		p.unloadScheduler.RecordModelUse(modelFromCtx, backendID)
	}
	if p.weightTuner != nil {
		p.weightTuner.RecordOutcome(backendID, modelFromCtx, latencyMs, success)
	}

	// Записываем latency для адаптивного таймаута
	recordLatency(state, latencyMs, modelFromCtx, success)

	return nil
}

// proxyNonStreamingResponse — проксирует тело ответа как есть (без streaming).
func (p *Proxy) proxyNonStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	w.WriteHeader(resp.StatusCode)
	w.Write(body)
	return nil
}

// ModelNotFoundError — кастомная ошибка, сигнализирующая что модели нет на бэкенде
type ModelNotFoundError struct {
	BackendID string
	Model     string
	Status    int
	Body      []byte
}

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("model '%s' not found on backend %s (HTTP %d): %s",
		e.Model, e.BackendID, e.Status, string(e.Body))
}

// BackendBusyError — кастомная ошибка, сигнализирующая что бэкенд перегружен (503)
type BackendBusyError struct {
	BackendID string
	Model     string
}

func (e *BackendBusyError) Error() string {
	return fmt.Sprintf("backend '%s' is busy (503) for model '%s'", e.BackendID, e.Model)
}

// isStreamingRequest - проверка, является ли запрос streaming запросом
func (p *Proxy) isStreamingRequest(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}

	if isStream, ok := r.Context().Value(streamContextKey).(bool); ok {
		return isStream
	}

	if r.Body == nil {
		return false
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}

	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		return false
	}

	if stream, ok := req["stream"].(bool); ok {
		return stream
	}

	return true
}

// isStreamingResponse - проверка на streaming ответ (SSE, NDJSON или chunked)
func (p *Proxy) isStreamingResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	// Ollama возвращает Content-Type: application/x-ndjson для streaming
	// /api/generate и /api/chat. Если не распознать это как стриминг,
	// балансер прочитает всё тело через ReadAll и установит Content-Length,
	// что сломает длинные стриминговые ответы.
	if strings.Contains(contentType, "application/x-ndjson") {
		return true
	}

	if resp.Header.Get("Transfer-Encoding") == "chunked" {
		return true
	}

	// Go's http.Response.TransferEncoding — это распарсенный массив из Transfer-Encoding header.
	// Если бэкенд вернул "Transfer-Encoding: chunked", Go помещает его сюда.
	if len(resp.TransferEncoding) > 0 && resp.TransferEncoding[0] == "chunked" {
		return true
	}

	if resp.ContentLength == -1 {
		return true
	}

	if resp.Header.Get("X-Accel-Buffering") == "no" {
		return true
	}

	return false
}

// recordRequest - записывает таймстемп запроса для расчёта RPS
func (p *Proxy) recordRequest(backendID string) {
	p.mu.RLock()
	state, ok := p.backends[backendID]
	p.mu.RUnlock()
	if !ok {
		return
	}

	now := time.Now()
	state.mu.Lock()
	state.RequestHistory = append(state.RequestHistory, now)
	cutoff := now.Add(-60 * time.Second)
	var startIdx int
	for i, t := range state.RequestHistory {
		if t.After(cutoff) {
			startIdx = i
			break
		}
	}
	if startIdx > 0 {
		state.RequestHistory = state.RequestHistory[startIdx:]
	}
	state.CalculatedRPS = float64(len(state.RequestHistory)) / 60.0
	state.mu.Unlock()
}

// warmupModel — загрузка модели в VRAM:
// для Ollama: POST /api/generate с пустым промптом,
// для llama.cpp: POST /load или аналогичный warmup вызов.
func (p *Proxy) warmupModel(backendID, host string, port int, model string) {
	if p.client == nil {
		logger.Get().Warnw("warmupModel: no HTTP client configured", "backend", backendID, "model", model)
		return
	}

	backend := p.GetBackend(backendID)
	if backend == nil {
		logger.Get().Warnw("warmupModel: backend not found", "backend", backendID, "model", model)
		return
	}

	engine := types.ResolveEngine(backend.Engine, backend.Type)

	// Шаг 1: проверяем наличие модели на бэкенде
	switch engine {
	case types.EngineLlamaCPP:
		p.warmupLlamaCppModel(backendID, host, port, model)
	default:
		p.warmupOllamaModel(backendID, host, port, model)
	}
}

// warmupOllamaModel — загрузка модели через Ollama API.
func (p *Proxy) warmupOllamaModel(backendID, host string, port int, model string) {
	tagsURL := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	tagsResp, err := p.client.Get(tagsURL)
	if err != nil {
		logger.Get().Warnw("warmupOllamaModel: cannot reach backend tags endpoint, skipping warmup",
			"backend", backendID, "model", model, "error", err)
		return
	}
	var tagsData struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(tagsResp.Body).Decode(&tagsData); err != nil {
		tagsResp.Body.Close()
		logger.Get().Warnw("warmupOllamaModel: failed to decode tags, skipping warmup",
			"backend", backendID, "model", model, "error", err)
		return
	}
	tagsResp.Body.Close()

	modelExists := false
	for _, m := range tagsData.Models {
		if m.Name == model || strings.Contains(m.Name, model) {
			modelExists = true
			break
		}
	}
	if !modelExists {
		logger.Get().Warnw("warmupOllamaModel: model not found in backend tags, skipping warmup (Ollama will pull on first request)",
			"backend", backendID, "model", model)
		return
	}

	genURL := fmt.Sprintf("http://%s:%d/api/generate", host, port)
	body := fmt.Sprintf(`{"model":"%s","prompt":"","stream":false,"keep_alive":"5m"}`, model)

	go func() {
		select {
		case p.warmupSem <- struct{}{}:
			defer func() { <-p.warmupSem }()
		case <-time.After(p.getWarmupSemaphoreTimeout()):
			logger.Get().Warnw("warmupOllamaModel: semaphore timeout, skipping warmup",
				"backend", backendID, "model", model)
			return
		}

		req, err := http.NewRequest("POST", genURL, strings.NewReader(body))
		if err != nil {
			logger.Get().Errorw("warmupOllamaModel: request creation failed", "backend", backendID, "model", model, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			logger.Get().Errorw("warmupOllamaModel: request failed", "backend", backendID, "model", model, "error", err)
			return
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			logger.Get().Infow("warmupOllamaModel: model loaded into VRAM",
				"backend", backendID, "model", model, "status", resp.StatusCode)
			p.updateRunningModelInMetrics(backendID, model)
		} else {
			logger.Get().Warnw("warmupOllamaModel: model load returned non-2xx",
				"backend", backendID, "model", model, "status", resp.StatusCode)
		}
	}()
}

// warmupLlamaCppModel — загрузка модели через llama.cpp backend.
func (p *Proxy) warmupLlamaCppModel(backendID, host string, port int, model string) {
	// Для llama.cpp используем CppWorkerPort из конфигурации бэкенда,
	// а не переданный port (который может быть OllamaPort=0).
	backend := p.GetBackend(backendID)
	if backend == nil {
		logger.Get().Warnw("warmupLlamaCppModel: backend not found", "backend", backendID, "model", model)
		return
	}
	actualPort := p.getBackendPort(backend)
	baseURL := fmt.Sprintf("http://%s:%d", host, actualPort)

	// Проверяем доступность бэкенда через health endpoint
	healthURL := baseURL + "/health"
	healthResp, err := p.client.Get(healthURL)
	if err != nil {
		logger.Get().Warnw("warmupLlamaCppModel: cannot reach backend health endpoint, skipping warmup",
			"backend", backendID, "model", model, "error", err)
		return
	}
	healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		logger.Get().Warnw("warmupLlamaCppModel: backend not healthy, skipping warmup",
			"backend", backendID, "model", model, "status", healthResp.StatusCode)
		return
	}

	// Загружаем модель через /load endpoint cppworker
	loadURL := baseURL + "/load"
	loadBody := map[string]interface{}{
		"name": model,
	}
	loadBodyBytes, _ := json.Marshal(loadBody)

	go func() {
		select {
		case p.warmupSem <- struct{}{}:
			defer func() { <-p.warmupSem }()
		case <-time.After(p.getWarmupSemaphoreTimeout()):
			logger.Get().Warnw("warmupLlamaCppModel: semaphore timeout, skipping warmup",
				"backend", backendID, "model", model)
			return
		}

		req, err := http.NewRequest("POST", loadURL, bytes.NewReader(loadBodyBytes))
		if err != nil {
			logger.Get().Errorw("warmupLlamaCppModel: request creation failed", "backend", backendID, "model", model, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			logger.Get().Errorw("warmupLlamaCppModel: load request failed", "backend", backendID, "model", model, "error", err)
			return
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			logger.Get().Infow("warmupLlamaCppModel: model loaded into VRAM",
				"backend", backendID, "model", model, "status", resp.StatusCode)
			p.updateLlamaCppRunningModelInMetrics(backendID, model)
		} else {
			logger.Get().Warnw("warmupLlamaCppModel: model load returned non-2xx",
				"backend", backendID, "model", model, "status", resp.StatusCode)
		}
	}()
}

// updateRunningModelInMetrics — обновляет RunningModels в метриках бэкенда
// после успешного warmup, чтобы не ждать следующего heartbeat от агента.
func (p *Proxy) updateRunningModelInMetrics(backendID, model string) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()

	metrics, ok := p.metricsMgr.metrics[backendID]
	if !ok {
		metrics = &types.BackendMetrics{
			ID: backendID,
			Ollama: types.OllamaMetrics{
				RunningModels: []types.RunningModel{},
			},
		}
		p.metricsMgr.metrics[backendID] = metrics
	}

	for _, m := range metrics.Ollama.RunningModels {
		if m.Name == model {
			return
		}
	}

	metrics.Ollama.RunningModels = append(metrics.Ollama.RunningModels, types.RunningModel{
		Name: model,
	})
	logger.Get().Debugw("updateRunningModelInMetrics: added model to running models",
		"backend", backendID, "model", model)
}

// updateLlamaCppRunningModelInMetrics — обновляет LlamaCppMetrics после успешного warmup.
func (p *Proxy) updateLlamaCppRunningModelInMetrics(backendID, model string) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()

	lm, ok := p.metricsMgr.llamaMetrics[backendID]
	if !ok {
		lm = &types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{},
		}
		p.metricsMgr.llamaMetrics[backendID] = lm
	}

	for _, m := range lm.LoadedModels {
		if m.Name == model {
			return
		}
	}

	lm.LoadedModels = append(lm.LoadedModels, types.LlamaCppModel{
		Name:  model,
		State: "loaded",
	})
	logger.Get().Debugw("updateLlamaCppRunningModelInMetrics: added model to llama.cpp metrics",
		"backend", backendID, "model", model)
}
