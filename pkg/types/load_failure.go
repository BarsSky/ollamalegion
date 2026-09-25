// load_failure.go — R83 (2026-09-25): причина провала загрузки модели.
//
// Звено связки cppworker → agent → webui. cppworker отдаёт причину в
// /api/models (поле load_failure), agent забирает её в своём обычном цикле
// опроса (каждые ~10 с) и пересылает в метриках бэкенда, балансер публикует
// уведомление в EventBus ТОЛЬКО на смену причины (transition), а WebUI рисует
// его в bell-меню.
//
// Почему так, а не через поллинг балансера: сбор данных — работа агента, у
// балансера своих задач достаточно. Балансер здесь ничего не опрашивает и
// ничего не агрегирует — только одно сравнение на уже полученный push.
package types

// LoadFailureInfo — причина последнего провала загрузки.
type LoadFailureInfo struct {
	// Model — имя модели, под которым её запросили (может содержать Ollama-тег).
	Model string `json:"model"`
	// Reason — машиночитаемый код: model_not_found, config_out_of_bounds,
	// insufficient_resources, gguf_incompatible, backend_not_ready,
	// load_timeout, alias_broken, unknown (см. cppworker/load_failure_reason.go).
	Reason string `json:"reason"`
	// Error — сырой текст (llama.cpp/наши проверки). Для «подробностей» в UI.
	Error string `json:"error,omitempty"`
	// At — когда произошло (RFC3339).
	At string `json:"at,omitempty"`
	// Severity — info/warning/error/critical, подсказка для UI.
	Severity string `json:"severity,omitempty"`
	// Diagnostics — числа для разбора: запрошенный n_ctx, границы, свободная
	// память, список доступных моделей, подсказка.
	Diagnostics map[string]interface{} `json:"diagnostics,omitempty"`
}

// IsEmpty — есть ли что показывать. Нужна, чтобы не публиковать уведомление на
// пустом объекте и не считать «сменой» отсутствие данных.
func (l *LoadFailureInfo) IsEmpty() bool {
	if l == nil {
		return true
	}
	return l.Reason == "" && l.Model == "" && l.Error == ""
}

// Key — ключ дедупликации для transition-проверки в балансере: уведомление
// публикуется только когда ключ изменился. Модель входит в ключ, потому что
// провалы разных моделей — разные события.
//
// Пустой LoadFailureInfo даёт пустой ключ (а не "\x00"): иначе «нет данных от
// cppworker» выглядело бы как провал с пустой причиной и порождало уведомление
// на пустом месте (поймано тестом TestR83_LoadFailureEvent_EmptyInfoIsNotAFailure).
func (l *LoadFailureInfo) Key() string {
	if l.IsEmpty() {
		return ""
	}
	return l.Model + "\x00" + l.Reason
}
