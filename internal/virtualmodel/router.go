package virtualmodel

import (
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Router маршрутизирует запросы через срезы виртуальных моделей.
// Определяет, является ли модель виртуальной, и направляет запрос
// через соответствующий pipeline.
type Router struct {
	registry *Registry
	mu       sync.RWMutex
}

// NewRouter создаёт новый маршрутизатор виртуальных моделей.
func NewRouter(registry *Registry) *Router {
	return &Router{
		registry: registry,
	}
}

// Route направляет запрос через виртуальную модель, если применимо.
// Возвращает true, если запрос был обработан как виртуальный.
// Параметры:
//   - modelName: имя виртуальной модели
//   - input: тело запроса (JSON)
//   - params: дополнительные параметры
//   - opts: опции pipeline (WithBackendLoadFn, WithRequestID)
func (r *Router) Route(modelName string, input []byte, params map[string]string, opts ...PipelineOption) ([]byte, bool, error) {
	if !r.registry.IsEnabled() {
		return nil, false, nil
	}

	vm := r.registry.Get(modelName)
	if vm == nil {
		return nil, false, nil
	}

	logger.Get().Infow("routing through virtual model",
		"model", modelName,
		"slices", len(vm.Config.Slices),
		"mode", vm.Config.Coordination.Mode)

	// Выполняем pipeline
	result, err := vm.ExecutePipeline(input, params, opts...)
	if err != nil {
		return nil, true, fmt.Errorf("virtual model %s pipeline failed: %w", modelName, err)
	}

	return result, true, nil
}

// GetSliceTarget выбирает целевой бэкенд для среза.
// Возвращает строку вида "host:port".
// sliceConfig — это types.ModelSliceConfig.
func (r *Router) GetSliceTarget(sliceConfig interface{}) string {
	cfg, ok := sliceConfig.(types.ModelSliceConfig)
	if !ok {
		logger.Get().Errorw("invalid slice config type")
		return ""
	}

	if len(cfg.TargetBackends) == 0 {
		return ""
	}

	// Возвращаем первый доступный бэкенд
	host, port, err := selectBackendForSlice(cfg.TargetBackends, nil)
	if err != nil {
		logger.Get().Warnw("failed to select backend for slice",
			"slice", cfg.ID, "error", err)
		return ""
	}

	return fmt.Sprintf("%s:%d", host, port)
}

// GetRegistry возвращает реестр виртуальных моделей.
func (r *Router) GetRegistry() *Registry {
	return r.registry
}

// ListVirtualModels возвращает список всех виртуальных моделей с информацией.
func (r *Router) ListVirtualModels() []map[string]interface{} {
	if !r.registry.IsEnabled() {
		return nil
	}

	models := r.registry.List()
	result := make([]map[string]interface{}, 0, len(models))
	for _, vm := range models {
		info := map[string]interface{}{
			"name":        vm.Config.Name,
			"description": vm.Config.Description,
			"slices":      len(vm.Config.Slices),
			"mode":        vm.Config.Coordination.Mode,
			"activeJobs":  vm.GetActiveJobs(),
			"timeoutMs":   vm.Config.Coordination.TimeoutMs,
		}
		// Добавляем информацию о срезах
		slices := make([]map[string]interface{}, 0, len(vm.Config.Slices))
		for _, slice := range vm.Config.Slices {
			slices = append(slices, map[string]interface{}{
				"id":             slice.ID,
				"modelName":      slice.ModelName,
				"ordinal":        slice.Ordinal,
				"targetBackends": slice.TargetBackends,
				"fallbackMode":   slice.FallbackMode,
			})
		}
		info["slices"] = slices
		result = append(result, info)
	}
	return result
}

// GetVirtualModelStatus возвращает статус конкретной виртуальной модели.
func (r *Router) GetVirtualModelStatus(name string) map[string]interface{} {
	if !r.registry.IsEnabled() {
		return nil
	}

	vm := r.registry.Get(name)
	if vm == nil {
		return nil
	}

	return map[string]interface{}{
		"name":        vm.Config.Name,
		"description": vm.Config.Description,
		"slices":      len(vm.Config.Slices),
		"mode":        vm.Config.Coordination.Mode,
		"activeJobs":  vm.GetActiveJobs(),
		"timeoutMs":   vm.Config.Coordination.TimeoutMs,
		"enabled":     r.registry.IsEnabled(),
	}
}
