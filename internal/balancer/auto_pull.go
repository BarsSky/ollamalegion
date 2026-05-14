package balancer

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// PullState — состояние активной загрузки модели
type PullState struct {
	Model      string
	BackendID  string
	StartedAt  time.Time
	Done       chan struct{}
	Error      error
	HTTPStatus int
}

// AutoPullManager — менеджер автоматической загрузки моделей по запросу (Pull-on-Demand).
// Когда клиент запрашивает модель, которой нет на бэкенде, AutoPullManager:
// 1. Проверяет, не загружается ли уже эта модель где-то (dedup)
// 2. Если нет — запускает POST /api/pull на лучшем доступном бэкенде
// 3. Ожидает завершения загрузки с конфигурируемым таймаутом
// 4. Возвращает backendID, на который можно направить повторный запрос
type AutoPullManager struct {
	proxy       *Proxy
	mu          sync.Mutex
	activePulls map[string]*PullState // model -> state
	config      types.AutoPullConfig
	httpClient  *http.Client
}

// NewAutoPullManager — создание менеджера авто-загрузки
func NewAutoPullManager(proxy *Proxy, config types.AutoPullConfig) *AutoPullManager {
	apm := &AutoPullManager{
		proxy:       proxy,
		config:      config,
		activePulls: make(map[string]*PullState),
		httpClient: &http.Client{
			Timeout: 10 * time.Minute, // Долгий таймаут для pull больших моделей
		},
	}

	// Значения по умолчанию
	if config.MaxConcurrent <= 0 {
		apm.config.MaxConcurrent = 3
	}
	if config.PullTimeout == "" {
		apm.config.PullTimeout = "5m"
	}
	if config.RetryCount <= 0 {
		apm.config.RetryCount = 1
	}

	return apm
}

// GetConfig — возвращает текущую конфигурацию
func (apm *AutoPullManager) GetConfig() types.AutoPullConfig {
	apm.mu.Lock()
	defer apm.mu.Unlock()
	return apm.config
}

// SetConfig — обновляет конфигурацию
func (apm *AutoPullManager) SetConfig(cfg types.AutoPullConfig) {
	apm.mu.Lock()
	defer apm.mu.Unlock()
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 3
	}
	if cfg.PullTimeout == "" {
		cfg.PullTimeout = "5m"
	}
	if cfg.RetryCount <= 0 {
		cfg.RetryCount = 1
	}
	apm.config = cfg
}

// IsEnabled — проверяет, включён ли механизм
func (apm *AutoPullManager) IsEnabled() bool {
	apm.mu.Lock()
	defer apm.mu.Unlock()
	return apm.config.Enabled
}

// GetActivePulls — возвращает список активных загрузок (для мониторинга)
func (apm *AutoPullManager) GetActivePulls() []map[string]interface{} {
	apm.mu.Lock()
	defer apm.mu.Unlock()

	result := make([]map[string]interface{}, 0, len(apm.activePulls))
	for model, state := range apm.activePulls {
		result = append(result, map[string]interface{}{
			"model":      model,
			"backendId":  state.BackendID,
			"startedAt":  state.StartedAt.UTC().Format(time.RFC3339),
			"elapsedSec": int(time.Since(state.StartedAt).Seconds()),
		})
	}
	return result
}

// IsPullInProgress — проверяет, не загружается ли уже модель
func (apm *AutoPullManager) IsPullInProgress(model string) bool {
	apm.mu.Lock()
	defer apm.mu.Unlock()
	_, exists := apm.activePulls[model]
	return exists
}

