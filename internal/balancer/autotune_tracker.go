// autotune_tracker.go — R636 (2026-11-19): учёт попыток: AutoTuneTracker, снимки и доступ с Proxy.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"sync"
	"time"
)

// AutoTuneTracker — Round 54.4 (2026-08-24): per-Proxy tracker для circuit breakers.
// Хранит AutoTuneCircuit для каждой пары (backendID, modelName).
//
// Использование:
//
//	p.autoTuneTracker.GetCircuit(backendID, modelName) → returns circuit (or new)
//	p.autoTuneTracker.GetConfig() → circuit config
//
// Thread-safe (sync.RWMutex).
type AutoTuneTracker struct {
	mu       sync.RWMutex
	circuits map[string]*AutoTuneCircuit // key: "backendID|modelName"
	config   AutoTuneCircuitConfig
	// defers — R283: backoff после исхода «занято» (модель занята живым
	// инференсом). key тот же. Живёт здесь, а не в Proxy: трекер уже владеет
	// per-(backend,model) состоянием и мьютексом.
	defers map[string]*autoTuneDeferState
}

// NewAutoTuneTracker — Round 54.4 (2026-08-24): constructor.
func NewAutoTuneTracker(cfg AutoTuneCircuitConfig) *AutoTuneTracker {
	return &AutoTuneTracker{
		circuits: make(map[string]*AutoTuneCircuit),
		config:   cfg,
	}
}

// GetConfig — returns current circuit config.
func (t *AutoTuneTracker) GetConfig() AutoTuneCircuitConfig {
	if t == nil {
		return DefaultAutoTuneCircuitConfig()
	}
	return t.config
}

// GetCircuit — returns existing circuit or creates a new one (zero-state = closed).
func (t *AutoTuneTracker) GetCircuit(backendID, modelName string) *AutoTuneCircuit {
	if t == nil {
		return nil
	}
	key := backendID + "|" + modelName
	t.mu.RLock()
	c, ok := t.circuits[key]
	t.mu.RUnlock()
	if ok {
		return c
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	// Double-check after lock upgrade
	if c, ok := t.circuits[key]; ok {
		return c
	}
	c = &AutoTuneCircuit{}
	t.circuits[key] = c
	return c
}

// ResetCircuit — очистить circuit для (backend, model). Используется при successful reload.
func (t *AutoTuneTracker) ResetCircuit(backendID, modelName string) {
	if t == nil {
		return
	}
	key := backendID + "|" + modelName
	t.mu.Lock()
	defer t.mu.Unlock()
	c, ok := t.circuits[key]
	if ok {
		c.LastAttempt = time.Time{}
		c.LastSuccess = time.Time{}
		c.LastError = ""
	}
}

// GetCircuitSnapshot — Round 54.4 (2026-08-24): debug helper для /api/v1/admin/autotune.
// Возвращает копию circuit'а для API response.
func (t *AutoTuneTracker) GetCircuitSnapshot(backendID, modelName string) AutoTuneCircuitSnapshot {
	c := t.GetCircuit(backendID, modelName)
	if c == nil {
		return AutoTuneCircuitSnapshot{}
	}
	return AutoTuneCircuitSnapshot{
		LastAttempt: c.LastAttempt,
		LastSuccess: c.LastSuccess,
		LastError:   c.LastError,
		CanReload:   true, // computed by caller with config
	}
}

// AutoTuneCircuitSnapshot — API-friendly snapshot of circuit state.
type AutoTuneCircuitSnapshot struct {
	LastAttempt time.Time `json:"lastAttempt,omitempty"`
	LastSuccess time.Time `json:"lastSuccess,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	CanReload   bool      `json:"canReload"`
}

// AutoTuneReloadResult — Round 54.4 (2026-08-24): результат triggerAutoTuneReload.
// Используется в transport для логирования и метрик.
type AutoTuneReloadResult struct {
	Triggered     bool                // true если reload был запущен
	SkippedReason string              // если Triggered=false, почему
	Plan          *AutoTuneReloadPlan // nil если ничего не нужно менять
	CircuitState  AutoTuneCircuitSnapshot
}
