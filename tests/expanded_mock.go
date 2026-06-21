package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/types"
)

// MockBehavior — конфигурация поведения мок-сервера для разных сценариев
type MockBehavior struct {
	// ModelLoadDelay — задержка перед отправкой первого чанка (симуляция загрузки модели)
	ModelLoadDelay time.Duration

	// ChunkDelay — задержка между отправкой чанков (симуляция медленного токена)
	ChunkDelay time.Duration

	// DropAfterChunks — оборвать соединение после N отправленных чанков (0 = не обрывать)
	DropAfterChunks int

	// DropOnStream — если true, обрывает соединение при streaming запросе
	DropOnStream bool

	// ErrorStatusCode — если >0, все запросы возвращают эту ошибку (симуляция отказа бэкенда)
	ErrorStatusCode int

	// StreamResponseSize — количество чанков в streaming ответе
	StreamResponseSize int

	// ResponseDelay — задержка перед отправкой HTTP заголовков ответа
	ResponseDelay time.Duration

	// RateLimitAfter — после N запросов начать возвращать 429/503
	RateLimitAfter int

	// RateLimitStatus — HTTP статус для rate limit
	RateLimitStatus int
}

// DefaultMockBehavior — поведение по умолчанию (как нормальный Ollama)
var DefaultMockBehavior = MockBehavior{
	ModelLoadDelay:     0,
	ChunkDelay:         10 * time.Millisecond,
	DropAfterChunks:    0,
	DropOnStream:       false,
	ErrorStatusCode:    0,
	StreamResponseSize: 3,
	ResponseDelay:      0,
	RateLimitAfter:     0,
	RateLimitStatus:    http.StatusServiceUnavailable,
}

// ExpandedMockServer — расширенный мок-сервер Ollama с настраиваемым поведением
type ExpandedMockServer struct {
	server *httptest.Server
	mu     sync.Mutex

	// Конфигурация поведения
	behavior MockBehavior

	// Счётчики запросов (атомарные + по эндпоинтам)
	GenerateCount int64
	ChatCount     int64
	EmbedCount    int64
	TagsCount     int64
	PsCount       int64
	VersionCount  int64
	ShowCount     int64
	CreateCount   int64
	PullCount     int64
	DeleteCount   int64
	CopyCount     int64
	PushCount     int64

	// Running models (для /api/ps)
	runningModels []types.RunningModel

	// Захваченные тела запросов для верификации
	lastRequestBody []byte

	// Канал для синхронизации: сигнал, что запрос получен
	requestReceived chan struct{}

	// ID бэкенда (для логирования)
	BackendID string
}

// NewExpandedMockServer — создание нового расширенного мок-сервера
func NewExpandedMockServer(behavior MockBehavior) *ExpandedMockServer {
	mock := &ExpandedMockServer{
		behavior:        behavior,
		requestReceived: make(chan struct{}, 100),
		BackendID:       "expanded-mock",
	}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mock.handleRequest(w, r)
	}))
	return mock
}

// NewDefaultMockServer — создание мок-сервера с поведением по умолчанию
func NewDefaultMockServer() *ExpandedMockServer {
	return NewExpandedMockServer(DefaultMockBehavior)
}

// SetBehavior — динамическое изменение поведения мок-сервера
func (m *ExpandedMockServer) SetBehavior(behavior MockBehavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.behavior = behavior
}

// SetRunningModels — установка списка запущенных моделей
func (m *ExpandedMockServer) SetRunningModels(models []types.RunningModel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runningModels = models
}

// URL — возвращает URL сервера
func (m *ExpandedMockServer) URL() string {
	return m.server.URL
}

// Close — закрытие сервера
func (m *ExpandedMockServer) Close() {
	m.server.Close()
}

// HostPort — парсинг host:port из URL
func (m *ExpandedMockServer) HostPort() (string, int) {
	url := m.server.URL
	url = strings.TrimPrefix(url, "http://")
	parts := strings.Split(url, ":")
	if len(parts) != 2 {
		return "localhost", 11434
	}
	port := 0
	fmt.Sscanf(parts[1], "%d", &port)
	if port == 0 {
		return "localhost", 11434
	}
	return parts[0], port
}

// LastRequestBody — тело последнего запроса (для верификации)
func (m *ExpandedMockServer) LastRequestBody() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastRequestBody
}

