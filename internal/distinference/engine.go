// Package distinference — Вариант D: Distributed Inference (Custom Backend)
// Полностью кастомная реализация распределённого инференса на Go.
// Использует CGo-модуль (LLama.cpp binding) для работы со срезами модели
// и gRPC для коммуникации между Worker'ами.
package distinference

import (
	"sync"
)

// Engine — движок распределённого инференса.
// Управляет Worker'ами, распределяет слои модели, координирует
// выполнение запросов через gRPC.
type Engine struct {
	mu       sync.RWMutex
	enabled  bool
	grpcPort int
	workers  map[string]*Worker
}

// NewEngine создаёт новый движок распределённого инференса.
func NewEngine() *Engine {
	return &Engine{
		enabled:  false,
		workers:  make(map[string]*Worker),
	}
}

// Start запускает движок.
func (e *Engine) Start() error {
	return nil
}

// Stop останавливает движок.
func (e *Engine) Stop() error {
	return nil
}

// IsEnabled возвращает статус включения.
func (e *Engine) IsEnabled() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.enabled
}

// SetEnabled включает/выключает движок.
func (e *Engine) SetEnabled(enabled bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = enabled
}

// Infer выполняет распределённый инференс.
func (e *Engine) Infer(model string, input []byte, params map[string]string) ([]byte, error) {
	_ = model
	_ = input
	_ = params
	// TODO: Split model layers → distribute to workers → merge results
	return nil, nil
}

// RegisterWorker добавляет Worker'а в движок.
func (e *Engine) RegisterWorker(worker *Worker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workers[worker.ID()] = worker
}

// UnregisterWorker удаляет Worker'а.
func (e *Engine) UnregisterWorker(workerID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.workers, workerID)
}

// GetWorkers возвращает список Worker'ов.
func (e *Engine) GetWorkers() []*Worker {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make([]*Worker, 0, len(e.workers))
	for _, w := range e.workers {
		result = append(result, w)
	}
	return result
}
