// Package rpccoordinator — Circuit Breaker (Phase 8.3: P.1 production mode).
//
// CircuitBreaker защищает ModelCoordinator от cascading failures при обращении
// к flaky workers. Реализует 3-state machine: Closed → Open → HalfOpen → Closed.
//
// State transitions:
//   - Closed → Open:      при failureCount >= failureThreshold
//   - Open → HalfOpen:    после resetTimeout с момента Open
//   - HalfOpen → Closed:  при первом успехе (successCount >= 1)
//   - HalfOpen → Open:    при первой неудаче (сбрасывает openSince, рестарт таймаута)
//
// Concurrency: mutex-protected. Allow() и Record*() thread-safe.
//
// Использование (Phase 9 dispatcher):
//
//	cb := rpccoordinator.NewCircuitBreaker(5, 30*time.Second)
//	if cb.Allow() {
//	    if err := worker.Infer(ctx, req); err != nil {
//	        cb.RecordFailure()
//	        // ... handle error
//	    } else {
//	        cb.RecordSuccess()
//	    }
//	}
package rpccoordinator

import (
	"sync"
	"time"
)

// CBState — состояние circuit breaker.
type CBState int

const (
	// StateClosed — normal operation, requests проходят.
	StateClosed CBState = iota
	// StateOpen — breaker открыт, requests заблокированы до resetTimeout.
	StateOpen
	// StateHalfOpen — пробуем 1 request для проверки восстановления.
	StateHalfOpen
)

// String возвращает читаемое имя состояния.
func (s CBState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// StateChangeCallback — callback при смене состояния.
// from, to — предыдущее и новое состояние.
type StateChangeCallback func(from, to CBState)

// CircuitBreakerConfig — параметры circuit breaker.
type CircuitBreakerConfig struct {
	// FailureThreshold — число failures перед Open. Default 5.
	FailureThreshold int
	// SuccessThreshold — число successes в HalfOpen для перехода в Closed.
	// Default 1 (one success → Closed).
	SuccessThreshold int
	// ResetTimeout — время в Open перед переходом в HalfOpen. Default 30s.
	ResetTimeout time.Duration
	// OnStateChange — optional callback для мониторинга / метрик.
	OnStateChange StateChangeCallback
}

// CircuitBreaker — thread-safe circuit breaker.
type CircuitBreaker struct {
	mu sync.Mutex

	state            CBState
	failureCount     int
	successCount     int
	halfOpenInFlight int // число requests в HalfOpen (для limit)

	// config — immutable copy при construction.
	failureThreshold int
	successThreshold int
	resetTimeout     time.Duration
	onStateChange    StateChangeCallback

	// openSince — время перехода в Open (для resetTimeout).
	openSince time.Time
	// lastTransition — для отладки / metrics.
	lastTransition time.Time
}

// NewCircuitBreaker создаёт breaker с дефолтами (5 failures, 30s reset).
func NewCircuitBreaker(failureThreshold int, resetTimeout time.Duration) *CircuitBreaker {
	return NewCircuitBreakerWithConfig(CircuitBreakerConfig{
		FailureThreshold: failureThreshold,
		SuccessThreshold: 1,
		ResetTimeout:     resetTimeout,
	})
}

// NewCircuitBreakerWithConfig создаёт breaker с полной конфигурацией.
func NewCircuitBreakerWithConfig(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = 1
	}
	if cfg.ResetTimeout <= 0 {
		cfg.ResetTimeout = 30 * time.Second
	}
	return &CircuitBreaker{
		state:            StateClosed,
		failureThreshold: cfg.FailureThreshold,
		successThreshold: cfg.SuccessThreshold,
		resetTimeout:     cfg.ResetTimeout,
		onStateChange:    cfg.OnStateChange,
		lastTransition:   time.Now(),
	}
}

// State возвращает текущее состояние (thread-safe).
func (cb *CircuitBreaker) State() CBState {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return cb.advanceState(cb.state, time.Now())
}

// Allow возвращает true если request может пройти. В HalfOpen — только
// первым concurrent caller'ам (по halfOpenInFlight counter).
func (cb *CircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	state := cb.advanceState(cb.state, now)

	switch state {
	case StateClosed:
		return true
	case StateOpen:
		return false
	case StateHalfOpen:
		// В HalfOpen — пропускаем только пока inFlight < 1 (probing).
		if cb.halfOpenInFlight < 1 {
			cb.halfOpenInFlight++
			return true
		}
		return false
	default:
		return false
	}
}

