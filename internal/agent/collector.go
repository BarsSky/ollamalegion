package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
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
	currentFlags   types.OllamaRuntimeFlags

	// Кэш версии Ollama (TTL 5 минут)
	cachedVersion      string
	cachedVersionAt    time.Time
	versionCacheTTL    time.Duration

	// Health HTTP-сервер (для Docker HEALTHCHECK)
	healthServer *http.Server
}

// requestRecord - запись о запросе для подсчета RPS
type requestRecord struct {
	timestamp time.Time
	duration  time.Duration
}

// NewAgent - создание нового агента
func NewAgent(config *types.AgentConfig) *Agent {
	return &Agent{
		config:         config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		startTime:       time.Now(),
		stopChan:        make(chan struct{}),
		balancerURL:     config.BalancerURL,
		requestHistory:  make([]requestRecord, 0),
		versionCacheTTL: 5 * time.Minute,
	}
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

// startHealthServer — запускает HTTP-сервер с /health эндпоинтом для Docker HEALTHCHECK
func (a *Agent) startHealthServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	addr := fmt.Sprintf(":%d", a.config.MetricsPort)
	a.healthServer = &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	go func() {
		fmt.Printf("[%s] Health server listening on %s\n", time.Now().Format(time.RFC3339), addr)
		if err := a.healthServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fmt.Printf("[%s] Health server error: %v\n", time.Now().Format(time.RFC3339), err)
		}
	}()
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

// getPublicHost - определение публичного хоста для регистрации
// Приоритет: AGENT_PUBLIC_HOST > auto-detected IP > hostname
func (a *Agent) getPublicHost() string {
	// Если явно задан PublicHost — используем его
	if a.config.PublicHost != "" {
		return a.config.PublicHost
	}

	// Попытка получить публичный IP
	publicIP := a.getOutboundIP()
	if publicIP != "" && publicIP != "127.0.0.1" {
		return publicIP
	}

	// Fallback на hostname
	hostname, _ := os.Hostname()
	if hostname != "" {
		return hostname
	}

	return "localhost"
}

// getOutboundIP - получение исходящего IP адреса (публичного интерфейса)
func (a *Agent) getOutboundIP() string {
	// Подключаемся к произвольному внешнему адресу для определения исходящего интерфейса
	// Используем адрес балансера если он доступен
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)
	if localAddr == nil {
		return ""
	}
	return localAddr.IP.String()
}

// register - регистрация на балансировщике
func (a *Agent) register() error {
	// Сбор информации о системе
	hostname, _ := os.Hostname()
	osName := os.Getenv("OS")
	if osName == "" {
		osName = "linux"
	}

	gpuInfo := a.collectGPUInfo()
	publicHost := a.getPublicHost()

	fmt.Printf("[%s] Detected public host for registration: %s\n",
		time.Now().Format(time.RFC3339), publicHost)

	// Извлекаем порт Ollama из конфигурации (OLLAMA_URL)
	ollamaPort := a.extractOllamaPort()

	// Отправляем регистрацию напрямую в формате, который ожидает балансировщик
	reqBody := map[string]interface{}{
		"agentId":    a.config.AgentID,
		"hostname":   hostname,
		"host":       publicHost,
		"ollamaPort": ollamaPort,
		"agentPort":  a.config.MetricsPort,
		"gpuCount":   gpuInfo.Count,
		"name":       a.config.AgentID,
		"labels":     []string{osName, "amd64", string(a.platformMode)},
		"weight":     a.config.Weight,
	}

	data, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("failed to marshal registration: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("%s/api/v1/agents/register", a.balancerURL),
		bytes.NewReader(data))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Agent-ID", a.config.AgentID)

	resp, err := a.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registration failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Балансировщик возвращает {success, action, agentId, backend, message}
	var registerResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
		return err
	}

	if success, ok := registerResp["success"].(bool); ok && !success {
		errorMsg := "unknown"
		if msg, ok := registerResp["error"].(string); ok {
			errorMsg = msg
		}
		return fmt.Errorf("registration rejected: %s", errorMsg)
	}

	fmt.Printf("[%s] Agent registered successfully: %s\n",
		time.Now().Format(time.RFC3339), a.config.AgentID)
	return nil
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
	return 3
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

	// Сериализация сообщения
	wrapper := map[string]interface{}{
		"type":      "heartbeat",
		"agentId":   a.config.AgentID,
		"timestamp": time.Now().UTC(),
		"uptime":    uptime,
		"sequence":  seq,
		"status":    status,
		"platform":  a.platformMode,
		"weight":    a.config.Weight,
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
}

