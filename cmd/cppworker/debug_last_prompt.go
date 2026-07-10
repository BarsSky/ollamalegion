package main

// debug_last_prompt.go — диагностический endpoint для отладки prompt-too-long.
//
// Причина (см. CHANGELOG.md, "Различение prompt_exceeds_context vs n_ctx_too_large_for_backend"):
// Cline/OpenWebUI могут присылать prompt+history+tools, которые в сумме превышают n_ctx.
// cppworker правильно возвращает HTTP 413 + PromptExceedsNCtxError, но непонятно, ЧТО именно
// занимает столько токенов: длинная история? tool definitions? system prompt?
//
// Endpoint GET /api/v1/cppworker/debug/last-prompt сохраняет метаданные последнего
// inference-запроса (model, prompt_chars, prompt_tokens, n_ctx, has_tools, status)
// и короткий preview текста. Это позволяет диагносту понять причину без чтения
// логов cppworker.
//
// ВАЖНО: endpoint хранит только последний запрос (per-process), не ведёт историю.
// В production endpoint защищён authMiddleware (см. router.go).

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"ollama-loadbalancer/c/bridge"
)

// LastPromptInfo — снапшот последнего inference-запроса.
//
// Поля:
//
//	Model           — имя модели (например, "gemma-4-E4B-it-Q4_K_M").
//	Endpoint        — путь, на который пришёл запрос ("/api/chat", "/v1/chat/completions").
//	PromptChars     — длина собранного prompt в символах (string, не rune — это
//	                 важно для ASCII-heavy JSON где char==rune; для Unicode
//	                 приблизительная оценка всё равно достаточна).
//	PromptTokens    — реальное число токенов через backend.CountTokens
//	                 (0 если модель не загружена на момент записи).
//	NCtxOverride    — n_ctx, который клиент прислал в options.num_ctx (0 если нет).
//	NCtxLoaded      — n_ctx, с которым модель сейчас загружена (0 если модель не
//	                 загружена).
//	HasTools        — true если в запросе был непустой tools[].
//	Status          — "ok", "n_ctx_too_large", "prompt_too_long", "model_not_loaded",
//	                 "inference_failed" — итог запроса.
//	Timestamp       — RFC3339Nano время завершения запроса.
//	PromptHead      — первые 512 символов prompt (для preview).
//	PromptTail      — последние 256 символов prompt (для preview).
//	PromptLinesHint — оценка числа "строк" prompt (число \n + 1) — для диагностики
//	                 длинной истории диалога.
//	Error           — если Status содержит ошибку, её текст (например, "prompt
//	                 exceeds n_ctx even with min floor").
type LastPromptInfo struct {
	Model          string    `json:"model"`
	Endpoint       string    `json:"endpoint"`
	PromptChars    int       `json:"prompt_chars"`
	PromptTokens   int       `json:"prompt_tokens"`
	NCtxOverride   int       `json:"n_ctx_override"`
	NCtxLoaded     int       `json:"n_ctx_loaded"`
	HasTools       bool      `json:"has_tools"`
	Status         string    `json:"status"`
	Timestamp      time.Time `json:"timestamp"`
	PromptHead     string    `json:"prompt_head"`
	PromptTail     string    `json:"prompt_tail"`
	PromptLinesHint int      `json:"prompt_lines_hint"`
	Error          string    `json:"error,omitempty"`
	// 2026-07-01: reasoning-поля. Для reasoning-моделей (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
	// разделяем output на (reasoning, content) и фиксируем размеры в snapshot'е.
	// Это помогает диагностировать случаи, когда модель сгенерировала много reasoning-токенов
	// и упёрлась в n_predict.
	ReasoningChars  int    `json:"reasoning_chars,omitempty"`
	ContentChars    int    `json:"content_chars,omitempty"`
	ReasoningHead   string `json:"reasoning_head,omitempty"`
	ContentHead     string `json:"content_head,omitempty"`
	ReasoningTail   string `json:"reasoning_tail,omitempty"`
	ContentTail     string `json:"content_tail,omitempty"`
	UnclosedThink   bool   `json:"unclosed_think,omitempty"` // true если модель не закрыла <think>
}

