package virtualmodel

import (
	"sync"

	"ollama-loadbalancer/pkg/types"
)

// Registry — реестр виртуальных моделей.
type Registry struct {
	mu       sync.RWMutex
	models   map[string]*VirtualModel // name → VirtualModel
	enabled  bool
}

// NewRegistry создаёт новый реестр виртуальных моделей.
func NewRegistry() *Registry {
	return &Registry{
		models:  make(map[string]*VirtualModel),
		enabled: false,
	}
}

// IsEnabled возвращает статус включения.
func (r *Registry) IsEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled
}

// SetEnabled включает/выключает реестр.
func (r *Registry) SetEnabled(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.enabled = enabled
}

// Register добавляет виртуальную модель в реестр.
func (r *Registry) Register(cfg types.VirtualModelConfig) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.models[cfg.Name] = NewVirtualModel(cfg)
	return nil
}

// Unregister удаляет виртуальную модель из реестра.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.models, name)
}

// Get возвращает виртуальную модель по имени.
func (r *Registry) Get(name string) *VirtualModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.models[name]
}

// List возвращает список всех виртуальных моделей.
func (r *Registry) List() []*VirtualModel {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*VirtualModel, 0, len(r.models))
	for _, vm := range r.models {
		result = append(result, vm)
	}
	return result
}

// IsVirtualModel проверяет, является ли имя модели виртуальной.
func (r *Registry) IsVirtualModel(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.models[name]
	return exists
}
