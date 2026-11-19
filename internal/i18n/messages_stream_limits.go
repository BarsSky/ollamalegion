package i18n

func init() { Register(messagesStreamR264) }

// messagesStreamR264 — R264 (2026-10-28): не-стрим путь и наш лимит строки.
//
// Ключ нужен, чтобы «ответ молча обрезался» перестал быть невидимым: причина
// (наш лимит, а не обрыв у воркера) должна быть в логе с числами.
var messagesStreamR264 = map[string]map[Lang]string{
	"balancer.nonstream_stream_limit.line_too_long": {
		RU: "не-стрим ответ: строка SSE больше нашего лимита (our_limit) — отдан собранный фрагмент",
		EN: "non-stream response: an SSE line exceeds our limit (our_limit) — the collected fragment was returned",
	},
}
