package i18n

func init() { Register(messagesDonorR304) }

// messagesDonorR304 — R304 (2026-10-30): «донор держит только память».
//
// Отдельный файл, а не дописывание в messages_cppbackend_r234.go: в проекте
// каждая ревизия живёт своим батчем, иначе параллельные правки одного файла
// конфликтуют (см. register.go).
//
// ТЕКСТЫ. Отказ загрузки читает оператор в панели и в чужом клиенте (OpenWebUI,
// Cline), поэтому message объясняет ЧТО произошло, а hint — ЧТО ДЕЛАТЬ. В hint
// намеренно нет советов «включить/выключить узел»: роль на этой машине объявляет
// балансер, и ручное вмешательство вернуло бы ровно ту путаницу, из-за которой
// появилась R304.
var messagesDonorR304 = map[string]map[Lang]string{
	"cppbackend.donor.load_refused": {
		RU: "эта машина — донор раскладки: она отдаёт свою память мастеру и не поднимает " +
			"собственных копий моделей",
		EN: "this machine is a layout donor: it gives its memory to the master and does not " +
			"raise copies of models of its own",
	},
	"cppbackend.donor.load_refused_hint": {
		RU: "загрузкой на доноре владеет мастер раскладки (балансер) — вручную ничего поднимать не нужно; " +
			"если роль назначена ошибочно, снимите её в панели бэкенда (роль donor объявляет только балансер), " +
			"а уже загруженные копии выгрузите через /api/models/unload",
		EN: "loading on a donor is owned by the layout master (the balancer) — nothing has to be raised by hand; " +
			"if the role was assigned by mistake, clear it in the backend panel (only the balancer declares the donor role), " +
			"and unload the copies that are already loaded via /api/models/unload",
	},
	"cppworker.donor.load_refused_log": {
		RU: "донор: подъём собственной копии модели отклонён",
		EN: "donor: raising an own model copy was refused",
	},
	"cppworker.donor.autoload_skipped": {
		RU: "донор: авто-загрузка моделей при старте пропущена — своих копий донор не держит",
		EN: "donor: the startup auto-load of models was skipped — a donor holds no copies of its own",
	},
	"cppworker.donor.reload_defaults_skipped": {
		RU: "донор: reload загруженных моделей под новые дефолты пропущен — своих копий донор не держит",
		EN: "donor: the reload of loaded models with the new defaults was skipped — a donor holds no copies of its own",
	},
	"cppworker.donor.role_applied": {
		RU: "роль бэкенда применена: объявлена балансером, действует на ходу и переживает пересоздание контейнера",
		EN: "the backend role was applied: declared by the balancer, it acts at runtime and survives container recreation",
	},
	"cppworker.donor.role_override_unparsed": {
		RU: "переопределение роли бэкенда в файле не разобрано — оставляю роль из конфигурации",
		EN: "the backend role override in the file was not parsed — keeping the role from the configuration",
	},
	"cppworker.donor.role_unknown_value": {
		RU: "неизвестная роль бэкенда в файле переопределений — трактуется как «роль не объявлена»",
		EN: "an unknown backend role in the overrides file — treated as \"role not declared\"",
	},
}
