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
// ЗАЧЕМ НОМЕР. Движок падает на ПЕРВОМ блоке за пределами того, что есть в
// файле, поэтому номер = число физических блоков: у Inkling-2-Mini это 24 при
// block_count=48 из заголовка. Именно эта пара чисел и объясняет оператору
// «loop-развёрнутая схема».
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

// modelSchemaFactsR704 — arch и block_count из ЗАГОЛОВКА файла.
//
// ИСТОЧНИК — кэш менеджера моделей (ScanModels/GetModelMeta читают header один
// раз и помнят), а не имя файла: имя — намерение автора модели, заголовок —
// факт. В отличие от R697 (имя архитектуры было в тексте ошибки) здесь движок
// архитектуру не печатает, поэтому её нужно взять из метаданных. Файл на месте
// по определению причины (иначе провал был бы model_not_found), поэтому чтение
// заголовка допустимо; сбой чтения не отменяет отказ — просто остаётся без
// чисел (arch="", blockCount=0).
func modelSchemaFactsR704(modelName string) (arch string, blockCount int) {
	if backend == nil || modelName == "" {
		return "", 0
	}
	mm := backend.ModelManager()
	if mm == nil {
		return "", 0
	}
	meta, err := mm.GetModelMetaResolved(modelName)
	if err != nil || meta == nil {
		return "", 0
	}
	return strings.TrimSpace(meta.Architecture), meta.NLayers
}

// modelSchemaDiagnosticsR704 — факты и подсказка для разбора.
func modelSchemaDiagnosticsR704(modelName string, loadErr error) map[string]interface{} {
	arch, blockCount := modelSchemaFactsR704(modelName)
	tensor := ""
	if loadErr != nil {
		tensor = modelSchemaUnsupportedTensorFromText(loadErr.Error())
	}
	physical := modelSchemaUnsupportedBlockFromTensor(tensor)
	build := modelUnsupportedBuildR697()
	diag := map[string]interface{}{
		"decision": modelSchemaUnsupportedCodeR704,
		"hint":     modelSchemaUnsupportedHintR704(arch, physical, build),
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
// «%s — фраза о схеме»: loop-развёрнутая (с числами) или общая, если номер
// блока/block_count неизвестны. Второй %s — сборка llama.cpp, дальше тензор и
// архитектура. Текст собран из каталога i18n (в отличие от R697 здесь две
// формы фразы, поэтому склейка в Go).
func modelSchemaUnsupportedMessageR704(arch string, blockCount, physical int, tensor, build string) string {
	schema := i18n.T("cppworker.load_failure.model_schema_unsupported_generic")
	if arch != "" && physical > 0 && blockCount > physical {
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
// Для loop-развёрнутой схемы подсказка конкретная (ре-экспорт как <arch>_loop с
// block_count=<физических блоков>): «обновите загрузчик» без направления
// заставляет искать архитектуру самому. Для случая без чисел — общая форма со
// сборкой.
func modelSchemaUnsupportedHintR704(arch string, physical int, build string) string {
	if arch != "" && physical > 0 {
		return i18n.T("cppworker.load_failure.model_schema_unsupported_hint_loop", arch, physical)
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
func writeModelSchemaUnsupportedResponseR704(w http.ResponseWriter, modelName string, loadErr error, arch string, blockCount, physical int, tensor string) {
	build := modelUnsupportedBuildR697()
	body := map[string]interface{}{
		"error":       modelSchemaUnsupportedMessageR704(arch, blockCount, physical, tensor, build),
		"code":        modelSchemaUnsupportedCodeR704,
		"reason":      modelSchemaUnsupportedCodeR704,
		"model":       modelName,
		"decision":    modelSchemaUnsupportedCodeR704,
		"hint":        modelSchemaUnsupportedHintR704(arch, physical, build),
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
	physical := modelSchemaUnsupportedBlockFromTensor(tensor)
	arch, blockCount := modelSchemaFactsR704(modelName)
	build := modelUnsupportedBuildR697()
	recordLoadFailureR697(modelName, loadErr)
	logger.Get().Errorw("load refused: llama.cpp cannot load the model tensor schema",
		"name", modelName, "arch", arch, "block_count", blockCount,
		"physical_blocks", physical, "tensor", tensor, "llama_build", build)
	writeModelSchemaUnsupportedResponseR704(w, modelName, loadErr, arch, blockCount, physical, tensor)
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
