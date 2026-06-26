// Package rpccoordinator — B6: Load Balancing между срезами.
//
// Selector — компонент, который при наличии нескольких worker'ов-кандидатов
// на одну LayerSlice выбирает наименее загруженного и делает failover
// на следующего при ошибке.
//
// Идея:
//
//   - DistributedModel.SliceLayers[i].WorkerID = primary worker.
//   - DistributedModel.SliceLayers[i].WorkerCandidates[] = все кандидаты (включая primary).
//   - Selector.SelectForSlice(candidates) → ordered list, первым least-loaded.
//   - Infer() пробует их по очереди, после каждой ошибки переходит к следующему.
//
// Две стратегии:
//
//  1. RoundRobin — простой round-robin (для тестов и low-load).
//  2. LeastLoaded — выбор по min(active_requests / capacity).
//
// Стратегии реализуют Selector interface и подменяются через SetSelector().

package rpccoordinator

import (
	"errors"
	"fmt"
	"sync"

	"ollama-loadbalancer/pkg/logger"
)

// =====================================================================
// Worker load info
// =====================================================================

// WorkerLoadInfo — информация о загрузке worker'а (для selector'а).
//
// Capacity — сколько одновременных запросов worker может обработать
// (определяется GPU layers / RAM / n_parallel). Default — 1 (sequential).
type WorkerLoadInfo struct {
	WorkerID        string
	ActiveRequests  int
	Capacity        int
	Healthy         bool
	LastError       string // для диагностики
}

// LoadRatio возвращает текущую загрузку worker'а как долю (0..∞).
//
// 0.0 = свободен, 1.0 = полностью загружен, > 1.0 = перегружен.
func (w *WorkerLoadInfo) LoadRatio() float64 {
	if w.Capacity <= 0 {
		// Без данных о capacity считаем 1 active request = полная загрузка.
		if w.ActiveRequests > 0 {
			return float64(w.ActiveRequests)
		}
		return 0
	}
	return float64(w.ActiveRequests) / float64(w.Capacity)
}

// ErrNoHealthyWorker — все кандидаты unhealthy / не зарегистрированы.
var ErrNoHealthyWorker = errors.New("no healthy worker available")

// ErrNoCandidates — у среза нет ни одного кандидата (LayerSlice.WorkerCandidates пуст).
var ErrNoCandidates = errors.New("no worker candidates for slice")

// =====================================================================
// Selector interface
// =====================================================================

// Selector — интерфейс выбора worker'а.
//
// Select возвращает ordered список workerID (от лучшего к худшему).
// Первый в списке — primary, остальные — для failover.
//
// Каждая реализация должна быть thread-safe (используется из горутин Infer).
type Selector interface {
	// Name — имя стратегии (для логов и метрик).
	Name() string

	// Select выбирает кандидатов для среза.
	//
	// candidates — список workerID, которые обслуживают эту позицию слоя.
	// loadInfo — текущая нагрузка каждого worker'а (если есть).
	// Возвращает ordered список: первым — primary, далее — для failover.
	Select(candidates []string, loadInfo map[string]WorkerLoadInfo) ([]string, error)
}

// =====================================================================
// LeastLoadedSelector
// =====================================================================

// LeastLoadedSelector — выбор по минимальному LoadRatio.
//
// Healthy workers всегда идут перед unhealthy. Внутри группы —
// сортировка по LoadRatio ascending (наименее загруженный первым).
//
// Thread-safe (lock-free — stateless).
type LeastLoadedSelector struct{}

// NewLeastLoadedSelector — конструктор по умолчанию.
func NewLeastLoadedSelector() *LeastLoadedSelector {
	return &LeastLoadedSelector{}
}

// Name возвращает имя стратегии.
func (s *LeastLoadedSelector) Name() string {
	return "least_loaded"
}

