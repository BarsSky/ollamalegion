package sdbackend

import (
	"context"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Idle-unload субпроцесса sd-server
// ============================================================
//
// ПОЧЕМУ ЭТО НУЖНО ОСОБЕННО СИЛЬНО ЗДЕСЬ: в отличие от llama.cpp (где idle
// освобождает KV/веса, но процесс остаётся), «выгрузка» image-модели = kill
// субпроцесса. Держать sd-server с 4–12 GB весов «на всякий случай» на карте,
// где параллельно работает текстовая модель, — прямой путь к OOM.
//
// Правила (по образцу internal/cppbackend/model_manager.go:883-987):
//   - idle считаем от LastUsedAt (не от LoadedAt!): иначе модель выгружалась бы
//     через N минут ПОСЛЕ ЗАГРУЗКИ, даже обслуживая запросы каждую секунду;
//   - активные запросы (inFlight > 0) блокируют выгрузку — kill во время
//     генерации оставит клиента без ответа, а VRAM не освободит;
//   - IdleUnloadMinutes == 0 → менеджер вообще не стартует.

// IdleUnloadManager периодически гасит простаивающий sd-server.
type IdleUnloadManager struct {
	sup      *Supervisor
	registry *Registry
	metrics  *Metrics
	cfg      *Config

	interval time.Duration
	stopCh   chan struct{}
	stopped  bool
}

// NewIdleUnloadManager создаёт менеджер. interval <= 0 → интервал вычисляется
// как min(idle/2, 5m), но не чаще раза в 10 секунд.
func NewIdleUnloadManager(cfg *Config, registry *Registry, sup *Supervisor, metrics *Metrics) *IdleUnloadManager {
	return &IdleUnloadManager{
		sup:      sup,
		registry: registry,
		metrics:  metrics,
		cfg:      cfg,
		stopCh:   make(chan struct{}),
	}
}

// Start запускает фоновый мониторинг (no-op, если idle-выгрузка выключена).
func (m *IdleUnloadManager) Start() {
	timeout := m.idleTimeout()
	if timeout <= 0 {
		sdLog().Infow("idle unload disabled (idleUnloadMinutes=0) — sd-server будет держать модель в VRAM до явного unload")
		return
	}
	m.interval = timeout / 2
	if m.interval > 5*time.Minute {
		m.interval = 5 * time.Minute
	}
	if m.interval < 10*time.Second {
		m.interval = 10 * time.Second
	}
	sdLog().Infow("idle unload enabled", "timeout", timeout.String(), "check_interval", m.interval.String())
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.checkAndUnload()
			case <-m.stopCh:
				return
			}
		}
	}()
}

// Stop останавливает мониторинг (идемпотентно).
func (m *IdleUnloadManager) Stop() {
	if m.stopped {
		return
	}
	m.stopped = true
	close(m.stopCh)
}

// idleTimeout — эффективный таймаут простоя.
//
// Приоритет профиля над конфигом: у тяжёлой FLUX-модели разумно держать
// модель дольше, чем у SD-Turbo, и это описывается в её профиле.
func (m *IdleUnloadManager) idleTimeout() time.Duration {
	cur := m.sup.CurrentModel()
	if cur != "" {
		if p, ok := m.registry.Profile(cur); ok && p.IdleUnloadMinutes > 0 {
			return time.Duration(p.IdleUnloadMinutes) * time.Minute
		}
	}
	if m.cfg != nil && m.cfg.IdleUnloadMinutes > 0 {
		return time.Duration(m.cfg.IdleUnloadMinutes) * time.Minute
	}
	return 0
}

// checkAndUnload — один проход проверки.
func (m *IdleUnloadManager) checkAndUnload() {
	if m.sup.State() != StateLoaded {
		return
	}
	// Активные запросы: не выгружаем.
	if m.sup.InFlight() > 0 {
		return
	}
	timeout := m.idleTimeout()
	if timeout <= 0 {
		return
	}
	reference := m.metrics.LastUsed()
	if reference.IsZero() {
		// Модель только что загружена и ещё не использовалась — считаем от
		// времени загрузки. Zero-время НЕ используем: now.Sub(zero) ≈ 2000 лет,
		// и модель выгружалась бы сразу после spawn (та же ловушка, что чинили
		// в cppworker Round 35).
		reference = m.metrics.LoadedAt()
	}
	if reference.IsZero() {
		return
	}
	idle := time.Since(reference)
	if idle <= timeout {
		return
	}
	model := m.sup.CurrentModel()
	sdLog().Infow("idle unload: killing sd-server",
		"model", model, "idle", idle.String(), "timeout", timeout.String(),
		"reference", reference.UTC().Format(time.RFC3339))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := m.sup.Unload(ctx); err != nil {
		sdLog().Warnw("idle unload failed", "model", model, "error", err)
	}
}

// EffectiveIdleMinutes — для /api/image/capabilities (какой таймаут действует).
func (m *IdleUnloadManager) EffectiveIdleMinutes() int {
	d := m.idleTimeout()
	if d <= 0 {
		return 0
	}
	return int(d.Minutes())
}

// DefaultProfileIdle — idle-минуты из профиля или конфига (для UI).
func DefaultProfileIdle(p *types.ImageModelProfile, cfg *Config) int {
	if p != nil && p.IdleUnloadMinutes > 0 {
		return p.IdleUnloadMinutes
	}
	if cfg != nil {
		return cfg.IdleUnloadMinutes
	}
	return 0
}
