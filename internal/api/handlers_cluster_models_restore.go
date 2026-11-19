// handlers_cluster_models_restore.go — R639 (2026-11-19): восстановление: сбор загруженных моделей, числовые помощники, откат предыдущей модели.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_cluster_models.go` держал 1628 строк:
// агрегированные списки, перезагрузку модели, массовые операции и состояние
// модели на бэкенде. Здесь — один из слоёв; поведение не менялось.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"ollama-loadbalancer/internal/balancer"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
	"sync"
	"time"
)

// clusterBulkModelsHandler — POST /api/v1/cluster/models/bulk.
//
// Массовая операция над списком моделей через балансировщик. Переиспользует
// executeReloadOnBackend (Session 17) для per-backend выполнения, добавляя только
// агрегацию результатов и контроль concurrency при больших списках.
//
// Семантика response (как у других cluster endpoints):
//   - HTTP 200 + per-model results[] даже если все операции упали.
//   - HTTP 4xx только для критичных ошибок (validation).
//   - HTTP 5xx только при недоступности cluster state / model manager.
//
// Требует аутентификации (X-API-Token).
func (s *Server) clusterBulkModelsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "method not allowed",
		})
		return
	}

	var req clusterBulkModelsRequest
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid request body",
				"message": err.Error(),
			})
			return
		}
	}

	// По умолчанию operation = "load".
	if req.Operation == "" {
		req.Operation = "load"
	}

	// Валидация операции.
	switch req.Operation {
	case "load", "unload", "reload", "all":
		// OK
	default:
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "invalid operation",
			"message": "supported operations: load, unload, reload",
		})
		return
	}

	// Семантический алиас: "all" → "reload".
	if req.Operation == "all" {
		req.Operation = "reload"
	}

	// Проверяем доступность cluster state.
	if s.getClusterState() == nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "cluster state unavailable",
		})
		return
	}

	// Резолвим список моделей: явный список или derived (для unload без списка).
	models := req.Models
	if len(models) == 0 {
		// "unload" без списка → берём все загруженные модели из cluster state.
		// Это позволяет UI реализовать "Unload All" одной кнопкой.
		if req.Operation == "unload" {
			models = s.collectAllLoadedModels(req.BackendID)
		} else {
			// "load" / "reload" без списка — опасно, нужен явный список моделей.
			s.writeJSON(w, http.StatusBadRequest, map[string]string{
				"error":   "models list required for operation",
				"message": "provide a non-empty 'models' array or use operation='unload' to unload all loaded",
			})
			return
		}
	}

	// Проверка лимита на количество моделей (защита от перегрузки cppworker).
	if len(models) > bulkOperationMaxModels {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{
			"error":   "too many models in bulk request",
			"message": "max " + intToString(bulkOperationMaxModels) + " models per request",
		})
		return
	}

	// Проверяем что есть ПРИГОДНЫЕ llama_cpp бэкенды для выполнения операции
	// (R263: offline/unhealthy/disabled узлы в цели не берём — иначе операция
	// уходит на мёртвый узел и «висит» до таймаута).
	bulkSel := s.selectReloadTargetsChecked(req.BackendID)
	if len(bulkSel.Targets) == 0 {
		if bulkSel.LlamaCppTotal == 0 {
			s.writeJSON(w, http.StatusNotFound, map[string]string{
				"error":   "no llama_cpp backends available",
				"message": "bulk operation requires at least one llama_cpp backend",
			})
			return
		}
		logger.Get().Warnw("cluster bulk operation refused: no usable backends",
			"operation", req.Operation, "skipped", len(bulkSel.Skipped))
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error":   "no usable llama_cpp backend",
			"message": "every llama_cpp backend is offline, unhealthy or excluded by the operator",
			"skipped": bulkSel.Skipped,
		})
		return
	}
	targets := bulkSel.Targets

	startedAt := time.Now().UTC()
	startedAtRFC := startedAt.Format(time.RFC3339Nano)

	logger.Get().Infow("cluster bulk operation requested",
		"operation", req.Operation,
		"models_count", len(models),
		"backend_id", req.BackendID,
		"context_size", ctxSizeForLog(req.ContextSize),
		"gpu_layers", gpuLayersForLog(req.GPULayers),
		"reason", req.Reason,
	)

	resp := clusterBulkModelsResponse{
		Operation: req.Operation,
		Total:     len(models),
		Results:   make([]clusterBulkModelResult, 0, len(models)),
	}

	// Выполняем операции. Для малых списков (<=50) — последовательно,
	// для больших (>50) — параллельно с ограничением concurrency.
	if len(models) <= bulkOperationConcurrency*2 {
		for i := range models {
			result := s.executeBulkModelItem(r.Context(), targets, &models[i], &req)
			resp.Results = append(resp.Results, result)
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
		}
	} else {
		resp.Results = s.executeBulkModelItemsParallel(r.Context(), targets, models, &req, &resp)
	}

	resp.DurationMs = time.Since(startedAt).Milliseconds()
	resp.StartedAt = startedAtRFC

	logger.Get().Infow("cluster bulk operation completed",
		"operation", req.Operation,
		"total", resp.Total,
		"succeeded", resp.Succeeded,
		"failed", resp.Failed,
		"duration_ms", resp.DurationMs,
	)

	s.writeJSON(w, http.StatusOK, resp)
}