// Select реализует least-loaded выбор.
func (s *LeastLoadedSelector) Select(candidates []string, loadInfo map[string]WorkerLoadInfo) ([]string, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}

	type entry struct {
		id      string
		healthy bool
		ratio   float64
		known   bool
	}

	entries := make([]entry, 0, len(candidates))
	for _, id := range candidates {
		e := entry{id: id, healthy: true, ratio: 0, known: false}
		if li, ok := loadInfo[id]; ok {
			e.known = true
			e.healthy = li.Healthy
			e.ratio = li.LoadRatio()
		}
		entries = append(entries, e)
	}

	// Partition: healthy + unhealthy.
	healthy := make([]entry, 0, len(entries))
	unhealthy := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.healthy {
			healthy = append(healthy, e)
		} else {
			unhealthy = append(unhealthy, e)
		}
	}

	// Sort healthy by LoadRatio ascending (least loaded first).
	for i := 0; i < len(healthy); i++ {
		for j := i + 1; j < len(healthy); j++ {
			if healthy[i].ratio > healthy[j].ratio {
				healthy[i], healthy[j] = healthy[j], healthy[i]
			}
		}
	}

	// Sort unhealthy: known with lower ratio first (best-effort failover),
	// unknown last.
	knownU := make([]entry, 0, len(unhealthy))
	unknownU := make([]entry, 0, len(unhealthy))
	for _, e := range unhealthy {
		if e.known {
			knownU = append(knownU, e)
		} else {
			unknownU = append(unknownU, e)
		}
	}
	for i := 0; i < len(knownU); i++ {
		for j := i + 1; j < len(knownU); j++ {
			if knownU[i].ratio > knownU[j].ratio {
				knownU[i], knownU[j] = knownU[j], knownU[i]
			}
		}
	}
	unhealthy = append(knownU, unknownU...)

	// Проверяем что хотя бы один worker доступен.
	hasAny := len(healthy) > 0 || len(unhealthy) > 0
	if !hasAny {
		return nil, ErrNoCandidates
	}

	out := make([]string, 0, len(healthy)+len(unhealthy))
	for _, e := range healthy {
		out = append(out, e.id)
	}
	for _, e := range unhealthy {
		out = append(out, e.id)
	}

	// Если в healthy никого нет — возвращаем best-effort (unhealthy) с warning.
	// Caller может решить использовать их как last-resort failover.
	return out, nil
}

// =====================================================================
// RoundRobinSelector
// =====================================================================

// RoundRobinSelector — простой round-robin (для тестов и homogeneous нагрузки).
//
// State — atomic counter, потокобезопасен.
type RoundRobinSelector struct {
	mu  sync.Mutex
	idx int
}

// NewRoundRobinSelector — конструктор.
func NewRoundRobinSelector() *RoundRobinSelector {
	return &RoundRobinSelector{}
}

// Name возвращает имя стратегии.
func (s *RoundRobinSelector) Name() string {
	return "round_robin"
}

// Select возвращает кандидатов, rotated на idx.
func (s *RoundRobinSelector) Select(candidates []string, _ map[string]WorkerLoadInfo) ([]string, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	s.mu.Lock()
	start := s.idx % len(candidates)
	s.idx++
	s.mu.Unlock()

	out := make([]string, len(candidates))
	for i := 0; i < len(candidates); i++ {
		out[i] = candidates[(start+i)%len(candidates)]
	}
	return out, nil
}

// =====================================================================
// Coordinator integration
// =====================================================================

// SetSelector заменяет стратегию coordinator.
//
// Если передан nil — устанавливается LeastLoadedSelector по умолчанию.
func (c *ModelCoordinator) SetSelector(sel Selector) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sel == nil {
		sel = NewLeastLoadedSelector()
	}
	c.selector = sel
	logger.Get().Infow("coordinator selector updated", "name", sel.Name())
}

// GetSelector возвращает текущую стратегию.
//
// Если не задана — возвращает LeastLoadedSelector (default).
func (c *ModelCoordinator) GetSelector() Selector {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.selector == nil {
		return NewLeastLoadedSelector()
	}
	return c.selector
}

// GetWorkerLoadInfo возвращает информацию о нагрузке всех зарегистрированных worker'ов.
//
// Используется selector'ом. По умолчанию — на основе GetMetrics RPC.
// Может быть переопределено через `WorkerLoadInfoProvider` если у worker'а
// есть другие источники нагрузки (например, Prometheus).
func (c *ModelCoordinator) GetWorkerLoadInfo() map[string]WorkerLoadInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make(map[string]WorkerLoadInfo, len(c.workers))
	for id, w := range c.workers {
		info := WorkerLoadInfo{
			WorkerID: id,
			Capacity: 1, // default sequential
		}
		// Используем кэшированные метрики из WorkerClient.
		if metrics, ok := w.LastMetrics.Load().(WorkerMetricsSnapshot); ok {
			info.ActiveRequests = int(metrics.ActiveRequests)
			if metrics.Capacity > 0 {
				info.Capacity = int(metrics.Capacity)
			}
			info.Healthy = w.IsHealthy()
		} else {
			info.Healthy = w.IsHealthy()
		}
		out[id] = info
	}
	return out
}

// SelectWorkersForSlice — convenience-обёртка: возвращает ordered список
// worker'ов-кандидатов через текущий selector.
//
// Если candidates пуст — возвращает ErrNoCandidates.
// Если selector возвращает ошибку — возвращает её.
func (c *ModelCoordinator) SelectWorkersForSlice(candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, ErrNoCandidates
	}
	loadInfo := c.GetWorkerLoadInfo()
	sel := c.GetSelector()
	ordered, err := sel.Select(candidates, loadInfo)
	if err != nil {
		return nil, fmt.Errorf("selector %q: %w", sel.Name(), err)
	}
	return ordered, nil
}