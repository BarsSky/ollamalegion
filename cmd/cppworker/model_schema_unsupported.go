// model_schema_unsupported.go — R704 (2026-11-24): «схема файла не поддержана
// текущим загрузчиком llama.cpp» как ОТДЕЛЬНЫЙ ответ воркера.
//
// ЖИВОЙ СЛУЧАЙ (стенд, cppworker gpu-r703 = пин llama.cpp b12642, файл
// Inkling-2-Mini-UD-Q4_K_M.gguf, 15.46 GiB, general.architecture="inkling2"):
//
//	POST /api/models/load → движок падает ПОЗЖЕ архитектуры:
//	[llama.cpp] llama_model_load: error loading model: check_tensor_dims:
//	            tensor 'blk.24.attn_q.weight' not found
//	[llama.cpp] llama_model_load_from_file_impl: failed to load model
//
// то есть архитектуру сборка уже ЗНАЕТ (R697 закрыл «unknown model
// architecture»), а падение идёт на тензорах.
//
// ПОЧЕМУ ЭТО БЫЛО НЕВИДИМО. classifyLoadFailureReason (load_failure_reason.go)
// видел в строке «not found» и присваивал reason=model_not_found. Машинный код
// врал: файл на месте (его размер и заголовок прочитаны), архитектура
// распознана — а потребитель (WebUI, Cline, оператор) получал «модель не
// найдена» и шёл искать файл.
//
// ЧТО НА САМОМ ДЕЛЕ. Файл loop-развёрнут: метаданные объявляют block_count=48 и
// ключ num_loops, а тензоры описаны для 24 физических блоков (blk.0..blk.23) —
// движок же читает block_count из заголовка и ищет blk.24. Такую схему текущий
// загрузчик не поддерживает; лечится ре-экспортом GGUF как inkling2_loop с
// block_count=24 или более новым загрузчиком. Поэтому у отказа свой код,
// человеческий текст с числами, имя архитектуры, сборка llama.cpp и подсказка.
//
// R712 (2026-11-25): у причины есть ВТОРАЯ форма, и до этой правки ответ на неё
// врал. Файл УЖЕ переведён в loop-схему (inkling2_loop, block_count=24 — это
// физические блоки, ключи loop.start/end/count на месте), но в нём нет
// обязательного blk.{6..17}.loop_msg_query.weight — граница конвертации R711.
// R704 отвечал на это «physical_blocks: 6» и советовал «ре-экспорт GGUF как
// inkling2_loop_loop с block_count=6», то есть предлагал сделать уже сделанное
// и с неверным числом: 6 — это core_start из имени тензора, а не число блоков.
// Теперь для loop-схемы физические блоки берутся из ЗАГОЛОВКА, а подсказка ведёт
// к фолду запросов (scripts/gguf_loop_meta_patch.py --fuse-loop-queries) или к
// полному ре-экспорту штатным конвертером (см. docs/features/inkling2-loop.md).
// Код причины не меняется: публичный контракт R704 остаётся model_schema_unsupported.
//
// ФОРМА ОТВЕТА. Та же, что у R697 (model_unsupported.go): 409 + code/reason +
// error/hint + raw_error, плюс факты этой причины (arch, block_count,
// physical_blocks, tensor). Новых полей протокола ЗАПРОСОВ не появляется:
// балансер проксирует статус и тело как есть, WebUI показывает error.
package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)

// modelSchemaUnsupportedCodeR704 — машинный код ответа. Совпадает с кодом
// причины (LoadFailureModelSchemaUnsupported), чтобы клиенту не приходилось
// держать два словаря.
const modelSchemaUnsupportedCodeR704 = LoadFailureModelSchemaUnsupported

// modelSchemaUnsupportedTensorRE — как llama.cpp называет пропавший тензор:
//
//	check_tensor_dims: tensor 'blk.24.attn_q.weight' not found
//
// Регулярка, а не поиск подстрок: «tensor» встречается и в путях к файлам
// (модель с «tensor» в имени), и тогда «…not found» рядом дало бы ложную
// классификацию. Форма с кавычками — единственная, которую печатает движок
// (llama-model-loader.cpp, check_tensor_dims).
var modelSchemaUnsupportedTensorRE = regexp.MustCompile(`(?i)tensor\s+['"]([^'"]+)['"]\s+not found`)

