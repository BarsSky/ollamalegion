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
		LoadFailureAliasBroken, LoadFailureUnknown:
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
	case LoadFailureModelNotFound, LoadFailureGGUFIncompatible, LoadFailureAliasBroken:
		return "error"
	case LoadFailureTimeout, LoadFailureBackendNotReady:
		return "warning"
	}
	return "warning"
}