// collectMetrics - сбор всех метрик
func (a *Agent) collectMetrics() *types.BackendMetrics {
	now := time.Now().UTC()

	metrics := &types.BackendMetrics{
		ID:        a.config.AgentID,
		Timestamp: now,
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

// GPUInfo - информация о GPU
type GPUInfo struct {
	Count  int      `json:"count"`
	Models []string `json:"models"`
}

// collectGPUInfo - сбор информации о GPU
func (a *Agent) collectGPUInfo() GPUInfo {
	info := GPUInfo{}

	// Если уже определено что GPU недоступна — сразу возвращаем пустой результат
	if a.gpuUnavailable {
		return info
	}

	// Попытка получить информацию через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// Если nvidia-smi недоступен, пробуем через NVML
		return a.collectGPUInfoNVML()
	}

	// Парсинг вывода nvidia-smi
	info.Count = countGPUs(output)
	info.Models = parseGPUMModels(output)

	return info
}

// collectGPUInfoNVML - сбор информации через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)

// collectGPUMetrics - сбор метрик GPU
func (a *Agent) collectGPUMetrics() types.GPUMetrics {
	metrics := types.GPUMetrics{}

	// Если уже определено что GPU недоступна — сразу возвращаем пустой результат
	if a.gpuUnavailable {
		return metrics
	}

	// Попытка получить метрики через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
		// Кэшируем недоступность GPU для последующих циклов
		a.gpuUnavailable = true
		fmt.Printf("[%s] nvidia-smi unavailable, GPU metrics disabled\n", time.Now().Format(time.RFC3339))
		return a.collectGPUMetricsNVML()
	}

	// Парсинг вывода nvidia-smi
	metrics = parseNvidiaSmiOutput(output)

	return metrics
}

// collectGPUMetricsNVML - сбор метрик через NVML
// Реализация в файле nvml_unix.go (Linux/Darwin) или nvml_windows.go (Windows stub)

// collectSystemMetrics - сбор системных метрик
func (a *Agent) collectSystemMetrics() types.SystemMetrics {
	metrics := types.SystemMetrics{}

	// CPU usage
	metrics.CPUUsagePercent = getCPUUsage()

	// Расширенные CPU метрики
	metrics.CPU = getCPUMetrics()

	// Memory
	total, used, free := getMemoryInfo()
	metrics.MemoryTotal = total
	metrics.MemoryUsed = used
	metrics.MemoryFree = free

	// Disk
	diskTotal, diskUsed, diskFree := getDiskInfo()
	metrics.DiskTotal = diskTotal
	metrics.DiskUsed = diskUsed
	metrics.DiskFree = diskFree

	// Network
	rx, tx := getNetworkIO()
	metrics.NetworkRX = rx
	metrics.NetworkTX = tx

	return metrics
}

// getOllamaBaseURL - базовый URL Ollama из конфигурации
func (a *Agent) getOllamaBaseURL() string {
	if a.config.OllamaURL != "" {
		return a.config.OllamaURL
	}
	return "http://localhost:11434"
}

// extractOllamaPort - извлечение порта Ollama из OllamaURL
func (a *Agent) extractOllamaPort() int {
	if a.config.OllamaURL != "" {
		// Парсим URL вида http://host:port или host:port
		urlStr := a.config.OllamaURL
		if !strings.HasPrefix(urlStr, "http://") && !strings.HasPrefix(urlStr, "https://") {
			urlStr = "http://" + urlStr
		}
		if u, err := url.Parse(urlStr); err == nil && u.Port() != "" {
			if port, err := strconv.Atoi(u.Port()); err == nil {
				return port
			}
		}
	}
	return 11434 // fallback
}

// healthCheckOllama - проверка доступности Ollama перед регистрацией
func (a *Agent) healthCheckOllama() error {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}
	resp, err := client.Get(fmt.Sprintf("%s/api/version", a.getOllamaBaseURL()))
	if err != nil {
		return fmt.Errorf("ollama health-check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama health-check returned status %d", resp.StatusCode)
	}
	return nil
}

