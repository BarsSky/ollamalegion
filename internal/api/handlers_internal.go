// Package api — Internal endpoints (Шаг «отображение загрузки в мониторе»).
//
// Эти endpoint'ы предназначены ТОЛЬКО для cppworker'а (внутренние callback'и).
// В production они не должны быть доступны извне кластера — рекомендуется
// держать API_TOKEN на балансировщике и передавать его cppworker'у через
// переменную окружения CPPWORKER_BALANCER_TOKEN.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// llamaModelLoadedRequest — тело POST /api/v1/internal/llama-model-loaded.
//
// Вызывается cppworker'ом fire-and-forget после успешной загрузки модели,
// чтобы балансировщик обновил кэш loadedModels немедленно, а не ждал
// 30-секундного poll.
type llamaModelLoadedRequest struct {
	BackendID   string `json:"backendId"`
	Model       string `json:"model"`
	SizeBytes   uint64 `json:"sizeBytes,omitempty"`
	ContextSize int    `json:"contextSize,omitempty"`
	GPULayers   int    `json:"gpuLayers,omitempty"`
}

// handleLlamaModelLoaded — POST /api/v1/internal/llama-model-loaded.
//
// При успехе: добавляет модель в llamaMetrics[backendID].LoadedModels
// (если её там ещё нет) и очищает LoadingModels (если там была).
//
// Авторизация выполняется на уровне роута через AuthMiddleware
// (X-API-Token, если задан в balancer config). Сам endpoint не валидирует
// тщательно, так как это внутренний callback из trusted cppworker.
func (s *Server) handleLlamaModelLoaded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req llamaModelLoadedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.BackendID == "" || req.Model == "" {
		http.Error(w, "backendId and model are required", http.StatusBadRequest)
		return
	}

	// Обновляем кэш llamaMetrics[req.BackendID] атомарно.
	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusServiceUnavailable)
		return
	}
	mm := s.proxy.GetMetricsManager()
	if mm == nil {
		http.Error(w, "metrics manager not initialized", http.StatusServiceUnavailable)
		return
	}

	mm.UpdateLlamaCppModelLoaded(req.BackendID, req.Model, req.SizeBytes, req.ContextSize, req.GPULayers)

	logger.Get().Debugw("internal/llama-model-loaded: processed",
		"backend", req.BackendID,
		"model", req.Model,
		"sizeBytes", req.SizeBytes,
		"contextSize", req.ContextSize,
		"gpuLayers", req.GPULayers)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": req.BackendID,
		"model":     req.Model,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// markLlamaModelLoading — вспомогательная функция, которая обновляет
// LoadingModels в кэше (вызывается из notifyModelLoaded path'а в balancer).
// Не HTTP endpoint, просто helper для будущего использования.
//
// Оставлено для будущей оптимизации: cppworker при старте загрузки
// мог бы делать notifyModelLoading() callback, чтобы UI сразу
// показывал спиннер. Сейчас UI получает эту информацию через
// llamaCppMetricsPoller.pollLoadingProgress каждые 2 сек.
var _ = func() bool {
	// compile-time: убедиться, что импорт types не пропадает при рефакторинге.
	_ = types.BackendTypeOllama
	return true
}()
