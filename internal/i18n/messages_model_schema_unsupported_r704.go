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
// R712 (2026-11-25): у причины есть ВТОРАЯ форма — файл УЖЕ переведён в
// loop-схему (general.architecture=inkling2_loop или канонические ключи
// loop.start/end/count), но в нём нет обязательного
// blk.<core>.loop_msg_query.weight. Там block_count из заголовка — это и есть
// число физических блоков, а действие — фолд запросов скриптом R711
// (scripts/gguf_loop_meta_patch.py --fuse-loop-queries) или полный ре-экспорт
// штатным конвертером llama.cpp. См. ключи *_loop_schema*.
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
	// %s — архитектура, %d — число физических блоков, %d — block_count из
	// заголовка. R712: числа атрибутированы источнику — иначе оператор не знает,
	// какому из двух чисел верить (у до-патчевого файла 24 — из имени
	// отсутствующего тензора, 48 — визиты из заголовка).
	"cppworker.load_failure.model_schema_unsupported_loop": {
		RU: "loop-развёрнутая схема (физических блоков: %d — по имени отсутствующего тензора; block_count из заголовка: %d, ключ num_loops)",
		EN: "a loop-expanded schema (%d physical blocks — taken from the name of the missing tensor; the header's block_count is %d, the num_loops key)",
	},
	// R712 (2026-11-25): файл УЖЕ в loop-схеме (inkling2_loop), но в нём нет
	// обязательного тензора конвертации. Здесь block_count из заголовка — это и
	// есть число физических блоков, поэтому «физических блоков» и «block_count»
	// совпадают; %d — одно и то же число.
	"cppworker.load_failure.model_schema_unsupported_loop_schema": {
		RU: "loop-схема без обязательного тензора конвертации (физических блоков: %d — block_count из заголовка)",
		EN: "a loop schema that lacks the mandatory conversion tensor (%d physical blocks — the header's block_count)",
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
	// R712: файл уже в loop-схеме, отсутствует обязательный blk.<core>.
	// loop_msg_query.weight — это ГРАНИЦА конвертации: метаданные переписаны, а
	// тензор запроса не свёрнут. %s — архитектура, %d — физические блоки из
	// заголовка, %s — имя отсутствующего тензора (факт движка).
	"cppworker.load_failure.model_schema_unsupported_hint_loop_schema": {
		RU: "файл уже в loop-схеме (%s, block_count=%d — физические блоки из заголовка), но отсутствует обязательный тензор %s: это конвертация loop-схемы — примените scripts/gguf_loop_meta_patch.py --fuse-loop-queries (см. docs/features/inkling2-loop.md) или пере-экспортируйте штатным конвертером llama.cpp, который пишет этот тензор сам. Повтор загрузки не поможет",
		EN: "the file is already in the loop schema (%s, block_count=%d — the physical blocks from the header), but the mandatory tensor %s is missing: this is the loop-schema conversion — run scripts/gguf_loop_meta_patch.py --fuse-loop-queries (see docs/features/inkling2-loop.md) or re-export with the stock llama.cpp converter, which writes this tensor itself. Retrying the load will not help",
	},
	// R712: файл уже в loop-схеме, но пропал ДРУГОЙ тензор — скрипт здесь не
	// поможет (он восстанавливает только loop_msg_query), нужен полный ре-экспорт.
	// %s — архитектура, %d — block_count из заголовка, %s — имя тензора.
	"cppworker.load_failure.model_schema_unsupported_hint_loop_schema_other": {
		RU: "файл уже в loop-схеме (%s, block_count=%d), но в нём нет тензора %s: scripts/gguf_loop_meta_patch.py --fuse-loop-queries восстанавливает только blk.<core>.loop_msg_query.weight, поэтому здесь нужен полный ре-экспорт штатным конвертером llama.cpp. Повтор загрузки не поможет",
		EN: "the file is already in the loop schema (%s, block_count=%d) but tensor %s is missing: scripts/gguf_loop_meta_patch.py --fuse-loop-queries restores only blk.<core>.loop_msg_query.weight, so this one needs a full re-export with the stock llama.cpp converter. Retrying the load will not help",
	},
	// R712: до-патчевый файл (arch inkling2). Поведение прежнее — ре-экспорт в
	// <arch>_loop, — но оба числа названы с источником, и в подсказке ДВА пути:
	// штатный конвертер llama.cpp и скрипт фолда (R711). %s — архитектура,
	// %d — физические блоки (из имени тензора), %d — block_count из заголовка
	// (визиты), %s — архитектура для имени <arch>_loop, %d — block_count для
	// ре-экспорта (= физические блоки).
	"cppworker.load_failure.model_schema_unsupported_hint_loop": {
		RU: "это loop-развёрнутый GGUF (%s): физических блоков %d — по имени отсутствующего тензора, а block_count=%d в заголовке описывает визиты; ре-экспортируйте файл штатным конвертером llama.cpp как %s_loop с block_count=%d или примените scripts/gguf_loop_meta_patch.py --fuse-loop-queries (см. docs/features/inkling2-loop.md) к копии файла. Повтор загрузки не поможет",
		EN: "this is a loop-expanded GGUF (%s): the %d physical blocks come from the name of the missing tensor, while block_count=%d in the header describes visits; re-export the file with the stock llama.cpp converter as %s_loop with block_count=%d, or run scripts/gguf_loop_meta_patch.py --fuse-loop-queries (see docs/features/inkling2-loop.md) on a copy of the file. Retrying the load will not help",
	},
	// %s — сборка llama.cpp.
	"cppworker.load_failure.model_schema_unsupported_hint": {
		RU: "нужен ре-экспорт GGUF под схему тензоров, которую понимает загрузчик llama.cpp (%s), или более новый загрузчик. Повтор загрузки не поможет",
		EN: "re-export the GGUF with a tensor schema this llama.cpp loader (%s) understands, or use a newer loader. Retrying the load will not help",
	},
}