// collectOllamaMetrics - сбор метрик Ollama
func (a *Agent) collectOllamaMetrics() types.OllamaMetrics {
	metrics := types.OllamaMetrics{}

	// Устанавливаем лимиты из конфигурации (-1 = не задано)
	metrics.MaxModels = a.config.MaxModels
	metrics.MaxConcurrentRequests = a.config.MaxConcurrentRequests

	// Сбор флагов запуска Ollama
	flags := a.collectOllamaRuntimeFlags()
	metrics.RuntimeFlags = flags
	a.currentFlags = flags

	// Получение запущенных (загруженных в память) моделей через /api/ps
	runningModels, err := a.getRunningModels()
	if err != nil {
		fmt.Printf("[%s] Failed to get running models: %v\n", time.Now().Format(time.RFC3339), err)
	} else {
		metrics.RunningModels = runningModels
	}

	// Получение доступных моделей через /api/tags
	availableModels, err := a.getAvailableModels()
	if err != nil {
		fmt.Printf("[%s] Failed to get available models: %v\n", time.Now().Format(time.RFC3339), err)
	} else {
		metrics.AvailableModels = availableModels
	}

	// Сбор информации о контексте моделей
	if len(runningModels) > 0 {
		metrics.ModelContexts = a.collectModelContextInfo(runningModels, flags)
	}

	// Оценка ёмкости бэкенда
	if len(availableModels) > 0 {
		metrics.BackendCapacity = a.calculateBackendCapacity(runningModels, availableModels, flags)
	}

	// Получение статистики запросов
	stats, err := a.getOllamaStats()
	if err != nil {
		fmt.Printf("[%s] Failed to get ollama stats: %v\n", time.Now().Format(time.RFC3339), err)
		// Возвращаем дефолтные значения при ошибке
		metrics.ActiveRequests = 0
		metrics.TotalRequests = 0
		metrics.AvgResponseTime = 0
		metrics.RequestsPerSecond = 0
	} else {
		metrics.ActiveRequests = stats.ActiveRequests
		metrics.TotalRequests = stats.TotalRequests
		metrics.AvgResponseTime = stats.AvgResponseTime
		metrics.RequestsPerSecond = stats.RequestsPerSecond
	}

	return metrics
}

