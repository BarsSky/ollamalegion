// Package rpccoordinator — Вариант B: External RPC Coordinator
// Реализует внешний микросервис ModelCoordinator для управления
// распределённым выполнением инференса через Worker'ы.
package rpccoordinator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Coordinator управляет распределённым инференсом через RPC.
// Принимает запросы от балансировщика, разбивает на подзапросы (split),
// отправляет Worker'ам, агрегирует ответы (merge).
type Coordinator struct {
	mu        sync.RWMutex
	enabled   bool
	url       string
	port      int
	protocol  string // "http" | "grpc"
	workers   []*ModelWorker
	client    *http.Client
}

// NewCoordinator создаёт новый RPC координатор.
func NewCoordinator() *Coordinator {
	return &Coordinator{
		enabled:  false,
		protocol: "http",
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Start запускает координатор.
func (c *Coordinator) Start() error {
	return nil
}

// Stop останавливает координатор.
func (c *Coordinator) Stop() error {
	return nil
}

// IsEnabled возвращает статус включения.
func (c *Coordinator) IsEnabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// SetEnabled включает/выключает координатор.
func (c *Coordinator) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = enabled
}

// SetConfig обновляет конфигурацию координатора.
func (c *Coordinator) SetConfig(url string, port int, protocol string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.url = url
	c.port = port
	c.protocol = protocol
}

// AddWorker добавляет Worker'а в пул координатора.
func (c *Coordinator) AddWorker(w *ModelWorker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workers = append(c.workers, w)
}

// GetWorkers возвращает копию списка Worker'ов.
func (c *Coordinator) GetWorkers() []*ModelWorker {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]*ModelWorker, len(c.workers))
	copy(result, c.workers)
	return result
}

// InferRequest — структура запроса к Coordinator.
type InferRequest struct {
	Model      string            `json:"model"`
	Prompt     string            `json:"prompt"`
	Parameters map[string]string `json:"parameters,omitempty"`
}

// InferResponse — структура ответа Coordinator.
type InferResponse struct {
	Model      string `json:"model"`
	Response   string `json:"response"`
	Done       bool   `json:"done"`
	WorkerID   string `json:"worker_id,omitempty"`
	SliceID    string `json:"slice_id,omitempty"`
}

// Infer отправляет запрос на инференс через координатор.
// Стратегия: отправляем запрос первому доступному Worker'у.
// В перспективе: split → send to multiple workers → merge.
func (c *Coordinator) Infer(model string, prompt []byte, params map[string]string) ([]byte, error) {
	c.mu.RLock()
	enabled := c.enabled
	workers := make([]*ModelWorker, len(c.workers))
	copy(workers, c.workers)
	c.mu.RUnlock()

	if !enabled {
		return nil, fmt.Errorf("rpc coordinator is disabled")
	}

	if len(workers) == 0 {
		return nil, fmt.Errorf("no workers available")
	}

	// Выбираем Worker с наименьшим ID (round-robin в перспективе)
	worker := workers[0]

	// Строим URL Worker'а
	workerURL := fmt.Sprintf("http://%s:%d/infer", worker.Host(), worker.Port())

	reqBody := InferRequest{
		Model:      model,
		Prompt:     string(prompt),
		Parameters: params,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Отправляем HTTP запрос к Worker'у
	httpReq, err := http.NewRequest("POST", workerURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to worker %s: %w", worker.WorkerID(), err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from worker %s: %w", worker.WorkerID(), err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker %s returned status %d: %s", worker.WorkerID(), resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return respBody, nil
}

// GetStatus возвращает статус координатора.
func (c *Coordinator) GetStatus() map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	workerStatuses := make([]map[string]interface{}, len(c.workers))
	for i, w := range c.workers {
		workerStatuses[i] = w.GetStatus()
	}

	return map[string]interface{}{
		"enabled":  c.enabled,
		"url":      c.url,
		"port":     c.port,
		"protocol": c.protocol,
		"workers":  workerStatuses,
	}
}

// GetStatus возвращает отформатированный статус для API.
func (c *Coordinator) GetStatusMap() map[string]interface{} {
	return c.GetStatus()
}
