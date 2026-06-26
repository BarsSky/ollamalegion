// Package rptensor — TensorParallelCoordinator (B8.2).
//
// Координатор параллельно отправляет один и тот же input на все rank'ы
// (worldSize workers), собирает partial outputs через barrier и
// агрегирует их через all-reduce.
//
// Каркас B8.2 не выполняет реальной матричной математики — partial
// outputs передаются как []byte (opaque, base64 в JSON), all-reduce
// реализован через байтовые операции (concat / xor / sum). Реальная
// TF32/FP16-агрегация через ggml — отдельная фаза (B8.7, post-1.0).
//
// Для тестирования coordinator использует RankTransport интерфейс
// (одна реализация — rpccoordinator.WorkerClient wrapper, вторая —
// mock через httptest в e2e тестах).
package rptensor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// =====================================================================
// Public types: TP Infer request/response
// =====================================================================

// TPInferRequest — входной запрос от балансировщика / API.
type TPInferRequest struct {
	ModelName string `json:"model_name"`
	Prompt    string `json:"prompt,omitempty"`
	Input     []byte `json:"input,omitempty"` // hidden states / embeddings
	TierCount int    `json:"tier_count"`      // worldSize (1..64)
	SessionID string `json:"session_id,omitempty"`
	// Stream пока зарезервирован — B8.5 реализует SSE через подписку на rank'и.
	Stream bool `json:"stream,omitempty"`
}

// TPInferResponse — ответ с агрегированным output'ом.
type TPInferResponse struct {
	Output       string        `json:"output"`
	OutputBytes  []byte        `json:"-"` // для тестов
	LatencyMs    int64         `json:"latency_ms"`
	LayerStats   []LayerStat   `json:"layer_stats,omitempty"`
	RankErrors   []RankError   `json:"rank_errors,omitempty"`
	Degraded     bool          `json:"degraded"`     // true если хоть один rank упал
	WorldSize    int           `json:"world_size"`
	NumLayers    int           `json:"num_layers"`
}

// LayerStat — per-layer статистика (latency по самому медленному rank'у).
type LayerStat struct {
	Layer      int   `json:"layer"`
	LatencyMs  int64 `json:"latency_ms"`
	Successful int   `json:"successful"`  // сколько rank'ов ответили OK
	Total      int   `json:"total"`       // worldSize
}

// RankError — ошибка от конкретного rank'а на конкретном слое.
type RankError struct {
	Rank  int    `json:"rank"`
	Layer int    `json:"layer"`
	Error string `json:"error"`
}

// =====================================================================
// RankTransport interface (для тестируемости и loose coupling)
// =====================================================================

// RankTransport — интерфейс, через который coordinator общается с одним rank'ом.
//
// WorkerClient из rpccoordinator реализует этот интерфейс через
// обёртку NewRPCWorkerTransport. Тесты подменяют на mockTransport.
type RankTransport interface {
	// WorkerID возвращает идентификатор worker'а для этого rank'а.
	WorkerID() string
	// Rank возвращает номер rank'а (0..worldSize-1).
	Rank() int
	// InferShard отправляет partial input на этот rank и возвращает partial output.
	// В stub-режиме (без реальной llama.cpp) длина output = len(input)/worldSize.
	InferShard(ctx context.Context, req TPShardInferRequest) (*TPShardInferResponse, error)
	// SyncKVShard сохраняет KV-shard для rank'а.
	SyncKVShard(ctx context.Context, sessionID string, shard []byte) error
	// FetchKVShard читает KV-shard.
	FetchKVShard(ctx context.Context, sessionID string) ([]byte, error)
}