// WaitForRequest — ожидание получения запроса (с таймаутом)
func (m *ExpandedMockServer) WaitForRequest(timeout time.Duration) bool {
	select {
	case <-m.requestReceived:
		return true
	case <-time.After(timeout):
		return false
	}
}

// getBehavior — потокобезопасное получение текущего поведения
func (m *ExpandedMockServer) getBehavior() MockBehavior {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.behavior
}

// handleRequest — диспетчеризация запросов по эндпоинтам
func (m *ExpandedMockServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	// Сигнализируем о получении запроса
	select {
	case m.requestReceived <- struct{}{}:
	default:
	}

	// Захватываем тело запроса
	if r.Body != nil {
		bodyBytes, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.lastRequestBody = bodyBytes
		m.mu.Unlock()
		r.Body = io.NopCloser(newBodyReader(bodyBytes))
	}

	switch r.URL.Path {
	case "/api/generate":
		atomic.AddInt64(&m.GenerateCount, 1)
		m.handleGenerate(w, r)
	case "/api/chat":
		atomic.AddInt64(&m.ChatCount, 1)
		m.handleChat(w, r)
	case "/api/embeddings":
		atomic.AddInt64(&m.EmbedCount, 1)
		m.handleEmbeddings(w, r)
	case "/api/tags":
		atomic.AddInt64(&m.TagsCount, 1)
		m.handleTags(w, r)
	case "/api/ps":
		atomic.AddInt64(&m.PsCount, 1)
		m.handlePs(w, r)
	case "/api/version":
		atomic.AddInt64(&m.VersionCount, 1)
		m.handleVersion(w, r)
	case "/api/show":
		atomic.AddInt64(&m.ShowCount, 1)
		m.handleShow(w, r)
	case "/api/create":
		atomic.AddInt64(&m.CreateCount, 1)
		m.handleCreate(w, r)
	case "/api/pull":
		atomic.AddInt64(&m.PullCount, 1)
		m.handlePull(w, r)
	case "/api/delete":
		atomic.AddInt64(&m.DeleteCount, 1)
		m.handleDelete(w, r)
	case "/api/copy":
		atomic.AddInt64(&m.CopyCount, 1)
		m.handleCopy(w, r)
	case "/api/push":
		atomic.AddInt64(&m.PushCount, 1)
		m.handlePush(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}
}

// checkRateLimit — проверка лимита запросов (возвращает true если превышен)
func (m *ExpandedMockServer) checkRateLimit(w http.ResponseWriter, currentCount int64) bool {
	b := m.getBehavior()
	if b.RateLimitAfter > 0 && int(currentCount) >= b.RateLimitAfter {
		status := b.RateLimitStatus
		if status <= 0 {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("rate limited after %d requests", b.RateLimitAfter),
		})
		return true
	}
	return false
}

// applyResponseDelay — задержка перед отправкой заголовков
func (m *ExpandedMockServer) applyResponseDelay() {
	b := m.getBehavior()
	if b.ResponseDelay > 0 {
		time.Sleep(b.ResponseDelay)
	}
}

// handleGenerate — обработка /api/generate
func (m *ExpandedMockServer) handleGenerate(w http.ResponseWriter, r *http.Request) {
	b := m.getBehavior()

	// Проверка rate limit
	if m.checkRateLimit(w, atomic.LoadInt64(&m.GenerateCount)) {
		return
	}

	// Ошибка, если задана
	if b.ErrorStatusCode > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(b.ErrorStatusCode)
		json.NewEncoder(w).Encode(map[string]string{"error": "simulated backend error"})
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	stream := true
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}

	model := ""
	if m, ok := req["model"].(string); ok {
		model = m
	}

	m.applyResponseDelay()

	if stream {
		m.handleGenerateStreaming(w, model)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model":           model,
			"response":        "Hello world!",
			"done":            true,
			"total_duration":  1234567890,
			"load_duration":   int64(b.ModelLoadDelay.Milliseconds()),
		})
	}
}

