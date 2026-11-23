package i18n

func init() { Register(messagesModelUnsupportedR697) }

// messagesModelUnsupportedR697 — R697 (2026-11-24): отказ воркера «сборка llama.cpp
// не знает архитектуру модели».
//
// Живой факт (стенд, cppworker gpu-r660): файл Inkling-2-Mini-UD-Q4_K_M.gguf лежит в
// /app/models, general.architecture="inkling2", а POST /api/models/load отвечал 500
// «load failed: load model failed: failed to load model from <путь>» — без причины.
// llama.cpp при этом печатал unknown model architecture: 'inkling2'.
//
// Эти строки — то, что видит оператор вместо пути к файлу: что именно не поддерживается
// (архитектура), какой сборкой, и что с этим делать (обновить сабмодуль + пересобрать
// образ). Тексты одинаковы для /api/models/load, для реестра провалов и для подсказки
// в уведомлении WebUI, поэтому живут в каталоге, а не в хендлере.
var messagesModelUnsupportedR697 = map[string]map[Lang]string{
	// === cmd/cppworker/model_unsupported.go ================================
	"cppworker.load_failure.model_unsupported": {
		RU: "архитектура %s не поддерживается сборкой llama.cpp %s: файл новее движка",
		EN: "architecture %s is not supported by this llama.cpp build %s: the file is newer than the engine",
	},
	"cppworker.load_failure.model_unsupported_hint": {
		RU: "обновите сабмодуль c/llama.cpp до коммита, где эта архитектура есть, и пересоберите образ воркера",
		EN: "update the c/llama.cpp submodule to a commit that knows this architecture and rebuild the worker image",
	},
	"cppworker.load_failure.model_unsupported_since": {
		RU: "Upstream llama.cpp умеет её с версии %s.",
		EN: "Upstream llama.cpp supports it since %s.",
	},
	"cppworker.load_failure.arch_unknown": {
		RU: "(имя архитектуры не названо движком)",
		EN: "(the engine did not name the architecture)",
	},
	"cppworker.load_failure.build_unknown": {
		RU: "неизвестной сборки",
		EN: "an unknown build",
	},
}
