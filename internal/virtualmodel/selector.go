package virtualmodel

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// Re-export типов из pkg/types для удобства использования в этом пакете.
// План §P.2 определяет SelectionStrategy в pkg/types (общий с config парсером),
// здесь делаем алиасы чтобы не дублировать константы.
type (
	SelectionStrategy = types.SelectionStrategy
)

const (
	SelectionRoundRobin  = types.SelectionRoundRobin
	SelectionLeastLoaded = types.SelectionLeastLoaded
	SelectionRandom      = types.SelectionRandom
)

// Selector — interface для выбора backend'а из пула.
// Phase 8 P.2: 3 реализации (RoundRobin, LeastLoaded, Random).
//
// Все методы thread-safe. Select() возвращает backend ID ("host:port")
// или error если pool пуст / все backends unhealthy.
type Selector interface {
	// Name возвращает имя стратегии (для логов и metrics).
	Name() string
	// Select выбирает backend из pool. candidates — все известные backend'ы;
	// healthyCandidates — subset, отфильтрованный по health check.
	// Если healthyCandidates непуст — выбираем из него, иначе из candidates.
	Select(candidates []string, healthyCandidates []string) (string, error)
	// Reset сбрасывает внутреннее состояние (для тестов и config reload).
	Reset()
}

// NewSelector — factory: создаёт Selector по SelectionStrategy.
// Unknown / empty strategy → round_robin (default).
func NewSelector(strategy SelectionStrategy) Selector {
	switch strategy {
	case SelectionLeastLoaded:
		return NewLeastLoadedSelector()
	case SelectionRandom:
		return NewRandomSelector()
	case SelectionRoundRobin, "":
		return NewRoundRobinSelector()
	default:
		logger.Get().Warnw("unknown selection strategy, falling back to round_robin",
			"strategy", strategy)
		return NewRoundRobinSelector()
	}
}

// =====================================================================
// RoundRobinSelector — atomic counter, инкремент на каждый Select.
// =====================================================================

// RoundRobinSelector — простой round-robin через atomic uint64.
// Counter увеличивается на каждый Select; index = counter % len(pool).
// При изменении pool (config reload) используем mod по новой длине —
// в худшем случае несколько запросов пойдут на тот же backend, что
// безопасно (failover на следующем кандидате).
type RoundRobinSelector struct {
	counter atomic.Uint64
}

// NewRoundRobinSelector создаёт round-robin selector.
func NewRoundRobinSelector() *RoundRobinSelector {
	return &RoundRobinSelector{}
}

// Name возвращает имя стратегии.
func (s *RoundRobinSelector) Name() string { return "round_robin" }

// Select возвращает candidates[idx % len(candidates)].
func (s *RoundRobinSelector) Select(candidates []string, healthyCandidates []string) (string, error) {
	pool := healthyCandidates
	if len(pool) == 0 {
		pool = candidates
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("empty backend pool")
	}
	idx := s.counter.Add(1) - 1 // start с 0 на первый Select
	return pool[int(idx%uint64(len(pool)))], nil
}

// Reset обнуляет counter.
func (s *RoundRobinSelector) Reset() {
	s.counter.Store(0)
}

// =====================================================================
// LeastLoadedSelector — выбирает backend с max FreeSlots.
// =====================================================================

// BackendLoadInfo — load info для LeastLoadedSelector.
// FreeSlots — сколько запросов backend может принять сейчас (capacity - active).
type BackendLoadInfo struct {
	BackendID string
	FreeSlots int
}

// LoadProvider — функция возвращает load info для backend'а.
// Используется LeastLoadedSelector для динамического выбора.
//
// В Phase 8 P.2 это no-op stub (возвращает FreeSlots=0 для всех);
// в Session 2 будет wired с Proxy.GetBackendMetrics() для реального load tracking.
type LoadProvider func(backendID string) (freeSlots int, ok bool)

