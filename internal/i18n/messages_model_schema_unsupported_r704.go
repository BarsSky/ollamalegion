package i18n

func init() { Register(messagesModelSchemaUnsupportedR704) }

// messagesModelSchemaUnsupportedR704 — R704 (2026-11-24): отказ воркера «схема
// файла не поддержана текущим загрузчиком llama.cpp».
//
// ЖИВОЙ ФАКТ (стенд, cppworker gpu-r703, пин llama.cpp b12642, файл
// Inkling-2-Mini-UD-Q4_K_M.gguf): архитектуру inkling2 сборка уже знает, поэтому
// движок идёт дальше и падает на тензорах:
//
//	llama_model_load: error loading model: check_tensor_dims:
//	    tensor 'blk.24.attn_q.weight' not found
//
// До R704 этот текст классифицировался как model_not_found (в нём есть «not
// found»), то есть машинный код врал: файл на месте, архитектура распознана, а
// оператору предлагалось искать модель. Эти строки называют настоящую причину
// (loop-развёрнутая схема: 24 физических блока при block_count=48, ключ
// num_loops) и что с ней делать (ре-экспорт в <arch>_loop с block_count=24 или
// более новый загрузчик).
//
// Тексты живут в каталоге, а не в хендлере: их видят и ответ POST
// /api/models/load, и реестр провалов (/api/models), и подсказка в уведомлении
// WebUI.
var messagesModelSchemaUnsupportedR704 = map[string]map[Lang]string{
	// === cmd/cppworker/model_schema_unsupported.go ========================
	// %s — фраза о схеме (loop-развёрнутая или общая), %s — сборка llama.cpp,
	// %s — имя тензора, %s — архитектура.
	"cppworker.load_failure.model_schema_unsupported": {
		RU: "файл модели не поддержан загрузчиком llama.cpp (%s): это %s, движок не нашёл тензора %s при распознанной архитектуре %s — файл на месте, ищите не файл, а схему",
		EN: "the model file is not supported by this llama.cpp loader (%s): it is %s, the engine did not find tensor %s while architecture %s is recognised — the file is present, so look at the schema, not at the file",
	},
	"cppworker.load_failure.model_schema_unsupported_loop": {
		RU: "loop-развёрнутая схема (физических блоков: %d при block_count=%d, ключ num_loops)",
		EN: "a loop-expanded schema (%d physical blocks while block_count=%d, the num_loops key)",
	},
	"cppworker.load_failure.model_schema_unsupported_generic": {
		RU: "схема тензоров, не совпадающая с ожиданиями движка",
		EN: "a tensor schema that does not match what the engine expects",
	},
	"cppworker.load_failure.tensor_unknown": {
		RU: "(имя тензора движок не напечатал)",
		EN: "(the engine did not print the tensor name)",
	},
	"cppworker.load_failure.arch_unknown_header": {
		RU: "(архитектура не прочитана из заголовка файла)",
		EN: "(the architecture could not be read from the file header)",
	},
	// %s — архитектура, %d — число физических блоков.
	"cppworker.load_failure.model_schema_unsupported_hint_loop": {
		RU: "нужен ре-экспорт GGUF как %s_loop с block_count=%d или более новый загрузчик llama.cpp: текущий читает block_count из заголовка и ищет блоки, которых в файле нет. Повтор загрузки не поможет",
		EN: "re-export the GGUF as %s_loop with block_count=%d, or use a newer llama.cpp loader: the current one reads block_count from the header and looks for blocks the file does not contain. Retrying the load will not help",
	},
	// %s — сборка llama.cpp.
	"cppworker.load_failure.model_schema_unsupported_hint": {
		RU: "нужен ре-экспорт GGUF под схему тензоров, которую понимает загрузчик llama.cpp (%s), или более новый загрузчик. Повтор загрузки не поможет",
		EN: "re-export the GGUF with a tensor schema this llama.cpp loader (%s) understands, or use a newer loader. Retrying the load will not help",
	},
}
