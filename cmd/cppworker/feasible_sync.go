// cmd/cppworker/feasible_sync.go — Round 37 (2026-08-18) background sync.
//
// PREVENTS: 2026-08-18 production bug class — profile.contextLength conservative
// vs hardware allows 2x more. Without this warning, operator doesn't know
// they're leaving context on the table until a Cline request fails 413.
//
// LIFECYCLE:
//   1. start() runs every 5 min (configurable via env)
//   2. for each loaded model, compare profile.contextLength vs ComputeFeasible
//   3. if profile < feasible - 10%, log warning with explicit recommendation
//
// The warning IS THE FIX — operator sees "profile conservative, increase to 65536
// via PUT /api/v1/cppworker/model-profiles/<model>" and can act immediately.
//
// See docs/runbook-tools.md for the operator runbook.
package main

import (
	"context"
	"sync"
	"time"

	"ollama-loadbalancer/internal/cppbackend"
)

// feasibleSyncT — periodic conservative-profile detector.
//
// Потокобезопасно: feasibleWarningsMu защищает lastWarnings map.
type feasibleSyncT struct {
	mu             sync.Mutex
	lastWarnings   map[string]feasibleWarning // modelName → last warning emitted
	balancerURL    string                     // для cross-reference с profileSyncer
	stopCh         chan struct{}
	warnThrottle   time.Duration // минимум между warnings для одной модели
	logger         loggerLike
	getProfileCtx  func(modelName string) int // возвращает profile.contextLength
	computeFeasCtx func(modelName string) (*cppbackend.FeasibleInfo, error)
	interval       time.Duration
}

// feasibleWarning — что мы залогировали (для throttle).
type feasibleWarning struct {
	ProfileNCtx    int       `json:"profile_n_ctx"`
	FeasibleNCtx   int       `json:"feasible_n_ctx"`
	Ratio          float64   `json:"ratio"` // feasible/profile — > 1.5 = 50% headroom
	EmittedAt      time.Time `json:"emitted_at"`
}

// loggerLike — интерфейс для тестирования (не тянем zap в test)
type loggerLike interface {
	Warnw(msg string, keysAndValues ...interface{})
	Infow(msg string, keysAndValues ...interface{})
	Debugw(msg string, keysAndValues ...interface{})
}

// newFeasibleSync создаёт syncer. interval = 0 → дефолт 5 min.
// warnThrottle = 0 → дефолт 1 час (чтобы не спамить).
func newFeasibleSync(interval, warnThrottle time.Duration, balancerURL string) *feasibleSyncT {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if warnThrottle <= 0 {
		warnThrottle = 1 * time.Hour
	}
	return &feasibleSyncT{
		lastWarnings: make(map[string]feasibleWarning),
		balancerURL:  balancerURL,
		stopCh:       make(chan struct{}),
		warnThrottle: warnThrottle,
		interval:     interval,
	}
}

// start запускает background loop. ctx отменяется → loop завершается.
func (fs *feasibleSyncT) start(ctx context.Context) {
	if fs == nil {
		return
	}
	// Wire defaults (с возможностью override в тестах)
	if fs.computeFeasCtx == nil {
		fs.computeFeasCtx = func(modelName string) (*cppbackend.FeasibleInfo, error) {
			b := cppbackend.GetBackend()
			if b == nil {
				return nil, errNoBackend
			}
			return b.ComputeFeasible(modelName)
		}
	}
	if fs.getProfileCtx == nil {
		fs.getProfileCtx = func(modelName string) int {
			if profileSyncer == nil {
				return 0
			}
			prof := profileSyncer.applyProfileOnLoad(modelName)
			if prof == nil {
				return 0
			}
			return prof.ContextLength
		}
	}
	if fs.logger == nil {
		fs.logger = defaultLogger{}
	}

	go fs.runLoop(ctx)
	fs.logger.Infow("feasibleSync: started",
		"interval", fs.interval,
		"warnThrottle", fs.warnThrottle,
		"balancerURL", fs.balancerURL)
}

// stop останавливает loop.
func (fs *feasibleSyncT) stop() {
	if fs == nil {
		return
	}
	select {
	case <-fs.stopCh:
		// already stopped
	default:
		close(fs.stopCh)
	}
}