// EnsureModel — главный метод:
// 1. Проверяет, есть ли модель на каком-либо бэкенде (через /api/tags агентов)
// 2. Если есть — возвращает backendID
// 3. Если нет — запускает pull на лучшем бэкенде, ждёт завершения, возвращает backendID
func (apm *AutoPullManager) EnsureModel(model string) (string, error) {
	if !apm.IsEnabled() {
		return "", fmt.Errorf("auto-pull disabled")
	}

	// Шаг 1: Проверяем, может модель уже есть на каком-то бэкенде (по данным агентов)
	if backendID := apm.findBackendWithModelReady(model); backendID != "" {
		logger.Get().Infow("auto-pull: model already available on backend",
			"model", model, "backend", backendID)
		return backendID, nil
	}

	// Шаг 2: Проверяем, не загружается ли уже эта модель где-то (dedup)
	apm.mu.Lock()
	if existing, ok := apm.activePulls[model]; ok {
		apm.mu.Unlock()
		logger.Get().Infow("auto-pull: pull already in progress, waiting",
			"model", model, "backend", existing.BackendID)

		// Ждём завершения уже запущенного pull'а
		timeout := apm.getPullTimeout()
		select {
		case <-existing.Done:
			if existing.Error != nil {
				return "", fmt.Errorf("auto-pull for %s failed: %w", model, existing.Error)
			}
			return existing.BackendID, nil
		case <-time.After(timeout):
			return "", fmt.Errorf("auto-pull for %s timed out waiting for in-progress pull", model)
		}
	}

	// Шаг 3: Проверяем лимит одновременных загрузок
	concurrentPulls := len(apm.activePulls)
	if apm.config.MaxConcurrent > 0 && concurrentPulls >= apm.config.MaxConcurrent {
		apm.mu.Unlock()
		return "", fmt.Errorf("auto-pull: max concurrent pulls reached (%d/%d)", concurrentPulls, apm.config.MaxConcurrent)
	}

	// Шаг 4: Ищем лучший бэкенд для загрузки
	backendID := apm.findBestBackendForPull(model)
	if backendID == "" {
		apm.mu.Unlock()
		return "", fmt.Errorf("auto-pull: no available backend for model %s", model)
	}

	// Шаг 5: Создаём состояние pull'а
	state := &PullState{
		Model:     model,
		BackendID: backendID,
		StartedAt: time.Now(),
		Done:      make(chan struct{}),
	}
	apm.activePulls[model] = state
	apm.mu.Unlock()

	// Шаг 6: Запускаем pull асинхронно
	go apm.executePull(state)

	// Шаг 7: Ждём завершения
	timeout := apm.getPullTimeout()
	logger.Get().Infow("auto-pull: waiting for model download",
		"model", model, "backend", backendID, "timeout", timeout)

	select {
	case <-state.Done:
		if state.Error != nil {
			// Очищаем состояние при ошибке
			apm.mu.Lock()
			delete(apm.activePulls, model)
			apm.mu.Unlock()
			return "", fmt.Errorf("auto-pull for %s failed: %w", model, state.Error)
		}
		logger.Get().Infow("auto-pull: model download complete",
			"model", model, "backend", backendID, "elapsed_sec", int(time.Since(state.StartedAt).Seconds()))
		return backendID, nil
	case <-time.After(timeout):
		// Таймаут — оставляем pull в фоне (он может завершиться позже)
		logger.Get().Warnw("auto-pull: timeout waiting for model download, pull continues in background",
			"model", model, "backend", backendID, "timeout", timeout)
		return "", fmt.Errorf("auto-pull for %s timed out after %v (pull continues in background)", model, timeout)
	}
}

// executePull — выполняет POST /api/pull на указанном бэкенде
func (apm *AutoPullManager) executePull(state *PullState) {
	defer close(state.Done)

	backend := apm.getBackend(state.BackendID)
	if backend == nil {
		err := fmt.Errorf("backend %s not found", state.BackendID)
		state.Error = err
		logger.Get().Errorw("auto-pull: backend not found",
			"model", state.Model, "backend", state.BackendID, "error", err)
		return
	}

	host := backend.Host
	port := backend.OllamaPort
	url := fmt.Sprintf("http://%s:%d/api/pull", host, port)

	// Используем stream:false чтобы получить полный ответ по завершении
	body := fmt.Sprintf(`{"name":"%s","stream":false}`, state.Model)
	logger.Get().Infow("auto-pull: starting model download",
		"model", state.Model, "backend", state.BackendID, "url", url)

	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		err = fmt.Errorf("failed to create pull request: %w", err)
		state.Error = err
		logger.Get().Errorw("auto-pull: request creation failed",
			"model", state.Model, "backend", state.BackendID, "error", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	logger.Get().Debugw("auto-pull: sending pull request",
		"model", state.Model, "backend", state.BackendID,
		"url", url, "timeout", apm.httpClient.Timeout)

	resp, err := apm.httpClient.Do(req)
	if err != nil {
		err = fmt.Errorf("pull request failed: %w", err)
		state.Error = err
		logger.Get().Errorw("auto-pull: HTTP request failed",
			"model", state.Model, "backend", state.BackendID, "error", err,
			"elapsed_sec", int(time.Since(state.StartedAt).Seconds()))
		return
	}
	defer resp.Body.Close()

	state.HTTPStatus = resp.StatusCode
	logger.Get().Debugw("auto-pull: backend responded",
		"model", state.Model, "backend", state.BackendID,
		"status", resp.StatusCode, "elapsed_sec", int(time.Since(state.StartedAt).Seconds()))

	// Читаем полный ответ (stream:false)
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		err = fmt.Errorf("failed to read pull response: %w", err)
		state.Error = err
		logger.Get().Errorw("auto-pull: failed to read response",
			"model", state.Model, "backend", state.BackendID, "error", err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		// Парсим ошибку
		var errResp struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(respBody, &errResp) == nil && errResp.Error != "" {
			state.Error = fmt.Errorf("ollama pull failed (HTTP %d): %s", resp.StatusCode, errResp.Error)
		} else {
			state.Error = fmt.Errorf("ollama pull failed (HTTP %d): %s", resp.StatusCode, string(respBody))
		}
		logger.Get().Errorw("auto-pull: Ollama returned error",
			"model", state.Model, "backend", state.BackendID,
			"status", resp.StatusCode, "error", state.Error)
		return
	}

	// Pull успешен — отмечаем в метриках
	logger.Get().Infow("auto-pull: model successfully downloaded",
		"model", state.Model, "backend", state.BackendID,
		"elapsed_sec", int(time.Since(state.StartedAt).Seconds()),
		"response_size", len(respBody))

	// Очищаем activePulls после успешной загрузки, чтобы:
	// 1) Не блокировать dedup для будущих запросов этой же модели
	// 2) Корректно отображать статус в WebUI (activePulls не должен расти бесконечно)
	apm.mu.Lock()
	delete(apm.activePulls, state.Model)
	apm.mu.Unlock()
}

// findBackendWithModelReady — ищет бэкенд, на котором модель уже доступна.
// Сначала проверяет RunningModels (модель уже загружена в память),
// затем AvailableModels (модель скачана, но не загружена — Ollama подгрузит автоматически).
func (apm *AutoPullManager) findBackendWithModelReady(model string) string {
	apm.proxy.mu.RLock()
	defer apm.proxy.mu.RUnlock()

	for id, state := range apm.proxy.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}
		apm.proxy.metricsMgr.mu.RLock()
		metrics, ok := apm.proxy.metricsMgr.metrics[id]
		apm.proxy.metricsMgr.mu.RUnlock()
		if !ok {
			continue
		}

		// Шаг 1: модель уже загружена в память (RunningModels)
		for _, m := range metrics.Ollama.RunningModels {
			if m.Name == model || strings.Contains(m.Name, model) {
				return id
			}
		}

		// Шаг 2: модель скачана, но не загружена (AvailableModels)
		// Ollama автоматически загрузит модель при первом запросе.
		if metrics.Ollama.AvailableModels != nil {
			for _, m := range metrics.Ollama.AvailableModels {
				if m.Name == model || strings.Contains(m.Name, model) {
					logger.Get().Infow("auto-pull: model found in available models (not running, will auto-load)",
						"model", model, "backend", id)
					return id
				}
			}
		}
	}
	return ""
}