// modelSchemaUnsupportedBlockRE — номер блока из имени тензора (blk.<N>.<...>).
var modelSchemaUnsupportedBlockRE = regexp.MustCompile(`(?i)^blk\.(\d+)\.`)

// isModelSchemaUnsupportedText — текст ошибки это «движок не нашёл ожидаемого
// тензора при распознанной архитектуре».
//
// Условие по существу (и почему проверка стоит ПОСЛЕ вердикта про архитектуру):
//   - вердикт «unknown/unsupported model architecture» приоритетнее: там до
//     тензоров дело не доходит, и это другой код (model_unsupported);
//   - сам признак — «not found» именно про тензор: либо строка с именем
//     тензора в кавычках, либо (если движок имени не напечатал) строка
//     check_tensor_dims с «not found».
//
// Обратная сторона важна не меньше: настоящий model_not_found («failed to open
// GGUF file … No such file or directory»), битый формат (bad magic) и OOM этот
// признак не дают — иначе новый код присвоил бы себе чужие провалы.
func isModelSchemaUnsupportedText(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	low := strings.ToLower(text)
	// Приоритет архитектуры: если движок сказал «такой архитектуры нет», это
	// model_unsupported, а не схема (строки «not found» там может и не быть).
	if isModelUnsupportedText(low) {
		return false
	}
	if modelSchemaUnsupportedTensorRE.MatchString(text) {
		return true
	}
	for _, line := range strings.Split(low, "\n") {
		if strings.Contains(line, "check_tensor_dims") && strings.Contains(line, "not found") {
			return true
		}
	}
	return false
}