// handleGenerateStreaming — streaming ответ с настраиваемым поведением
func (m *ExpandedMockServer) handleGenerateStreaming(w http.ResponseWriter, model string) {
	b := m.getBehavior()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	// Симуляция задержки загрузки модели
	if b.ModelLoadDelay > 0 {
		// Отправляем прогресс загрузки
		progressData, _ := json.Marshal(map[string]interface{}{
			"status": "loading model",
			"model":  model,
		})
		fmt.Fprintf(w, "data: %s\n\n", progressData)
		flusher.Flush()
		time.Sleep(b.ModelLoadDelay)
	}

	// Количество чанков для отправки
	numChunks := b.StreamResponseSize
	if numChunks <= 0 {
		numChunks = 3
	}

	tokens := []string{"Hello", " expanded", " mock", " server", "!"}
	doneSent := false

	for i := 0; i < numChunks && i < len(tokens); i++ {
		// Проверка: не нужно ли оборвать соединение после N чанков
		if b.DropAfterChunks > 0 && i >= b.DropAfterChunks {
			if b.DropOnStream {
				// Обрываем соединение (закрываем без уведомления)
				if hijacker, ok := w.(http.Hijacker); ok {
					conn, _, _ := hijacker.Hijack()
					conn.Close()
					return
				}
			}
			return
		}

		isLast := (i == numChunks-1) || (b.DropAfterChunks > 0 && i == b.DropAfterChunks-1)
		if isLast && b.DropOnStream && b.DropAfterChunks == 0 {
			// Если drop включён, но DropAfterChunks=0 — обрываем на последнем
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, _, _ := hijacker.Hijack()
				conn.Close()
				return
			}
		}

		event := map[string]interface{}{
			"model":    model,
			"response": tokens[i],
			"done":     isLast && !b.DropOnStream,
		}
		if isLast && !b.DropOnStream {
			event["total_duration"] = 1234567890
			doneSent = true
		}

		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		// Задержка между чанками
		if b.ChunkDelay > 0 && i < numChunks-1 {
			time.Sleep(b.ChunkDelay)
		}
	}

	// Если не отправили done — отправляем принудительно (для завершения потока)
	if !doneSent && !b.DropOnStream {
		doneEvent, _ := json.Marshal(map[string]interface{}{
			"model":          model,
			"response":       "",
			"done":           true,
			"total_duration": 1234567890,
		})
		fmt.Fprintf(w, "data: %s\n\n", doneEvent)
		flusher.Flush()
	}
}

// hasToolRoleMessages проверяет, есть ли сообщения с role="tool" в массиве messages.
func hasToolRoleMessages(messages []interface{}) bool {
	for _, msg := range messages {
		if m, ok := msg.(map[string]interface{}); ok {
			if role, ok := m["role"].(string); ok && role == "tool" {
				return true
			}
		}
	}
	return false
}

// buildMockToolCalls создаёт фиктивные tool_calls для тестирования.
func buildMockToolCalls() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"id":   "call_mock_search",
			"type": "function",
			"function": map[string]interface{}{
				"name":      "search",
				"arguments": `{"q":"test query"}`,
			},
		},
		{
			"id":   "call_mock_calculate",
			"type": "function",
			"function": map[string]interface{}{
				"name":      "calculate",
				"arguments": `{"expression":"2+2"}`,
			},
		},
	}
}

// handleChat — обработка /api/chat с поддержкой tools/tool_calls
func (m *ExpandedMockServer) handleChat(w http.ResponseWriter, r *http.Request) {
	b := m.getBehavior()

	if m.checkRateLimit(w, atomic.LoadInt64(&m.ChatCount)) {
		return
	}
	if b.ErrorStatusCode > 0 {
		w.WriteHeader(b.ErrorStatusCode)
		json.NewEncoder(w).Encode(map[string]string{"error": "simulated backend error"})
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	stream := true
	if s, ok := req["stream"].(bool); ok {
		stream = s
	}

	model := ""
	if m, ok := req["model"].(string); ok {
		model = m
	}

	// Проверяем наличие tools в запросе
	tools, hasTools := req["tools"].([]interface{})
	// Проверяем наличие tool-сообщений (результаты вызова инструментов)
	messages, _ := req["messages"].([]interface{})
	hasToolResults := hasToolRoleMessages(messages)

	m.applyResponseDelay()

	// Если есть tools и нет tool-результатов — возвращаем tool_calls
	if hasTools && len(tools) > 0 && !hasToolResults {
		toolCalls := buildMockToolCalls()
		if stream {
			// Streaming with tools: буферизированный режим (как в реальном cppworker)
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				return
			}
			// Один чанк с tool_calls
			chunk := map[string]interface{}{
				"model": model,
				"message": map[string]interface{}{
					"role":       "assistant",
					"content":    "",
					"tool_calls": toolCalls,
				},
				"done": true,
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "%s\n", data)
			flusher.Flush()
			return
		}
		// Non-streaming with tool_calls
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model": model,
			"message": map[string]interface{}{
				"role":       "assistant",
				"content":    "",
				"tool_calls": toolCalls,
			},
			"done": true,
		})
		return
	}

	// Если есть tool-результаты — возвращаем итоговый текстовый ответ
	if hasToolResults {
		responseText := "Based on the search results, I can provide you with the following information."
		if stream {
			m.handleChatStreamingWithText(w, model, responseText)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model": model,
			"message": map[string]interface{}{
				"role":    "assistant",
				"content": responseText,
			},
			"done": true,
		})
		return
	}

	// Обычный чат без инструментов
	if stream {
		m.handleChatStreaming(w, model)
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"model": model,
			"message": map[string]string{
				"role":    "assistant",
				"content": "Hello from expanded mock!",
			},
			"done": true,
		})
	}
}

