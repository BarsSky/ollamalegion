package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// OllamaStats - статистика Ollama
type OllamaStats struct {
	ActiveRequests    int     `json:"activeRequests"`
	TotalRequests     int64   `json:"totalRequests"`
	AvgResponseTime   float64 `json:"avgResponseTime"`
	RequestsPerSecond float64 `json:"requestsPerSecond"`
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

// collectOllamaMetrics - сбор метрик Ollama (оптимизировано: 2 запроса вместо 4)
func (a *Agent) collectOllamaMetrics() types.OllamaMetrics {
	metrics := types.OllamaMetrics{}

	// Устанавливаем лимиты из конфигурации (-1 = не задано)
	metrics.MaxModels = a.config.MaxModels
	metrics.MaxConcurrentRequests = a.config.MaxConcurrentRequests

	// Сбор флагов запуска Ollama
	flags := a.collectOllamaRuntimeFlags()
	metrics.RuntimeFlags = flags
	a.currentFlags = flags

	// Единый запрос к /api/tags (один раз вместо двух)
	tagsResp, tagsErr := a.fetchOllamaTags()
	var availableModels []types.RunningModel
	if tagsErr != nil {
		fmt.Printf("[%s] Failed to get available models: %v\n", time.Now().Format(time.RFC3339), tagsErr)
		// Ollama API недоступен — устанавливаем флаг
		metrics.OllamaAvailable = false
	} else {
		availableModels = tagsResp.models
		metrics.AvailableModels = availableModels

		// Заполняем ModelSizes из /api/tags (размеры всех доступных моделей)
		modelSizes := make(map[string]int64, len(availableModels))
		for _, m := range availableModels {
			if m.Size > 0 {
				modelSizes[m.Name] = int64(m.Size)
			}
		}
		metrics.ModelSizes = modelSizes
	}

	// Единый запрос к /api/ps (один раз вместо двух)
	var detailsMap map[string]types.ModelDetails
	if tagsResp != nil {
		detailsMap = tagsResp.details
	}
	runningModels, psErr := a.getRunningModelsWithDetails(detailsMap)
	if psErr != nil {
		fmt.Printf("[%s] Failed to get running models: %v\n", time.Now().Format(time.RFC3339), psErr)
		// Ollama API недоступен если оба запроса провалились
		if tagsErr != nil {
			metrics.OllamaAvailable = false
		}
	} else {
		metrics.RunningModels = runningModels
		metrics.OllamaAvailable = true
	}

	// Если хотя бы один из запросов успешен — Ollama доступна
	if tagsErr == nil || psErr == nil {
		metrics.OllamaAvailable = true
	}

	// Сбор информации о контексте моделей
	if len(runningModels) > 0 {
		metrics.ModelContexts = a.collectModelContextInfo(runningModels, flags)
	}

	// Оценка ёмкости бэкенда
	if len(availableModels) > 0 {
		metrics.BackendCapacity = a.calculateBackendCapacity(runningModels, availableModels, flags)
	}

	// Статистика запросов — агент не может получить реальные active requests,
	// это отслеживается балансировщиком через счётчик прокси.
	metrics.ActiveRequests = 0
	metrics.TotalRequests = 0
	metrics.AvgResponseTime = 0
	metrics.RequestsPerSecond = 0

	return metrics
}

// tagsResult — результат запроса к /api/tags (модели + details map)
type tagsResult struct {
	models  []types.RunningModel
	details map[string]types.ModelDetails
}

// fetchOllamaTags — единый запрос к /api/tags, возвращает модели и details
func (a *Agent) fetchOllamaTags() (*tagsResult, error) {
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

	models := make([]types.RunningModel, len(result.Models))
	details := make(map[string]types.ModelDetails, len(result.Models))
	for i, m := range result.Models {
		models[i] = types.RunningModel{
			Name:          m.Name,
			Size:          m.Size,
			Digest:        m.Digest,
			Family:        m.Details.Family,
			Format:        m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.Quantization,
		}
		details[m.Name] = types.ModelDetails{
			Family:        m.Details.Family,
			Format:        m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.Quantization,
		}
	}

	return &tagsResult{models: models, details: details}, nil
}

// getRunningModelsWithDetails — получение запущенных моделей с details из кэша
func (a *Agent) getRunningModelsWithDetails(detailsMap map[string]types.ModelDetails) ([]types.RunningModel, error) {
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

// getRunningModels - получение списка запущенных моделей (legacy, использует getRunningModelsWithDetails)
func (a *Agent) getRunningModels() ([]types.RunningModel, error) {
	// Получаем details из /api/tags для богатых метаданных
	detailsMap := a.getModelDetailsMap()
	return a.getRunningModelsWithDetails(detailsMap)
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

	models := make([]types.RunningModel, len(result.Models))
	for i, m := range result.Models {
		models[i] = types.RunningModel{
			Name:          m.Name,
			Size:          m.Size,
			Digest:        m.Digest,
			Family:        m.Details.Family,
			Format:        m.Details.Format,
			ParameterSize: m.Details.ParameterSize,
			Quantization:  m.Details.Quantization,
		}
	}

	return models, nil
}