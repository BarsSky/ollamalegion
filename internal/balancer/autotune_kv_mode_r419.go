// autotune_kv_mode_r419.go — R419 (2026-11-03): AutoTune больше НЕ меняет
// KV-cache автоматически по свободной VRAM.
//
// ПОЧЕМУ. Анализ AutoTune строит «оптимальный» тип KV-cache из СВОБОДНОЙ VRAM в
// момент замера. Это величина, которая меняется сама собой после каждой
// перезагрузки, поэтому рекомендация становится обратной сама себе:
//
//	загружено q8_0 → свободной VRAM меньше → «оптимально q4_0»
//	загружено q4_0 → свободной VRAM больше → «оптимально q8_0»
//
// R416/R416b/R416c/R416d остановили БЕСКОНЕЧНУЮ петлю (профиль, гистерезис 30
// минут, слои, окно). Но остался более общий вопрос: смена KV-cache — это
// выбор КАЧЕСТВА, а не только места в памяти, и AutoTune не должен решать его
// за оператора, тем более по кратковременному значению свободной VRAM.
//
// ПОЛИТИКА (env LB_AUTOTUNE_KV_MODE):
//
//	auto      — прежнее поведение: AutoTune может применить смену KV сам
//	            (с профилем и гистерезисом R416). Осознанный выбор оператора.
//	recommend — ПО УМОЛЧАНИЮ: AutoTune показывает рекомендацию в панели, но НЕ
//	            делает reload ради KV. Применить можно вручную (кнопка
//	            «применить план» / POST /api/v1/admin/autotune/{id}/apply).
//
// Любое иное/пустое значение трактуется как recommend: «не описано — значит не
// трогаем качество модели».
package balancer

import (
	"os"
	"strings"
)

const (
	// AutoTuneKVModeAuto — AutoTune сам применяет смену KV-cache.
	AutoTuneKVModeAuto = "auto"
	// AutoTuneKVModeRecommend — только рекомендация, без автоматического reload.
	AutoTuneKVModeRecommend = "recommend"
)

// autoTuneKVModeR419 — текущая политика смены KV-cache (env LB_AUTOTUNE_KV_MODE).
func autoTuneKVModeR419() string {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LB_AUTOTUNE_KV_MODE")))
	if v == AutoTuneKVModeAuto {
		return AutoTuneKVModeAuto
	}
	return AutoTuneKVModeRecommend
}

// autoTuneKVAutomaticR419 — разрешена ли автоматическая смена KV-cache.
func autoTuneKVAutomaticR419() bool {
	return autoTuneKVModeR419() == AutoTuneKVModeAuto
}