// findBestBackendForPull — находит лучший бэкенд для загрузки модели
// Критерии: healthy, достаточно свободного места (диск + VRAM), наименее загружен
func (apm *AutoPullManager) findBestBackendForPull(model string) string {
	apm.proxy.mu.RLock()
	defer apm.proxy.mu.RUnlock()

	var bestID string
	bestScore := -1.0

	for id, state := range apm.proxy.backends {
		if state.Backend.Status != types.StatusHealthy {
			continue
		}

		// Проверяем ресурсы
		if !apm.proxy.checkResourceLimits(id) {
			continue
		}

		// Не загружаем на сильно загруженный бэкенд
		state.mu.Lock()
		active := state.ActiveReqs
		maxReqs := state.Backend.MaxConcurrentReqs
		state.mu.Unlock()

		if maxReqs > 0 && float64(active)/float64(maxReqs) > 0.9 {
			continue
		}

		// Проверяем, не загружается ли уже эта модель на этом бэкенде
		state.mu.Lock()
		_, isWarming := state.WarmingUpModels[model]
		state.mu.Unlock()
		if isWarming {
			continue
		}

		apm.proxy.metricsMgr.mu.RLock()
		metrics, hasMetrics := apm.proxy.metricsMgr.metrics[id]
		apm.proxy.metricsMgr.mu.RUnlock()

		score := 0.0
		if hasMetrics {
			// Чем больше свободного места на диске — тем лучше
			if metrics.System.DiskFree > 0 {
				diskGB := float64(metrics.System.DiskFree) / 1024.0 // MB → GB
				if diskGB > 1.0 {
					score += diskGB * 10
				}
			}

			// Штраф за загруженность бэкенда
			loadRatio := 0.0
			if maxReqs > 0 {
				loadRatio = float64(active) / float64(maxReqs)
			}
			score -= loadRatio * 50

			// Если модель уже доступна на этом бэкенде (available, не running) — бонус
			for _, m := range metrics.Ollama.AvailableModels {
				if m.Name == model || strings.Contains(m.Name, model) {
					score += 80
					break
				}
			}
		}

		if score > bestScore {
			bestScore = score
			bestID = id
		}
	}

	return bestID
}

// getBackend — возвращает Backend по ID
func (apm *AutoPullManager) getBackend(backendID string) *types.Backend {
	apm.proxy.mu.RLock()
	defer apm.proxy.mu.RUnlock()

	if state, ok := apm.proxy.backends[backendID]; ok {
		return state.Backend
	}
	return nil
}

// getPullTimeout — парсит таймаут из конфигурации
func (apm *AutoPullManager) getPullTimeout() time.Duration {
	apm.mu.Lock()
	timeoutStr := apm.config.PullTimeout
	apm.mu.Unlock()

	d, err := time.ParseDuration(timeoutStr)
	if err != nil {
		return 5 * time.Minute // fallback
	}
	return d
}

// CleanupStalePulls — очищает устаревшие pull'ы (для фонового цикла)
func (apm *AutoPullManager) CleanupStalePulls(maxAge time.Duration) {
	apm.mu.Lock()
	defer apm.mu.Unlock()

	now := time.Now()
	for model, state := range apm.activePulls {
		if now.Sub(state.StartedAt) > maxAge {
			logger.Get().Warnw("auto-pull: cleaning stale pull", "model", model,
				"backend", state.BackendID, "age_sec", int(now.Sub(state.StartedAt).Seconds()))
			delete(apm.activePulls, model)
		}
	}
}
