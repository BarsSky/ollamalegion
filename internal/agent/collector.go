package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/protocol"
	"ollama-loadbalancer/pkg/types"
)

// Agent - агент сбора метрик
type Agent struct {
	config         *types.AgentConfig
	httpClient     *http.Client
	metricsSeq     int64
	heartbeatSeq   int64
	startTime      time.Time
	mu             sync.Mutex
	currentMetrics *types.BackendMetrics
	stopChan       chan struct{}
	balancerURL    string
	registered     bool
	platformMode   types.PlatformMode
	gpuUnavailable bool // кэш: GPU недоступна на этой ноде

	// Ollama статистика
	ollamaStats    *OllamaStats
	statsMu        sync.Mutex
	requestHistory []requestRecord

	// Текущие флаги Ollama (кэш)
	currentFlags types.OllamaRuntimeFlags

	// Кэш версии Ollama (TTL 5 минут)
	cachedVersion   string
	cachedVersionAt time.Time
	versionCacheTTL time.Duration

	// Llama-коллектор (для llama.cpp бэкендов)
	llamaCollector *LlamaCollector

	// Health HTTP-сервер (для Docker HEALTHCHECK)
	healthServer *http.Server

	// Log buffer for /api/logs endpoint
	logBufMu    sync.Mutex
	logBuffer   []string
	maxLogLines int
}

// requestRecord - запись о запросе для подсчета RPS
type requestRecord struct {
	timestamp time.Time
	duration  time.Duration
}

// NewAgent - создание нового агента
func NewAgent(config *types.AgentConfig) *Agent {
	a := &Agent{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		startTime:       time.Now(),
		stopChan:        make(chan struct{}),
		balancerURL:     config.BalancerURL,
		requestHistory:  make([]requestRecord, 0),
		versionCacheTTL: 5 * time.Minute,
		logBuffer:       make([]string, 0, 1000),
		maxLogLines:     1000,
	}
	// Инициализация llama-коллектора для llama.cpp бэкендов
	if config.BackendType == types.BackendTypeLlamaCpp {
		cppURL := config.CppWorkerURL
		if cppURL == "" {
			cppURL = "http://localhost:18091"
		}
		a.llamaCollector = NewLlamaCollector(cppURL)
		fmt.Printf("[%s] LlamaCollector initialized for %s\n", time.Now().Format(time.RFC3339), cppURL)
	}
	return a
}

// Start - запуск агента
func (a *Agent) Start() error {
	// Определяем режим платформы
	a.platformMode = a.detectPlatformMode()
	fmt.Printf("[%s] Platform mode detected: %s\n", time.Now().Format(time.RFC3339), a.platformMode)

	// Регистрация на балансировщике
	if err := a.register(); err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}

	a.registered = true

	// Запуск сбора метрик
	go a.collectLoop()

	// Запуск heartbeat
	go a.heartbeatLoop()

	// Запуск health HTTP-сервера (для Docker HEALTHCHECK)
	a.startHealthServer()

	return nil
}

// Stop - остановка агента
func (a *Agent) Stop() {
	close(a.stopChan)
	// Остановка health-сервера
	if a.healthServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.healthServer.Shutdown(ctx)
	}
}

// detectPlatformMode - runtime автоопределение GPU/CPU режима
func (a *Agent) detectPlatformMode() types.PlatformMode {
	// Если явно задан режим
	if a.config.GPUMode != types.ModeAuto {
		fmt.Printf("[%s] Platform mode override: %s\n", time.Now().Format(time.RFC3339), a.config.GPUMode)
		return a.config.GPUMode
	}

	// Проверяем nvidia-smi
	if _, err := exec.LookPath("nvidia-smi"); err == nil {
		// Пробуем выполнить
		cmd := exec.Command("nvidia-smi", "--query-gpu=name", "--format=csv,noheader")
		if out, err := cmd.Output(); err == nil && len(bytes.TrimSpace(out)) > 0 {
			fmt.Printf("[%s] nvidia-smi found, GPU mode detected\n", time.Now().Format(time.RFC3339))
			return types.ModeGPU
		}
	}

	// Проверяем NVML (если собрано с тегом)
	if nvmlAvailable() {
		fmt.Printf("[%s] NVML available, GPU mode detected\n", time.Now().Format(time.RFC3339))
		return types.ModeGPU
	}

	// Проверяем /dev/nvidia* (Linux)
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/nvidia0"); err == nil {
			fmt.Printf("[%s] /dev/nvidia0 found, GPU mode detected\n", time.Now().Format(time.RFC3339))
			return types.ModeGPU
		}
	}

	fmt.Printf("[%s] No GPU detected, CPU mode\n", time.Now().Format(time.RFC3339))
	return types.ModeCPU
}