// lastPromptMu защищает lastPromptSnapshot от concurrent read/write.
// Запись редкая (раз в запрос), чтение тоже редкое (только при вызове endpoint).
var lastPromptMu sync.RWMutex

// lastPromptSnapshot — последний снапшот. nil если ни одного запроса не было.
var lastPromptSnapshot *LastPromptInfo

// recordLastPromptFromError — удобная обёртка для записи статуса из error.
//
// Маппинг ошибок → статус (для удобства диагноста):
//
//	nil                                            → "ok"
//	*PromptExceedsNCtxError                        → "prompt_too_long"
//	*ReloadLoopLimitError                          → "reload_loop_limit"
//	*ReloadDisabledForToolsError                   → "reload_disabled_for_tools"
//	*InsufficientResourcesError                    → "insufficient_resources"
//	bridge код 2 (ErrCodeNCtxNeedsReload)          → "n_ctx_too_large"
//	bridge код 3 (ErrCodePromptTooLong)            → "prompt_too_long"
//	bridge код 4 (ErrCodeGPUOOM)                   → "gpu_oom"
//	bridge код 6 (ErrCodeInsufficientResources)    → "insufficient_resources"
//	остальное                                      → "inference_failed"
//
// Параметр hasTools проставляется caller'ом в зависимости от наличия tools[]
// в исходном запросе (для OpenAI /api/chat и Ollama /api/chat).
//
// Backward-compat: эта функция НЕ детектит client_disconnect. Используйте
// recordLastPromptFromErrorWithContext если хотите видеть обрывы стримов
// в debug snapshot (Round 5 Fix 3).
func recordLastPromptFromError(model, endpoint, prompt string, params *bridge.GenerationParams, hasTools bool, err error) {
	recordLastPromptFromErrorWithContext(model, endpoint, prompt, params, hasTools, err, nil)
}

// recordLastPromptFromErrorWithContext — расширенная версия: при err==nil
// проверяет ctx.Err() и фиксирует обрыв клиента как "client_disconnected".
// Если r == nil — fallback на legacy-поведение (status="ok" при err==nil).
func recordLastPromptFromErrorWithContext(model, endpoint, prompt string, params *bridge.GenerationParams, hasTools bool, err error, r *http.Request) {
	status := "ok"
	errStr := ""
	if err != nil {
		status, errStr = classifyLastPromptStatus(err)
	} else if r != nil && r.Context() != nil {
		// Bug fix (Round 5 Fix 3): если err == nil, но клиент уже отвалился —
		// фиксируем как "client_disconnected", чтобы диагност видел обрывы,
		// которые раньше маскировались под "ok".
		if ctxErr := r.Context().Err(); ctxErr != nil {
			status = "client_disconnected"
			errStr = ctxErr.Error()
		}
	}

	promptTokens := 0
	nCtxLoaded := 0
	if backend != nil {
		if info, getErr := backend.GetModel(model); getErr == nil && info != nil {
			nCtxLoaded = info.ContextSize
			if model != "" {
				promptTokens = backend.CountTokens(model, prompt)
			}
		}
	}

	nCtxOverride := 0
	if params != nil {
		nCtxOverride = params.NCtxOverride
	}

	recordLastPrompt(model, endpoint, prompt, promptTokens, nCtxOverride, nCtxLoaded, hasTools, status, errStr)
}

