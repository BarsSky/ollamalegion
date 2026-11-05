// autotune_apply_mode_r444.go — R444 (2026-11-05): AutoTune по умолчанию только
// РЕКОМЕНДУЕТ и не перезагружает модель.
//
// ПОЧЕМУ. Живой прогон (Cline + Qwen3-4B, стенд 8 ГБ). Логи балансера подряд:
//
//	autotune: kv-cache change suppressed (R416)                       ← KV-смена отменена
//	autotune: triggering async reload … kv_cache q8_0 → q4_0          ← reload всё равно!
//	autotune: unloaded model for AutoTune apply
//	autotune: plan applied (unload+load)                              ← 43–67 секунд
//
// Каждый ход агентского цикла платил 43–67 с за unload+load, потому что анализ
// строил «оптимальный» план по СИЮМИНУТНОЙ свободной VRAM: освободилось место →
// «поднять слои»; занято другой моделью → «опустить». Для агентского клиента это
// выглядит как «модель думает по 2–3 минуты», хотя генерация уже закончилась.
//
// ПОЛИТИКА (env LB_AUTOTUNE_APPLY):
//
//	recommend — ПО УМОЛЧАНИЮ: анализ и рекомендации работают как раньше (панель
//	            показывает «оптимально столько-то слоёв/KV/окно»), но reload
//	            НЕ запускается. Применить можно вручную — карточкой модели в
//	            WebUI / POST /api/v1/admin/autotune/{id}/apply.
//	auto      — прежнее поведение: AutoTune применяет план сам (с гардами R416,
//	            R416b/R416c/R416d и circuit breaker'ом).
//
// Отличие от LB_AUTOTUNE_KV_MODE (R419): тот ограничивает только смену KV-cache,
// а здесь речь обо ВСЁМ плане (слои, KV, окно). Обе политики независимы; в
// режиме recommend reload не запускается вообще.
package balancer

import (
	"os"
	"strings"
)

const (
	// AutoTuneApplyModeRecommend — только рекомендации (по умолчанию).
	AutoTuneApplyModeRecommend = "recommend"
	// AutoTuneApplyModeAuto — AutoTune применяет план сам.
	AutoTuneApplyModeAuto = "auto"
)

// autoTuneApplyModeR444 — текущая политика применения плана AutoTune.
func autoTuneApplyModeR444() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTOTUNE_APPLY"))) {
	case AutoTuneApplyModeAuto:
		return AutoTuneApplyModeAuto
	}
	return AutoTuneApplyModeRecommend
}

// autoTuneApplyAutomaticR444 — разрешено ли AutoTune применять план сам.
func autoTuneApplyAutomaticR444() bool {
	return autoTuneApplyModeR444() == AutoTuneApplyModeAuto
}