// RecordSuccess отмечает успешный request. В HalfOpen — может перевести в Closed.
// В Closed — сбрасывает failure counter.
func (cb *CircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	switch cb.state {
	case StateClosed:
		// В Closed: success сбрасывает failure counter (consecutive failures only).
		cb.failureCount = 0
	case StateHalfOpen:
		cb.halfOpenInFlight--
		cb.successCount++
		if cb.successCount >= cb.successThreshold {
			cb.transition(StateClosed, now)
		}
	case StateOpen:
		// Ignore (не должно случаться — Allow() = false в Open).
	}
}

// RecordFailure отмечает failed request. Может перевести в Open (из Closed)
// или остаться в Open (из HalfOpen).
func (cb *CircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	switch cb.state {
	case StateClosed:
		cb.failureCount++
		if cb.failureCount >= cb.failureThreshold {
			cb.transition(StateOpen, now)
		}
	case StateHalfOpen:
		cb.halfOpenInFlight--
		cb.failureCount++
		// HalfOpen failure → немедленно обратно в Open.
		cb.transition(StateOpen, now)
	case StateOpen:
		// Already Open — ignore.
	}
}

// advanceState — internal helper. Если в Open и timeout прошёл — переход в
// HalfOpen. Не вызывает transition() (чтобы избежать recursion в lock).
func (cb *CircuitBreaker) advanceState(current CBState, now time.Time) CBState {
	if current == StateOpen && now.Sub(cb.openSince) >= cb.resetTimeout {
		// Transition Open → HalfOpen. Mutate state in place.
		cb.state = StateHalfOpen
		cb.successCount = 0
		cb.failureCount = 0
		cb.halfOpenInFlight = 0
		cb.lastTransition = now
		if cb.onStateChange != nil {
			from := StateOpen
			to := StateHalfOpen
			// Call callback OUTSIDE lock to avoid deadlock.
			go cb.onStateChange(from, to)
		}
		return StateHalfOpen
	}
	return current
}

// transition — internal helper. Вызывается с lock held.
// Меняет state, обновляет counters, fires callback async.
func (cb *CircuitBreaker) transition(to CBState, now time.Time) {
	if cb.state == to {
		return // no-op
	}
	from := cb.state
	cb.state = to
	cb.lastTransition = now

	switch to {
	case StateOpen:
		cb.openSince = now
		cb.successCount = 0
		// Keep failureCount для diagnostics.
	case StateClosed:
		cb.failureCount = 0
		cb.successCount = 0
		cb.halfOpenInFlight = 0
	case StateHalfOpen:
		cb.successCount = 0
		cb.failureCount = 0
		cb.halfOpenInFlight = 0
	}

	// Fire callback async (вне lock — иначе deadlock если callback
	// обращается к breaker'у).
	if cb.onStateChange != nil {
		go cb.onStateChange(from, to)
	}
}

// Stats — диагностическая информация о breaker.
type CBStats struct {
	State            CBState     `json:"state"`
	FailureCount     int         `json:"failureCount"`
	SuccessCount     int         `json:"successCount"`
	HalfOpenInFlight int         `json:"halfOpenInFlight"`
	OpenSince        time.Time   `json:"openSince"`
	LastTransition   time.Time   `json:"lastTransition"`
	FailureThreshold int         `json:"failureThreshold"`
	SuccessThreshold int         `json:"successThreshold"`
	ResetTimeout     interface{} `json:"resetTimeout"` // time.Duration as string
}

// Stats возвращает снимок состояния breaker (для /metrics / debug endpoint).
func (cb *CircuitBreaker) Stats() CBStats {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return CBStats{
		State:            cb.state,
		FailureCount:     cb.failureCount,
		SuccessCount:     cb.successCount,
		HalfOpenInFlight: cb.halfOpenInFlight,
		OpenSince:        cb.openSince,
		LastTransition:   cb.lastTransition,
		FailureThreshold: cb.failureThreshold,
		SuccessThreshold: cb.successThreshold,
		ResetTimeout:     cb.resetTimeout.String(),
	}
}