// modelSchemaUnsupportedTensorFromText — имя пропавшего тензора из строки
// движка. Пустая строка — движок имени не назвал (тогда работаем без чисел).
func modelSchemaUnsupportedTensorFromText(text string) string {
	m := modelSchemaUnsupportedTensorRE.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// modelSchemaUnsupportedBlockFromTensor — номер блока из имени тензора:
// "blk.24.attn_q.weight" → 24. 0 = имя не про блок (или разобрать не вышло).
//
// ЗАЧЕМ НОМЕР. У ДО-патчевого файла движок падает на ПЕРВОМ блоке за пределами
// того, что есть в файле, поэтому номер = число физических блоков: у
// Inkling-2-Mini это 24 при block_count=48 из заголовка. Именно эта пара чисел и
// объясняет оператору «loop-развёрнутая схема».
//
// R712: у файла, УЖЕ переведённого в loop-схему, это неверно — blk.6.
// loop_msg_query.weight даёт 6 (core_start), а физических блоков 24 (block_count
// из заголовка). Поэтому число отсюда больше не идёт в ответ напрямую: см.
// modelSchemaPhysicalBlocksR712.
func modelSchemaUnsupportedBlockFromTensor(tensor string) int {
	m := modelSchemaUnsupportedBlockRE.FindStringSubmatch(strings.TrimSpace(tensor))
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// modelSchemaHeaderFactsR712 — arch, block_count и признак loop-схемы из
// ЗАГОЛОВКА файла.
//
// ИСТОЧНИК — кэш менеджера моделей (ScanModels/GetModelMeta читают header один
// раз и помнят), а не имя файла: имя — намерение автора модели, заголовок —
// факт. В отличие от R697 (имя архитектуры было в тексте ошибки) здесь движок
// архитектуру не печатает, поэтому её нужно взять из метаданных. Файл на месте
// по определению причины (иначе провал был бы model_not_found), поэтому чтение
// заголовка допустимо; сбой чтения не отменяет отказ — просто остаётся без
// чисел (arch="", blockCount=0, hasLoopSchemaKeys=false).
func modelSchemaHeaderFactsR712(modelName string) (arch string, blockCount int, hasLoopSchemaKeys bool) {
	if backend == nil || modelName == "" {
		return "", 0, false
	}
	mm := backend.ModelManager()
	if mm == nil {
		return "", 0, false
	}
	meta, err := mm.GetModelMetaResolved(modelName)
	if err != nil || meta == nil {
		return "", 0, false
	}
	return strings.TrimSpace(meta.Architecture), meta.NLayers, meta.HasLoopSchemaKeys
}

// modelSchemaLoopSchemaR712 — файл уже находится в loop-схеме.
//
// ДВА ПРИЗНАКА, потому что одного мало:
//   - general.architecture = inkling2_loop — то, что пишет конвертер/патч R711;
//   - в заголовке есть КАНОНИЧЕСКИЕ ключи loop.start/loop.end/loop.count — даже
//     если архитектуру ещё не переименовали (старая орфография num_loops +
//     loop.core_start/loop.core_end есть и у до-патчевого файла, поэтому
//     HasLoopSchemaKeys смотрит только канонические имена).
//
// ЗАЧЕМ. Именно на этом различии строится вся ветка R712: файлу, который уже в
// loop-схеме, нельзя советовать «ре-экспорт в inkling2_loop_loop» (дефект), а
// число физических блоков у него — block_count из заголовка, а не номер блока из
// имени отсутствующего тензора.
func modelSchemaLoopSchemaR712(arch string, hasLoopSchemaKeys bool) bool {
	if hasLoopSchemaKeys {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(arch), "inkling2_loop")
}

// modelSchemaPrePatchArchR712 — архитектура ДО перевода в loop-схему (inkling2):
// у таких файлов block_count в заголовке — визиты (48), а физические блоки (24)
// видны только по имени отсутствующего тензора. Loop-подсказка «ре-экспорт как
// <arch>_loop» уместна ТОЛЬКО здесь: для прочих архитектур loop-схемы не
// существует, и прежняя формулировка приписывала её любому файлу с номером
// блока в имени тензора (например «ре-экспорт как qwen3_loop»).
func modelSchemaPrePatchArchR712(arch string) bool {
	return strings.EqualFold(strings.TrimSpace(arch), "inkling2")
}

// modelSchemaPhysicalBlocksR712 — число физических блоков для поля ответа.
//
// ДЕФЕКТ, КОТОРЫЙ ЗАКРЫВАЕТСЯ. Раньше оно ВСЕГДА выводилось из имени
// отсутствующего тензора (blk.<N> → N). Для до-патчевого файла это верно: движок
// падает на первом блоке, которого нет, поэтому N = числу физических блоков
// (blk.24 при block_count=48). Но для файла, УЖЕ переведённого в loop-схему, N —
// это core_start (blk.6.loop_msg_query.weight), и подставлять его как «6
// физических блоков» — то самое враньё из дефекта. У loop-схемы источник —
// block_count из заголовка; если заголовок не прочитан, число не выдумывается
// (0 = «неизвестно»), а не берётся из имени тензора.
func modelSchemaPhysicalBlocksR712(loopSchema bool, blockCount int, tensor string) int {
	if loopSchema {
		if blockCount > 0 {
			return blockCount
		}
		return 0
	}
	return modelSchemaUnsupportedBlockFromTensor(tensor)
}

// modelSchemaLoopQueryTensorR712 — отсутствующий тензор это именно свёрнутый
// запрос loop-схемы (blk.<core>.loop_msg_query.weight). Пустое имя не отсекаем:
// движок мог не напечатать тензор, но у loop-схемы это тот же класс отказа.
func modelSchemaLoopQueryTensorR712(tensor string) bool {
	if strings.TrimSpace(tensor) == "" {
		return true
	}
	return strings.Contains(tensor, "loop_msg_query")
}

// modelSchemaDiagnosticsR704 — факты и подсказка для разбора.
func modelSchemaDiagnosticsR704(modelName string, loadErr error) map[string]interface{} {
	arch, blockCount, hasLoopSchemaKeys := modelSchemaHeaderFactsR712(modelName)
	tensor := ""
	if loadErr != nil {
		tensor = modelSchemaUnsupportedTensorFromText(loadErr.Error())
	}
	loopSchema := modelSchemaLoopSchemaR712(arch, hasLoopSchemaKeys)
	physical := modelSchemaPhysicalBlocksR712(loopSchema, blockCount, tensor)
	build := modelUnsupportedBuildR697()
	diag := map[string]interface{}{
		"decision": modelSchemaUnsupportedCodeR704,
		"hint":     modelSchemaUnsupportedHintR704(arch, blockCount, physical, loopSchema, tensor, build),
	}
	if arch != "" {
		diag["arch"] = arch
	}
	if build != "" {
		diag["llama_build"] = build
	}
	if blockCount > 0 {
		diag["block_count"] = blockCount
	}
	if physical > 0 {
		diag["physical_blocks"] = physical
	}
	if tensor != "" {
		diag["tensor"] = tensor
	}
	return diag
}

// modelSchemaUnsupportedMessageR704 — человеческая причина для клиента.
//
// «%s — фраза о схеме»: три формы, и выбор между ними — это и есть R712:
//   - файл уже в loop-схеме (block_count из заголовка = физические блоки);
//   - до-патчевый loop-развёрнутый файл (24 физических при 48 визитах) — только
//     для arch inkling2, у прочих архитектур loop-схемы не существует;
//   - общая схема (чисел нет или они ничего не объясняют).
//
// Второй %s — сборка llama.cpp, дальше тензор и архитектура. Текст собран из
// каталога i18n (в отличие от R697 здесь несколько форм фразы, поэтому склейка в Go).
func modelSchemaUnsupportedMessageR704(arch string, blockCount, physical int, loopSchema bool, tensor, build string) string {
	schema := i18n.T("cppworker.load_failure.model_schema_unsupported_generic")
	switch {
	case loopSchema && blockCount > 0:
		schema = i18n.T("cppworker.load_failure.model_schema_unsupported_loop_schema", blockCount)
	case !loopSchema && modelSchemaPrePatchArchR712(arch) && physical > 0 && blockCount > physical:
		schema = i18n.T("cppworker.load_failure.model_schema_unsupported_loop", physical, blockCount)
	}
	tensorLabel := tensor
	if tensorLabel == "" {
		tensorLabel = i18n.T("cppworker.load_failure.tensor_unknown")
	}
	archLabel := arch
	if archLabel == "" {
		// Архитектуру берём из ЗАГОЛОВКА файла (модель схемы), а не из текста
		// движка — в отличие от R697 здесь её в сообщении нет.
		archLabel = i18n.T("cppworker.load_failure.arch_unknown_header")
	}
	return i18n.T("cppworker.load_failure.model_schema_unsupported",
		modelUnsupportedBuildLabelR697(build), schema, tensorLabel, archLabel)
}

// modelSchemaUnsupportedHintR704 — что делать оператору.
//
// R712: четыре формы, и главное — не советовать файлу, который УЖЕ в loop-схеме,
// ещё раз «ре-экспорт в <arch>_loop» (прежний ответ давал inkling2_loop_loop с
// block_count из номера тензора — см. дефект R709):
//   - loop-схема без blk.<core>.loop_msg_query.weight — это граница конвертации:
//     фолд запросов скриптом R711 или полный ре-экспорт штатным конвертером,
//     который пишет тензор сам;
//   - loop-схема без другого тензора — фолд не поможет, нужен полный ре-экспорт;
//   - до-патчевый inkling2 (24 физических при 48 визитах) — ре-экспорт как
//     inkling2_loop с block_count=24 или тот же скрипт; числа названы с
//     источником (24 — из имени тензора, 48 — из заголовка);
//   - без чисел — общая форма со сборкой.
//
// Для не-inkling2 архитектур loop-подсказки больше нет: прежняя формулировка
// приписывала loop-схему любому файлу, у которого в имени отсутствующего тензора
// есть номер блока («ре-экспорт как qwen3_loop»), — это тот же класс вранья.
func modelSchemaUnsupportedHintR704(arch string, blockCount, physical int, loopSchema bool, tensor, build string) string {
	if loopSchema && blockCount > 0 {
		tensorLabel := tensor
		if strings.TrimSpace(tensorLabel) == "" {
			tensorLabel = i18n.T("cppworker.load_failure.tensor_unknown")
		}
		key := "cppworker.load_failure.model_schema_unsupported_hint_loop_schema_other"
		if modelSchemaLoopQueryTensorR712(tensor) {
			key = "cppworker.load_failure.model_schema_unsupported_hint_loop_schema"
		}
		return i18n.T(key, arch, blockCount, tensorLabel)
	}
	if !loopSchema && modelSchemaPrePatchArchR712(arch) && physical > 0 && blockCount > physical {
		return i18n.T("cppworker.load_failure.model_schema_unsupported_hint_loop",
			arch, physical, blockCount, arch, physical)
	}
	return i18n.T("cppworker.load_failure.model_schema_unsupported_hint",
		modelUnsupportedBuildLabelR697(build))
}

// writeModelSchemaUnsupportedResponseR704 — ответ 409 с причиной, числами и
// подсказкой.
//
// СТАТУС 409, а не 500: отказ по СОДЕРЖИМОМУ файла, повтор не поможет (тело
// говорит это прямым текстом). 5xx заставил бы клиентов повторять запрос, а
// оператора — искать поломку воркера. Тот же выбор у R697 и у imageworker (R696,
// bundle_unsupported → 409).
func writeModelSchemaUnsupportedResponseR704(w http.ResponseWriter, modelName string, loadErr error, arch string, blockCount, physical int, loopSchema bool, tensor string) {
	build := modelUnsupportedBuildR697()
	body := map[string]interface{}{
		"error":       modelSchemaUnsupportedMessageR704(arch, blockCount, physical, loopSchema, tensor, build),
		"code":        modelSchemaUnsupportedCodeR704,
		"reason":      modelSchemaUnsupportedCodeR704,
		"model":       modelName,
		"decision":    modelSchemaUnsupportedCodeR704,
		"hint":        modelSchemaUnsupportedHintR704(arch, blockCount, physical, loopSchema, tensor, build),
		"llama_build": build,
	}
	if arch != "" {
		body["arch"] = arch
	}
	if blockCount > 0 {
		body["block_count"] = blockCount
	}
	if physical > 0 {
		body["physical_blocks"] = physical
	}
	if tensor != "" {
		body["tensor"] = tensor
	}
	if loadErr != nil {
		body["raw_error"] = loadErr.Error()
	}
	writeJSON(w, http.StatusConflict, body)
}

// respondModelSchemaUnsupportedR704 — единая точка для путей загрузки: если
// движок не нашёл ожидаемого тензора при распознанной архитектуре, ответ уже
// записан (true).
//
// Вызывается ПОСЛЕ обычной записи в лог и ДО общего 500 — как R697: остальные
// провалы загрузки обязаны остаться 500 с прежним текстом.
func respondModelSchemaUnsupportedR704(w http.ResponseWriter, modelName string, loadErr error) bool {
	if loadErr == nil || !isModelSchemaUnsupportedText(loadErr.Error()) {
		return false
	}
	tensor := modelSchemaUnsupportedTensorFromText(loadErr.Error())
	arch, blockCount, hasLoopSchemaKeys := modelSchemaHeaderFactsR712(modelName)
	loopSchema := modelSchemaLoopSchemaR712(arch, hasLoopSchemaKeys)
	physical := modelSchemaPhysicalBlocksR712(loopSchema, blockCount, tensor)
	build := modelUnsupportedBuildR697()
	recordLoadFailureR697(modelName, loadErr)
	logger.Get().Errorw("load refused: llama.cpp cannot load the model tensor schema",
		"name", modelName, "arch", arch, "block_count", blockCount,
		"physical_blocks", physical, "loop_schema", loopSchema,
		"tensor", tensor, "llama_build", build)
	writeModelSchemaUnsupportedResponseR704(w, modelName, loadErr, arch, blockCount, physical, loopSchema, tensor)
	return true
}

// respondLoadRefusalR704 — единый вход для путей загрузки: сначала вердикт про
// архитектуру (R697), затем вердикт про схему (R704).
//
// ЗАЧЕМ ОДИН ВХОД. Хендлеры (handlers_model.go, handlers_model_load_params.go)
// отвечают на провал загрузки в одном месте; если каждый код вызывать отдельной
// строкой, новый вид отказа легко забыть на одном из двух путей — ровно это и
// случилось бы с R704. Порядок внутри важен и совпадает с порядком в
// classifyLoadFailureReason: неизвестная архитектура не может быть «схемой».
func respondLoadRefusalR704(w http.ResponseWriter, modelName string, loadErr error) bool {
	if respondModelUnsupportedR697(w, modelName, loadErr) {
		return true
	}
	return respondModelSchemaUnsupportedR704(w, modelName, loadErr)
}