// handleChatStreamingWithText — streaming chat ответ с заданным текстом
func (m *ExpandedMockServer) handleChatStreamingWithText(w http.ResponseWriter, model string, text string) {
	b := m.getBehavior()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	if b.ModelLoadDelay > 0 {
		time.Sleep(b.ModelLoadDelay)
	}

	// Разбиваем текст на слова для имитации stream
	words := strings.Fields(text)
	if len(words) == 0 {
		words = []string{text}
	}

	for i, word := range words {
		if b.DropAfterChunks > 0 && i >= b.DropAfterChunks-1 {
			if b.DropOnStream {
				if hijacker, ok := w.(http.Hijacker); ok {
					conn, _, _ := hijacker.Hijack()
					conn.Close()
					return
				}
			}
			return
		}

		isLast := (i == len(words)-1)
		// Добавляем пробел обратно, кроме последнего слова
		content := word
		if !isLast {
			content = word + " "
		}
		chunk := map[string]interface{}{
			"model": model,
			"message": map[string]string{
				"role":    "assistant",
				"content": content,
			},
			"done": isLast && !b.DropOnStream,
		}
		if isLast && !b.DropOnStream {
			chunk["total_duration"] = 1000000
		}

		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", data)
		flusher.Flush()

		if b.ChunkDelay > 0 && i < len(words)-1 {
			time.Sleep(b.ChunkDelay)
		}
	}
}

// handleChatStreaming — streaming chat ответ
func (m *ExpandedMockServer) handleChatStreaming(w http.ResponseWriter, model string) {
	b := m.getBehavior()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	if b.ModelLoadDelay > 0 {
		time.Sleep(b.ModelLoadDelay)
	}

	numChunks := b.StreamResponseSize
	if numChunks <= 0 {
		numChunks = 3
	}

	messages := []string{"Hi", " there", " from", " chat", "!"}

	for i := 0; i < numChunks && i < len(messages); i++ {
		if b.DropAfterChunks > 0 && i >= b.DropAfterChunks {
			if b.DropOnStream {
				if hijacker, ok := w.(http.Hijacker); ok {
					conn, _, _ := hijacker.Hijack()
					conn.Close()
					return
				}
			}
			return
		}

		isLast := (i == numChunks-1) || (b.DropAfterChunks > 0 && i == b.DropAfterChunks-1)
		event := map[string]interface{}{
			"model": model,
			"message": map[string]string{
				"role":    "assistant",
				"content": messages[i],
			},
			"done": isLast && !b.DropOnStream,
		}

		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()

		if b.ChunkDelay > 0 && i < numChunks-1 {
			time.Sleep(b.ChunkDelay)
		}
	}
}

// handleEmbeddings — обработка /api/embeddings
func (m *ExpandedMockServer) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if m.getBehavior().ErrorStatusCode > 0 {
		w.WriteHeader(m.getBehavior().ErrorStatusCode)
		json.NewEncoder(w).Encode(map[string]string{"error": "simulated backend error"})
		return
	}

	m.applyResponseDelay()

	body, _ := io.ReadAll(r.Body)
	var req map[string]interface{}
	json.Unmarshal(body, &req)

	model := ""
	if mod, ok := req["model"].(string); ok {
		model = mod
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"model":      model,
		"embeddings": []float64{0.1, 0.2, 0.3, 0.4, 0.5},
	})
}

// handleTags — обработка /api/tags
func (m *ExpandedMockServer) handleTags(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": []map[string]interface{}{
			{
				"name":    "llama3.1:8b",
				"size":    4928300000,
				"digest":  "sha256:abc123",
				"details": map[string]interface{}{"family": "llama", "parameter_size": "8B"},
			},
			{
				"name":    "qwen2.5:14b",
				"size":    8965234567,
				"digest":  "sha256:def456",
				"details": map[string]interface{}{"family": "qwen", "parameter_size": "14B"},
			},
		},
	})
}

