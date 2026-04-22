package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/protocol"
	"ollama-loadbalancer/pkg/types"
)

// Agent - агент сбора метрик
type Agent struct {
	config          *types.AgentConfig
	httpClient      *http.Client
	metricsSeq      int64
	heartbeatSeq    int64
	startTime       time.Time
	mu              sync.Mutex
	currentMetrics  *types.BackendMetrics
	stopChan        chan struct{}
	balancerURL     string
	registered      bool
	
	// Ollama статистика
	ollamaStats     *OllamaStats
	statsMu         sync.Mutex
	requestHistory  []requestRecord
}

// requestRecord - запись о запросе для подсчета RPS
type requestRecord struct {
	timestamp time.Time
	duration  time.Duration
}

// NewAgent - создание нового агента
func NewAgent(config *types.AgentConfig) *Agent {
	return &Agent{
		config: config,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		startTime:      time.Now(),
		stopChan:       make(chan struct{}),
		balancerURL:    config.BalancerURL,
		requestHistory: make([]requestRecord, 0),
	}
}

// Start - запуск агента
func (a *Agent) Start() error {
	// Регистрация на балансировщике
	if err := a.register(); err != nil {
		return fmt.Errorf("registration failed: %w", err)
	}
	
	a.registered = true
	
	// Запуск сбора метрик
	go a.collectLoop()
	
	// Запуск heartbeat
	go a.heartbeatLoop()
	
	return nil
}

// Stop - остановка агента
func (a *Agent) Stop() {
	close(a.stopChan)
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
	ollamaVersion := a.getOllamaVersion()
	
	req := protocol.NewRegisterRequest(
		a.config.AgentID,
		hostname,
		osName,
		"amd64",
		gpuInfo.Count,
		gpuInfo.Models,
		ollamaVersion,
		11434,
	)
	
	msg, err := protocol.NewMessage(protocol.MsgTypeRegister, a.config.AgentID, req)
	if err != nil {
		return err
	}
	
	data, err := msg.Marshal()
	if err != nil {
		return err
	}
	
	resp, err := a.httpClient.Post(
		fmt.Sprintf("%s/api/v1/agents/register", a.balancerURL),
		"application/json",
		bytes.NewReader(data),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("registration failed with status %d: %s", resp.StatusCode, string(body))
	}
	
	var registerResp protocol.RegisterResponse
	if err := json.NewDecoder(resp.Body).Decode(&registerResp); err != nil {
		return err
	}
	
	if !registerResp.Success {
		return fmt.Errorf("registration rejected: %s", registerResp.Error)
	}
	
	return nil
}

// collectLoop - цикл сбора метрик
func (a *Agent) collectLoop() {
	interval := time.Duration(a.config.CollectInterval) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
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
	
	a.mu.Lock()
	a.metricsSeq++
	seq := a.metricsSeq
	a.currentMetrics = metrics
	a.mu.Unlock()
	
	_ = protocol.NewMetricsMessage(a.config.AgentID, seq, metrics)
	
	// Сериализация сообщения
	wrapper := map[string]interface{}{
		"type":      "metrics",
		"agentId":   a.config.AgentID,
		"timestamp": time.Now().UTC(),
		"sequence":  seq,
		"metrics":   metrics,
	}
	
	data, err := json.Marshal(wrapper)
	if err != nil {
		fmt.Printf("[%s] Failed to marshal metrics: %v\n", time.Now().Format(time.RFC3339), err)
		return
	}
	
	// Отправка метрик
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
		// Проверка на критические метрики
		if a.currentMetrics.GPU.UsagePercent > 95 || 
		   a.currentMetrics.GPU.Temperature > 90 {
			status = "degraded"
		}
	}
	a.mu.Unlock()
	
	_ = protocol.NewHeartbeatMessage(a.config.AgentID, seq, uptime, status)
	
	// Сериализация сообщения
	wrapper := map[string]interface{}{
		"type":      "heartbeat",
		"agentId":   a.config.AgentID,
		"timestamp": time.Now().UTC(),
		"uptime":    uptime,
		"sequence":  seq,
		"status":    status,
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
	
	// Сбор GPU метрик
	metrics.GPU = a.collectGPUMetrics()
	
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
	
	// Попытка получить метрики через nvidia-smi
	output, err := executeNvidiaSmi()
	if err != nil {
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

// collectOllamaMetrics - сбор метрик Ollama
func (a *Agent) collectOllamaMetrics() types.OllamaMetrics {
	metrics := types.OllamaMetrics{}
	
	// Получение запущенных моделей через /api/ps
	runningModels, err := a.getRunningModels()
	if err != nil {
		fmt.Printf("[%s] Failed to get running models: %v\n", time.Now().Format(time.RFC3339), err)
	} else {
		metrics.RunningModels = runningModels
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
	resp, err := a.httpClient.Get(fmt.Sprintf("http://localhost:11434/api/ps"))
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
			Size      uint64    `json:"size"`
			Digest    string    `json:"digest"`
			ExpiresAt time.Time `json:"expires_at"`
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
			ExpiresAt: m.ExpiresAt,
			VRAMUsage: estimateVRAMUsage(m.Size),
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
	resp, err := client.Get("http://localhost:11434/api/ps")
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
	
	// Количество активных запросов = количество работающих моделей
	stats.ActiveRequests = len(psResponse.Models)
	
	// Вычисление RPS на основе истории запросов
	a.statsMu.Lock()
	
	// Добавляем текущий запрос в историю
	now := time.Now()
	a.requestHistory = append(a.requestHistory, requestRecord{
		timestamp: now,
		duration:  time.Second, // предполагаем, что запрос занимает ~1 секунду
	})
	
	// Очищаем старую историю (старше 10 секунд)
	cutoff := now.Add(-10 * time.Second)
	filtered := a.requestHistory[:0]
	for _, rec := range a.requestHistory {
		if rec.timestamp.After(cutoff) {
			filtered = append(filtered, rec)
		}
	}
	a.requestHistory = filtered
	
	// Подсчет RPS
	if len(a.requestHistory) > 0 {
		// Считаем количество запросов за последнюю секунду
		oneSecondAgo := now.Add(-time.Second)
		recentRequests := 0
		for _, rec := range a.requestHistory {
			if rec.timestamp.After(oneSecondAgo) {
				recentRequests++
			}
		}
		stats.RequestsPerSecond = float64(recentRequests)
		
		// Общее количество запросов (за последние 10 секунд)
		stats.TotalRequests = int64(len(a.requestHistory))
		
		// Среднее время ответа (предполагаемое)
		stats.AvgResponseTime = 1000.0 // 1000ms = 1 секунда
	}
	
	a.statsMu.Unlock()
	
	return stats, nil
}

// estimateVRAMUsage - оценка использования VRAM по размеру модели
func estimateVRAMUsage(modelSize uint64) uint64 {
	// Грубая оценка: модель занимает примерно свой размер в VRAM
	// Для квантованных моделей может быть меньше
	return modelSize / 1024 / 1024 // конвертация в MB
}

// getOllamaVersion - получение версии Ollama
func (a *Agent) getOllamaVersion() string {
	resp, err := a.httpClient.Get("http://localhost:11434/api/version")
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
	
	return result.Version
}
