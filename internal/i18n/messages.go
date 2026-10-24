package i18n

// messages — каталог сообщений: ключ -> {ru, en}.
//
// ПРАВИЛА КЛЮЧЕЙ.
//   - Ключ — это слаг вида "<компонент>.<область>.<смысл>" (balancer.env.log_level).
//   - Ключ НЕ меняется при правке текста: по нему считаются непереведённые места
//     (scripts/i18n-scan.py) и он попадает в docs/development/i18n-logs.md.
//   - Текст может содержать fmt-глаголы (%s/%d/%v); набор глаголов в ru и en
//     обязан совпадать — это проверяет TestCatalogVerbParity.
//
// КАК ДОБАВЛЯТЬ. Сначала ключ и оба текста здесь, потом замена литерала в коде
// на i18n.T("ключ", ...)/i18n.Logf("ключ", ...). Обратный порядок ломает сборку.
var messages = map[string]map[Lang]string{
	// === Язык логов и ошибок (R234) ===================================
	"i18n.env.override": {
		RU: "[i18n] язык логов и ошибок: %s (из %s=%s)",
		EN: "[i18n] log and error language: %s (from %s=%s)",
	},
	"i18n.locale.fallback": {
		RU: "[i18n] язык логов и ошибок: %s (по локали LANG/LC_ALL)",
		EN: "[i18n] log and error language: %s (from LANG/LC_ALL locale)",
	},
	"i18n.config.used": {
		RU: "[i18n] язык логов и ошибок: %s (из logging.language в конфиге)",
		EN: "[i18n] log and error language: %s (from logging.language in the config)",
	},
	"i18n.start.applied": {
		RU: "[i18n] язык логов и ошибок: %s (источник: %s)",
		EN: "[i18n] log and error language: %s (source: %s)",
	},
	"i18n.changed": {
		RU: "[i18n] язык логов и ошибок переключён на %s",
		EN: "[i18n] log and error language switched to %s",
	},
	"i18n.remote.applied": {
		RU: "[i18n] язык с балансера: %s",
		EN: "[i18n] language from the balancer: %s",
	},
	"i18n.remote.unavailable": {
		RU: "[i18n] балансер не отдал язык (беру локальную настройку): %v",
		EN: "[i18n] the balancer did not return a language (keeping the local setting): %v",
	},

	// === API: /api/v1/i18n ============================================
	"api.i18n.unsupported": {
		RU: "неподдерживаемый язык %q: допустимо %s",
		EN: "unsupported language %q: allowed values are %s",
	},
	"api.i18n.read_only": {
		RU: "смена языка требует прав администратора",
		EN: "changing the language requires administrator rights",
	},
	"api.i18n.saved": {
		RU: "язык логов и ошибок сохранён: %s",
		EN: "log and error language saved: %s",
	},
	"api.i18n.save_failed": {
		RU: "язык применён, но не сохранён в конфиг: %v",
		EN: "the language is applied but was not saved to the config: %v",
	},
	"api.i18n.bad_body": {
		RU: "не удалось разобрать тело запроса: %v",
		EN: "failed to parse the request body: %v",
	},

	// === Конфигурация =================================================
	"config.log_language.invalid": {
		RU: "[config] logging.language=%q не распознан, беру %s",
		EN: "[config] logging.language=%q is not recognised, using %s",
	},

	// === Балансер: запуск =============================================
	"balancer.config.fatal": {
		RU: "[CONFIG] FATAL: балансер не может запуститься: %v",
		EN: "[CONFIG] FATAL: cannot start balancer: %v",
	},
	"balancer.env.log_level": {
		RU: "[ENV] LOG_LEVEL=%s переопределяет уровень из конфига",
		EN: "[ENV] LOG_LEVEL=%s overrides the config level",
	},
	"balancer.env.log_lang": {
		RU: "[ENV] %s=%s задаёт язык логов и ошибок",
		EN: "[ENV] %s=%s sets the log and error language",
	},
}