// handlePs — обработка /api/ps
func (m *ExpandedMockServer) handlePs(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	models := make([]types.RunningModel, len(m.runningModels))
	copy(models, m.runningModels)
	m.mu.Unlock()

	m.applyResponseDelay()

	var ollamaModels []map[string]interface{}
	for _, rm := range models {
		ollamaModels = append(ollamaModels, map[string]interface{}{
			"name":       rm.Name,
			"size":       rm.Size,
			"digest":     rm.Digest,
			"expires_at": rm.ExpiresAt,
			"size_vram":  rm.VRAMUsage,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"models": ollamaModels,
	})
}

// handleVersion — обработка /api/version
func (m *ExpandedMockServer) handleVersion(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version": "0.3.0",
	})
}

// handleShow — обработка /api/show
func (m *ExpandedMockServer) handleShow(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"license":    "MIT",
		"modelfile":  "# Modelfile",
		"parameters": "num_ctx 4096",
		"template":   "[INST] {{ .Prompt }} [/INST]",
		"details": map[string]interface{}{
			"parent_model":      "",
			"format":            "gguf",
			"family":            "llama",
			"families":          []string{"llama"},
			"parameter_size":    "8B",
			"quantization_level": "Q4_0",
		},
	})
}

// handleCreate — обработка /api/create
func (m *ExpandedMockServer) handleCreate(w http.ResponseWriter, r *http.Request) {
	b := m.getBehavior()
	m.applyResponseDelay()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	responses := []map[string]interface{}{
		{"status": "reading manifest"},
		{"status": "pulling base model"},
		{"status": "success"},
	}

	for i, resp := range responses {
		if b.DropAfterChunks > 0 && i >= b.DropAfterChunks {
			return
		}
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// handlePull — обработка /api/pull
func (m *ExpandedMockServer) handlePull(w http.ResponseWriter, r *http.Request) {
	b := m.getBehavior()
	m.applyResponseDelay()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	responses := []map[string]interface{}{
		{"status": "pulling manifest"},
		{"status": "downloading", "completed": 1024, "total": 4096},
		{"status": "success"},
	}

	for i, resp := range responses {
		if b.DropAfterChunks > 0 && i >= b.DropAfterChunks {
			return
		}
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// handleDelete — обработка /api/delete
func (m *ExpandedMockServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"deleted": true,
	})
}

// handleCopy — обработка /api/copy
func (m *ExpandedMockServer) handleCopy(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"copied": true,
	})
}

