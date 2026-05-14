// Package rpccoordinator — Вариант B: External RPC Coordinator
// Управляет распределённым выполнением инференса моделей через worker'ы.
// Каждый worker обслуживает срез (slice) модели — набор слоёв.
package rpccoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ModelCoordinator управляет распределённым выполнением инференса.
type ModelCoordinator struct {
	config     types.RpcCoordinatorConfig
	mu         sync.RWMutex
	workers    map[string]*WorkerClient       // workerID → client
	models     map[string]*DistributedModel   // modelName → распределённая модель
	activeJobs map[string]*InferenceJob       // jobID → активная задача
	httpClient *http.Client
	enabled    bool
}

// DistributedModel описывает модель, распределённую по worker'ам.
type DistributedModel struct {
	Name        string
	Description string
	SliceLayers []LayerSlice // Покрытие слоёв по worker'ам
	Workers     []string     // ID worker'ов
	mu          sync.RWMutex
}

// LayerSlice описывает диапазон слоёв на конкретном worker'е.
type LayerSlice struct {
	StartLayer int
	EndLayer   int
	WorkerID   string
}

// InferenceJob — контекст одной inference-задачи.
type InferenceJob struct {
	JobID      string
	ModelName  string
	Input      []byte
	Output     []byte
	Status     JobStatus
	SliceResults map[string]*SliceResult // workerID → результат среза
	Errors     []error
	CreatedAt  time.Time
	FinishedAt time.Time
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelFunc
}

// JobStatus — статус inference-задачи.
type JobStatus string

const (
	JobPending    JobStatus = "pending"
	JobRunning    JobStatus = "running"
	JobCompleted  JobStatus = "completed"
	JobFailed     JobStatus = "failed"
	JobCancelled  JobStatus = "cancelled"
)

// SliceResult — результат выполнения среза.
type SliceResult struct {
	WorkerID   string
	StartLayer int
	EndLayer   int
	Output     []byte
	Latency    time.Duration
	Error      error
}

// InferRequest — запрос на inference от балансировщика.
type InferRequest struct {
	ModelName string            `json:"model_name"`
	Prompt    string            `json:"prompt"`
	Params    map[string]string `json:"params,omitempty"`
	SessionID string            `json:"session_id,omitempty"`
	Stream    bool              `json:"stream,omitempty"`
}

// InferResponse — ответ координатора.
type InferResponse struct {
	RequestID  string   `json:"request_id"`
	Output     string   `json:"output"`
	SliceStats []SliceStat `json:"slice_stats,omitempty"`
	TotalMs    int64    `json:"total_ms"`
}

// SliceStat — статистика по срезу.
type SliceStat struct {
	SliceID    string `json:"slice_id"`
	WorkerID   string `json:"worker_id"`
	LatencyMs  int64  `json:"latency_ms"`
	Success    bool   `json:"success"`
}

// NewModelCoordinator создаёт новый координатор.
func NewModelCoordinator(cfg types.RpcCoordinatorConfig) *ModelCoordinator {
	timeout := 30 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}

	return &ModelCoordinator{
		config:     cfg,
		workers:    make(map[string]*WorkerClient),
		models:     make(map[string]*DistributedModel),
		activeJobs: make(map[string]*InferenceJob),
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxConnsPerHost:     50,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		enabled: cfg.Enabled,
	}
}

// Enabled возвращает статус включения.
func (c *ModelCoordinator) Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// SetEnabled включает/выключает координатор.
func (c *ModelCoordinator) SetEnabled(v bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enabled = v
}