// getRunningModels - получение списка запущенных моделей
func (a *Agent) getRunningModels() ([]types.RunningModel, error) {
	resp, err := a.httpClient.Get(fmt.Sprintf("%s/api/ps", a.getOllamaBaseURL()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name      string    `json:"name"`
			Model     string    `json:"model"`
			Size      uint64    `json:"size"`
			Digest    string    `json:"digest"`
			ExpiresAt time.Time `json:"expires_at"`
			SizeVRAM  uint64    `json:"size_vram"`
			Details   struct {
				Format        string   `json:"format"`
				Family        string   `json:"family"`
				Families      []string `json:"families"`
				ParameterSize string   `json:"parameter_size"`
				Quantization  string   `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	// Получаем details из /api/tags для богатых метаданных
	detailsMap := a.getModelDetailsMap()

	models := make([]types.RunningModel, len(result.Models))
	for i, m := range result.Models {
		models[i] = types.RunningModel{
			Name:          m.Name,
			Size:          m.Size,
			Digest:        m.Digest,
			ExpiresAt:     m.ExpiresAt,
			Family:        m.Details.Family,
			Format:        m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.Quantization,
		}

		// Fallback на данные из /api/tags если details пустые
		if models[i].Family == "" {
			if details, ok := detailsMap[m.Name]; ok {
				models[i].Family = details.Family
				models[i].Format = details.Format
				models[i].ParameterSize = details.ParameterSize
				models[i].Quantization = details.Quantization
			}
		}

		// Используем реальное значение VRAM от Ollama если доступно
		if m.SizeVRAM > 0 {
			models[i].VRAMUsage = m.SizeVRAM / 1024 / 1024 // bytes → MB
		} else if a.platformMode == types.ModeGPU {
			// Fallback: оценка по размеру модели
			models[i].VRAMUsage = estimateVRAMUsage(m.Size)
		}

		if a.platformMode == types.ModeCPU {
			// CPU: модели загружаются в RAM
			models[i].RAMUsage = estimateRAMUsage(m.Size)
		}
	}

	return models, nil
}

// getModelDetailsMap - получение мапы details моделей из /api/tags
func (a *Agent) getModelDetailsMap() map[string]types.ModelDetails {
	resp, err := a.httpClient.Get(fmt.Sprintf("%s/api/tags", a.getOllamaBaseURL()))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Models []struct {
			Name    string `json:"name"`
			Details struct {
				Format        string   `json:"format"`
				Family        string   `json:"family"`
				Families      []string `json:"families"`
				ParameterSize string   `json:"parameter_size"`
				Quantization  string   `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	detailsMap := make(map[string]types.ModelDetails)
	for _, m := range result.Models {
		detailsMap[m.Name] = types.ModelDetails{
			Family:        m.Details.Family,
			Format:        m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.Quantization,
		}
	}

	return detailsMap
}

// getAvailableModels - получение списка доступных моделей из /api/tags
func (a *Agent) getAvailableModels() ([]types.RunningModel, error) {
	resp, err := a.httpClient.Get(fmt.Sprintf("%s/api/tags", a.getOllamaBaseURL()))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code: %d", resp.StatusCode)
	}

	var result struct {
		Models []struct {
			Name       string `json:"name"`
			Model      string `json:"model"`
			Size       uint64 `json:"size"`
			Digest     string `json:"digest"`
			ModifiedAt string `json:"modified_at"`
			Details    struct {
				Format        string `json:"format"`
				Family        string `json:"family"`
				Families      []string `json:"families"`
				ParameterSize string `json:"parameter_size"`
				Quantization  string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	models := make([]types.RunningModel, len(result.Models))
	for i, m := range result.Models {
		models[i] = types.RunningModel{
			Name:      m.Name,
			Size:      m.Size,
			Digest:    m.Digest,
			Family:    m.Details.Family,
			Format:    m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization: m.Details.Quantization,
		}
	}

	return models, nil
}

// OllamaStats - статистика Ollama
type OllamaStats struct {
	ActiveRequests    int     `json:"activeRequests"`
	TotalRequests     int64   `json:"totalRequests"`
	AvgResponseTime   float64 `json:"avgResponseTime"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
}

// getOllamaStats - получение статистики Ollama
func (a *Agent) getOllamaStats() (*OllamaStats, error) {
	stats := &OllamaStats{
		ActiveRequests:    0,
		TotalRequests:     0,
		AvgResponseTime:   0,
		RequestsPerSecond: 0,
	}

	// Создаем HTTP клиент с таймаутом для запроса к Ollama API
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	// Запрос к /api/ps для получения активных процессов
	resp, err := client.Get(fmt.Sprintf("%s/api/ps", a.getOllamaBaseURL()))
	if err != nil {
		return stats, fmt.Errorf("failed to connect to Ollama: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return stats, fmt.Errorf("Ollama API returned status %d", resp.StatusCode)
	}

	// Парсинг ответа
	var psResponse struct {
		Models []struct {
			Name      string    `json:"name"`
			Size      uint64    `json:"size"`
			Digest    string    `json:"digest"`
			ExpiresAt time.Time `json:"expires_at"`
			SizeVRAM  uint64    `json:"size_vram"`
		} `json:"models"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&psResponse); err != nil {
		return stats, fmt.Errorf("failed to parse Ollama response: %w", err)
	}

	// Ollama /api/ps возвращает загруженные в память модели, НЕ активные HTTP-запросы.
	// Реальное количество активных запросов нельзя получить через публичный API Ollama.
	// Оставляем 0 — точное значение будет рассчитываться балансировщиком по счётчику проксируемых запросов.
	stats.ActiveRequests = 0

	// RPS/TotalRequests также нельзя достоверно получить на стороне агента.
	// Балансировщик имеет точные счётчики проксированных запросов.
	stats.RequestsPerSecond = 0
	stats.TotalRequests = 0
	stats.AvgResponseTime = 0

	return stats, nil
}

// estimateVRAMUsage - оценка использования VRAM по размеру модели
func estimateVRAMUsage(modelSize uint64) uint64 {
	// Грубая оценка: модель занимает примерно свой размер в VRAM
	// Для квантованных моделей может быть меньше
	return modelSize / 1024 / 1024 // конвертация в MB
}

// estimateRAMUsage - оценка использования RAM на CPU по размеру модели
func estimateRAMUsage(modelSize uint64) uint64 {
	// На CPU модели загружаются в RAM с небольшим оверхедом
	return modelSize / 1024 / 1024 // конвертация в MB
}

// getOllamaVersion - получение версии Ollama с кэшированием
func (a *Agent) getOllamaVersion() string {
	// Проверяем кэш
	if a.cachedVersion != "" && time.Since(a.cachedVersionAt) < a.versionCacheTTL {
		return a.cachedVersion
	}

	resp, err := a.httpClient.Get(fmt.Sprintf("%s/api/version", a.getOllamaBaseURL()))
	if err != nil {
		return "unknown"
	}
	defer resp.Body.Close()

	var result struct {
		Version string `json:"version"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "unknown"
	}

	// Обновляем кэш
	a.cachedVersion = result.Version
	a.cachedVersionAt = time.Now()

	return result.Version
}
