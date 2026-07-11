package virtualmodel

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =====================================================================
// RoundRobinSelector tests
// =====================================================================

func TestRoundRobinSelector_EmptyPool(t *testing.T) {
	t.Parallel()
	s := NewRoundRobinSelector()
	_, err := s.Select(nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty backend pool")

	_, err = s.Select([]string{}, []string{})
	require.Error(t, err)
}

func TestRoundRobinSelector_SingleBackend(t *testing.T) {
	t.Parallel()
	s := NewRoundRobinSelector()
	// Один backend — всегда выбирается.
	for i := 0; i < 5; i++ {
		got, err := s.Select([]string{"a:1"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "a:1", got)
	}
}

func TestRoundRobinSelector_MultipleBackends_Concurrency(t *testing.T) {
	t.Parallel()
	s := NewRoundRobinSelector()
	pool := []string{"a:1", "b:2", "c:3", "d:4"}

	// 12 итераций (3 полных цикла) — каждая позиция должна получить 3 hits.
	counts := make(map[string]int)
	for i := 0; i < 12; i++ {
		got, err := s.Select(pool, nil)
		require.NoError(t, err)
		counts[got]++
	}
	for _, p := range pool {
		assert.Equal(t, 3, counts[p], "each backend should get 3 hits in 12 iterations")
	}
}

func TestRoundRobinSelector_HealthyCandidatesPreferred(t *testing.T) {
	t.Parallel()
	s := NewRoundRobinSelector()
	candidates := []string{"a:1", "b:2", "c:3"}
	healthy := []string{"b:2"} // только b:2 healthy

	for i := 0; i < 3; i++ {
		got, err := s.Select(candidates, healthy)
		require.NoError(t, err)
		assert.Equal(t, "b:2", got, "should always pick from healthy subset")
	}
}

func TestRoundRobinSelector_Reset(t *testing.T) {
	t.Parallel()
	s := NewRoundRobinSelector()
	s.Select([]string{"a:1", "b:2"}, nil)
	s.Select([]string{"a:1", "b:2"}, nil)
	s.Reset()
	// После reset counter=0, следующий Select даст pool[0] = "a:1".
	got, err := s.Select([]string{"a:1", "b:2"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "a:1", got)
}

// =====================================================================
// LeastLoadedSelector tests
// =====================================================================

func TestLeastLoadedSelector_EmptyPool(t *testing.T) {
	t.Parallel()
	s := NewLeastLoadedSelector()
	_, err := s.Select(nil, nil)
	require.Error(t, err)
}

func TestLeastLoadedSelector_NoLoadProvider_Fallback(t *testing.T) {
	t.Parallel()
	s := NewLeastLoadedSelector()
	// Без LoadProvider — fallback FreeSlots=1 для всех, выбор round-robin между всеми.
	pool := []string{"a:1", "b:2", "c:3"}
	counts := make(map[string]int)
	for i := 0; i < 6; i++ {
		got, err := s.Select(pool, nil)
		require.NoError(t, err)
		counts[got]++
	}
	// 6 итераций × 3 backends с fallback=1 → каждый получает 2 hits.
	for _, p := range pool {
		assert.Equal(t, 2, counts[p])
	}
}

func TestLeastLoadedSelector_WithLoadProvider_PicksMaxFree(t *testing.T) {
	t.Parallel()
	s := NewLeastLoadedSelector()
	// b:2 имеет max FreeSlots (10), должен выбираться всегда.
	s.SetLoadProvider(func(backendID string) (int, bool) {
		switch backendID {
		case "a:1":
			return 2, true
		case "b:2":
			return 10, true
		case "c:3":
			return 5, true
		}
		return 0, false
	})

	pool := []string{"a:1", "b:2", "c:3"}
	for i := 0; i < 5; i++ {
		got, err := s.Select(pool, nil)
		require.NoError(t, err)
		assert.Equal(t, "b:2", got, "b:2 has max FreeSlots, should always win")
	}
}

func TestLeastLoadedSelector_RoundRobinBetweenEquals(t *testing.T) {
	t.Parallel()
	s := NewLeastLoadedSelector()
	// a:1 и c:3 имеют equal max FreeSlots=5, b:2 меньше (2).
	// Ожидаем: выбор между a:1 и c:3 round-robin.
	s.SetLoadProvider(func(backendID string) (int, bool) {
		switch backendID {
		case "a:1":
			return 5, true
		case "b:2":
			return 2, true
		case "c:3":
			return 5, true
		}
		return 0, false
	})

	pool := []string{"a:1", "b:2", "c:3"}
	counts := make(map[string]int)
	for i := 0; i < 10; i++ {
		got, err := s.Select(pool, nil)
		require.NoError(t, err)
		counts[got]++
	}
	assert.Equal(t, 0, counts["b:2"], "b:2 has lowest load, should be skipped")
	assert.Equal(t, 5, counts["a:1"], "a:1 and c:3 should split 10 iterations")
	assert.Equal(t, 5, counts["c:3"])
}

// =====================================================================
// RandomSelector tests
// =====================================================================

func TestRandomSelector_EmptyPool(t *testing.T) {
	t.Parallel()
	s := NewRandomSelector()
	_, err := s.Select(nil, nil)
	require.Error(t, err)
}

func TestRandomSelector_SingleBackend(t *testing.T) {
	t.Parallel()
	s := NewRandomSelector()
	s.SetSeed(42) // reproducible
	for i := 0; i < 5; i++ {
		got, err := s.Select([]string{"a:1"}, nil)
		require.NoError(t, err)
		assert.Equal(t, "a:1", got)
	}
}

func TestRandomSelector_Distribution_Reproducible(t *testing.T) {
	t.Parallel()
	s := NewRandomSelector()
	s.SetSeed(42)
	pool := []string{"a:1", "b:2", "c:3", "d:4"}
	counts := make(map[string]int)
	for i := 0; i < 1000; i++ {
		got, err := s.Select(pool, nil)
		require.NoError(t, err)
		counts[got]++
	}
	// Random distribution — каждый получает ~250 (±50 — не строго, но в пределах).
	for _, p := range pool {
		assert.Greater(t, counts[p], 150, "expected ~250 hits per backend, got %d for %s", counts[p], p)
		assert.Less(t, counts[p], 350, "expected ~250 hits per backend, got %d for %s", counts[p], p)
	}
}

func TestRandomSelector_HealthyCandidatesPreferred(t *testing.T) {
	t.Parallel()
	s := NewRandomSelector()
	s.SetSeed(42)
	candidates := []string{"a:1", "b:2", "c:3"}
	healthy := []string{"c:3"}
	for i := 0; i < 10; i++ {
		got, err := s.Select(candidates, healthy)
		require.NoError(t, err)
		assert.Equal(t, "c:3", got)
	}
}

// =====================================================================
// NewSelector factory test
// =====================================================================

func TestNewSelector_AllStrategies(t *testing.T) {
	t.Parallel()
	tests := []struct {
		strategy  SelectionStrategy
		wantName  string
	}{
		{SelectionRoundRobin, "round_robin"},
		{SelectionLeastLoaded, "least_loaded"},
		{SelectionRandom, "random"},
		{"", "round_robin"}, // empty → default
		{"unknown_strategy", "round_robin"}, // unknown → fallback
	}
	for _, tt := range tests {
		s := NewSelector(tt.strategy)
		assert.Equal(t, tt.wantName, s.Name(), "strategy=%q", tt.strategy)
	}
}

// =====================================================================
// Concurrent safety test (run with -race)
// =====================================================================

func TestSelectors_ConcurrentSafety(t *testing.T) {
	t.Parallel()
	selectors := []Selector{
		NewRoundRobinSelector(),
		NewLeastLoadedSelector(),
		NewRandomSelector(),
	}
	pool := []string{"a:1", "b:2", "c:3", "d:4", "e:5"}

	var wg sync.WaitGroup
	var counter atomic.Int64
	for _, s := range selectors {
		for i := 0; i < 50; i++ {
			wg.Add(1)
			go func(sel Selector) {
				defer wg.Done()
				for j := 0; j < 100; j++ {
					got, err := sel.Select(pool, nil)
					if err == nil {
						assert.Contains(t, pool, got)
						counter.Add(1)
					}
				}
			}(s)
		}
	}
	wg.Wait()
	assert.Equal(t, int64(15000), counter.Load(),
		"all 3 selectors × 50 goroutines × 100 iters = 15000 selects")
}

// =====================================================================
// Regression: ensure Selector is interface-compatible
// =====================================================================

func TestSelector_InterfaceAssignment(t *testing.T) {
	t.Parallel()
	var _ Selector = (*RoundRobinSelector)(nil)
	var _ Selector = (*LeastLoadedSelector)(nil)
	var _ Selector = (*RandomSelector)(nil)

	// Compile-time check: NewSelector returns interface.
	s := NewSelector(SelectionRoundRobin)
	_ = s
	// Use fmt to suppress "imported and not used" if needed.
	_ = fmt.Sprintf("%T", s)
}