// minCollectInterval — минимальный допустимый интервал сбора метрик
// для предотвращения DDoS Ollama API со стороны агента
const minCollectInterval = 10 * time.Second

// collectLoop - цикл сбора метрик
func (a *Agent) collectLoop() {
	interval := time.Duration(a.config.CollectInterval) * time.Second
	if interval < minCollectInterval {
		// Защита от слишком частого опроса: не чаще чем раз в 10 секунд
		fmt.Printf("[%s] WARNING: collect interval %v is below minimum %v, clamping to %v\n",
			time.Now().Format(time.RFC3339), interval, minCollectInterval, minCollectInterval)
		interval = minCollectInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Первый сбор сразу
	a.collectAndSend()

	for {
		select {
		case <-ticker.C:
			a.collectAndSend()
		case <-a.stopChan:
			return
		}
	}
}

// collectAndSend - сбор и отправка метрик
func (a *Agent) collectAndSend() {
	metrics := a.collectMetrics()

	// Логирование собранных метрик для диагностики
	if a.platformMode == types.ModeGPU {
		fmt.Printf("[%s] Metrics collected: CPU=%.1f%% RAM=%d/%dMB GPU=%.1f%% GPU_Mem=%d/%dMB RPS=%.1f Models=%d\n",
			time.Now().Format(time.RFC3339),
			metrics.System.CPUUsagePercent,
			metrics.System.MemoryUsed, metrics.System.MemoryTotal,
			metrics.GPU.UsagePercent,
			metrics.GPU.MemoryUsed, metrics.GPU.MemoryTotal,
			metrics.Ollama.RequestsPerSecond,
			len(metrics.Ollama.RunningModels),
		)
	} else {
		fmt.Printf("[%s] Metrics collected: CPU=%.1f%% Load=%.2f RAM=%d/%dMB RPS=%.1f Models=%d\n",
			time.Now().Format(time.RFC3339),
			metrics.System.CPUUsagePercent,
			metrics.System.CPU.LoadAverage1,
			metrics.System.MemoryUsed, metrics.System.MemoryTotal,
			metrics.Ollama.RequestsPerSecond,
			len(metrics.Ollama.RunningModels),
		)
	}

	a.mu.Lock()
	a.metricsSeq++
	seq := a.metricsSeq
	a.currentMetrics = metrics
	a.mu.Unlock()

	_ = protocol.NewMetricsMessage(a.config.AgentID, seq, metrics)

	// Отправляем метрики напрямую как BackendMetrics (без wrapper)
	data, err := json.Marshal(metrics)
	if err != nil {
		fmt.Printf("[%s] Failed to marshal metrics: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	a.sendMetrics(data)
}

// sendMetrics - отправка метрик на балансировщик
func (a *Agent) sendMetrics(data []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/agents/metrics", a.balancerURL),
		bytes.NewReader(data))
	if err != nil {
		fmt.Printf("[%s] Failed to create request: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", a.config.AgentID)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[%s] Failed to send metrics: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("[%s] Metrics send failed with status %d: %s\n",
			time.Now().Format(time.RFC3339), resp.StatusCode, string(body))
	}
}

// heartbeatLoop - цикл heartbeat
func (a *Agent) heartbeatLoop() {
	interval := time.Duration(a.heartbeatInterval()) * time.Second
	if interval == 0 {
		interval = 3 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.sendHeartbeat()
		case <-a.stopChan:
			return
		}
	}
}

// heartbeatInterval - интервал heartbeat из конфига
func (a *Agent) heartbeatInterval() int {
	if a.config.HeartbeatInterval > 0 {
		return a.config.HeartbeatInterval
	}
	return 5
}

// sendHeartbeat - отправка heartbeat
func (a *Agent) sendHeartbeat() {
	uptime := int64(time.Since(a.startTime).Seconds())

	a.mu.Lock()
	a.heartbeatSeq++
	seq := a.heartbeatSeq
	status := "healthy"

	if a.currentMetrics != nil {
		if a.platformMode == types.ModeGPU {
			// GPU: degraded при высокой загрузке GPU или VRAM
			if a.currentMetrics.GPU.UsagePercent > 95 ||
				(a.currentMetrics.GPU.MemoryTotal > 0 && float64(a.currentMetrics.GPU.MemoryUsed)/float64(a.currentMetrics.GPU.MemoryTotal)*100 > 95) ||
				a.currentMetrics.GPU.Temperature > 90 {
				status = "degraded"
			}
		} else {
			// CPU: degraded при высоком load average или RAM
			cpu := a.currentMetrics.System.CPU
			ramPercent := float64(0)
			if a.currentMetrics.System.MemoryTotal > 0 {
				ramPercent = float64(a.currentMetrics.System.MemoryUsed) / float64(a.currentMetrics.System.MemoryTotal) * 100
			}
			if (cpu.CoreCount > 0 && cpu.LoadAverage1 > float64(cpu.CoreCount)*2) || ramPercent > 95 {
				status = "degraded"
			}
		}
	}
	a.mu.Unlock()

	_ = protocol.NewHeartbeatMessage(a.config.AgentID, seq, uptime, status, a.config.Weight)

	// Сериализация сообщения с локальными лимитами
	wrapper := map[string]interface{}{
		"type":                  "heartbeat",
		"agentId":               a.config.AgentID,
		"timestamp":             time.Now().UTC(),
		"uptime":                uptime,
		"sequence":              seq,
		"status":                status,
		"platform":              a.platformMode,
		"weight":                a.config.Weight,
		"maxConcurrentRequests": a.config.MaxConcurrentRequests,
		"maxModels":             a.config.MaxModels,
		"backendType":           string(a.config.BackendType),
		"nodeLabels":            a.config.NodeLabels,
	}

	data, err := json.Marshal(wrapper)
	if err != nil {
		fmt.Printf("[%s] Failed to marshal heartbeat: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/agents/heartbeat", a.balancerURL),
		bytes.NewReader(data))
	if err != nil {
		fmt.Printf("[%s] Failed to create heartbeat request: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent-ID", a.config.AgentID)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		fmt.Printf("[%s] Failed to send heartbeat: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	defer resp.Body.Close()

	// Принимаем runtime-лимиты из ответа балансера
	if resp.Body != nil {
		var respBody map[string]interface{}
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		if err := json.Unmarshal(bodyBytes, &respBody); err == nil {
			if cfg, ok := respBody["config"].(map[string]interface{}); ok {
				if maxConcurrent, ok := cfg["maxConcurrentRequests"].(float64); ok {
					if maxConcurrent > 0 {
						a.config.MaxConcurrentRequests = int(maxConcurrent)
						fmt.Printf("[%s] Heartbeat response: applied maxConcurrentRequests=%d from balancer\n",
							time.Now().Format(time.RFC3339), int(maxConcurrent))
					} else {
						fmt.Printf("[%s] Heartbeat response: keeping local maxConcurrentRequests=%d (balancer returned %.0f)\n",
							time.Now().Format(time.RFC3339), a.config.MaxConcurrentRequests, maxConcurrent)
					}
				}
				if maxModels, ok := cfg["maxModels"].(float64); ok {
					if maxModels > 0 {
						a.config.MaxModels = int(maxModels)
					} else {
						fmt.Printf("[%s] Heartbeat response: keeping local maxModels=%d (balancer returned %.0f)\n",
							time.Now().Format(time.RFC3339), a.config.MaxModels, maxModels)
					}
				}
			}

			// Принимаем желаемую конфигурацию Ollama из ответа балансера
			if ollamaCfg, ok := respBody["ollamaConfig"].(map[string]interface{}); ok {
				applied := false
				flags := a.desiredOllamaFlags()

				if v, ok := ollamaCfg["numGpuLayers"].(float64); ok && int(v) >= 0 {
					flags.NumGPULayers = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["contextLength"].(float64); ok && int(v) > 0 {
					flags.ContextLength = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["numParallel"].(float64); ok && int(v) > 0 {
					flags.NumParallel = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["numThreads"].(float64); ok && int(v) > 0 {
					flags.NumThreads = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["batchSize"].(float64); ok && int(v) > 0 {
					flags.BatchSize = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["maxLoadedModels"].(float64); ok && int(v) >= 0 {
					flags.MaxLoadedModels = int(v)
					applied = true
				}
				if v, ok := ollamaCfg["flashAttention"].(bool); ok {
					flags.FlashAttention = v
					applied = true
				}
				if v, ok := ollamaCfg["kvCacheQuant"].(string); ok && v != "" {
					flags.KVCacheQuant = v
					applied = true
				}

				if applied {
					if err := a.applyOllamaConfig(flags); err != nil {
						fmt.Printf("[%s] Heartbeat response: failed to apply ollamaConfig: %v\n",
							time.Now().Format(time.RFC3339), err)
					} else {
						fmt.Printf("[%s] Heartbeat response: applied ollamaConfig (ngl=%d, ctx=%d, parallel=%d, threads=%d, batch=%d, maxModels=%d)\n",
							time.Now().Format(time.RFC3339), flags.NumGPULayers, flags.ContextLength,
							flags.NumParallel, flags.NumThreads, flags.BatchSize, flags.MaxLoadedModels)
					}
				}
			}
		}
	}
}

// collectMetrics - сбор всех метрик
func (a *Agent) collectMetrics() *types.BackendMetrics {
	now := time.Now().UTC()

	metrics := &types.BackendMetrics{
		ID:        a.config.AgentID,
		Timestamp: now,
	}

	// Для llama.cpp бэкендов используем LlamaCollector вместо Ollama
	if a.config.BackendType == types.BackendTypeLlamaCpp && a.llamaCollector != nil {
		return a.collectLlamaMetrics(metrics)
	}

	// Сбор GPU метрик (только для GPU нод)
	if a.platformMode == types.ModeGPU {
		metrics.GPU = a.collectGPUMetrics()
	} else {
		metrics.GPU = types.GPUMetrics{}
	}

	// Сбор системных метрик
	metrics.System = a.collectSystemMetrics()

	// Сбор Ollama метрик
	metrics.Ollama = a.collectOllamaMetrics()

	return metrics
}

// collectLlamaMetrics — сбор метрик через LlamaCollector для llama.cpp бэкендов
func (a *Agent) collectLlamaMetrics(base *types.BackendMetrics) *types.BackendMetrics {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	llamaMetrics, err := a.llamaCollector.Collect(ctx)
	if err != nil {
		fmt.Printf("[%s] LlamaCollector failed: %v, falling back to system metrics\n",
			time.Now().Format(time.RFC3339), err)
		// Fallback: системные метрики
		base.System = a.collectSystemMetrics()
		base.Ollama = types.OllamaMetrics{RunningModels: []types.RunningModel{}}
		return base
	}

	// Маппим LlamaMetrics → BackendMetrics
	// GPU: приоритет — данные от CppWorker (llamaMetrics.GPUMetrics имеет актуальную информацию),
	// затем nvidia-smi/NVML (collectGPUMetrics), только если platformMode == GPU.
	if len(llamaMetrics.GPUMetrics) > 0 {
		base.GPU = mapLlamaGPUMetrics(llamaMetrics.GPUMetrics)
	} else if a.platformMode == types.ModeGPU {
		base.GPU = a.collectGPUMetrics()
	}
	base.System = a.collectSystemMetrics()

	// Конвертируем Llama-модели в RunningModel
	var runningModels []types.RunningModel
	for _, m := range llamaMetrics.Models {
		runningModels = append(runningModels, types.RunningModel{
			Name:   m.Name,
			Size:   uint64(m.SizeBytes),
			Format: "gguf",
		})
	}

	// Заполняем Ollama-метрики из llama.cpp данных
	base.Ollama = types.OllamaMetrics{
		RunningModels:     runningModels,
		RequestsPerSecond: llamaMetrics.RequestsRPS,
	}

	// Устанавливаем BackendType в метрики для корректной фильтрации
	base.BackendType = types.BackendTypeLlamaCpp
	base.Engine = types.EngineLlamaCPP

	return base
}

// mapLlamaGPUMetrics конвертирует Llama LlamaGPUInfo → types.GPUMetrics
func mapLlamaGPUMetrics(gpus []LlamaGPUInfo) types.GPUMetrics {
	if len(gpus) == 0 {
		return types.GPUMetrics{}
	}
	var totalVRAM uint64
	for _, g := range gpus {
		totalVRAM += uint64(g.VRAMTotalMB)
	}
	var usedVRAM uint64
	for _, g := range gpus {
		free := g.VRAMFreeMB
		if free > g.VRAMTotalMB {
			free = g.VRAMTotalMB
		}
		usedVRAM += uint64(g.VRAMTotalMB - free)
	}
	return types.GPUMetrics{
		UsagePercent: 0, // llama.cpp не даёт процент загрузки GPU
		MemoryTotal:  totalVRAM,
		MemoryUsed:   usedVRAM,
		MemoryFree:   totalVRAM - usedVRAM,
		Temperature:  0,
		PowerUsage:   0,
		PowerLimit:   0,
		GPUClock:     0,
		MemClock:     0,
	}
}