// RegisterWorker регистрирует нового worker'а.
func (c *ModelCoordinator) RegisterWorker(cfg types.RpcWorkerConfig) error {
	if cfg.WorkerID == "" {
		return fmt.Errorf("workerID is required")
	}
	if cfg.Host == "" {
		return fmt.Errorf("host is required")
	}
	if cfg.Port <= 0 {
		cfg.Port = 18080 // default RPC worker port
	}

	client := NewWorkerClient(cfg.WorkerID, cfg.Host, cfg.Port, c.config.Protocol, c.httpClient)

	c.mu.Lock()
	defer c.mu.Unlock()

	// Health check перед регистрацией
	if ok, err := client.HealthCheck(); !ok {
		logger.Get().Warnw("worker health check failed during registration",
			"worker", cfg.WorkerID, "host", cfg.Host, "port", cfg.Port, "error", err)
		// Регистрируем, но помечаем unhealthy — будет retry в фоне
	}

	c.workers[cfg.WorkerID] = client

	logger.Get().Infow("worker registered",
		"worker", cfg.WorkerID,
		"host", cfg.Host,
		"port", cfg.Port,
		"layers", cfg.SliceLayers,
		"protocol", c.config.Protocol)
	return nil
}

// UnregisterWorker удаляет worker'а.
func (c *ModelCoordinator) UnregisterWorker(workerID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.workers, workerID)
	logger.Get().Infow("worker unregistered", "worker", workerID)
}

// GetWorker возвращает клиент worker'а.
func (c *ModelCoordinator) GetWorker(workerID string) *WorkerClient {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.workers[workerID]
}

// ListWorkers возвращает список всех worker'ов.
func (c *ModelCoordinator) ListWorkers() []types.RpcWorkerConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]types.RpcWorkerConfig, 0, len(c.workers))
	for id, w := range c.workers {
		result = append(result, types.RpcWorkerConfig{
			WorkerID: id,
			Host:     w.Host,
			Port:     w.Port,
		})
	}
	return result
}

// RegisterDistributedModel регистрирует распределённую модель.
func (c *ModelCoordinator) RegisterDistributedModel(name, description string, slices []LayerSlice) error {
	if name == "" {
		return fmt.Errorf("model name is required")
	}
	if len(slices) == 0 {
		return fmt.Errorf("at least one slice is required")
	}

	// Проверяем, что все worker'ы зарегистрированы
	c.mu.RLock()
	for _, s := range slices {
		if _, ok := c.workers[s.WorkerID]; !ok {
			c.mu.RUnlock()
			return fmt.Errorf("worker %s not registered", s.WorkerID)
		}
	}
	c.mu.RUnlock()

	dm := &DistributedModel{
		Name:        name,
		Description: description,
		SliceLayers: slices,
	}

	// Собираем список worker'ов
	workerSet := make(map[string]bool)
	for _, s := range slices {
		workerSet[s.WorkerID] = true
	}
	for wID := range workerSet {
		dm.Workers = append(dm.Workers, wID)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.models[name] = dm

	logger.Get().Infow("distributed model registered",
		"model", name,
		"slices", len(slices),
		"workers", len(dm.Workers))
	return nil
}

// GetDistributedModel возвращает распределённую модель.
func (c *ModelCoordinator) GetDistributedModel(name string) *DistributedModel {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.models[name]
}

// HasDistributedModel проверяет, зарегистрирована ли модель.
func (c *ModelCoordinator) HasDistributedModel(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.models[name]
	return ok
}

// ListDistributedModels возвращает список всех моделей.
func (c *ModelCoordinator) ListDistributedModels() []map[string]interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]map[string]interface{}, 0, len(c.models))
	for name, dm := range c.models {
		info := map[string]interface{}{
			"name":        dm.Name,
			"description": dm.Description,
			"slices":      len(dm.SliceLayers),
			"workers":     len(dm.Workers),
		}
		// Детали срезов
		sliceInfos := make([]map[string]interface{}, 0, len(dm.SliceLayers))
		for _, s := range dm.SliceLayers {
			sliceInfos = append(sliceInfos, map[string]interface{}{
				"startLayer": s.StartLayer,
				"endLayer":   s.EndLayer,
				"workerId":   s.WorkerID,
			})
		}
		info["sliceDetails"] = sliceInfos
		info["name"] = name
		result = append(result, info)
	}
	return result
}

