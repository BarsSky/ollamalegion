package i18n

func init() { Register(messagesFinalR234) }

// messagesFinalR234 — последние два сообщения, которые не поймал сканер: лог с
// «нестандартным» получателем (`appLog()`, `ridLog(...)`). После их выноса
// правило сканера расширено цепочкой `).Warnw(` — чтобы такие места больше не
// терялись, а новые не появлялись.
var messagesFinalR234 = map[string]map[Lang]string{
	"sdworker.handlers_sdapi.model_not_switched": {
		RU: "sdapi/options: модель не переключена (hot-swap в sd-server отсутствует)",
		EN: "sdapi/options: the model was not switched (sd-server has no hot-swap)",
	},
	"sdworker.handlers_sdapi.model_switch_hint": {
		RU: "используйте POST /api/image/models/load для смены модели (kill+spawn)",
		EN: "use POST /api/image/models/load to switch the model (kill+spawn)",
	},
	"balancer.autoload_wait.progress_deadline_extended": {
		RU: "autoload_wait: загрузка прогрессирует, дедлайн продлён",
		EN: "autoload_wait: loading is progressing, the deadline was extended",
	},
}
