// growth_numbers_ctx_r447.go — R447 (2026-11-06): числа «сколько нужно / сколько
// загружено» в теле отказа обязаны совпадать с числами в его тексте.
//
// ЖИВОЙ ДЕФЕКТ. Тело отказа собиралось ДВАЖДЫ: сначала вердикт (R301/R385)
// считал числа и строил текст («the loaded window is 2048 … retry in 60
// seconds»), затем writer (`writePreflightDeferredR299`) пересчитывал их заново
// из метрик. Между этими двумя моментами балансер успевал запустить
// перезагрузку — и метрики уже показывали ЦЕЛЕВОЕ окно. Клиент получал тело,
// где `loadedWindowTokens` = 32768, а в тексте «the loaded window is 2048»; и
// `retryAfterSec` = 30 при тексте «retry in 60 seconds». Один ответ описывал
// два разных состояния мира.
//
// Правило простое: ответ описывает состояние НА МОМЕНТ РЕШЕНИЯ. Поэтому числа
// снимаются один раз (в вердикте) и передаются writer'у готовым снимком, а не
// пересчитываются по изменчивому состоянию.
package balancer

import "context"

// growthNumbersR447 — снимок чисел роста окна на момент вердикта.
type growthNumbersR447 struct {
	// Set — снимок заполнен вердиктом (нулевые числа тоже бывают значимыми).
	Set           bool
	Estimated     int
	Loaded        int
	Needed        int
	Active        int
	ReloadPending bool
	// RetryAfterSec — тот же срок, который назван в тексте причины (0 — не
	// назван, тогда writer берёт значение вызывающего).
	RetryAfterSec int
}

// growthNumbersKeyR447 — ключ снимка в контексте запроса.
type growthNumbersKeyR447 struct{}

// fillFrom — заполнить снимок числами вердикта.
func (s *growthNumbersR447) fillFrom(v growthVerdictR301) {
	if s == nil {
		return
	}
	s.Set = true
	s.Estimated = v.Estimated
	s.Loaded = v.Loaded
	s.Needed = v.Needed
	s.Active = v.Active
	s.ReloadPending = v.ReloadPending
	s.RetryAfterSec = v.RetryAfterSec
}

// growthNumbersSlotR447 — завести в контексте запроса место под снимок.
//
// Контекст выбран намеренно: вердикт считается в одном слое (preflight), а тело
// отказа пишется в другом (транспорт). Сигнатуры этих функций вызываются из
// десятков мест (включая тесты), поэтому «ещё одно возвращаемое значение» —
// цена выше пользы. Слот создаёт вызывающий и передаёт свой же ctx вниз;
// preflight заполняет слот, writer читает. Время жизни — один HTTP-запрос,
// пишет и читает один goroutine.
func growthNumbersSlotR447(ctx context.Context) (context.Context, *growthNumbersR447) {
	if ctx == nil {
		return nil, &growthNumbersR447{}
	}
	slot := &growthNumbersR447{}
	return context.WithValue(ctx, growthNumbersKeyR447{}, slot), slot
}

// withGrowthNumbersR447 — положить готовый снимок вердикта в контекст.
//
// Нужен там, где вердикт и запись тела стоят в одном месте (ServeHTTP на
// :18080): слот не нужен, числа уже есть.
func withGrowthNumbersR447(ctx context.Context, v growthVerdictR301) context.Context {
	if ctx == nil {
		return nil
	}
	slot := &growthNumbersR447{}
	slot.fillFrom(v)
	return context.WithValue(ctx, growthNumbersKeyR447{}, slot)
}

// recordGrowthNumbersR447 — записать снимок в слот контекста (если он есть).
func recordGrowthNumbersR447(ctx context.Context, v growthVerdictR301) {
	if slot, ok := ctx.Value(growthNumbersKeyR447{}).(*growthNumbersR447); ok {
		slot.fillFrom(v)
	}
}

// growthNumbersFromCtxR447 — снимок вердикта из контекста (ok=false — его нет
// или он не заполнен, тогда writer считает числа прежним путём).
func growthNumbersFromCtxR447(ctx context.Context) (growthNumbersR447, bool) {
	if ctx == nil {
		return growthNumbersR447{}, false
	}
	slot, ok := ctx.Value(growthNumbersKeyR447{}).(*growthNumbersR447)
	if !ok || slot == nil || !slot.Set {
		return growthNumbersR447{}, false
	}
	return *slot, true
}
