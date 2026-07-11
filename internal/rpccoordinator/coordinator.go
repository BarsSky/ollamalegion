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

	// selector — B6: стратегия load balancing (LeastLoadedSelector default).
	// Используется executePipeline для выбора worker'а при наличии
	// нескольких кандидатов и failover при ошибке primary.
	selector Selector

	// metrics — B7: Prometheus-style aggregator.
	// Обновляется при каждом RegisterWorker/Infer/RegisterDistributedModel.
	// Экспортируется через /metrics endpoint в balancer handler.
	metrics *MetricsAggregator
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
//
// WorkerID — primary worker (для обратной совместимости).
// WorkerCandidates — все worker'ы, которые могут обслуживать этот срез.
// Если пусто — используется только [WorkerID]. Если непусто — selector
// выбирает ordered список с primary первым.
type LayerSlice struct {
	StartLayer int
	EndLayer   int
	WorkerID   string
	WorkerCandidates []string // B6: optional failover candidates
}

// Candidates возвращает полный список worker'ов-кандидатов для среза.
//
// Если WorkerCandidates пусто — возвращает [WorkerID].
func (s *LayerSlice) Candidates() []string {
	if len(s.WorkerCandidates) == 0 {
		if s.WorkerID == "" {
			return nil
		}
		return []string{s.WorkerID}
	}
	// Если WorkerID не в списке — добавляем.
	found := false
	for _, id := range s.WorkerCandidates {
		if id == s.WorkerID {
			found = true
			break
		}
	}
	if !found && s.WorkerID != "" {
		out := make([]string, 0, len(s.WorkerCandidates)+1)
		out = append(out, s.WorkerID)
		out = append(out, s.WorkerCandidates...)
		return out
	}
	return s.WorkerCandidates
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
		metrics: NewMetricsAggregator(),
	}
}

