// Package balancer — load failure backoff (Round 26 v0.5.14, 2026-08-04).
//
// Проблема из v0.5.13: ensureModelLoadedOnBackend не имел backoff после failed load.
// При upstream bug (например, gemma-4 GGML_ASSERT при n_ctx>32768) cppworker
// падал, Docker перезапускал его, balancer auto-load'ил снова, cppworker
// падал снова, бесконечный loop.
//
// Решение: track failure count per (backend, model). После N consecutive failures
// (default 3) открывается circuit breaker, balancer возвращает ошибку сразу без
// попытки load'а. Через TTL (default 60s) backoff сбрасывается, пробуем снова.
//
// Это не устраняет upstream bug, но предотвращает бесконечный retry и даёт
// пользователю понятную ошибку вместо 502 каждые 1m36s.
package balancer

import (
	"sync"
	"time"

	"ollama-loadbalancer/pkg/logger"
)

// loadBackoffConfig — настройки backoff'а для failed loads.
type loadBackoffConfig struct {
	// maxConsecutiveFailures — после скольких подряд failed load открываем breaker.
	maxConsecutiveFailures int
	// breakerOpenDuration — как долго breaker остаётся открытым.
	breakerOpenDuration time.Duration
	// failureWindow — "consecutive" failures считаются в этом окне.
	// Если последний failure был > failureWindow назад, счётчик сбрасывается.
	failureWindow time.Duration
}

func defaultLoadBackoffConfig() loadBackoffConfig {
	return loadBackoffConfig{
		maxConsecutiveFailures: 3,
		breakerOpenDuration:    60 * time.Second,
		failureWindow:          5 * time.Minute,
	}
}

// loadBackoffState — состояние circuit breaker'а для конкретной пары (backend, model).
type loadBackoffState struct {
	failures         int       // текущее количество подряд failures
	lastFailureAt    time.Time // время последней failure
	breakerOpenUntil time.Time // если != zero, breaker открыт до этого времени
}

// loadBackoff — thread-safe registry backoff'ов.
type loadBackoff struct {
	mu      sync.Mutex
	states  map[string]*loadBackoffState // key: backendID + "|" + modelName
	config  loadBackoffConfig
}

func newLoadBackoff() *loadBackoff {
	return &loadBackoff{
		states: make(map[string]*loadBackoffState),
		config: defaultLoadBackoffConfig(),
	}
}

func key(backendID, modelName string) string {
	return backendID + "|" + modelName
}

// shouldSkip — true если breaker открыт (не пытаемся load'ить сейчас).
// Также возвращает reason для логирования.
func (lb *loadBackoff) shouldSkip(backendID, modelName string) (bool, string, time.Time) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	st, ok := lb.states[key(backendID, modelName)]
	if !ok {
		return false, "", time.Time{}
	}

	now := time.Now()

	// Breaker открыт?
	if !st.breakerOpenUntil.IsZero() && now.Before(st.breakerOpenUntil) {
		return true, "circuit breaker open", st.breakerOpenUntil
	}

	// Breaker был открыт но уже истёк — reset
	if !st.breakerOpenUntil.IsZero() && !now.Before(st.breakerOpenUntil) {
		st.failures = 0
		st.breakerOpenUntil = time.Time{}
	}

	// failureWindow истёк — сбрасываем счётчик
	if !st.lastFailureAt.IsZero() && now.Sub(st.lastFailureAt) > lb.config.failureWindow {
		st.failures = 0
	}

	return false, "", time.Time{}
}

// recordFailure — увеличивает счётчик failures, открывает breaker если нужно.
func (lb *loadBackoff) recordFailure(backendID, modelName, errMsg string) (breakerOpened bool, retryAfter time.Duration) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	k := key(backendID, modelName)
	st, ok := lb.states[k]
	if !ok {
		st = &loadBackoffState{}
		lb.states[k] = st
	}

	now := time.Now()
	// Если прошло больше failureWindow с последней failure — сбрасываем
	// счётчик. Это предотвращает "старые" failures накапливаться со свежими.
	if !st.lastFailureAt.IsZero() && now.Sub(st.lastFailureAt) > lb.config.failureWindow {
		st.failures = 0
	}
	st.failures++
	st.lastFailureAt = now

	if st.failures >= lb.config.maxConsecutiveFailures {
		st.breakerOpenUntil = now.Add(lb.config.breakerOpenDuration)
		breakerOpened = true
		retryAfter = lb.config.breakerOpenDuration
		logger.Get().Warnw("loadBackoff: circuit breaker opened",
			"backend", backendID, "model", modelName,
			"failures", st.failures,
			"openUntil", st.breakerOpenUntil.Format(time.RFC3339),
			"errMsg", errMsg)
	} else {
		logger.Get().Debugw("loadBackoff: failure recorded",
			"backend", backendID, "model", modelName,
			"failures", st.failures,
			"threshold", lb.config.maxConsecutiveFailures,
			"errMsg", errMsg)
	}

	return breakerOpened, retryAfter
}

