package balancer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// OllamaErrorResponse — структура ошибки от Ollama API
type OllamaErrorResponse struct {
	Error string `json:"error"`
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
		return fmt.Errorf("backend %s not found", backendID)
	}

	p.recordRequest(backendID)
	startTime := time.Now()

	targetURL := fmt.Sprintf("http://%s:%d", state.Backend.Host, state.Backend.OllamaPort)

	target, err := url.Parse(targetURL)
	if err != nil {
		return fmt.Errorf("invalid backend URL: %v", err)
	}

	isStreamingRequest := p.isStreamingRequest(r)

	client := p.client
	if isStreamingRequest {
		client = p.streamingClient
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL+r.URL.String(), r.Body)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
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

	resp, err := client.Do(req)
	if err != nil {
		p.logStreamingError(backendID, err)
		return fmt.Errorf("backend error: %v", err)
	}
	defer resp.Body.Close()

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

	// Сессии создаются только для реальных клиентских запросов chat/generate/embeddings.
	path := r.URL.Path
	isClientRequest := (path == "/api/generate" || path == "/api/chat" || path == "/api/embeddings")
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
	// Если это не streaming, и ответ от Ollama — это ошибка "model not found",
	// мы прерываем проксирование и сигнализируем вызывающему коду (ServeHTTP),
	// что нужно запустить auto-pull и повторить запрос.
	if !isStreaming && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest) {
		// Читаем тело ответа для анализа
		bodyBytes, readErr := io.ReadAll(resp.Body)
		if readErr == nil && isModelNotFoundError(bodyBytes) {
			modelFromCtx := ""
			if m, ok := r.Context().Value(modelContextKey).(string); ok {
				modelFromCtx = m
			}
			logger.Get().Warnw("model not found on backend, auto-pull may be triggered",
				"backend", backendID, "model", modelFromCtx,
				"status", resp.StatusCode, "ollama_error", string(bodyBytes))
			return &ModelNotFoundError{
				BackendID: backendID,
				Model:     modelFromCtx,
				Status:    resp.StatusCode,
				Body:      bodyBytes,
			}
		}
		// Если это не model-not-found, но ошибка — восстанавливаем тело
		// Не streaming, используем ReadAll и копируем
		if readErr == nil {
			w.WriteHeader(resp.StatusCode)
			w.Write(bodyBytes)
			return nil
		}
		// Если не удалось прочитать тело — проксируем как обычно
		return p.proxyNonStreamingResponse(w, r, resp, backendID)
	}

	// ##### КОНЕЦ НОВОГО #####

	if isStreaming {
		p.handleStreamingResponse(w, r, resp, backendID)
	} else {
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}

	// Record for background controllers
	modelFromCtx := ""
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

// proxyNonStreamingResponse — проксирует тело ответа как есть (без streaming)
func (p *Proxy) proxyNonStreamingResponse(w http.ResponseWriter, r *http.Request, resp *http.Response, backendID string) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
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

// warmupModel — загрузка модели на Ollama через POST /api/pull (асинхронно)
func (p *Proxy) warmupModel(backendID, host string, port int, model string) {
	url := fmt.Sprintf("http://%s:%d/api/pull", host, port)
	body := fmt.Sprintf(`{"name":"%s","stream":false}`, model)
	go func() {
		req, err := http.NewRequest("POST", url, strings.NewReader(body))
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
		logger.Get().Infow("warmupModel: model loaded", "backend", backendID, "model", model, "status", resp.StatusCode)
	}()
}
