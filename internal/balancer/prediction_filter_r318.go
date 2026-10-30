package balancer

// prediction_filter_r318.go — R318 (2026-10-30): фильтр по прогнозу — настройка,
// а не константа.
//
// ЖИВОЙ ДЕФЕКТ. Тумблер «Prediction filtering» в панели настроек сервер принимал
// и молча не применял, а GET всегда отдавал `true`. При этом сам фильтр — не
// «сортировка», а ЖЁСТКОЕ ИСКЛЮЧЕНИЕ: узел, у которого
// `Prediction.SecondsToCritical` меньше пяти минут, выпадает из кандидатов.
// Значит, шумный прогноз или битые метрики выбивают живой узел из выбора, и
// клиент получает «нет доступных бэкендов» при здоровом стенде.
//
// ЧТО ЗДЕСЬ: одна точка правды для четырёх мест, где фильтр применяется
// (candidate.go, backend_selector.go ×3). Пока проверка была размазана по
// четырём условиям, выключить фильтр целиком было нельзя — правка в одном месте
// оставляла три.
//
// Дефолт — включён: поведение не меняется, пока оператор не выключит.

import "ollama-loadbalancer/internal/settings"

// predictionFilterEnabledR318 — фильтровать ли узлы по прогнозу ресурсов.
func predictionFilterEnabledR318() bool {
	if v, ok := settings.HotInt("LB_PREDICTION_FILTER"); ok {
		return v != 0
	}
	return true
}

// predictedCriticalR318 — прогноз говорит «узел скоро упрётся» (или фильтр выключен).
func predictedCriticalR318(secondsToCritical float64) bool {
	if !predictionFilterEnabledR318() {
		return false
	}
	return secondsToCritical > 0 && secondsToCritical < 300
}
// PredictionFilterEnabled — действующее значение фильтра (для API/панели).
//
// Экспорт нужен, чтобы GET /api/v1/cluster/config отдавал РЕАЛЬНОЕ значение, а не
// константу: до R318 там стояло `true`, и панель показывала включённый фильтр
// независимо от того, что решил оператор.
func PredictionFilterEnabled() bool { return predictionFilterEnabledR318() }