package balancer

import "testing"

// TestR443_UnknownContextIsNotARealChange — R443: если фактическое окно
// загруженной модели ещё неизвестно (снапшот пустой/устаревший), «сохранение
// контекста» в плане НЕ считается изменением и не даёт reload.
//
// Живой дефект (Cline + Qwen3-4B): AutoTune выгружал/загружал модель (~43 с)
// после каждого хода, в причине стояло «kv_cache q8_0 → q4_0», хотя KV уже
// вычеркнул гард R419 (recommend). Перезагрузку давало поле ContextSize при
// неизвестном текущем окне.
func TestR443_UnknownContextIsNotARealChange(t *testing.T) {
	plan := &AutoTuneReloadPlan{ContextSize: 32768}
	if autoTunePlanHasRealChangeR416d(plan, 0) {
		t.Fatal("неизвестное текущее окно (0) принято за изменение — будет лишний reload")
	}
	if autoTunePlanHasRealChangeR416d(plan, -1) {
		t.Fatal("отрицательное (неизвестное) окно принято за изменение")
	}
	// Известное и отличающееся — по-прежнему изменение.
	if !autoTunePlanHasRealChangeR416d(plan, 16384) {
		t.Fatal("реальное изменение окна 16384 → 32768 не считается изменением")
	}
	// Известное и совпадающее — не изменение (R416d).
	if autoTunePlanHasRealChangeR416d(plan, 32768) {
		t.Fatal("сохранение текущего окна принято за изменение")
	}
	// KV/слои/флеш/mmap по-прежнему дают reload.
	if !autoTunePlanHasRealChangeR416d(&AutoTuneReloadPlan{KVCacheType: "q8_0"}, 0) {
		t.Fatal("смена KV должна давать reload")
	}
}
