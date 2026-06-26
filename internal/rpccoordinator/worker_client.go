// Package rpccoordinator — Worker Client для взаимодействия с RPC worker'ами.
package rpccoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/internal/rptensor"
)

// WorkerMetricsSnapshot — кэшированные метрики worker'а для selector'а.
//
// Обновляется через WorkerClient.RefreshMetrics() (вызывается из HeartbeatLoop).
// Selector читает атомарно через LastMetrics.Load().
type WorkerMetricsSnapshot struct {
	ActiveRequests int64
	Capacity       int64
	LoadedSlices   int64
	TotalInfer     int64
	TotalErrors    int64
	UpdatedAt      time.Time
}

// WorkerClient — HTTP/gRPC клиент для вызова worker'а.
type WorkerClient struct {
	WorkerID    string
	Host        string
	Port        int
	Protocol    string // "http" | "grpc"
	SliceLayers string // "1-40" — какие слои обслуживает
	httpClient  *http.Client
	mu          sync.RWMutex
	healthy     bool
	lastCheck   time.Time

	// LastMetrics — кэш метрик для selector'а (B6).
	LastMetrics atomic.Value // WorkerMetricsSnapshot
}

// NewWorkerClient создаёт новый клиент worker'а.
func NewWorkerClient(workerID, host string, port int, protocol string, httpClient *http.Client) *WorkerClient {
	if protocol == "" {
		protocol = "http"
	}
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}
	return &WorkerClient{
		WorkerID:   workerID,
		Host:       host,
		Port:       port,
		Protocol:   protocol,
		httpClient: httpClient,
		healthy:    true,
		lastCheck:  time.Now(),
	}
}

// BaseURL возвращает базовый URL worker'а.
func (wc *WorkerClient) BaseURL() string {
	return fmt.Sprintf("http://%s:%d", wc.Host, wc.Port)
}

// HealthCheck проверяет доступность worker'а.
func (wc *WorkerClient) HealthCheck() (bool, error) {
	url := fmt.Sprintf("%s/rpc/health", wc.BaseURL())
	resp, err := wc.httpClient.Get(url)
	if err != nil {
		wc.setHealthy(false)
		return false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		wc.setHealthy(false)
		return false, fmt.Errorf("health check returned %d", resp.StatusCode)
	}

	wc.setHealthy(true)
	return true, nil
}

// LoadSlice загружает срез модели на worker.
func (wc *WorkerClient) LoadSlice(ctx context.Context, modelName string, layers string) error {
	url := fmt.Sprintf("%s/rpc/load", wc.BaseURL())
	reqBody := map[string]interface{}{
		"model_name": modelName,
		"layers":     layers,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := wc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("load slice failed: %d: %s", resp.StatusCode, string(body))
	}

	wc.SliceLayers = layers
	return nil
}

// UnloadSlice выгружает срез модели.
func (wc *WorkerClient) UnloadSlice(ctx context.Context, modelName string) error {
	url := fmt.Sprintf("%s/rpc/unload", wc.BaseURL())
	reqBody := map[string]interface{}{
		"model_name": modelName,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := wc.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unload slice failed: %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// InferSlice выполняет inference среза модели.
func (wc *WorkerClient) InferSlice(ctx context.Context, req SliceInferRequest) ([]byte, error) {
	url := fmt.Sprintf("%s/rpc/infer", wc.BaseURL())
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := wc.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("infer slice failed: %d: %s", resp.StatusCode, string(respBody))
	}

	return io.ReadAll(resp.Body)
}

// GetMetrics получает метрики worker'а.
func (wc *WorkerClient) GetMetrics() (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/rpc/metrics", wc.BaseURL())
	resp, err := wc.httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics returned %d", resp.StatusCode)
	}

	var metrics map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&metrics); err != nil {
		return nil, err
	}
	return metrics, nil
}

// IsHealthy возвращает последний статус здоровья.
func (wc *WorkerClient) IsHealthy() bool {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.healthy
}

