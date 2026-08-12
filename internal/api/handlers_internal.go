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
//
// Round 34 (2026-08-12) Phase 2: добавлены runtime params (kvCacheType,
// flashAttnType, useMmap) для profile mismatch detection в preflight_nctx.
type llamaModelLoadedRequest struct {
	BackendID     string `json:"backendId"`
	Model         string `json:"model"`
	SizeBytes     uint64 `json:"sizeBytes,omitempty"`
	ContextSize   int    `json:"contextSize,omitempty"`
	GPULayers     int    `json:"gpuLayers,omitempty"`
	KVCacheType   string `json:"kvCacheType,omitempty"`
	FlashAttnType int    `json:"flashAttnType,omitempty"`
	UseMmap       bool   `json:"useMmap,omitempty"`
}

// llamaModelUnloadedRequest — тело POST /api/v1/internal/llama-model-unloaded.
//
// Round 34 (2026-08-12) Phase 3: cppworker шлёт после выгрузки модели
// (handleUnloadModel, idle_unload_after). Без этого lastKnownNCtx в
// NCtxReloadCoordinator остаётся прежним после unload → preflight думает
// модель загружена с большим n_ctx, не триггерит reload → пользователь
// получает 502 connection refused.
type llamaModelUnloadedRequest struct {
	BackendID string `json:"backendId"`
	Model     string `json:"model"`
}

// handleLlamaModelLoaded — POST /api/v1/internal/llama-model-loaded.
//
// При успехе:
//  1. Добавляет модель в llamaMetrics[backendID].LoadedModels (если её там ещё нет).
//  2. Очищает LoadingModels (если там была).
//  3. Обновляет runtime params (kvCacheType, flashAttnType, useMmap) для
//     profile mismatch detection (Phase 2).
//  4. Обновляет nctxReload.SetLastKnownNCtx(backendID, contextSize) — чтобы
//     preflight видел актуальное n_ctx без 30s poll'а (Phase 3).
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

	mm.UpdateLlamaCppModelLoaded(req.BackendID, req.Model, req.SizeBytes, req.ContextSize, req.GPULayers,
		req.KVCacheType, req.FlashAttnType, req.UseMmap)

	// Round 34 Phase 3: обновляем nctxReload coordinator с актуальным contextSize
	// (если известен). Это устраняет stale state когда cppworker reload'ит модель
	// извне balancer (WebUI / settings / manual API call). Без этого preflight
	// видел устаревший lastKnownNCtx и либо триггерил лишний reload, либо
	// наоборот не триггерил когда надо.
	if req.ContextSize > 0 {
		if nctxReload := s.proxy.GetNCtxReloadCoordinator(); nctxReload != nil {
			nctxReload.SetLastKnownNCtx(req.BackendID, req.ContextSize)
		}
	}

	logger.Get().Debugw("internal/llama-model-loaded: processed",
		"backend", req.BackendID,
		"model", req.Model,
		"sizeBytes", req.SizeBytes,
		"contextSize", req.ContextSize,
		"gpuLayers", req.GPULayers,
		"kvCacheType", req.KVCacheType,
		"flashAttnType", req.FlashAttnType,
		"useMmap", req.UseMmap)

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":   true,
		"backendId": req.BackendID,
		"model":     req.Model,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
}

// handleLlamaModelUnloaded — POST /api/v1/internal/llama-model-unloaded.
//
// Round 34 (2026-08-12) Phase 3: cppworker шлёт после выгрузки модели.
// Удаляет модель из LoadedModels + сбрасывает lastKnownNCtx в
// nctxReload coordinator (чтобы preflight не использовал stale значение
// после `idle_unload_after` 10m).
func (s *Server) handleLlamaModelUnloaded(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req llamaModelUnloadedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.BackendID == "" || req.Model == "" {
		http.Error(w, "backendId and model are required", http.StatusBadRequest)
		return
	}

	if s.proxy == nil {
		http.Error(w, "proxy not initialized", http.StatusServiceUnavailable)
		return
	}
	mm := s.proxy.GetMetricsManager()
	if mm == nil {
		http.Error(w, "metrics manager not initialized", http.StatusServiceUnavailable)
		return
	}

	// Round 34 Phase 3: убрать модель из LoadedModels.
	mm.UpdateLlamaCppModelUnloaded(req.BackendID, req.Model)

	// Сбросить lastKnownNCtx — иначе preflight будет думать модель загружена.
	if nctxReload := s.proxy.GetNCtxReloadCoordinator(); nctxReload != nil {
		nctxReload.SetLastKnownNCtx(req.BackendID, 0)
	}

	logger.Get().Debugw("internal/llama-model-unloaded: processed",
		"backend", req.BackendID,
		"model", req.Model)

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