// Infer выполняет распределённый inference.
// Pipeline: sequential (каждый слой получает hidden states от предыдущего).
func (c *ModelCoordinator) Infer(ctx context.Context, req InferRequest) (*InferResponse, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("rpc coordinator is disabled")
	}

	dm := c.GetDistributedModel(req.ModelName)
	if dm == nil {
		return nil, fmt.Errorf("distributed model %s not found", req.ModelName)
	}

	jobID := fmt.Sprintf("infer-%s-%d", req.ModelName, time.Now().UnixNano())
	jobCtx, cancel := context.WithCancel(ctx)

	job := &InferenceJob{
		JobID:        jobID,
		ModelName:    req.ModelName,
		Input:        []byte(req.Prompt),
		Status:       JobPending,
		SliceResults: make(map[string]*SliceResult),
		CreatedAt:    time.Now(),
		ctx:          jobCtx,
		cancel:       cancel,
	}

	c.mu.Lock()
	c.activeJobs[jobID] = job
	c.mu.Unlock()

	// Выполняем pipeline
	start := time.Now()
	resp, err := c.executePipeline(job, dm, req)
	duration := time.Since(start)

	// Очищаем задачу
	cancel()
	c.mu.Lock()
	delete(c.activeJobs, jobID)
	c.mu.Unlock()

	if err != nil {
		return nil, fmt.Errorf("inference failed: %w", err)
	}

	return &InferResponse{
		RequestID:  jobID,
		Output:     string(resp),
		TotalMs:    duration.Milliseconds(),
		SliceStats: c.buildSliceStats(job),
	}, nil
}

// executePipeline выполняет последовательный pipeline через все срезы.
func (c *ModelCoordinator) executePipeline(job *InferenceJob, dm *DistributedModel, req InferRequest) ([]byte, error) {
	job.Status = JobRunning

	// Сортируем срезы по слоям
	sortedSlices := make([]LayerSlice, len(dm.SliceLayers))
	copy(sortedSlices, dm.SliceLayers)
	for i := 0; i < len(sortedSlices)-1; i++ {
		for j := i + 1; j < len(sortedSlices); j++ {
			if sortedSlices[i].StartLayer > sortedSlices[j].StartLayer {
				sortedSlices[i], sortedSlices[j] = sortedSlices[j], sortedSlices[i]
			}
		}
	}

	currentInput := []byte(req.Prompt)

	for _, slice := range sortedSlices {
		worker := c.GetWorker(slice.WorkerID)
		if worker == nil {
			return nil, fmt.Errorf("worker %s not found for slice %d-%d", slice.WorkerID, slice.StartLayer, slice.EndLayer)
		}

		sliceReq := SliceInferRequest{
			ModelName:    req.ModelName,
			StartLayer:   slice.StartLayer,
			EndLayer:     slice.EndLayer,
			Input:        currentInput,
			SessionID:    req.SessionID,
			Params:       req.Params,
		}

		sliceStart := time.Now()
		result, err := worker.InferSlice(job.ctx, sliceReq)
		latency := time.Since(sliceStart)

		job.mu.Lock()
		job.SliceResults[slice.WorkerID] = &SliceResult{
			WorkerID:   slice.WorkerID,
			StartLayer: slice.StartLayer,
			EndLayer:   slice.EndLayer,
			Latency:    latency,
			Output:     result,
			Error:      err,
		}
		if err != nil {
			job.Errors = append(job.Errors, err)
		}
		job.mu.Unlock()

		if err != nil {
			// Retry logic
			if c.config.MaxRetries > 0 {
				for attempt := 1; attempt <= c.config.MaxRetries; attempt++ {
					logger.Get().Warnw("slice infer failed, retrying",
						"worker", slice.WorkerID,
						"attempt", attempt,
						"maxRetries", c.config.MaxRetries,
						"error", err)

					time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)

					result, err = worker.InferSlice(job.ctx, sliceReq)
					if err == nil {
						job.mu.Lock()
						job.SliceResults[slice.WorkerID].Output = result
						job.SliceResults[slice.WorkerID].Error = nil
						job.mu.Unlock()
						break
					}
				}
			}
			if err != nil {
				job.Status = JobFailed
				return nil, fmt.Errorf("slice %d-%d on worker %s failed after retries: %w",
					slice.StartLayer, slice.EndLayer, slice.WorkerID, err)
			}
		}

		currentInput = result
	}

	job.Status = JobCompleted
	job.FinishedAt = time.Now()
	return currentInput, nil
}