// recordSuccess — сбрасывает счётчик failures (load succeeded).
func (lb *loadBackoff) recordSuccess(backendID, modelName string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	k := key(backendID, modelName)
	if st, ok := lb.states[k]; ok {
		if st.failures > 0 || !st.breakerOpenUntil.IsZero() {
			logger.Get().Infow("loadBackoff: success, resetting state",
				"backend", backendID, "model", modelName,
				"previousFailures", st.failures)
		}
		delete(lb.states, k)
	}
}

// Reset — R60.11 (2026-09-07): manual reset of circuit breaker for (backend, model).
//
// Use case: after operator has manually fixed the underlying issue (e.g.
// `docker restart cppworker`, fixed config, etc.) they want the balancer
// to retry immediately without waiting for the TTL (60s default).
// Without this, the only way to clear the breaker was to wait 60s
// or restart the balancer container.
//
// Returns true if state was actually cleared (or didn't exist), false if
// the (backend, model) wasn't tracked.
func (lb *loadBackoff) Reset(backendID, modelName string) bool {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	k := key(backendID, modelName)
	if _, ok := lb.states[k]; !ok {
		return false // no state to reset
	}
	delete(lb.states, k)
	logger.Get().Infow("loadBackoff: manual reset (R60.11)",
		"backend", backendID, "model", modelName)
	return true
}

// ResetAll — R60.11: reset all circuit breaker states. Used by admin
// endpoint `POST /api/v1/balancer/load-backoff/reset` without query params.
// Returns count of states that were cleared.
func (lb *loadBackoff) ResetAll() int {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	count := len(lb.states)
	if count > 0 {
		logger.Get().Infow("loadBackoff: manual reset all (R60.11)",
			"states_cleared", count)
	}
	lb.states = make(map[string]*loadBackoffState)
	return count
}

// state — snapshot для diagnostics.
type loadBackoffSnapshot struct {
	BackendID       string `json:"backendId"`
	ModelName       string `json:"modelName"`
	Failures        int    `json:"failures"`
	LastFailureAt   string `json:"lastFailureAt,omitempty"`
	BreakerOpenUntil string `json:"breakerOpenUntil,omitempty"`
}

// LoadBackoffSnapshot — R60.11 (2026-09-07): public alias для admin endpoint.
type LoadBackoffSnapshot = loadBackoffSnapshot

// snapshot — список всех backoff states.
func (lb *loadBackoff) snapshot() []loadBackoffSnapshot {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	result := make([]loadBackoffSnapshot, 0, len(lb.states))
	for k, st := range lb.states {
		parts := splitKey(k)
		snap := loadBackoffSnapshot{
			BackendID: parts[0],
			ModelName: parts[1],
			Failures:  st.failures,
		}
		if !st.lastFailureAt.IsZero() {
			snap.LastFailureAt = st.lastFailureAt.Format(time.RFC3339)
		}
		if !st.breakerOpenUntil.IsZero() {
			snap.BreakerOpenUntil = st.breakerOpenUntil.Format(time.RFC3339)
		}
		result = append(result, snap)
	}
	return result
}

// splitKey — обратно к (backendID, modelName).
func splitKey(k string) []string {
	for i := 0; i < len(k); i++ {
		if k[i] == '|' {
			return []string{k[:i], k[i+1:]}
		}
	}
	return []string{k, ""}
}

// Snapshot — R60.11 (2026-09-07): public API для admin endpoint
// (GET /api/v1/balancer/load-backoff). Returns all current states.
func (lb *loadBackoff) Snapshot() []loadBackoffSnapshot {
	return lb.snapshot()
}