// LeastLoadedSelector выбирает backend с max FreeSlots (least busy).
// При равенстве — round-robin между равными (через atomic counter).
type LeastLoadedSelector struct {
	mu       sync.RWMutex
	counter  atomic.Uint64
	loadFunc LoadProvider
	// fallbackFreeSlots — default FreeSlots если loadFunc == nil или backend unknown.
	fallbackFreeSlots int
}

// NewLeastLoadedSelector создаёт selector с LoadProvider.
func NewLeastLoadedSelector() *LeastLoadedSelector {
	return &LeastLoadedSelector{
		fallbackFreeSlots: 1, // optimistic default
	}
}

// SetLoadProvider устанавливает функцию получения load info.
// Если nil — selector использует fallbackFreeSlots для всех.
func (s *LeastLoadedSelector) SetLoadProvider(fn LoadProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadFunc = fn
}

// Name возвращает имя стратегии.
func (s *LeastLoadedSelector) Name() string { return "least_loaded" }

// Select выбирает backend с max FreeSlots. При равенстве — round-robin.
func (s *LeastLoadedSelector) Select(candidates []string, healthyCandidates []string) (string, error) {
	pool := healthyCandidates
	if len(pool) == 0 {
		pool = candidates
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("empty backend pool")
	}

	s.mu.RLock()
	loadFn := s.loadFunc
	fallback := s.fallbackFreeSlots
	s.mu.RUnlock()

	// Собираем load info для каждого backend'а.
	type scored struct {
		id        string
		freeSlots int
	}
	scoredPool := make([]scored, len(pool))
	maxFree := -1
	for i, id := range pool {
		free := fallback
		if loadFn != nil {
			if f, ok := loadFn(id); ok {
				free = f
			}
		}
		scoredPool[i] = scored{id: id, freeSlots: free}
		if free > maxFree {
			maxFree = free
		}
	}

	// Фильтруем тех, у кого maxFree (best).
	best := make([]string, 0, len(pool))
	for _, s := range scoredPool {
		if s.freeSlots == maxFree {
			best = append(best, s.id)
		}
	}

	// Round-robin между равными (используем atomic counter).
	idx := s.counter.Add(1) - 1
	return best[int(idx%uint64(len(best)))], nil
}

// Reset сбрасывает counter и load provider.
func (s *LeastLoadedSelector) Reset() {
	s.counter.Store(0)
	s.mu.Lock()
	s.loadFunc = nil
	s.mu.Unlock()
}

// =====================================================================
// RandomSelector — math/rand для тестов и stress testing.
// =====================================================================

// RandomSelector — равномерное случайное распределение.
type RandomSelector struct {
	mu  sync.Mutex
	rng *rand.Rand
}

// NewRandomSelector создаёт selector с seeded RNG.
// В production используется без seed (для тестов можно переопределить через SetSeed).
func NewRandomSelector() *RandomSelector {
	return &RandomSelector{
		rng: rand.New(rand.NewSource(int64(rand.Uint64()))),
	}
}

// SetSeed устанавливает seed для RNG (для reproducible тестов).
func (s *RandomSelector) SetSeed(seed int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rng = rand.New(rand.NewSource(seed))
}

// Name возвращает имя стратегии.
func (s *RandomSelector) Name() string { return "random" }

// Select возвращает случайный backend из pool.
func (s *RandomSelector) Select(candidates []string, healthyCandidates []string) (string, error) {
	pool := healthyCandidates
	if len(pool) == 0 {
		pool = candidates
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("empty backend pool")
	}
	s.mu.Lock()
	idx := s.rng.Intn(len(pool))
	s.mu.Unlock()
	return pool[idx], nil
}

// Reset пересоздаёт RNG с новым seed.
func (s *RandomSelector) Reset() {
	s.mu.Lock()
	s.rng = rand.New(rand.NewSource(int64(rand.Uint64())))
	s.mu.Unlock()
}