// runLoop — periodic check.
func (fs *feasibleSyncT) runLoop(ctx context.Context) {
	t := time.NewTicker(fs.interval)
	defer t.Stop()

	// Initial check after 30s (даём время на load + profile sync)
	initialTimer := time.NewTimer(30 * time.Second)
	defer initialTimer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-fs.stopCh:
			return
		case <-initialTimer.C:
			fs.checkOnce(ctx)
		case <-t.C:
			fs.checkOnce(ctx)
		}
	}
}

// checkOnce — одна итерация проверки всех загруженных моделей.
func (fs *feasibleSyncT) checkOnce(ctx context.Context) {
	b := cppbackend.GetBackend()
	if b == nil {
		return
	}
	models := b.ListModels()
	for _, m := range models {
		if m.State != cppbackend.StateLoaded {
			continue
		}
		fs.checkModel(ctx, m.Name)
	}
}

// checkModel — проверка одной модели: profile vs feasible.
func (fs *feasibleSyncT) checkModel(ctx context.Context, modelName string) {
	info, err := fs.computeFeasCtx(modelName)
	if err != nil || info == nil {
		fs.logger.Debugw("feasibleSync: ComputeFeasible failed",
			"model", modelName, "error", err)
		return
	}

	profileNCtx := fs.getProfileCtx(modelName)
	if profileNCtx <= 0 {
		// No profile synced yet — не warning, ещё нечего сравнивать
		return
	}

	feasibleNCtx := feasibleAutoRecommended(info)
	if feasibleNCtx <= 0 {
		return
	}

	// Conservative threshold: profile < 90% of feasible
	threshold := int(float64(feasibleNCtx) * 0.9)
	if profileNCtx >= threshold {
		// Profile is healthy (within 10% of feasible)
		// Clear any previous warning so we can re-warn if it gets worse
		fs.mu.Lock()
		delete(fs.lastWarnings, modelName)
		fs.mu.Unlock()
		return
	}

	// Conservative! Should we warn?
	ratio := float64(feasibleNCtx) / float64(profileNCtx)
	if ratio < 1.1 {
		// Less than 10% headroom — not worth warning
		return
	}

	fs.mu.Lock()
	last, exists := fs.lastWarnings[modelName]
	now := time.Now()
	if exists && now.Sub(last.EmittedAt) < fs.warnThrottle {
		fs.mu.Unlock()
		return // throttled
	}
	fs.lastWarnings[modelName] = feasibleWarning{
		ProfileNCtx:  profileNCtx,
		FeasibleNCtx: feasibleNCtx,
		Ratio:        ratio,
		EmittedAt:    now,
	}
	fs.mu.Unlock()

	fs.logger.Warnw("conservative profile detected (Round 37: auto-adapt recommends larger n_ctx)",
		"model", modelName,
		"profile_n_ctx", profileNCtx,
		"feasible_n_ctx", feasibleNCtx,
		"headroom_ratio", ratio,
		"gguf_max", info.GGUFMax,
		"kv_cache_type", info.KVCacheType,
		"recommendation", "PUT /api/v1/cppworker/model-profiles/"+modelName+
			" with contextLength: "+itoa(feasibleNCtx),
		"or set contextLengthAuto: true for fully automatic adaptation",
	)
}

// itoa — strconv.Itoa alias для краткости (избегаем import strconv в этом файле).
func itoa(i int) string {
	// Ручная реализация для hot path (warning emission)
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// errNoBackend — sentinel для случая "backend не инициализирован"
var errNoBackend = &feasibleError{msg: "backend not initialized"}

type feasibleError struct{ msg string }

func (e *feasibleError) Error() string { return e.msg }

// defaultLogger — fallback когда logger package недоступен (тесты).
type defaultLogger struct{}

func (defaultLogger) Warnw(msg string, keysAndValues ...interface{}) {
	// no-op (тесты подменяют на свой logger через установку fs.logger)
}

func (defaultLogger) Infow(msg string, keysAndValues ...interface{}) {
	// no-op
}

func (defaultLogger) Debugw(msg string, keysAndValues ...interface{}) {
	// no-op
}
