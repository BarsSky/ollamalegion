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

	p.recordRequest(backendID)
	startTime := time.Now()

	targetURL := fmt.Sprintf("http://%s:%d", state.Backend.Host, state.Backend.OllamaPort)

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

	client := p.client
	if isStreamingRequest {
		client = p.streamingClient
		logger.Get().Debugw("proxyRequest: using streaming client (no timeout)",
			"backend", backendID, "model", modelFromCtx)
	}

	// Читаем тело запроса в буфер, чтобы можно было восстановить r.Body
	// если потребуется повторный запрос (например, при auto-pull retry).
	// Go http.Client.Do() читает и закрывает r.Body, что делает его непригодным
	// для повторного использования.
	var bodyBuf []byte
	if r.Body != nil {
		bodyBuf, err = io.ReadAll(r.Body)
		if err != nil {
			logger.Get().Errorw("proxyRequest: failed to read request body",
				"backend", backendID, "error", err)
			// Если не удалось прочитать тело — используем оригинальное поведение
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
		}
		r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
	}

	// Для streaming запросов создаём контекст с таймаутом, чтобы предотвратить
	// бесконечное зависание если Ollama на бэкенде перестала генерировать токены.
	reqCtx := r.Context()
	if isStreamingRequest {
		streamTimeout := time.Duration(p.config.Balancing.StreamTimeout) * time.Second
		if streamTimeout <= 0 {
			// Без таймаута: полагаемся на heartbeat + proxy_read_timeout nginx.
			// Жёсткий таймаут (ранее 10 минут) обрывал длинные генерации и вызывал
			// TransferEncodingError у OpenWebUI.
			streamTimeout = 0
		}
		if streamTimeout > 0 {
			var streamCancel context.CancelFunc
			reqCtx, streamCancel = context.WithTimeout(r.Context(), streamTimeout)
			defer streamCancel()
		}
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
		// Ollama может кратковременно перезагружаться (OOM killer → restart за ~500ms),
		// и retry предотвращает UND_ERR_SOCKET/TransferEncodingError у OpenWebUI.
		if isStreamingRequest && errType != "context_deadline_exceeded" && errType != "context_canceled" {
			logger.Get().Warnw("PROXY_STREAMING_RETRY_SAME_BACKEND",
				"backend", backendID,
				"model", modelFromCtx,
				"error_type", errType,
				"retry_delay_ms", 500,
			)
			time.Sleep(p.getStreamingRetryDelay())

			// Восстанавливаем тело запроса
			if len(bodyBuf) > 0 {
				r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
			}
			// Создаём новый контекст с увеличенным таймаутом для retry
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
		// Восстанавливаем тело r.Body перед возвратом, чтобы вызывающий код
		// мог повторить запрос (например, при auto-pull retry)
		if len(bodyBuf) > 0 {
			r.Body = io.NopCloser(bytes.NewBuffer(bodyBuf))
		}
		return fmt.Errorf("backend error [%s]: %v", errType, err)
	}
retrySucceeded:
	// НЕ делаем defer resp.Body.Close() здесь, т.к. handleStreamingResponse
	// сама управляет закрытием resp.Body для корректного drain.
	// Non-streaming пути закрывают body явно.

	logger.Get().Debugw("proxyRequest: backend responded",
		"backend", backendID, "status", resp.StatusCode,
		"model", modelFromCtx, "content_type", resp.Header.Get("Content-Type"),
		"elapsed_ms", time.Since(startTime).Milliseconds())

	atomic.AddInt64(&p.totalRequests, 1)
	atomic.AddInt64(&state.TotalRequests, 1)

	// Копируем заголовки ответа бэкенда, НО исключаем Transfer-Encoding и Content-Length,
	// так как Go's http.ResponseWriter управляет этими заголовками автоматически.
	for key, values := range resp.Header {
		keyLower := strings.ToLower(key)
		if keyLower == "transfer-encoding" || keyLower == "content-length" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}

	// Сессии создаются только для реальных клиентских запросов chat/generate.
	// Embeddings (/api/embed, /api/embeddings) исключаются из session stickiness.
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
	}

	isStreaming := p.isStreamingResponse(resp)

	// ##### НОВОЕ: Обработка ошибки "model not found" #####
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

	// ##### Обработка 503 Service Unavailable от Ollama #####
	// Ollama может вернуть 503 когда перегружена или модель ещё загружается.
	// Для streaming-запросов: сигнализируем ошибку вызывающему коду для retry на другом бэкенде.
	// Для non-streaming: возвращаем 503 клиенту с Retry-After заголовком.
	if resp.StatusCode == http.StatusServiceUnavailable {
		modelFromCtx := ""
		if m, ok := r.Context().Value(modelContextKey).(string); ok {
			modelFromCtx = m
		}
		if isStreaming {
			logger.Get().Warnw("backend returned 503 for streaming request, will retry on alternate backend",
				"backend", backendID, "model", modelFromCtx)
			// Drain body и закрываем
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			// Возвращаем ошибку — ServeHTTP попробует другой бэкенд через retry-loop
			return &BackendBusyError{
				BackendID: backendID,
				Model:     modelFromCtx,
			}
		}
		// Non-streaming: читаем тело ошибки и отправляем клиенту
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

	// ##### КОНЕЦ НОВОГО #####

	if isStreaming {
		p.handleStreamingResponse(w, r, resp, backendID)
	} else {
		// Non-streaming: читаем всё тело и устанавливаем Content-Length,
		// чтобы предотвратить chunked encoding при передаче клиенту
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			logger.Get().Errorw("proxyRequest: failed to read non-streaming body",
				"backend", backendID, "error", readErr)
			// Даже при ошибке чтения пытаемся отправить то, что получили
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

	return nil
}

// proxyNonStreamingResponse — проксирует тело ответа как есть (без streaming).
// Важно: устанавливает Content-Length явно, чтобы избежать chunked encoding
// и связанных с ним ошибок TransferEncodingError.
func (p *Proxy) proxyNonStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) error {
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	// Устанавливаем Content-Length, чтобы Go использовал identity encoding вместо chunked
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
// и нужно попробовать другой бэкенд.
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

// isStreamingResponse - проверка на streaming ответ (SSE или chunked)
func (p *Proxy) isStreamingResponse(resp *http.Response) bool {
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		return true
	}

	if resp.Header.Get("Transfer-Encoding") == "chunked" {
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

// warmupModel — загрузка модели в VRAM на Ollama через POST /api/generate с пустым промптом.
// Использует /api/generate вместо /api/pull, т.к. /api/pull скачивает модель с интернета/кеша
// (может занять минуты), а /api/generate с пустым промптом загружает модель в VRAM за секунды.
// Если модель не скачана на бэкенде — возвращает ошибку, и запрос должен быть обработан
// через fallback (Ollama загрузит модель сама при обработке запроса).
func (p *Proxy) warmupModel(backendID, host string, port int, model string) {
	if p.client == nil {
		logger.Get().Warnw("warmupModel: no HTTP client configured", "backend", backendID, "model", model)
		return
	}

	// Быстрая проверка: модель скачана на бэкенде? Делаем HEAD-подобный запрос к /api/tags.
	// Если модели нет — не пытаемся /api/generate, т.к. Ollama вернёт 404.
	tagsURL := fmt.Sprintf("http://%s:%d/api/tags", host, port)
	tagsResp, err := p.client.Get(tagsURL)
	if err != nil {
		logger.Get().Warnw("warmupModel: cannot reach backend tags endpoint, skipping warmup",
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
		logger.Get().Warnw("warmupModel: failed to decode tags, skipping warmup",
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
		logger.Get().Warnw("warmupModel: model not found in backend tags, skipping warmup (Ollama will pull on first request)",
			"backend", backendID, "model", model)
		return
	}

	// Модель скачана — загружаем в VRAM через /api/generate с пустым промптом
	genURL := fmt.Sprintf("http://%s:%d/api/generate", host, port)
	body := fmt.Sprintf(`{"model":"%s","prompt":"","stream":false,"keep_alive":"5m"}`, model)

	go func() {
		// Семафор: ограничиваем число одновременных warmup
		// (каждый warmup делает POST /api/generate, блокирующий слот Ollama)
		select {
		case p.warmupSem <- struct{}{}:
			defer func() { <-p.warmupSem }()
		case <-time.After(p.getWarmupSemaphoreTimeout()):
			// Семафор занят > таймаут — другой warmup завис, пропускаем
			logger.Get().Warnw("warmupModel: semaphore timeout, skipping warmup",
				"backend", backendID, "model", model)
			return
		}

		req, err := http.NewRequest("POST", genURL, strings.NewReader(body))
		if err != nil {
			logger.Get().Errorw("warmupModel: request creation failed", "backend", backendID, "model", model, "error", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.client.Do(req)
		if err != nil {
			logger.Get().Errorw("warmupModel: request failed", "backend", backendID, "model", model, "error", err)
			return
		}
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			logger.Get().Infow("warmupModel: model loaded into VRAM",
				"backend", backendID, "model", model, "status", resp.StatusCode)
			// Сразу обновляем локальные метрики, чтобы checkModelReadyUnsafe увидел модель
			p.updateRunningModelInMetrics(backendID, model)
		} else {
			logger.Get().Warnw("warmupModel: model load returned non-2xx",
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

	// Проверяем, нет ли уже такой модели
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
