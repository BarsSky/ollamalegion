// load_failure_reason.go — R83 (2026-09-25): машиночитаемая ПРИЧИНА провала загрузки.
//
// ЗАЧЕМ. До этой правки провал загрузки жил в реестре как свободный текст
// (load_failures.go: loadFailureEntry{At, Err string}) и отдавался как
// {"state":"failed","error":"..."}. Из строки нельзя было ни выбрать реакцию,
// ни построить понятное уведомление оператору: UI мог только показать сырой
// текст llama.cpp вроде "failed to open GGUF file models/qwen3.8:latest.gguf".
//
// Здесь — единый словарь причин. Он же уходит в /api/models (поле load_failure)
// и дальше по связке cppworker → agent → webui, где превращается в уведомление
// с числами и подсказкой.
//
// ВАЖНО: это ПОЛЕ ОТВЕТА, а не запроса. Строгий JSON-декодер cppworker
// (types.DecodeJSONRequest, DisallowUnknownFields) и guard-тесты на
// `reason`/`adaptiveStage` в телах ЗАПРОСОВ здесь ни при чём — протокол запросов
// не меняется.
package main

import (
	"errors"
	"strings"
)

// Коды причин провала загрузки. Строки стабильны — на них опираются agent,
// балансер и WebUI, поэтому меняются только вместе с ними.
const (
	// LoadFailureModelNotFound — файла модели нет (имя не разрезолвилось).
	LoadFailureModelNotFound = "model_not_found"
	// LoadFailureConfigOutOfBounds — запрошенный n_ctx вне физических границ.
	LoadFailureConfigOutOfBounds = "config_out_of_bounds"
	// LoadFailureInsufficientResources — не хватило VRAM/RAM даже с выгрузкой.
	LoadFailureInsufficientResources = "insufficient_resources"
	// LoadFailureGGUFIncompatible — файл есть, но llama.cpp его не принял.
	LoadFailureGGUFIncompatible = "gguf_incompatible"
	// LoadFailureModelUnsupported — llama.cpp не знает АРХИТЕКТУРУ модели
	// (R697, 2026-11-24).
	//
	// Отдельный код, потому что это не «битый файл»: заголовок GGUF читается,
	// general.architecture называет архитектуру, которой нет в таблице сборки
	// (c/llama.cpp: src/llama-arch.cpp, LLM_ARCH_NAMES). Оператору бесполезно
	// «проверить файл» — нужно обновить сабмодуль и образ воркера, поэтому
	// ответ несёт причину, имя архитектуры и сборку llama.cpp (см.
	// model_unsupported.go).
	LoadFailureModelUnsupported = "model_unsupported"
	// LoadFailureModelSchemaUnsupported — архитектуру сборка ЗНАЕТ, но схема
	// тензоров не та: движок не нашёл ожидаемого тензора (R704, 2026-11-24).
	//
	// Отдельный код, потому что прежняя классификация отправляла такой текст в
	// model_not_found: в строке llama.cpp `check_tensor_dims: tensor
	// 'blk.24.attn_q.weight' not found` есть «not found», и машинный код врал —
	// файл на месте, архитектура распознана (иначе движок не дошёл бы до
	// тензоров). Живой случай — Inkling-2-Mini-UD-Q4_K_M.gguf на пине b12642:
	// файл loop-развёрнут (24 физических блока при block_count=48, ключ
	// num_loops), текущий загрузчик такую схему не читает. Лечится ре-экспортом
	// GGUF, а не поиском файла — поэтому и код отдельный. См.
	// model_schema_unsupported.go.
	LoadFailureModelSchemaUnsupported = "model_schema_unsupported"
	// LoadFailureBackendNotReady — бэкенд/менеджер моделей не инициализирован.
	LoadFailureBackendNotReady = "backend_not_ready"
	// LoadFailureTimeout — загрузка не уложилась в отведённое время.
	LoadFailureTimeout = "load_timeout"
	// LoadFailureAliasBroken — алиас есть, файл-источник пропал.
	LoadFailureAliasBroken = "alias_broken"
	// LoadFailureUnknown — не распознали; сырой текст сохраняется отдельно.
	LoadFailureUnknown = "unknown"
)

// LoadFailureReason — код причины (для JSON и для switch на клиентах).
type LoadFailureReason = string

