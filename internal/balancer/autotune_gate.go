// autotune_gate.go — R636 (2026-11-19): гейт автотюна и план перезагрузки: IsAutoTuneEnabled, AutoTuneReloadPlan, NeedsReload.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import (
	"fmt"
	"time"
)

// AutoTuneReloadPlan — Round 54.4 (2026-08-24): конкретный план reload'a для
// применения AutoTune рекомендаций. Это то, что передаётся в executeLlamaCppLoad.
//
// Если рекомендация не требует изменения (current = optimal), поля nil/0 →
// reload не выполняется.
//
// КРИТИЧНО: пустой план (все поля = zero value) означает "ничего не делать".
// PlanApplyAutoTune ниже возвращает (nil, false) если план пуст.
type AutoTuneReloadPlan struct {
	ModelName    string
	ContextSize  int    // 0 = no change
	KVCacheType  string // "" = no change
	NumGPULayers int    // 0 = no change (negative values = -1 all, -2 auto)
	FlashAttn    int    // 0 = no change
	UseMmap      *bool  // nil = no change
	Reason       string // for logging (e.g., "R54.4: n_ctx over-allocation")

	// R230: маркер «reload идёт», уже взятый вызывающим. Async trigger берёт его
	// СИНХРОННО (до запуска горутины и до форварда текущего запроса), чтобы
	// параллельные запросы не успели перехватить reload ленивой загрузкой по
	// профилю. Не сериализуется (unexported).
	externalFinish func()
}

// NeedsReload — true если хотя бы одно поле отличается от current (т.е. есть что менять).
func (p *AutoTuneReloadPlan) NeedsReload() bool {
	return p.ContextSize > 0 || p.KVCacheType != "" || p.NumGPULayers != 0 ||
		p.FlashAttn != 0 || p.UseMmap != nil
}

// AutoTuneCircuitBreaker — Round 54.4 (2026-08-24): защита от reload storm.
//
// Проблема: если AutoTune детектит sub-optimal state, а reload не удаётся
// (cppworker busy / timeout / wrong params), без breaker'а балансер будет
// пытаться reload каждым запросом — нагрузка на cppworker, логи захлёбываются.
//
// Решение: track last_attempt + last_success per (backend, model).
// Пропускаем reload если:
//   - Last attempt < 30s ago AND last attempt failed (cool-down)
//   - Last success < 60s ago (don't re-fix what was just fixed)
//
// Хранится в-memory (per-Proxy). Не персистится — после restart balancer'a
// AutoTune попробует заново, что и хочется.
type AutoTuneCircuit struct {
	LastAttempt time.Time
	LastSuccess time.Time
	LastError   string
}

// AutoTuneCircuitConfig — Round 54.4 (2026-08-24): tunable breakers.
type AutoTuneCircuitConfig struct {
	// CooldownAfterErrorSec: после неуспешного reload не пытаемся N секунд.
	CooldownAfterErrorSec int
	// CooldownAfterSuccessSec: после успешного reload не пытаемся N секунд.
	CooldownAfterSuccessSec int
	// MaxRetries: сколько reloads в одном "burst" (circuit opens after).
	MaxRetries int
}

// DefaultAutoTuneCircuitConfig — sane defaults для 4B-class моделей на 8GB VRAM.
func DefaultAutoTuneCircuitConfig() AutoTuneCircuitConfig {
	return AutoTuneCircuitConfig{
		CooldownAfterErrorSec:   60,  // 1 min cool-down после fail
		CooldownAfterSuccessSec: 300, // 5 min "stable period" после success
		MaxRetries:              3,   // 3 fail → open circuit
	}
}

// CanReload — Round 54.4 (2026-08-24): возвращает true если circuit closed
// и можно делать reload. Возвращает (false, reason) если open.
func (c *AutoTuneCircuit) CanReload(cfg AutoTuneCircuitConfig) (bool, string) {
	now := time.Now()
	if !c.LastAttempt.IsZero() && c.LastError != "" {
		coolDownEnd := c.LastAttempt.Add(time.Duration(cfg.CooldownAfterErrorSec) * time.Second)
		if now.Before(coolDownEnd) {
			return false, fmt.Sprintf("circuit-cooling-down: %.0fs left after error: %s",
				coolDownEnd.Sub(now).Seconds(), c.LastError)
		}
	}
	if !c.LastSuccess.IsZero() {
		stableEnd := c.LastSuccess.Add(time.Duration(cfg.CooldownAfterSuccessSec) * time.Second)
		if now.Before(stableEnd) {
			return false, fmt.Sprintf("circuit-stable: %.0fs left after success",
				stableEnd.Sub(now).Seconds())
		}
	}
	return true, ""
}

// RecordAttempt — фиксирует попытку reload. Call this BEFORE the actual reload.
func (c *AutoTuneCircuit) RecordAttempt() {
	c.LastAttempt = time.Now()
}

// RecordSuccess — фиксирует успешный reload. Reset error.
func (c *AutoTuneCircuit) RecordSuccess() {
	c.LastSuccess = time.Now()
	c.LastError = ""
}

// RecordError — фиксирует failed reload.
func (c *AutoTuneCircuit) RecordError(err string) {
	c.LastError = err
}