// handlePush — обработка /api/push
func (m *ExpandedMockServer) handlePush(w http.ResponseWriter, r *http.Request) {
	m.applyResponseDelay()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	responses := []map[string]interface{}{
		{"status": "pushing"},
		{"status": "success"},
	}

	for _, resp := range responses {
		data, _ := json.Marshal(resp)
		fmt.Fprintf(w, "data: %s\n\n", data)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ResetCounters — сброс всех счётчиков
func (m *ExpandedMockServer) ResetCounters() {
	atomic.StoreInt64(&m.GenerateCount, 0)
	atomic.StoreInt64(&m.ChatCount, 0)
	atomic.StoreInt64(&m.EmbedCount, 0)
	atomic.StoreInt64(&m.TagsCount, 0)
	atomic.StoreInt64(&m.PsCount, 0)
	atomic.StoreInt64(&m.VersionCount, 0)
	atomic.StoreInt64(&m.ShowCount, 0)
	atomic.StoreInt64(&m.CreateCount, 0)
	atomic.StoreInt64(&m.PullCount, 0)
	atomic.StoreInt64(&m.DeleteCount, 0)
	atomic.StoreInt64(&m.CopyCount, 0)
	atomic.StoreInt64(&m.PushCount, 0)
}

// ===== Вспомогательные функции =====

// newBodyReader — создаёт Reader из []byte для повторного чтения
type bodyReader struct {
	data []byte
	pos  int
}

func newBodyReader(data []byte) *bodyReader {
	return &bodyReader{data: data, pos: 0}
}

func (r *bodyReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

func (r *bodyReader) Close() error {
	return nil
}

// SetupExpandedProxy — настройка прокси с расширенным мок-сервером
func SetupExpandedProxy(t testing.TB, mock *ExpandedMockServer, extraCfg ...func(*types.LoadBalancerConfig)) (*httptest.Server, *balancer.Proxy) {
	t.Helper()

	host, port := mock.HostPort()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    0,
			APIPort: 0,
		},
		Backends: []types.Backend{
			{
				ID:                mock.BackendID,
				Name:              "Expanded Mock",
				Host:              host,
				OllamaPort:        port,
				AgentPort:         9090,
				Weight:            1,
				MaxConcurrentReqs: 10,
				Status:            types.StatusHealthy,
			},
		},
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}

	for _, fn := range extraCfg {
		fn(config)
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	proxy.UpdateMetrics(mock.BackendID, &types.BackendMetrics{
		ID: mock.BackendID,
		GPU: types.GPUMetrics{
			UsagePercent: 30,
			MemoryTotal:  24576,
			MemoryUsed:   8000,
			MemoryFree:   16576,
		},
		System: types.SystemMetrics{
			CPUUsagePercent: 20,
			MemoryTotal:     65536,
			MemoryUsed:      16000,
			MemoryFree:      49536,
			DiskFree:        20480,
		},
		Ollama: types.OllamaMetrics{
			MaxModels:             5,
			MaxConcurrentRequests: 10,
			ActiveRequests:        0,
			OllamaAvailable:       true,
			RunningModels: []types.RunningModel{
				{Name: "llama3.1:8b", VRAMUsage: 6000},
			},
		},
	})

	proxyServer := httptest.NewServer(proxy)
	return proxyServer, proxy
}

// SetupMultiBackendProxy — настройка прокси с несколькими мок-серверами
func SetupMultiBackendProxy(t testing.TB, mocks []*ExpandedMockServer, extraCfg ...func(*types.LoadBalancerConfig)) (*httptest.Server, *balancer.Proxy) {
	t.Helper()

	config := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host:    "localhost",
			Port:    0,
			APIPort: 0,
		},
		Backends: make([]types.Backend, 0, len(mocks)),
		Balancing: types.BalancingSettings{
			Algorithm:         types.AlgorithmResourceAware,
			ModelAffinity:     true,
			SessionStickiness: true,
			RequestTimeout:    30,
			QueueTimeout:      60,
			QueueMaxSize:      100,
			QueueWorkers:      4,
		},
		Resources: types.ResourceLimits{
			GPU:    types.GPULimits{MaxUsagePercent: 90, MaxVRAMUsagePercent: 95},
			CPU:    types.CPULimits{MaxUsagePercent: 90},
			Memory: types.MemoryLimits{MaxUsagePercent: 90},
			Disk:   types.DiskLimits{MinFreeMB: 1024},
		},
	}

	for i, mock := range mocks {
		host, port := mock.HostPort()
		mock.BackendID = fmt.Sprintf("mock-backend-%d", i+1)
		config.Backends = append(config.Backends, types.Backend{
			ID:                mock.BackendID,
			Name:              fmt.Sprintf("Mock Backend %d", i+1),
			Host:              host,
			OllamaPort:        port,
			AgentPort:         9090,
			Weight:            100 - i*10, // Первый бэкенд имеет больший вес
			MaxConcurrentReqs: 10,
			Status:            types.StatusHealthy,
		})
	}

	for _, fn := range extraCfg {
		fn(config)
	}

	proxy := balancer.NewProxy(config)
	proxy.SetQueueManagerProxy()

	for _, mock := range mocks {
		proxy.UpdateMetrics(mock.BackendID, &types.BackendMetrics{
			ID: mock.BackendID,
			GPU: types.GPUMetrics{
				UsagePercent: 30,
				MemoryTotal:  24576,
				MemoryUsed:   8000,
				MemoryFree:   16576,
			},
			System: types.SystemMetrics{
				CPUUsagePercent: 20,
				MemoryTotal:     65536,
				MemoryUsed:      16000,
				MemoryFree:      49536,
				DiskFree:        20480,
			},
			Ollama: types.OllamaMetrics{
				MaxModels:             5,
				MaxConcurrentRequests: 10,
				ActiveRequests:        0,
				OllamaAvailable:       true,
				RunningModels: []types.RunningModel{
					{Name: "llama3.1:8b", VRAMUsage: 6000},
				},
			},
		})
	}

	proxyServer := httptest.NewServer(proxy)
	return proxyServer, proxy
}