// executeBulkModelItem — выполняет одну модель из bulk-запроса на всех целевых бэкендах.
// Переиспользует executeReloadOnBackend (Session 17), собирая результаты в clusterBulkModelResult.
func (s *Server) executeBulkModelItem(ctx context.Context, targets []string, item *bulkModelItem, req *clusterBulkModelsRequest) clusterBulkModelResult {
	// Резолвим per-item операцию: приоритет item.Operation > req.Operation.
	op := req.Operation
	if item.Operation != "" {
		op = item.Operation
	}
	// Нормализация "all" → "reload".
	if op == "all" {
		op = "reload"
	}

	// Резолвим per-item backendId.
	backendID := req.BackendID
	if item.BackendID != "" {
		backendID = item.BackendID
	}

	// Резолвим per-item contextSize / gpuLayers.
	contextSize := req.ContextSize
	if item.ContextSize != nil {
		contextSize = item.ContextSize
	}
	gpuLayers := req.GPULayers
	if item.GPULayers != nil {
		gpuLayers = item.GPULayers
	}

	// Резолвим per-item reason.
	reason := req.Reason
	if item.Reason != "" {
		reason = item.Reason
	}

	// Строим clusterReloadModelRequest для executeReloadOnBackend.
	perBackendReq := &clusterReloadModelRequest{
		Operation:   op,
		BackendID:   backendID,
		ContextSize: contextSize,
		GPULayers:   gpuLayers,
		Insecure:    req.Insecure,
		Stream:      req.Stream,
		Reason:      reason,
		// R66c: force выгружает занятую модель (cppworker ?force=true).
		// Per-item override приоритетнее глобального, как и остальные поля.
		Force: forceForItem(item, req),
	}

	// Резолвим фактические targets для этой модели (R263: только пригодные узлы).
	perSel := s.selectReloadTargetsChecked(backendID)
	modelTargets := perSel.Targets
	if len(modelTargets) == 0 {
		// Нет подходящих бэкендов для этой модели → результат с ошибкой и ПРИЧИНОЙ
		// (R263: раньше писалась «no llama_cpp backends», хотя узел есть — просто
		// offline/unhealthy/исключён; оператор шёл искать несуществующий бэкенд).
		reason := perSel.ExplicitReason
		if reason == "" {
			reason = "no usable llama_cpp backend"
		}
		return clusterBulkModelResult{
			Model:     item.Model,
			Operation: op,
			Succeeded: false,
			Results: []clusterReloadModelBackendResult{{
				BackendID:  backendID,
				Status:     "error",
				Error:      "backend is not usable for model load (" + reason + ")",
				HTTPStatus: http.StatusServiceUnavailable,
			}},
		}
	}

	results := make([]clusterReloadModelBackendResult, 0, len(modelTargets))
	allOK := true
	for _, bid := range modelTargets {
		res := s.executeReloadOnBackend(ctx, bid, item.Model, perBackendReq)
		if res.Status != "ok" {
			allOK = false
		}
		results = append(results, res)
	}

	return clusterBulkModelResult{
		Model:     item.Model,
		Operation: op,
		Succeeded: allOK,
		Results:   results,
	}
}