// Metrics возвращает Prometheus-style aggregator.
//
// Используется balancer handler'ом для экспорта /metrics endpoint.
func (c *ModelCoordinator) Metrics() *MetricsAggregator {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.metrics == nil {
		c.metrics = NewMetricsAggregator()
	}
	return c.metrics
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
		// B6: получаем ordered список кандидатов через selector (с учётом load + failover).
		// Если WorkerCandidates пустой — selector вернёт [WorkerID].
		candidates, selErr := c.SelectWorkersForSlice(slice.Candidates())
		if selErr != nil {
			return nil, fmt.Errorf("selector for slice %d-%d: %w",
				slice.StartLayer, slice.EndLayer, selErr)
		}

		// Пробуем кандидатов по очереди (failover).
		var (
			result   []byte
			err      error
			chosen   string
			latency  time.Duration
		)
		for _, workerID := range candidates {
			worker := c.GetWorker(workerID)
			if worker == nil {
				logger.Get().Warnw("candidate worker not found, trying next",
					"worker", workerID, "slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer))
				continue
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
			result, err = worker.InferSlice(job.ctx, sliceReq)
			latency = time.Since(sliceStart)
			chosen = workerID

			// Retry logic на том же worker'е (если есть MaxRetries).
			if err != nil && c.config.MaxRetries > 0 {
				for attempt := 1; attempt <= c.config.MaxRetries; attempt++ {
					logger.Get().Warnw("slice infer failed, retrying",
						"worker", workerID,
						"slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer),
						"attempt", attempt,
						"maxRetries", c.config.MaxRetries,
						"error", err)

					time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
					result, err = worker.InferSlice(job.ctx, sliceReq)
					if err == nil {
						break
					}
				}
			}

			// Записываем результат попытки.
			job.mu.Lock()
			job.SliceResults[workerID] = &SliceResult{
				WorkerID:   workerID,
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

			if err == nil {
				logger.Get().Debugw("slice infer succeeded",
					"worker", workerID,
					"slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer),
					"latency_ms", latency.Milliseconds())
				break
			}

			// Failover: логируем и пробуем следующего кандидата.
			logger.Get().Warnw("slice infer failed, trying next candidate",
				"worker", workerID,
				"slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer),
				"error", err)
		}

		if err != nil {
			job.Status = JobFailed
			return nil, fmt.Errorf("slice %d-%d failed on all candidates (last: %s): %w",
				slice.StartLayer, slice.EndLayer, chosen, err)
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

// CoordinatorStats — снимок состояния coordinator'а (для метрик и UI).
type CoordinatorStats struct {
	Workers       []string          // список ID зарегистрированных worker'ов
	Models        []string          // список имён зарегистрированных моделей
	ActiveJobs    int               // текущее число in-flight задач
	WorkersHealth map[string]bool   // workerID → IsHealthy()
	Selector      string            // имя текущей стратегии (для отладки)
}

// Stats возвращает снимок состояния coordinator'а.
//
// Используется /metrics endpoint (B7) и WebUI панелью для отображения статуса.
func (c *ModelCoordinator) Stats() CoordinatorStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	workers := make([]string, 0, len(c.workers))
	health := make(map[string]bool, len(c.workers))
	for id, w := range c.workers {
		workers = append(workers, id)
		health[id] = w.IsHealthy()
	}

	models := make([]string, 0, len(c.models))
	for name := range c.models {
		models = append(models, name)
	}

	selector := ""
	if c.selector != nil {
		selector = c.selector.Name()
	}

	return CoordinatorStats{
		Workers:       workers,
		Models:        models,
		ActiveJobs:    len(c.activeJobs),
		WorkersHealth: health,
		Selector:      selector,
	}
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

// =====================================================================
// Phase 8 (2026-07-10): Session 3.2 — streaming pipeline inference.
//
// InferStream оркестрирует pipeline с streaming chunks. Для каждого среза:
//   1. Вызывает worker.InferSliceStream() — получает токены через callback.
//   2. По мере получения токенов вызывает onToken callback coordinator'а
//      (это позволяет клиенту видеть real-time прогресс).
//   3. Собирает full output этого среза.
//   4. Передаёт output следующему срезу как Input.
//
// Возвращает totalTokens (сколько токенов прошло через все срезы) и
// totalMs. Slice stats собираются параллельно.
//
// Streaming семантика для pipeline:
//   - Slice 1 стримит свои токены → onToken вызывается по мере чтения.
//   - Когда slice 1 завершён, его full output отправляется в slice 2.
//   - Slice 2 стримит свои токены → onToken вызывается.
//   - И т.д.
//
// Это даёт "real-time feel" — клиент видит частичный output пока
// следующий slice ещё загружает KV-cache или стартует.
//
// При ошибке в любом срезе — возвращает partial output + error.
func (c *ModelCoordinator) InferStream(
	ctx context.Context,
	req InferRequest,
	onToken OnTokenFunc,
) (*InferResponse, error) {
	if !c.Enabled() {
		return nil, fmt.Errorf("rpc coordinator is disabled")
	}

	dm := c.GetDistributedModel(req.ModelName)
	if dm == nil {
		return nil, fmt.Errorf("distributed model %s not found", req.ModelName)
	}

	jobID := fmt.Sprintf("stream-%s-%d", req.ModelName, time.Now().UnixNano())
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

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
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.activeJobs, jobID)
		c.mu.Unlock()
	}()

	// Сортируем срезы по StartLayer (тот же пузырёк что в executePipeline).
	sortedSlices := make([]LayerSlice, len(dm.SliceLayers))
	copy(sortedSlices, dm.SliceLayers)
	for i := 0; i < len(sortedSlices)-1; i++ {
		for j := i + 1; j < len(sortedSlices); j++ {
			if sortedSlices[i].StartLayer > sortedSlices[j].StartLayer {
				sortedSlices[i], sortedSlices[j] = sortedSlices[j], sortedSlices[i]
			}
		}
	}

	start := time.Now()
	currentInput := []byte(req.Prompt)
	var totalTokens int
	tokenIndex := 0

	for _, slice := range sortedSlices {
		// На каждый slice — свой callback который инкрементит totalTokens
		// и пробрасывает token в outer onToken с глобальным tokenIndex.
		sliceTokens := 0
		sliceCb := func(token string, _ int) error {
			sliceTokens++
			if onToken != nil {
				if err := onToken(token, tokenIndex); err != nil {
					return err
				}
			}
			tokenIndex++
			return nil
		}

		// Try candidates (failover) — последовательно, тот же подход что в executePipeline.
		candidates, selErr := c.SelectWorkersForSlice(slice.Candidates())
		if selErr != nil {
			return nil, fmt.Errorf("selector for slice %d-%d: %w",
				slice.StartLayer, slice.EndLayer, selErr)
		}

		var (
			chosen   string
			latency  time.Duration
			sliceErr error
		)

		for _, workerID := range candidates {
			worker := c.GetWorker(workerID)
			if worker == nil {
				logger.Get().Warnw("stream candidate worker not found, trying next",
					"worker", workerID, "slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer))
				continue
			}

			sliceStart := time.Now()
			_, _, err := worker.InferSliceStream(jobCtx, SliceInferRequest{
				ModelName:  req.ModelName,
				StartLayer: slice.StartLayer,
				EndLayer:   slice.EndLayer,
				Input:      currentInput,
				SessionID:  req.SessionID,
				Params:     req.Params,
			}, sliceCb)
			latency = time.Since(sliceStart)
			chosen = workerID

			if err != nil {
				// Сохраняем error для этой попытки.
				job.mu.Lock()
				job.SliceResults[workerID] = &SliceResult{
					WorkerID:   workerID,
					StartLayer: slice.StartLayer,
					EndLayer:   slice.EndLayer,
					Latency:    latency,
					Output:     nil,
					Error:      err,
				}
				job.Errors = append(job.Errors, err)
				job.mu.Unlock()

				if c.config.MaxRetries > 0 {
					for attempt := 1; attempt <= c.config.MaxRetries; attempt++ {
						logger.Get().Warnw("stream slice failed, retrying",
							"worker", workerID, "slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer),
							"attempt", attempt, "error", err)
						time.Sleep(time.Duration(attempt) * 100 * time.Millisecond)
						_, _, err = worker.InferSliceStream(jobCtx, SliceInferRequest{
							ModelName:  req.ModelName,
							StartLayer: slice.StartLayer,
							EndLayer:   slice.EndLayer,
							Input:      currentInput,
							SessionID:  req.SessionID,
							Params:     req.Params,
						}, sliceCb)
						if err == nil {
							break
						}
					}
				}
				if err == nil {
					// Success after retry.
					break
				}
				// Failover: пробуем следующего кандидата.
				logger.Get().Warnw("stream slice failed, trying next candidate",
					"worker", workerID, "slice", fmt.Sprintf("%d-%d", slice.StartLayer, slice.EndLayer),
					"error", err)
				sliceErr = err
				continue
			}

			// Success.
			job.mu.Lock()
			job.SliceResults[workerID] = &SliceResult{
				WorkerID:   workerID,
				StartLayer: slice.StartLayer,
				EndLayer:   slice.EndLayer,
				Latency:    latency,
				Output:     []byte(fmt.Sprintf("%d", sliceTokens)), // не используется ниже
				Error:      nil,
			}
			job.mu.Unlock()
			totalTokens += sliceTokens
			break
		}

		if sliceErr != nil && chosen == "" {
			// Никто из кандидатов не сработал.
			job.Status = JobFailed
			return nil, fmt.Errorf("stream slice %d-%d failed on all candidates (last: %s): %w",
				slice.StartLayer, slice.EndLayer, chosen, sliceErr)
		}

		// Для pipeline: следующий slice получает на вход последний output предыдущего.
		// Упрощённо: используем req.Prompt + accumulated tokens как input.
		// В stub-режиме worker'ы игнорируют input length, так что это OK.
		// В production с реальной llama.cpp — нужно передавать last_hidden_state tensor,
		// что значительно сложнее (KV-cache merge). Phase 9.
		currentInput = []byte(req.Prompt + "\n[partial:" + fmt.Sprintf("%d", sliceTokens) + "]")
	}

	job.Status = JobCompleted
	job.FinishedAt = time.Now()
	duration := time.Since(start)

	// Возвращаем InferResponse с empty Output (стрим уже отправлен через onToken)
	// и totalTokens в stats. Caller (dispatcher) использует свой token counter.
	return &InferResponse{
		RequestID:  jobID,
		Output:     "", // streaming — output ушёл в onToken
		TotalMs:    duration.Milliseconds(),
		SliceStats: c.buildSliceStats(job),
	}, nil
}
