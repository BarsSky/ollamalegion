package rpccoordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ModelWorker — легковесный процесс, запускаемый рядом с Ollama.
// Управляет конкретным "срезом" модели (слои X-Y).
// Принимает запросы от Coordinator, выполняет инференс на своём срезе,
// возвращает частичные результаты.
type ModelWorker struct {
	workerID    string
	backendID   string
	host        string
	port        int
	sliceLayers string
	client      *http.Client
}

// NewModelWorker создаёт нового Worker'а.
func NewModelWorker(workerID, backendID, host string, port int, sliceLayers string) *ModelWorker {
	return &ModelWorker{
		workerID:    workerID,
		backendID:   backendID,
		host:        host,
		port:        port,
		sliceLayers: sliceLayers,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WorkerID возвращает идентификатор Worker'а.
func (w *ModelWorker) WorkerID() string {
	return w.workerID
}

// BackendID возвращает идентификатор связанного бэкенда Ollama.
func (w *ModelWorker) BackendID() string {
	return w.backendID
}

// Host возвращает хост Worker'а.
func (w *ModelWorker) Host() string {
	return w.host
}

// Port возвращает порт Worker'а.
func (w *ModelWorker) Port() int {
	return w.port
}

// SliceLayers возвращает срез слоёв, закреплённый за Worker'ом.
func (w *ModelWorker) SliceLayers() string {
	return w.sliceLayers
}

// Start запускает Worker (HTTP/gRPC сервер).
func (w *ModelWorker) Start() error {
	return nil
}

// Stop останавливает Worker.
func (w *ModelWorker) Stop() error {
	return nil
}

// OllamaRequest — структура запроса к API Ollama.
type OllamaRequest struct {
	Model     string            `json:"model"`
	Prompt    string            `json:"prompt"`
	Options   map[string]interface{} `json:"options,omitempty"`
	Stream    bool              `json:"stream"`
	SliceInfo string            `json:"x-slice-layers,omitempty"` // Информация о срезе для отладки
}

// OllamaResponse — структура ответа от API Ollama.
type OllamaResponse struct {
	Model     string `json:"model"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Infer выполняет инференс на срезе модели.
// Отправляет HTTP запрос к локальному экземпляру Ollama.
func (w *ModelWorker) Infer(input []byte) ([]byte, error) {
	// Парсим входные данные как InferRequest
	var req InferRequest
	if err := json.Unmarshal(input, &req); err != nil {
		return nil, fmt.Errorf("failed to parse input: %w", err)
	}

	// Строим URL к API Ollama generate
	ollamaURL := fmt.Sprintf("http://%s:%d/api/generate", w.host, w.port)

	// Формируем запрос к Ollama
	ollamaReq := OllamaRequest{
		Model:     req.Model,
		Prompt:    req.Prompt,
		Stream:    false,
		SliceInfo: w.sliceLayers,
	}

	// Пробрасываем параметры
	if len(req.Parameters) > 0 {
		opts := make(map[string]interface{}, len(req.Parameters))
		for k, v := range req.Parameters {
			opts[k] = v
		}
		ollamaReq.Options = opts
	}

	body, err := json.Marshal(ollamaReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal ollama request: %w", err)
	}

	// Отправляем запрос к Ollama
	httpReq, err := http.NewRequest("POST", ollamaURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create ollama request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := w.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to call ollama at %s: %w", ollamaURL, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read ollama response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// Парсим ответ Ollama в InferResponse
	var ollamaResp OllamaResponse
	if err := json.Unmarshal(respBody, &ollamaResp); err != nil {
		return nil, fmt.Errorf("failed to parse ollama response: %w", err)
	}

	// Формируем ответ Worker'а
	inferResp := InferResponse{
		Model:    ollamaResp.Model,
		Response: ollamaResp.Response,
		Done:     ollamaResp.Done,
		WorkerID: w.workerID,
		SliceID:  w.sliceLayers,
	}

	result, err := json.Marshal(inferResp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response: %w", err)
	}

	return result, nil
}

// GetStatus возвращает статус Worker'а.
func (w *ModelWorker) GetStatus() map[string]interface{} {
	return map[string]interface{}{
		"workerID":    w.workerID,
		"backendID":   w.backendID,
		"host":        w.host,
		"port":        w.port,
		"sliceLayers": w.sliceLayers,
	}
}
