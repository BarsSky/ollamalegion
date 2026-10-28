package i18n

func init() { Register(messagesBackendDisableR250) }

// messagesBackendDisableR250 — R250 (2026-10-28): флаг «не использовать бэкенд».
//
// Отдельный файл, а не дописывание в messages_api_r234.go/balancer_r234.go: в
// проекте каждая область/ревизия живёт своим батчем, иначе параллельные правки
// одного файла конфликтуют (см. register.go).
var messagesBackendDisableR250 = map[string]map[Lang]string{
	"balancer.backend_disable.changed": {
		RU: "решение оператора по бэкенду сохранено: «не использовать» изменено",
		EN: "the operator decision for the backend was saved: \"do not use\" changed",
	},
	"balancer.backend_disable.all_disabled": {
		RU: "все подходящие бэкенды исключены оператором («не использовать») — запрос некуда направить",
		EN: "every matching backend is excluded by the operator (\"do not use\") — no target for the request",
	},
	"api.handlers_backends.enabled_changed": {
		RU: "тумблер «не использовать бэкенд»: состояние изменено",
		EN: "the \"do not use backend\" toggle: state changed",
	},
	"api.handlers_backends.persist_disabled_failed": {
		RU: "не удалось сохранить флаг «не использовать бэкенд» в persistent-override",
		EN: "failed to persist the \"do not use backend\" flag into the persistent override",
	},
	"api.handlers_backends.restore_disabled_failed": {
		RU: "не удалось прочитать сохранённые флаги «не использовать бэкенд»",
		EN: "failed to read the saved \"do not use backend\" flags",
	},
	"api.handlers_backends.restore_disabled_done": {
		RU: "флаги «не использовать бэкенд» восстановлены с диска",
		EN: "the \"do not use backend\" flags were restored from disk",
	},
	"api.handlers_backends.reapply_disabled_returned": {
		RU: "флаг «не использовать бэкенд» возвращён в запись (запись пересобрана без него)",
		EN: "the \"do not use backend\" flag was returned into the record (it was rebuilt without it)",
	},
}