// classifyLoadFailureReason определяет причину по ошибке и/или её тексту.
//
// Сначала пробуем ТИПИЗИРОВАННЫЕ ошибки (надёжнее строк), затем — текстовые
// признаки. Порядок проверок важен: например, "exceeds ... n_ctx" — это границы
// конфига, а не «модель не найдена», хотя llama.cpp в обоих случаях пишет
// "failed to load model".
func classifyLoadFailureReason(err error, errText string) LoadFailureReason {
	text := strings.ToLower(errText)
	if err != nil && text == "" {
		text = strings.ToLower(err.Error())
	}

	// --- типизированные ---
	var irErr *InsufficientResourcesError
	if errors.As(err, &irErr) {
		return LoadFailureInsufficientResources
	}

	// R697: ПЕРВОЙ текстовой проверкой — вердикт движка про архитектуру.
	//
	// ЖИВОЙ ФАКТ: llama.cpp на таком файле печатает
	// `llama_model_load: error loading model: unknown model architecture: 'inkling2'`,
	// а мост отдаёт «failed to load model from <путь>» (см.
	// internal/cppbackend/load_error_tail.go — причина теперь приклеивается к
	// ошибке). Проверка обязана стоять ДО общей ветки gguf/unsupported: иначе
	// осмысленный «архитектура не поддерживается сборкой» размазывается в
	// «формат/архитектура», и оператор идёт проверять целостность файла.
	if isModelUnsupportedText(text) {
		return LoadFailureModelUnsupported
	}

	// R704: ВТОРОЙ текстовой проверкой — «архитектура распознана, а схемы такой
	// нет».
	//
	// ЖИВОЙ ФАКТ (стенд, пин b12642, Inkling-2-Mini-UD-Q4_K_M.gguf):
	// `llama_model_load: error loading model: check_tensor_dims: tensor
	// 'blk.24.attn_q.weight' not found`. До этой проверки текст уходил в
	// model_not_found (ветка ниже ловит общее «not found»), и оператор шёл
	// проверять наличие файла, которого не теряли: файл лежит, arch прочитан,
	// движок ищет блоки, которых в файле нет (loop-развёрнутая схема).
	//
	// ПОЧЕМУ ИМЕННО ЗДЕСЬ. Проверка обязана стоять ПОСЛЕ вердикта про
	// архитектуру (иначе «unknown model architecture» размажется в «схема») и
	// ДО веток model_not_found/gguf_incompatible: в тексте есть «not found» и
	// «failed to load model», то есть обе ветки ниже его уже присвоили бы себе.
	// OOM и границы конфига сюда не попадают: в них нет имени тензора.
	if isModelSchemaUnsupportedText(text) {
		return LoadFailureModelSchemaUnsupported
	}

	switch {
	// Границы конфига: явные сообщения наших проверок.
	case strings.Contains(text, "exceeds ram-fallback-max-n-ctx"),
		strings.Contains(text, "exceeds the physically feasible maximum"),
		strings.Contains(text, "exceeds feasible_max_context"),
		strings.Contains(text, "n_ctx_infeasible"):
		return LoadFailureConfigOutOfBounds

	case strings.Contains(text, "insufficient"), strings.Contains(text, "out of memory"),
		strings.Contains(text, "cannot allocate"), strings.Contains(text, "oom"),
		strings.Contains(text, "failed to allocate"):
		return LoadFailureInsufficientResources

	case strings.Contains(text, "alias source"):
		return LoadFailureAliasBroken

	case strings.Contains(text, "no such file"), strings.Contains(text, "not found"),
		strings.Contains(text, "failed to open gguf file"),
		strings.Contains(text, "could not resolve model path"):
		return LoadFailureModelNotFound

	case strings.Contains(text, "timeout"), strings.Contains(text, "timed out"),
		strings.Contains(text, "deadline exceeded"):
		return LoadFailureTimeout

	case strings.Contains(text, "not initialized"), strings.Contains(text, "model manager"):
		return LoadFailureBackendNotReady

	case strings.Contains(text, "failed to load model"), strings.Contains(text, "llama_model_load"),
		strings.Contains(text, "gguf"), strings.Contains(text, "unsupported"),
		strings.Contains(text, "bad magic"), strings.Contains(text, "unknown architecture"):
		return LoadFailureGGUFIncompatible
	}
	return LoadFailureUnknown
}

// isLoadFailureReason валиден ли код (для клиентов, которые присылают его назад
// в запросах — например, при подтверждении «показывал уведомление»).
func isLoadFailureReason(reason string) bool {
	switch reason {
	case LoadFailureModelNotFound, LoadFailureConfigOutOfBounds,
		LoadFailureInsufficientResources, LoadFailureGGUFIncompatible,
		LoadFailureBackendNotReady, LoadFailureTimeout,
		LoadFailureAliasBroken, LoadFailureUnknown,
		LoadFailureModelUnsupported, LoadFailureModelSchemaUnsupported:
		return true
	}
	return false
}

// loadFailureSeverity — насколько причина требует внимания оператора.
// Используется уведомлениями: config_out_of_bounds и insufficient_resources —
// это про конфиг/железо (оператор может исправить), gguf_incompatible и
// model_not_found — про конкретную модель, timeout — транзиентная.
func loadFailureSeverity(reason LoadFailureReason) string {
	switch reason {
	case LoadFailureConfigOutOfBounds, LoadFailureInsufficientResources:
		return "error"
	// R697: не «файл сломан», а «сборка не знает эту архитектуру» — оператору
	// действовать (обновить сабмодуль/образ), поэтому error, а не warning.
	//
	// R704: то же для «схема файла не поддержана» — действовать нужно
	// (ре-экспорт GGUF или новый загрузчик), повтор загрузки не поможет.
	case LoadFailureModelNotFound, LoadFailureGGUFIncompatible, LoadFailureAliasBroken,
		LoadFailureModelUnsupported, LoadFailureModelSchemaUnsupported:
		return "error"
	case LoadFailureTimeout, LoadFailureBackendNotReady:
		return "warning"
	}
	return "warning"
}