// buildSliceStats собирает статистику по срезам для ответа.
func (c *ModelCoordinator) buildSliceStats(job *InferenceJob) []SliceStat {
	job.mu.Lock()
	defer job.mu.Unlock()

	stats := make([]SliceStat, 0, len(job.SliceResults))
	for _, res := range job.SliceResults {
		stats = append(stats, SliceStat{
			SliceID:   fmt.Sprintf("%d-%d", res.StartLayer, res.EndLayer),
			WorkerID:  res.WorkerID,
			LatencyMs: res.Latency.Milliseconds(),
			Success:   res.Error == nil,
		})
	}
	return stats
}

// GetActiveJobs возвращает количество активных задач.
func (c *ModelCoordinator) GetActiveJobs() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.activeJobs)
}

// GetJobStatus возвращает статус задачи.
func (c *ModelCoordinator) GetJobStatus(jobID string) map[string]interface{} {
	c.mu.RLock()
	job, ok := c.activeJobs[jobID]
	c.mu.RUnlock()
	if !ok {
		return nil
	}

	job.mu.Lock()
	defer job.mu.Unlock()

	return map[string]interface{}{
		"jobId":      job.JobID,
		"modelName":  job.ModelName,
		"status":     string(job.Status),
		"createdAt":  job.CreatedAt,
		"finishedAt": job.FinishedAt,
		"slices":     len(job.SliceResults),
		"errors":     len(job.Errors),
	}
}

// CancelJob отменяет активную задачу.
func (c *ModelCoordinator) CancelJob(jobID string) bool {
	c.mu.RLock()
	job, ok := c.activeJobs[jobID]
	c.mu.RUnlock()
	if !ok {
		return false
	}
	job.cancel()
	job.Status = JobCancelled
	return true
}

// HealthCheck проверяет доступность всех worker'ов.
func (c *ModelCoordinator) HealthCheck() map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make(map[string]bool, len(c.workers))
	for id, w := range c.workers {
		ok, _ := w.HealthCheck()
		result[id] = ok
	}
	return result
}

// Close закрывает координатор и все соединения.
func (c *ModelCoordinator) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Отменяем все активные задачи
	for _, job := range c.activeJobs {
		job.cancel()
	}

	c.enabled = false
	logger.Get().Infow("rpc coordinator closed",
		"workers", len(c.workers),
		"models", len(c.models))
}

// ---------- Helper types ----------

// SliceInferRequest — запрос к worker'у для выполнения среза.
type SliceInferRequest struct {
	ModelName  string            `json:"model_name"`
	StartLayer int               `json:"start_layer"`
	EndLayer   int               `json:"end_layer"`
	Input      []byte            `json:"input"`
	SessionID  string            `json:"session_id,omitempty"`
	Params     map[string]string `json:"params,omitempty"`
}

// SliceInferResponse — ответ worker'а.
type SliceInferResponse struct {
	Output    []byte `json:"output"`
	Error     string `json:"error,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
}

// SendSliceRequest отправляет HTTP-запрос к worker'у для среза.
func SendSliceRequest(client *http.Client, url string, req SliceInferRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("worker returned %d: %s", resp.StatusCode, string(body))
	}

	return io.ReadAll(resp.Body)
}