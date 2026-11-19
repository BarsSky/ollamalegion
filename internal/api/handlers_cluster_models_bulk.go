// handlers_cluster_models_bulk.go — R639 (2026-11-19): массовые операции: запрос/ответ, обработчик и параллельное исполнение по элементам.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_cluster_models.go` держал 1628 строк:
// агрегированные списки, перезагрузку модели, массовые операции и состояние
// модели на бэкенде. Здесь — один из слоёв; поведение не менялось.
package api

// bulkModelItem — одна модель в bulk-запросе.
type bulkModelItem struct {
	Model       string `json:"model"`                 // имя модели (обязательно)
	Operation   string `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: req.Operation)
	BackendID   string `json:"backendId,omitempty"`   // override глобального BackendID
	ContextSize *int   `json:"contextSize,omitempty"` // override глобального ContextSize
	GPULayers   *int   `json:"gpuLayers,omitempty"`   // override глобального GPUlayers
	Reason      string `json:"reason,omitempty"`      // комментарий для логов
	// Force — R66c: per-item override для выгрузки занятой модели (см.
	// clusterBulkModelsRequest.Force).
	Force *bool `json:"force,omitempty"`
}

// forceForItem — R66c: разрешает per-item override force поверх глобального.
func forceForItem(item *bulkModelItem, req *clusterBulkModelsRequest) *bool {
	if item != nil && item.Force != nil {
		return item.Force
	}
	if req != nil {
		return req.Force
	}
	return nil
}

// clusterBulkModelsRequest — тело POST /api/v1/cluster/models/bulk.
//
// Поддерживает два режима:
//  1. **Явный список моделей** (Models != nil && len > 0) — для multi-select UI
//     (Load/Unload/Delete Selected). Каждый элемент может переопределить
//     BackendID/ContextSize/GPULayers глобально или оставить их пустыми.
//  2. **Операция без списка** (Models == nil или пустой массив) — для командных
//     сценариев типа "Load All Available" / "Unload All Loaded". В этом случае:
//     - "unload" без списка → выгружаются все загруженные модели из cluster state.
//     - "load" без списка → 400 (опасно: нет списка моделей для загрузки).
//
// Приоритет параметров (от высшего к низшему):
//  1. Per-model override в BulkOperation.
//  2. Глобальный BackendID/ContextSize/GPULayers/Operation в запросе.
//  3. Значения по умолчанию ("load", n_ctx из профиля, gpu_layers=-1=auto).
type clusterBulkModelsRequest struct {
	Operation   string          `json:"operation,omitempty"`   // "load" | "unload" | "reload" (default: "load")
	Models      []bulkModelItem `json:"models"`                // список моделей (опционально для unload)
	BackendID   string          `json:"backendId,omitempty"`   // глобальный backendId override
	ContextSize *int            `json:"contextSize,omitempty"` // глобальный n_ctx override
	GPULayers   *int            `json:"gpuLayers,omitempty"`   // глобальный gpu_layers override (-1=auto)
	Insecure    bool            `json:"insecure,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	// Force — R66c (2026-09-22): принудительная выгрузка занятых моделей
	// (cppworker ?force=true). Применяется ко всем моделям списка, если у
	// элемента нет собственного Force.
	Force *bool `json:"force,omitempty"`
}

// clusterBulkModelsResponse — агрегированный результат bulk-операции по всем моделям.
//
// HTTP-статус всегда 200, кроме критичных ошибок (cluster state unavailable,
// невалидный JSON, превышение лимита моделей). Per-model и per-backend ошибки
// не ломают общий ответ — это согласуется с семантикой cluster endpoints из Session 17.
type clusterBulkModelsResponse struct {
	Operation  string                   `json:"operation"`  // фактически применённая операция
	Total      int                      `json:"total"`      // всего моделей в запросе
	Succeeded  int                      `json:"succeeded"`  // успешно выполнено (все бэкенды status=ok)
	Failed     int                      `json:"failed"`     // хотя бы один бэкенд ответил error
	Results    []clusterBulkModelResult `json:"results"`    // детали по каждой модели
	StartedAt  string                   `json:"startedAt"`  // RFC3339Nano начала выполнения
	DurationMs int64                    `json:"durationMs"` // общее время выполнения
}

// clusterBulkModelResult — результат одной модели из bulk-запроса.
type clusterBulkModelResult struct {
	Model     string                            `json:"model"`
	Operation string                            `json:"operation"` // применённая операция
	Succeeded bool                              `json:"succeeded"` // все бэкенды status=ok
	Results   []clusterReloadModelBackendResult `json:"results"`   // детали per-backend (переиспользуем Session 17)
}