// executeBulkModelItemsParallel — параллельное выполнение bulk-операций с лимитом concurrency.
// Используется только для больших списков (>10 моделей), чтобы не блокировать UI.
func (s *Server) executeBulkModelItemsParallel(ctx context.Context, targets []string, models []bulkModelItem, req *clusterBulkModelsRequest, resp *clusterBulkModelsResponse) []clusterBulkModelResult {
	results := make([]clusterBulkModelResult, len(models))
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, bulkOperationConcurrency)

	for i := range models {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			result := s.executeBulkModelItem(ctx, targets, &models[idx], req)
			results[idx] = result
			mu.Lock()
			if result.Succeeded {
				resp.Succeeded++
			} else {
				resp.Failed++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	return results
}

// collectAllLoadedModels — собирает список загруженных моделей из cluster state
// для операции "unload без явного списка моделей".
//
// Возвращает []bulkModelItem с уникальными именами моделей (если модель загружена
// на нескольких бэкендах, она попадёт в список один раз — executeReloadOnBackend
// сам обработает все бэкенды для каждого имени).
//
// Если backendID задан, фильтрует только модели с этого бэкенда.
func (s *Server) collectAllLoadedModels(backendID string) []bulkModelItem {
	cs := s.getClusterState()
	if cs == nil {
		return nil
	}

	seen := make(map[string]bool)
	result := make([]bulkModelItem, 0)

	for _, b := range cs.Backends {
		if b.BackendType != types.BackendTypeLlamaCpp {
			continue
		}
		if backendID != "" && b.ID != backendID {
			continue
		}
		for _, m := range b.LlamaCpp.LoadedModels {
			if seen[m.Name] {
				continue
			}
			seen[m.Name] = true
			result = append(result, bulkModelItem{
				Model:     m.Name,
				Operation: "unload",
			})
		}
	}

	return result
}

// intToString — простой helper для сообщений об ошибках (избегаем strconv импорта в этом файле).
func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// restorePreviousModelOnBackend — R297 (2026-10-29): вернуть модель в прежней
// конфигурации, если новая не поднялась.
//
// ЗАЧЕМ. Живой дефект: reload с невозможным окном выгружал модель и падал, а
// стенд оставался без модели вообще (`loaded_models:0`) — «ошибка настройки»
// превращалась в «сервиса нет», и на восстановление уходила минута с лишним.
// Отказ обязан быть обратимым: сняли — вернули то, что было.
//
// Возвращает true, только если модель реально снова загружена.
func (s *Server) restorePreviousModelOnBackend(backendID, modelName string, prev loadedModelParams) bool {
	mm := s.proxy.GetModelManager()
	if mm == nil {
		return false
	}
	op := balancer.ModelOpRequest{Operation: "load", ModelName: modelName}
	if prev.ContextKnown && prev.ContextSize > 0 {
		ctx := prev.ContextSize
		op.ContextSize = &ctx
	}
	if prev.KVCacheType != "" {
		kv := prev.KVCacheType
		op.KVCacheType = &kv
	}
	if prev.FlashAttn != nil {
		fa := *prev.FlashAttn
		op.FlashAttn = &fa
	}
	if prev.Mmap != nil {
		mmv := *prev.Mmap
		op.UseMmap = &mmv
	}
	if prev.BatchSize > 0 {
		bs := prev.BatchSize
		op.BatchSize = &bs
	}
	res := mm.ExecuteOperation(backendID, op)
	if res == nil || !res.Success {
		logger.Get().Warnw("R297: previous model configuration was NOT restored",
			"backend", backendID, "model", modelName,
			"previous_context", prev.ContextSize, "error", opErrText(res))
		return false
	}
	logger.Get().Warnw("R297: reload failed — previous model configuration restored",
		"backend", backendID, "model", modelName, "restored_context", prev.ContextSize)
	return true
}

// opErrText — nil-safe текст ошибки операции (для лога восстановления).
func opErrText(res *balancer.ModelOpResult) string {
	if res == nil {
		return "no result"
	}
	return res.Error
}
