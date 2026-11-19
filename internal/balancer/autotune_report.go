// autotune_report.go — R636 (2026-11-19): отчёт: severityRank, countSubOptimal, FormatRecommendationShort.
//
// ЗАЧЕМ ВЫНЕСЕНО. `autotune.go` держал 1666 строк: типы профиля, правила
// выбора KV-типа и числа слоёв, отчёт, предохранитель, трекер, две перезагрузки
// и планирование применения. Здесь — один из слоёв; поведение не менялось.
package balancer

import "fmt"

// severityRank — упорядочивает severity для "worst" вычисления.
func severityRank(s string) int {
	switch s {
	case "critical":
		return 4
	case "warning":
		return 3
	case "info":
		return 2
	case "ok":
		return 1
	default:
		return 0
	}
}

func countSubOptimal(models []AutoTuneAnalysis) int {
	n := 0
	for _, m := range models {
		if m.IsSubOptimal {
			n++
		}
	}
	return n
}

// FormatRecommendationShort — краткая однострочная рекомендация для логов.
func FormatRecommendationShort(r AutoTuneRecommendation) string {
	return fmt.Sprintf("[%s/%s] %s", r.Severity, r.Category, r.Message)
}

// IsAutoTuneEnabled — Round 54.2 (2026-08-24): resolves AutoTune master switch
// for a specific model. Returns false if global AutoTune is off OR per-model
// profile has AutoTune=false (explicit manual override).
//
// Resolution order (highest priority first):
//  1. Per-model profile.AutoTune (if non-nil) — explicit override
//  2. BalancingSettings.AutoTune (default true)
//
// Использование:
//
//	p.config.Balancing.AutoTune — global switch (default true)
//	profile.AutoTune = &false — manual override для этой модели
func IsAutoTuneEnabled(p *Proxy, modelName string) bool {
	if p == nil {
		return false
	}
	// 0) R83 (2026-09-29): primary-модель — настройки зафиксированы администратором,
	// авто-оптимизация её не трогает. Иначе AutoTune перезагрузил бы модель по своим
	// рекомендациям и «сдвинул» ровно те параметры, которые оператор зафиксировал.
	//
	// R401 (2026-11-05): ветка больше не срабатывает — isPrimaryModel всегда false,
	// флаг primary отключён (см. primary_model_policy.go). Оставлена, чтобы при
	// возврате политики «зафиксировать модель» правка была в одном месте; сейчас
	// AutoTune решается только профилем и глобальным свитчем.
	if modelName != "" && p.isPrimaryModel(modelName) {
		return false
	}
	// 1) Per-model profile override (highest priority)
	if modelName != "" {
		profile, ok := p.GetModelProfile(modelName)
		if ok && profile.AutoTune != nil {
			return *profile.AutoTune
		}
	}
	// 2) Global default (defensive nil check)
	if p.config == nil {
		return true // default behavior
	}
	return p.config.Balancing.AutoTune
}