func (wc *WorkerClient) setHealthy(v bool) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	wc.healthy = v
	wc.lastCheck = time.Now()
}

// LastCheck возвращает время последней проверки.
func (wc *WorkerClient) LastCheck() time.Time {
	wc.mu.RLock()
	defer wc.mu.RUnlock()
	return wc.lastCheck
}

// TPInferSlice — B8: tensor parallelism partial inference.
//
// Шлёт POST /rpc/tp/infer на worker'а для конкретного rank'а.
// Worker возвращает partial output длиной len(input)/worldSize с
// rank-маркированными байтами (в stub-режиме) или реальным partial
// tensor'ом (в production с реальной llama.cpp интеграцией).
//
// Метод НЕ выполняет all-reduce — только отправляет shard на один rank.
// Для параллельной обработки всех rank'ов используй TensorParallelCoordinator.
func (wc *WorkerClient) TPInferSlice(ctx context.Context, req rptensor.TPShardInferRequest) (*rptensor.TPShardInferResponse, error) {
	url := fmt.Sprintf("%s/rpc/tp/infer", wc.BaseURL())
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := wc.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tp infer failed: %d: %s", resp.StatusCode, string(respBody))
	}

	var out rptensor.TPShardInferResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tp infer decode: %w", err)
	}
	return &out, nil
}

// TPKvSync — B8: сохранить KV-shard для rank'а сессии.
//
// Шлёт POST /rpc/tp/kv_sync с rank-keyed shard. В stub-режиме worker
// хранит в in-memory KVStore; в production — сериализует ggml cache.
func (wc *WorkerClient) TPKvSync(ctx context.Context, sessionID string, rank int, shard []byte) error {
	url := fmt.Sprintf("%s/rpc/tp/kv_sync", wc.BaseURL())
	body, err := json.Marshal(map[string]interface{}{
		"session_id": sessionID,
		"rank":       rank,
		"shard":      shard,
	})
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := wc.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("tp kv_sync failed: %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// TPKvFetch — B8: получить KV-shard для rank'а сессии.
//
// Шлёт GET /rpc/tp/kv_fetch?session_id=X&rank=N. nil byte-slice и nil error
// если shard не найден (404 → возвращается (nil, nil) без ошибки).
func (wc *WorkerClient) TPKvFetch(ctx context.Context, sessionID string, rank int) ([]byte, error) {
	url := fmt.Sprintf("%s/rpc/tp/kv_fetch?session_id=%s&rank=%d",
		wc.BaseURL(), sessionID, rank)
	httpReq, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := wc.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // shard не найден — не ошибка
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tp kv_fetch failed: %d: %s", resp.StatusCode, string(respBody))
	}

	var out struct {
		Shard []byte `json:"shard"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tp kv_fetch decode: %w", err)
	}
	return out.Shard, nil
}

// RefreshMetrics обновляет кэш метрик (B6) через GET /rpc/metrics.
//
// Используется HeartbeatLoop или другими периодическими задачами.
// Обновляет LastMetrics атомарно.
//
// При ошибке не паникует — возвращает error и оставляет кэш как есть.
func (wc *WorkerClient) RefreshMetrics(ctx context.Context) error {
	metrics, err := wc.GetMetrics()
	if err != nil {
		return fmt.Errorf("refresh metrics: %w", err)
	}

	snap := WorkerMetricsSnapshot{UpdatedAt: time.Now()}
	if v, ok := metrics["active_requests"].(float64); ok {
		snap.ActiveRequests = int64(v)
	}
	if v, ok := metrics["loaded_slices"].(float64); ok {
		snap.LoadedSlices = int64(v)
	}
	if v, ok := metrics["total_infer"].(float64); ok {
		snap.TotalInfer = int64(v)
	}
	if v, ok := metrics["total_errors"].(float64); ok {
		snap.TotalErrors = int64(v)
	}
	// Capacity по умолчанию — 1. Можно расширить через worker config.
	snap.Capacity = 1

	wc.LastMetrics.Store(snap)
	return nil
}
