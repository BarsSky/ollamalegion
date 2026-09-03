// Package agent — коллектор метрик для llama.cpp (CppWorker)
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// LlamaCollector — сборщик метрик с CppWorker (llama.cpp)
type LlamaCollector struct {
	cppWorkerURL    string
	httpClient      *http.Client
	lastMetrics     *LlamaMetrics
	lastCollectTime time.Time
}

// LlamaMetrics — метрики llama.cpp
type LlamaMetrics struct {
	GPUMetrics  []LlamaGPUInfo `json:"gpuMetrics,omitempty"`
	Models      []ModelInfo    `json:"models,omitempty"`
	ModelCount  int            `json:"modelCount"`
	Uptime      string         `json:"uptime"`
	Version     string         `json:"version"`
	GPUCount    int            `json:"gpuCount"`
	RequestsRPS float64        `json:"requestsRps"`
}

// LlamaGPUInfo — информация о GPU из llama.cpp CppWorker
type LlamaGPUInfo struct {
	Index       int                 `json:"index"`
	Name        string              `json:"name"`
	VRAMTotalMB int                 `json:"vramTotalMB"`
	VRAMFreeMB  int                 `json:"vramFreeMB"`
	Metrics     map[string]float64  `json:"metrics,omitempty"`
}

// ModelInfo — информация о загруженной модели
type ModelInfo struct {
	Name         string `json:"name"`
	Architecture string `json:"architecture"`
	SizeBytes    int64  `json:"sizeBytes"`
	LoadedAt     string `json:"loadedAt"`
}

// NewLlamaCollector создаёт коллектор для llama.cpp
func NewLlamaCollector(cppWorkerURL string) *LlamaCollector {
	return &LlamaCollector{
		cppWorkerURL: cppWorkerURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// Collect собирает метрики с CppWorker.
//
// R58 (2026-09-03): 3 endpoint calls (gpu / models / info) теперь в ПАРАЛЛЕЛЬ
// (sync.WaitGroup) вместо последовательного. Ускоряет collect cycle в ~3x
// (раньше: t_gpu + t_models + t_info; теперь: max(t_gpu, t_models, t_info)).
//
// Каждый call best-effort — ошибка одного endpoint не прерывает остальные
// (поведение сохранено с pre-R58). Если все 3 упадут, Collect вернёт
// (zero-value metrics, nil error) — это документировано в тестах.
//
// Context cancellation работает: ctx.Done() приходит во все 3 горутины
// одновременно (через NewRequestWithContext в каждой).
func (lc *LlamaCollector) Collect(ctx context.Context) (*LlamaMetrics, error) {
	metrics := &LlamaMetrics{}

	// Параллельный запуск 3 endpoint calls.
	var (
		wg     sync.WaitGroup
		gpuR   *gpuResponse
		modR   *modelsResponse
		infoR  *infoResponse
	)

	wg.Add(3)
	go func() {
		defer wg.Done()
		// GPU-метрики опциональны — продолжаем без них при ошибке
		r, err := lc.collectGPUMetrics(ctx)
		if err == nil {
			gpuR = r
		}
	}()
	go func() {
		defer wg.Done()
		// Модели опциональны
		r, err := lc.collectModels(ctx)
		if err == nil {
			modR = r
		}
	}()
	go func() {
		defer wg.Done()
		// Info — version + uptime
		r, err := lc.collectInfo(ctx)
		if err == nil {
			infoR = r
		}
	}()
	wg.Wait()

	// Сбор результатов (каждый optional).
	if gpuR != nil {
		metrics.GPUMetrics = gpuR.Devices
		metrics.GPUCount = gpuR.GPUCount
	}
	if modR != nil {
		metrics.Models = modR.Models
		metrics.ModelCount = modR.Count
	}
	if infoR != nil {
		metrics.Uptime = infoR.Uptime
		metrics.Version = infoR.Version
	}

	lc.lastMetrics = metrics
	lc.lastCollectTime = time.Now()
	return metrics, nil
}

// HealthCheck проверяет доступность CppWorker
func (lc *LlamaCollector) HealthCheck(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/cppworker/health", lc.cppWorkerURL), nil)
	if err != nil {
		return false
	}
	resp, err := lc.httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// collectGPUMetrics собирает GPU метрики с /api/gpu
func (lc *LlamaCollector) collectGPUMetrics(ctx context.Context) (*gpuResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/gpu", lc.cppWorkerURL), nil)
	if err != nil {
		return nil, err
	}
	resp, err := lc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gpu metrics: status %d", resp.StatusCode)
	}
	var result gpuResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

type gpuResponse struct {
	GPUCount int            `json:"gpuCount"`
	Devices  []LlamaGPUInfo `json:"devices"`
}

// collectModels собирает список загруженных моделей с /api/models
func (lc *LlamaCollector) collectModels(ctx context.Context) (*modelsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/models", lc.cppWorkerURL), nil)
	if err != nil {
		return nil, err
	}
	resp, err := lc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: status %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var result modelsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

type modelsResponse struct {
	Models []ModelInfo `json:"models"`
	Count  int         `json:"count"`
}

// collectInfo собирает общую информацию с /api/info
func (lc *LlamaCollector) collectInfo(ctx context.Context) (*infoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/info", lc.cppWorkerURL), nil)
	if err != nil {
		return nil, err
	}
	resp, err := lc.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("info: status %d", resp.StatusCode)
	}
	var result infoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

type infoResponse struct {
	Uptime  string `json:"uptime"`
	Version string `json:"version"`
}

// GetLastMetrics возвращает последние собранные метрики
func (lc *LlamaCollector) GetLastMetrics() *LlamaMetrics {
	return lc.lastMetrics
}

// GetLastCollectTime возвращает время последнего сбора
func (lc *LlamaCollector) GetLastCollectTime() time.Time {
	return lc.lastCollectTime
}