// TPShardInferRequest — запрос на один rank.
type TPShardInferRequest struct {
	ModelName  string `json:"model_name"`
	Rank       int    `json:"rank"`
	WorldSize  int    `json:"world_size"`
	Layer      int    `json:"layer"`
	Input      []byte `json:"input"`
	KVShard    []byte `json:"kv_shard,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	Temperature float32 `json:"temperature,omitempty"`
	MaxTokens  int    `json:"max_tokens,omitempty"`
}

// TPShardInferResponse — ответ от одного rank'а.
type TPShardInferResponse struct {
	Rank        int    `json:"rank"`
	Output      []byte `json:"output"`
	KVShard     []byte `json:"kv_shard,omitempty"`
	NeedsReduce bool   `json:"needs_reduce"`
	LatencyMs   int64  `json:"latency_ms"`
}

// =====================================================================
// Coordinator
// =====================================================================

// TensorParallelCoordinator — параллельный orchestrator для tensor parallelism.
//
// Каждый rank обслуживается через RankTransport. Для каждого слоя:
//  1. Параллельно через goroutines отправляем input на все rank'ы.
//  2. Barrier (sync.WaitGroup) собирает partial outputs.
//  3. allReduce агрегирует результаты → input для следующего слоя.
//  4. Финальный output — после allReduce последнего слоя.
type TensorParallelCoordinator struct {
	mu      sync.RWMutex
	model   *ShardedModel
	ranks   []RankTransport // длина == model.WorldSize
	enabled bool

	// AllReduce strategy (default: AllReduceConcat).
	allReduce AllReduceFunc

	// Metrics (atomic счётчики для экспорта).
	totalInfer  atomic.Int64
	totalErrors atomic.Int64
	totalRanks  atomic.Int64
	sumLatency  atomic.Int64
}

// AllReduceFunc — функция all-reduce для partial outputs.
//
// Определения: AllReduceConcat, AllReduceSumBytes, AllReduceXorBytes
// находятся в allreduce.go.
type AllReduceFunc func(partials map[int][]byte, worldSize int) ([]byte, error)

// CoordinatorConfig — настройки coordinator'а.
//
// По умолчанию coordinator включён (Enabled=true). Чтобы выключить —
// передайте `Disabled: true` либо вызовите Close() после создания.
type CoordinatorConfig struct {
	AllReduce AllReduceFunc // default: AllReduceConcat
	Disabled  bool          // default: false (coordinator enabled)
}

// NewTensorParallelCoordinator — конструктор.
func NewTensorParallelCoordinator(model *ShardedModel, ranks []RankTransport, cfg CoordinatorConfig) (*TensorParallelCoordinator, error) {
	if model == nil {
		return nil, errors.New("rptensor: model is required")
	}
	if err := model.Validate(); err != nil {
		return nil, fmt.Errorf("rptensor: invalid model: %w", err)
	}
	if len(ranks) == 0 {
		return nil, errors.New("rptensor: at least one rank transport required")
	}
	if len(ranks) != model.WorldSize {
		return nil, fmt.Errorf("rptensor: ranks count (%d) != model.WorldSize (%d)",
			len(ranks), model.WorldSize)
	}

	// Проверяем что rank'ы уникальны и покрывают [0..worldSize).
	rankSet := make(map[int]bool, len(ranks))
	for _, r := range ranks {
		if rankSet[r.Rank()] {
			return nil, fmt.Errorf("rptensor: duplicate rank %d", r.Rank())
		}
		rankSet[r.Rank()] = true
	}
	for i := 0; i < model.WorldSize; i++ {
		if !rankSet[i] {
			return nil, fmt.Errorf("rptensor: missing rank %d", i)
		}
	}

	ar := cfg.AllReduce
	if ar == nil {
		ar = AllReduceConcat
	}

	return &TensorParallelCoordinator{
		model:     model,
		ranks:     ranks,
		enabled:   !cfg.Disabled,
		allReduce: ar,
	}, nil
}

// Enabled — статус включения (для /api/v1/rpc/tp/status).
func (c *TensorParallelCoordinator) Enabled() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.enabled
}

// WorldSize — число rank'ов.
func (c *TensorParallelCoordinator) WorldSize() int {
	return c.model.WorldSize
}

// NumLayers — число слоёв модели.
func (c *TensorParallelCoordinator) NumLayers() int {
	return c.model.NumLayers
}

// Stats — счётчики для /metrics endpoint.
type TPStats struct {
	TotalInfer   int64 `json:"total_infer"`
	TotalErrors  int64 `json:"total_errors"`
	TotalRanks   int64 `json:"total_ranks"`
	AvgLatencyMs int64 `json:"avg_latency_ms"`
}

// Stats — снимок счётчиков.
func (c *TensorParallelCoordinator) Stats() TPStats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	total := c.totalInfer.Load()
	sum := c.sumLatency.Load()
	avg := int64(0)
	if total > 0 {
		avg = sum / total
	}
	return TPStats{
		TotalInfer:   total,
		TotalErrors:  c.totalErrors.Load(),
		TotalRanks:   int64(len(c.ranks)),
		AvgLatencyMs: avg,
	}
}

// Infer — параллельное исполнение через все rank'ы по всем слоям.
func (c *TensorParallelCoordinator) Infer(ctx context.Context, req TPInferRequest) (*TPInferResponse, error) {
	if !c.Enabled() {
		return nil, errors.New("rptensor: tensor parallel coordinator is disabled")
	}
	if req.TierCount > 0 && req.TierCount != c.model.WorldSize {
		return nil, fmt.Errorf("rptensor: requested tierCount=%d but coordinator has worldSize=%d",
			req.TierCount, c.model.WorldSize)
	}
	if req.ModelName != "" && req.ModelName != c.model.Name {
		return nil, fmt.Errorf("rptensor: model name mismatch: request=%q, coordinator=%q",
			req.ModelName, c.model.Name)
	}

	c.totalInfer.Add(1)
	start := time.Now()

	currentInput := req.Input
	if len(currentInput) == 0 && req.Prompt != "" {
		currentInput = []byte(req.Prompt)
	}

	layerStats := make([]LayerStat, 0, c.model.NumLayers)
	var rankErrors []RankError
	degraded := false

	for layer := 0; layer < c.model.NumLayers; layer++ {
		layerStart := time.Now()

		partials, errs := c.executeLayer(ctx, layer, currentInput, req)
		layerElapsed := time.Since(layerStart).Milliseconds()

		successful := 0
		for rank := 0; rank < c.model.WorldSize; rank++ {
			if _, ok := partials[rank]; ok {
				successful++
			}
		}
		layerStats = append(layerStats, LayerStat{
			Layer:      layer,
			LatencyMs:  layerElapsed,
			Successful: successful,
			Total:      c.model.WorldSize,
		})

		for rank, err := range errs {
			rankErrors = append(rankErrors, RankError{
				Rank: rank, Layer: layer, Error: err.Error(),
			})
			logger.Get().Warnw("tp layer partial failed",
				"rank", rank, "layer", layer, "error", err.Error())
		}

		if len(partials) == 0 {
			// Все rank'ы упали — прерываем.
			c.totalErrors.Add(1)
			return nil, fmt.Errorf("rptensor: all ranks failed at layer %d", layer)
		}
		if len(partials) < c.model.WorldSize {
			degraded = true
		}

		// AllReduce partials → input для следующего слоя.
		reduced, arErr := c.allReduce(partials, c.model.WorldSize)
		if arErr != nil {
			c.totalErrors.Add(1)
			return nil, fmt.Errorf("rptensor: all-reduce failed at layer %d: %w", layer, arErr)
		}
		currentInput = reduced
	}

	totalElapsed := time.Since(start).Milliseconds()
	c.sumLatency.Add(totalElapsed)

	return &TPInferResponse{
		Output:      string(currentInput),
		OutputBytes: currentInput,
		LatencyMs:   totalElapsed,
		LayerStats:  layerStats,
		RankErrors:  rankErrors,
		Degraded:    degraded,
		WorldSize:   c.model.WorldSize,
		NumLayers:   c.model.NumLayers,
	}, nil
}

// executeLayer — параллельная отправка input на все rank'ы, barrier, сбор partials.
//
// Возвращает partials (rank → output) и errors (rank → error).
// Failed rank'ы отсутствуют в partials, но присутствуют в errors.
func (c *TensorParallelCoordinator) executeLayer(
	ctx context.Context,
	layer int,
	input []byte,
	req TPInferRequest,
) (map[int][]byte, map[int]error) {
	partials := make(map[int][]byte, c.model.WorldSize)
	errs := make(map[int]error)

	var wg sync.WaitGroup
	var partialsMu sync.Mutex

	for _, transport := range c.ranks {
		wg.Add(1)
		go func(t RankTransport) {
			defer wg.Done()

			shardReq := TPShardInferRequest{
				ModelName:   req.ModelName,
				Rank:        t.Rank(),
				WorldSize:   c.model.WorldSize,
				Layer:       layer,
				Input:       input,
				SessionID:   req.SessionID,
				Temperature: 0.0,
				MaxTokens:   0,
			}

			// Подгружаем KV-shard если есть session_id (best-effort).
			if req.SessionID != "" {
				kv, kvErr := t.FetchKVShard(ctx, req.SessionID)
				if kvErr == nil && kv != nil {
					shardReq.KVShard = kv
				}
				// Если ошибка — игнорируем, продолжаем без KV.
			}

			resp, err := t.InferShard(ctx, shardReq)
			partialsMu.Lock()
			defer partialsMu.Unlock()
			if err != nil {
				errs[t.Rank()] = err
				return
			}
			if resp == nil {
				errs[t.Rank()] = errors.New("nil response from rank")
				return
			}
			partials[t.Rank()] = resp.Output

			// Сохраняем KV-shard rank'а обратно (best-effort).
			if req.SessionID != "" && len(resp.KVShard) > 0 {
				if syncErr := t.SyncKVShard(ctx, req.SessionID, resp.KVShard); syncErr != nil {
					logger.Get().Debugw("tp sync kv shard failed",
						"rank", t.Rank(), "session", req.SessionID, "error", syncErr)
				}
			}
		}(transport)
	}

	wg.Wait()
	return partials, errs
}

// Close — закрывает coordinator (в stub-режиме — noop).
func (c *TensorParallelCoordinator) Close() error {
	c.mu.Lock()
	c.enabled = false
	c.mu.Unlock()
	return nil
}