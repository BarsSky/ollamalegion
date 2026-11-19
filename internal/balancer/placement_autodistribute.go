// placement_autodistribute.go — R629 (2026-11-19): автоподсказки раскладки и их
// исполнение (`AutoDistributeMove` + откат).
//
// ЗАЧЕМ ВЫНЕСЕНО. Эти три функции — единственное место в `proxy.go`, которое
// ХОДИТ к воркерам (`/api/models/unload`, `/api/models/load`), то есть меняет
// состояние кластера. Рядом с `ServeHTTP` их было легко принять за часть
// маршрутизации, хотя это отдельный «применяющий» слой: его вызывает только
// `POST /api/v1/admin/cluster/autosuggest/apply`.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть, порядок шагов (unload → load →
// rollback при ошибке) сохранён.
package balancer

import (
	"fmt"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// R59 (2026-09-03): ComputeAutosuggestions — pure delegation to ComputeSuggestions.
// Accepts loadedByBackend (map backendId → []modelName) and returns ordered suggestions.
func (p *Proxy) ComputeAutosuggestions(loadedByBackend map[string][]string) []Suggestion {
	backends := p.GetAllBackends()
	if len(backends) == 0 {
		return nil
	}
	// GetAllBackends returns []types.Backend; ComputeSuggestions expects []*types.Backend
	ptrs := make([]*types.Backend, len(backends))
	for i := range backends {
		ptrs[i] = &backends[i]
	}
	return ComputeSuggestions(ptrs, loadedByBackend)
}

// R59.2 (2026-09-03): AutoDistributeMove — wire applyFn to real cppworker.
//
// Orchestrates a "move" suggestion by:
//  1. POST /api/models/unload to the source backend (free VRAM)
//  2. POST /api/models/load to the destination backend (use freed VRAM)
//  3. On load failure: rollback by reloading on source (best-effort)
//
// Returns nil on success, error string on failure (with rollback attempted).
// Used by handlers_autosuggest.go (POST /api/v1/admin/cluster/autosuggest/apply).
func (p *Proxy) AutoDistributeMove(fromBackendID, toBackendID, modelName string) error {
	if p.modelManager == nil {
		return fmt.Errorf("modelManager not initialized")
	}

	// Step 1: unload from source
	unloadReq := ModelOpRequest{
		Operation: "unload",
		ModelName: modelName,
	}
	unloadResult := p.modelManager.ExecuteOperation(fromBackendID, unloadReq)
	if unloadResult == nil {
		return fmt.Errorf("unload returned nil result")
	}
	if !unloadResult.Success {
		return fmt.Errorf("unload failed: %s", unloadResult.Error)
	}
	logger.Get().Infow("AutoDistributeMove: unloaded from source",
		"backend", fromBackendID, "model", modelName, "message", unloadResult.Message)

	// Step 2: load on destination
	loadReq := ModelOpRequest{
		Operation: "load",
		ModelName: modelName,
	}
	loadResult := p.modelManager.ExecuteOperation(toBackendID, loadReq)
	if loadResult == nil {
		// Try rollback
		p.rollbackAutoDistributeLoad(fromBackendID, modelName, "load returned nil result")
		return fmt.Errorf("load returned nil result")
	}
	if !loadResult.Success {
		// Try rollback
		p.rollbackAutoDistributeLoad(fromBackendID, modelName, loadResult.Error)
		return fmt.Errorf("load failed (rollback attempted): %s", loadResult.Error)
	}
	logger.Get().Infow("AutoDistributeMove: loaded on destination",
		"backend", toBackendID, "model", modelName, "message", loadResult.Message)
	return nil
}

// rollbackAutoDistributeLoad — best-effort reload on source after a failed load on destination.
// Logs the result but always returns (rollback is best-effort).
func (p *Proxy) rollbackAutoDistributeLoad(sourceBackendID, modelName, originalError string) {
	logger.Get().Warnw("AutoDistributeMove: rolling back — reloading on source after failed load",
		"backend", sourceBackendID, "model", modelName, "originalError", originalError)
	loadReq := ModelOpRequest{
		Operation: "load",
		ModelName: modelName,
	}
	result := p.modelManager.ExecuteOperation(sourceBackendID, loadReq)
	if result != nil && result.Success {
		logger.Get().Infow("AutoDistributeMove: rollback succeeded", "backend", sourceBackendID, "model", modelName)
	} else {
		errMsg := "unknown"
		if result != nil {
			errMsg = result.Error
		}
		logger.Get().Errorw("AutoDistributeMove: rollback FAILED — model lost",
			"backend", sourceBackendID, "model", modelName, "error", errMsg)
	}
}
