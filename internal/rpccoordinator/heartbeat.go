// Package rpccoordinator — Heartbeat & Auto-Discovery (B3).
//
// HeartbeatLoop — фоновая горутина, которая периодически делает
// HealthCheck() на каждом зарегистрированном worker'е и помечает unhealthy.
//
// B3 контракт:
//   - HealthCheckInterval: 30s (default), 0 = отключён.
//   - UnhealthyThreshold: 3 (default) — после 3 неудач подряд worker помечается unhealthy.
//   - Workers, которые вернулись healthy, — восстанавливаются обратно.
//   - Здоровье отслеживается атомарно через WorkerClient.setHealthy / IsHealthy.
package rpccoordinator

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// HeartbeatConfig — настройки heartbeat loop.
type HeartbeatConfig struct {
	Interval           time.Duration // 0 = heartbeat выключен
	UnhealthyThreshold int           // сколько подряд неудач до unhealthy
}

// DefaultHeartbeatConfig — дефолты.
func DefaultHeartbeatConfig() HeartbeatConfig {
	return HeartbeatConfig{
		Interval:           30 * time.Second,
		UnhealthyThreshold: 3,
	}
}

// heartbeatState — счётчик неудач и метрики для одного worker'а.
type heartbeatState struct {
	consecutiveFailures atomic.Int64
	lastSuccessAt       atomic.Int64 // unix nano
	lastFailureAt       atomic.Int64
}

// HeartbeatLoop — фоновый health-checker для всех зарегистрированных worker'ов.
//
// B3: thread-safe; запускается через Start() и останавливается через Stop().
type HeartbeatLoop struct {
	mu      sync.Mutex
	states  map[string]*heartbeatState // workerID → state
	cfg     HeartbeatConfig
	cancel  context.CancelFunc
	doneCh  chan struct{}
	coord   *ModelCoordinator
	running atomic.Bool
}

// NewHeartbeatLoop создаёт новый loop.
func NewHeartbeatLoop(coord *ModelCoordinator, cfg HeartbeatConfig) *HeartbeatLoop {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultHeartbeatConfig().Interval
	}
	if cfg.UnhealthyThreshold <= 0 {
		cfg.UnhealthyThreshold = DefaultHeartbeatConfig().UnhealthyThreshold
	}
	return &HeartbeatLoop{
		states: make(map[string]*heartbeatState),
		cfg:    cfg,
		coord:  coord,
	}
}

// Start запускает фоновый heartbeat в отдельной горутине.
//
// Если loop уже запущен — no-op. Идемпотентно.
func (h *HeartbeatLoop) Start() {
	if !h.running.CompareAndSwap(false, true) {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.doneCh = make(chan struct{})

	go h.run(ctx)
	logger.Get().Infow("heartbeat loop started",
		"interval", h.cfg.Interval,
		"unhealthy_threshold", h.cfg.UnhealthyThreshold)
}

// Stop останавливает heartbeat loop и ждёт завершения горутины.
//
// Идемпотентно — повторный вызов no-op.
func (h *HeartbeatLoop) Stop() {
	if !h.running.CompareAndSwap(true, false) {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	if h.doneCh != nil {
		<-h.doneCh
	}
	logger.Get().Infow("heartbeat loop stopped")
}

// IsRunning возвращает true, если loop активен.
func (h *HeartbeatLoop) IsRunning() bool {
	return h.running.Load()
}

// StateFor возвращает snapshot heartbeat state для worker'а.
//
// Если worker не наблюдался — возвращает nil.
func (h *HeartbeatLoop) StateFor(workerID string) (consecutiveFailures int64, lastSuccess, lastFailure time.Time, tracked bool) {
	h.mu.Lock()
	state, ok := h.states[workerID]
	h.mu.Unlock()
	if !ok || state == nil {
		return 0, time.Time{}, time.Time{}, false
	}
	return state.consecutiveFailures.Load(), time.Unix(0, state.lastSuccessAt.Load()),
		time.Unix(0, state.lastFailureAt.Load()), true
}

// run — основной цикл heartbeat'а.
func (h *HeartbeatLoop) run(ctx context.Context) {
	defer close(h.doneCh)

	ticker := time.NewTicker(h.cfg.Interval)
	defer ticker.Stop()

	// Первый проход сразу (не ждём первый tick).
	h.tickAll()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.tickAll()
		}
	}
}

// tickAll делает HealthCheck() для всех worker'ов и обновляет состояния.
func (h *HeartbeatLoop) tickAll() {
	workers := h.coord.ListWorkers()
	for _, w := range workers {
		h.tickWorker(w.WorkerID)
	}
}

// tickWorker делает один HealthCheck и обновляет state.
func (h *HeartbeatLoop) tickWorker(workerID string) {
	state := h.stateForOrCreate(workerID)
	wc := h.coord.GetWorker(workerID)
	if wc == nil {
		return // worker уже удалён
	}

	ok, err := wc.HealthCheck()
	if err != nil {
		logger.Get().Warnw("heartbeat: worker health check failed",
			"worker", workerID, "error", err)
	}
	state.lastFailureAt.Store(time.Now().UnixNano())
	if ok {
		state.consecutiveFailures.Store(0)
		state.lastSuccessAt.Store(time.Now().UnixNano())
		// Если worker вернулся healthy после unhealthy — restore.
		if !wc.IsHealthy() {
			logger.Get().Infow("heartbeat: worker recovered",
				"worker", workerID)
		}
		return
	}

	// Health check failed.
	fails := state.consecutiveFailures.Add(1)
	if !wc.IsHealthy() {
		// Уже unhealthy — без действий.
		return
	}
	if int(fails) >= h.cfg.UnhealthyThreshold {
		// Перешли порог — помечаем unhealthy.
		// WorkerClient.setHealthy делает wc.healthy=false.
		// Через wc.IsHealthy() балансер увидит unhealthy state.
		wc.setHealthy(false)
		logger.Get().Errorw("heartbeat: worker marked unhealthy after consecutive failures",
			"worker", workerID,
			"consecutive_failures", fails,
			"threshold", h.cfg.UnhealthyThreshold)
	}
}

// stateForOrCreate возвращает state для worker'а, создавая если нет.
func (h *HeartbeatLoop) stateForOrCreate(workerID string) *heartbeatState {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.states[workerID]; ok && s != nil {
		return s
	}
	s := &heartbeatState{}
	h.states[workerID] = s
	return s
}

// Forget удаляет state для worker'а (например, при UnregisterWorker).
//
// Не блокирует.
func (h *HeartbeatLoop) Forget(workerID string) {
	h.mu.Lock()
	delete(h.states, workerID)
	h.mu.Unlock()
}