// classifyLastPromptStatus — маппит err на (status, errorText) для диагностики.
func classifyLastPromptStatus(err error) (string, string) {
	if err == nil {
		return "ok", ""
	}

	// Сначала проверяем конкретные типы ошибок cppworker.
	var perr *PromptExceedsNCtxError
	if errors.As(err, &perr) {
		return "prompt_too_long", err.Error()
	}
	var rllErr *ReloadLoopLimitError
	if errors.As(err, &rllErr) {
		return "reload_loop_limit", err.Error()
	}
	var rdftErr *ReloadDisabledForToolsError
	if errors.As(err, &rdftErr) {
		return "reload_disabled_for_tools", err.Error()
	}
	var irErr *InsufficientResourcesError
	if errors.As(err, &irErr) {
		return "insufficient_resources", err.Error()
	}

	// Иначе смотрим bridge error code (последняя ошибка C-bridge).
	if info := bridge.GetLastErrorInfo(); info != nil {
		switch info.Code {
		case bridge.ErrCodeNCtxNeedsReload:
			return "n_ctx_too_large", err.Error()
		case bridge.ErrCodePromptTooLong:
			return "prompt_too_long", err.Error()
		case bridge.ErrCodeGPUOOM:
			return "gpu_oom", err.Error()
		case bridge.ErrCodeInsufficientResources:
			return "insufficient_resources", err.Error()
		}
	}

	return "inference_failed", err.Error()
}

// recordLastPrompt — обновляет lastPromptSnapshot метаданными текущего запроса.
//
// Параметры:
//
//	model          — имя модели.
//	endpoint       — путь запроса.
//	prompt         — финальный prompt (после chat template).
//	promptTokens   — фактическое число токенов из backend.CountTokens (0 если
//	                 модель не загружена).
//	nCtxOverride   — что клиент прислал в options.num_ctx (0 если не задано).
//	nCtxLoaded     — что вернулось из backend.GetModel (0 если модель не загружена).
//	hasTools       — true если в запросе был tools[].
//	status         — итог: "ok", "prompt_too_long", "n_ctx_too_large", ...
//	errStr         — текст ошибки ("" если всё ok).
func recordLastPrompt(model, endpoint, prompt string, promptTokens, nCtxOverride, nCtxLoaded int, hasTools bool, status, errStr string) {
	if model == "" && prompt == "" {
		// Защита от пустых вызовов (не должны происходить, но проверим).
		return
	}

	const headMax = 512
	const tailMax = 256

	var head, tail string
	if prompt != "" {
		if len(prompt) <= headMax+tailMax {
			head = prompt
		} else {
			head = prompt[:headMax]
			tail = prompt[len(prompt)-tailMax:]
		}
	}

	// Оценка числа строк — помогает понять, длинная ли история диалога.
	linesHint := 1
	if strings.Contains(prompt, "\n") {
		linesHint = strings.Count(prompt, "\n") + 1
	}

	info := &LastPromptInfo{
		Model:          model,
		Endpoint:       endpoint,
		PromptChars:    len(prompt),
		PromptTokens:   promptTokens,
		NCtxOverride:   nCtxOverride,
		NCtxLoaded:     nCtxLoaded,
		HasTools:       hasTools,
		Status:         status,
		Timestamp:      time.Now().UTC(),
		PromptHead:     head,
		PromptTail:     tail,
		PromptLinesHint: linesHint,
		Error:          errStr,
	}

	lastPromptMu.Lock()
	lastPromptSnapshot = info
	lastPromptMu.Unlock()
}

// handleDebugLastPrompt — GET /api/v1/cppworker/debug/last-prompt.
//
// Возвращает JSON с метаданными последнего inference-запроса. Если запросов ещё
// не было — возвращает HTTP 404 с подсказкой.
//
// NB: endpoint предназначен только для диагностики. В production он не должен
// раскрывать содержимое prompt пользователя (там может быть приватная информация).
// Поэтому endpoint защищён authMiddleware (требуется X-API-Token).
func handleDebugLastPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}

	lastPromptMu.RLock()
	info := lastPromptSnapshot
	lastPromptMu.RUnlock()

	if info == nil {
		writeError(w, http.StatusNotFound, "no inference requests recorded yet")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}