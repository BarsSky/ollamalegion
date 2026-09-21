package main

import (
	"encoding/json"

	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
	"ollama-loadbalancer/pkg/types"
)

// ============================================================
// Chat types (Ollama /api/chat)
// ============================================================

type chatMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	// R65d (2026-09-20): images в сообщении (Ollama vision-запросы).
	// До R65d строгий декодер отвечал 400 `unknown field "images"` на любой
	// vision-запрос из OpenWebUI/ollama run. Текстовая часть обрабатывается
	// как обычно; сами изображения требуют mmproj в C-bridge, поэтому пока
	// принимаются и логируются (не падаем — это важнее, чем 400).
	Images []string `json:"images,omitempty"`
	// 2026-07-01: reasoning-??????? ??? reasoning-??????? (qwen3.5, deepseek-r1,
	// gemma-4 ? ?.?.). ? OpenAI-??????????? ??????? ?????????? `reasoning_content`,
	// ? Ollama ? `reasoning`. ???????????? ??? non-streaming ??????, ????? ???????
	// ????? ?????????? think-???? ? ??????? ????? ?????????.
	Reasoning string `json:"reasoning,omitempty"`
}

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream"`
	Temperature *float64      `json:"temperature,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	NumCtx      *int          `json:"num_ctx,omitempty"`
	Tools       []openAITool  `json:"tools,omitempty"`

	// ===== R65d (2026-09-20): полный Ollama-набор полей =====================
	//
	// Аудит 2026-09-20 (находка 1.1) показал: балансер транслировал /api/chat в
	// /v1/chat/completions, но Ollama-поля терялись/отвергались. После R65d
	// Ollama-пути идут в cppworker НАТИВНО (см. internal/balancer/
	// llamacpp_native_path.go), поэтому этот обработчик обязан принимать всё,
	// что реально шлют Ollama-клиенты (OpenWebUI, ollama run/python, Cline в
	// ollama-режиме), — иначе строгий декодер ниже отвечает 400
	// `unknown field "options"`.
	//
	// Options переиспользует generateOptions (types.go:11-30) — единый источник
	// истины для Ollama options.*. До R65d набор жил только в /api/generate, из-за
	// чего /api/chat молча терял top_k/repeat_penalty/seed/min_p/mirostat*.
	Options generateOptions `json:"options"`

	// KeepAlive — Ollama-семантика времени жизни модели ("5m", "0", "-1").
	// До R65d /api/chat его вообще не принимал → клиент не мог ни выгрузить
	// модель, ни продлить её жизнь.
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`

	// Format — "json", "" (свободный текст) ИЛИ объект JSON-схемы
	// (structured outputs). RawMessage, потому что Ollama допускает оба типа.
	Format json.RawMessage `json:"format,omitempty"`

	// Think — включить/выключить reasoning у thinking-моделей (qwen3, gemma-4,
	// deepseek-r1). Может быть bool или строкой ("low"/"medium"/"high").
	Think json.RawMessage `json:"think,omitempty"`

	// System/Template/Raw — Ollama-семантика переопределения промпта.
	System   string `json:"system,omitempty"`
	Template string `json:"template,omitempty"`
	Raw      bool   `json:"raw,omitempty"`

	// Truncate — обрезать историю, чтобы влезть в контекст (Ollama >= 0.1.x).
	Truncate *bool `json:"truncate,omitempty"`

	// Options для logprobs (Ollama 0.9+). Принимаем, чтобы не падать 400;
	// фактическая выдача logprobs зависит от поддержки в C-bridge.
	Logprobs    *bool `json:"logprobs,omitempty"`
	TopLogprobs *int  `json:"top_logprobs,omitempty"`

	// _keepAliveDuration — результат parseKeepAlive(req.KeepAliveRaw); не из JSON.
	_keepAliveDuration time.Duration `json:"-"`
}

type chatResponse struct {
	Model         string      `json:"model"`
	CreatedAt     string      `json:"created_at"`
	Message       chatMessage `json:"message"`
	Done          bool        `json:"done"`
	TotalDuration int64       `json:"total_duration,omitempty"`
}

// ============================================================
// Chat handler
// ============================================================

func handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "use POST")
		return
	}
	var req chatRequest
	// Round 36 Phase 2 (2026-08-17): strict JSON decoder. Rejects unknown
	// fields per the contract. Use MaxStreamingBodyBytes because chat
	// messages can be large (long history, system prompts with examples).
	if err := types.DecodeJSONRequest(r.Body, types.MaxStreamingBodyBytes, &req); err != nil {
		switch {
		case errors.Is(err, types.ErrBodyEmpty):
			writeError(w, http.StatusBadRequest, "empty request body")
		case errors.Is(err, types.ErrBodyTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		default:
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "messages array is required")
		return
	}

	// InFlight counter: ???????? ???????? ??????? ?? reload-??????.
	if req.Model != "" && backend.InFlight() != nil {
		backend.InFlight().Inc(req.Model)
		defer backend.InFlight().Dec(req.Model)
	}

	// Round 18 P0.3 (2026-08-04): per-user parallel admission (fair-share).
	// 0 = unlimited (default, backward-compat). Слот acquire ДО model load —
	// иначе 100 параллельных запросов вызовут 100 model load'ов до отказа.
	userID := getUserID(r)
	if max := maxParallelPerUser(); max > 0 {
		if !backend.UserTracker().TryAcquire(userID, max) {
			logger.Get().Warnw("handleChat: user exceeded MaxParallelPerUser",
				"user_id", userID, "max", max, "model", req.Model, "remote", r.RemoteAddr)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("user %q exceeded MaxParallelPerUser=%d (in-flight requests). Wait for current requests to complete.",
					userID, max))
			return
		}
		defer backend.UserTracker().Release(userID)
	}

	// Round 18 P0.2 (2026-08-03): per-request cancel tracking for /api/cancel.
	// ВАЖНО: используем helper setupCancelTracking, который re-bind r к child context —
	// без этого writeChatStreamResponse будет смотреть на parent (r.Context()) и не увидит
	// отмену через /api/cancel (фикс первого P0.2-бага).
	r, requestID, cancelCleanup := setupCancelTracking(r, req.Model, "cppworker-gpu", "chat")
	defer cancelCleanup()
	logger.Get().Debugw("handleChat: cancel tracking enabled",
		"request_id", requestID, "model", req.Model, "user_id", userID)

	// ??????? ???????? ??????
	actualModel, err := ensureModelLoaded(r.Context(), req.Model)
	if err != nil {
		if isModelLoadingError(err) {
			writeLoadingResponse(w, req.Model, err)
			return
		}
		writeError(w, http.StatusInternalServerError, "model load failed: "+err.Error())
		return
	}

	// Inject tool definitions into system prompt if tools are provided
	msgs := req.Messages
	if len(req.Tools) > 0 {
		msgs = augmentSystemWithTools(msgs, req.Tools)
	}

	// ???????? prompt ????? GGUF chat template (???? ????????)
	// ApplyChatTemplate ?????????? tokenizer.chat_template ?? GGUF ??????????,
	// ??? ??????????? ?????????? ??????????? ? ???????? segfault ??? ?????? ??????.
	prompt, err := buildChatPrompt(msgs, actualModel)
	if err != nil {
		logger.Get().Errorw("handleChat: failed to build prompt", "model", req.Model, "error", err)
		writeError(w, http.StatusInternalServerError, "chat prompt error: "+err.Error())
		return
	}

	logger.Get().Debugw("handleChat: assembled prompt",
		"model", req.Model,
		"messages_count", len(msgs),
		"prompt_len", len(prompt),
		"has_tools", len(req.Tools) > 0,
		"stream", req.Stream)

	genReq := buildGenerateRequestFromChat(req, prompt)

	params := buildGenerationParams(genReq)
	// Round 6 #6: прокидываем HasTools=true чтобы увеличить резерв токенов
	// под tools-prompt в n_ctx clamp (для Ollama-пути), как и в Round 4 для
	// OpenAI handlers. Без этого tools-запросы получают 1024 токен резерва →
	// n_ctx overflow → reload-loop → 413.
	ApplyCppCtxHeaderWithOptions(r, &params, CppCtxApplyOptions{
		HasTools: len(req.Tools) > 0,
	})
	// Antiprompts ??? gemma/non-gemma
	params.Antiprompts = append(params.Antiprompts, defaultAntipromptsForModel(req.Model)...)
	logger.Get().Debugw("handleChat: antiprompts",
		"model", req.Model, "count", len(params.Antiprompts),
		"antiprompts", params.Antiprompts)

	// Round 26 v0.5.13: n_ctx overflow detection + X-Model-Context-Warning.
	// Проверяем, помещается ли prompt + n_predict в n_ctx. Если нет —
	// 1) корректируем n_predict чтобы избежать overflow (Round 18 retry-loop)
	// 2) ставим X-Model-Context-Warning header чтобы клиент (OpenWebUI/Cline/Hermes)
	//    мог показать предупреждение пользователю ДО обрыва ответа.
	// Это объясняет cutoff-баг из Round 26: при длинной беседе prompt > n_ctx,
	// reload-loop после 3 попыток → 413 + abrupt stop.
	warning, adjustedNPredict := ComputeContextWarning(req.Model, prompt, params.NPredict, params.NCtxOverride)
	if warning.NPredict != params.NPredict {
		logger.Get().Warnw("handleChat: clamping n_predict to fit n_ctx",
			"model", req.Model, "old_n_predict", params.NPredict,
			"new_n_predict", warning.NPredict, "n_ctx", warning.NCtx,
			"prompt_tokens", warning.PromptTokens, "used_pct", warning.UsedPercent)
		params.NPredict = warning.NPredict
	}
	_ = adjustedNPredict
	SetContextWarningHeader(w, warning)

	// R65d (2026-09-20): применяем keep_alive, как это делает /api/generate
	// (handlers_generate.go:353-355). До R65d /api/chat не принимал keep_alive
	// вообще, поэтому клиент (OpenWebUI, ollama run) не мог ни выгрузить модель
	// ("0"), ни продлить её жизнь ("5m") — модель жила по дефолту cppworker.
	//
	// req._keepAliveDuration заполняется в buildGenerateRequestFromChat.
	// Семантика applyKeepAlive: 0 → UnloadModel, >0 → UpdateLastUsed(+dur).
	defer func() {
		applyKeepAlive(actualModel, genReq._keepAliveDuration)
	}()

	if req.Stream {
		// ??? stream=true && tools!=[] ??????? ?????? ??? ??????, ?? ???????????
		// ?????? output ???????????. ????? ?????????? ????????? ????????? tool_calls
		// ? ?????? ????????? ???? ? done:true + message.tool_calls ??? ?????????????.
		// ?????? ????? ??? bug: cppworker ????????? non-streaming JSON ????? writeJSON,
		// ??? ?????? OpenWebUI (?? ???? NDJSON ?????).
		if len(req.Tools) > 0 {
			writeChatStreamResponseWithTools(w, r, actualModel, prompt, params)
			return
		}
		writeChatStreamResponse(w, r, actualModel, prompt, params)
		return
	}

	start := time.Now()
	hasTools := len(req.Tools) > 0
	result, err := generateWithRamFallback(req.Model, prompt, params, hasTools)
	// 2026-06-25: ?????????? snapshot ?????????? inference ??? endpoint /debug/last-prompt.
	// ??? ???????? ??????????????? ?????? "prompt_exceeds_context" ? Cline/OpenWebUI.
	// Round 16 P2 fix (2026-07-30): closure form (consistent со всеми другими endpoints).
	defer func() {
		recordLastPromptFromError(req.Model, "/api/chat", prompt, &params, hasTools, err)
	}()
	if err != nil {
		// 2026-06-24: PromptExceedsNCtxError ? HTTP 413 (??. inference.go).
		if handleInferenceError(w, err) {
			return
		}
		// ??????????? ????????? reload-loop-limit (HTTP 413 ? ???????? message).
		if rllErr, ok := err.(*ReloadLoopLimitError); ok {
			writeReloadLoopLimitResponse(w, rllErr)
			return
		}
		statusCode := http.StatusInternalServerError
		if info := bridge.GetLastErrorInfo(); info.Code == bridge.ErrCodeNCtxNeedsReload || info.Code == bridge.ErrCodePromptTooLong || info.Code == bridge.ErrCodeBadRequest {
			statusCode = http.StatusBadRequest
		}
		writeCppWorkerErrorWithBridgeInfo(w, statusCode, "chat generate failed", err)
		return
	}
	duration := time.Since(start)

	// Parse tool_calls from output if tools were provided
	resp := chatResponse{
		Model:     req.Model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Message: chatMessage{
			Role:    "assistant",
			Content: result.Output,
		},
		Done:          true,
		TotalDuration: duration.Nanoseconds(),
	}
	if len(req.Tools) > 0 {
		if calls := parseToolCallsFromOutput(result.Output); len(calls) > 0 {
			resp.Message.ToolCalls = calls
			resp.Message.Content = ""
		}
	} else if IsReasoningModel(req.Model) {
		// 2026-07-01: ??? reasoning-??????? (qwen3.5/qwen3.6/deepseek-r1/gemma-4)
		// ????????? output ?? (reasoning, content) ? ?????? ? ????????? ????
		// (Ollama API ?????????? `reasoning`, OpenAI ? `reasoning_content`).
		r, c, has := SplitReasoningContent(result.Output)
		if has {
			resp.Message.Content = c
			resp.Message.Reasoning = r
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// buildGenerateRequestFromChat — R65d (2026-09-20): переносит ПОЛНЫЙ набор
// Ollama-параметров из /api/chat запроса в generateRequest, чтобы
// buildGenerationParams (handlers_generate.go:130-189) применил их так же, как
// для /api/generate.
//
// До R65d /api/chat переносил только temperature / max_tokens / num_ctx —
// top_k, repeat_penalty, seed, min_p, typical_p, tfs_z, mirostat*,
// repeat_last_n, frequency_penalty, presence_penalty, stop и keep_alive
// молча терялись (аудит 2026-09-20, находка 1.2).
//
// Порядок разрешения конфликтов:
//  1. Значения из Options.* (Ollama-канонический вложенный блок) — база.
//  2. Top-level поля запроса (temperature/max_tokens/num_ctx) переопределяют
//     Options.*, если заданы явно — так же, как normalizeGenerateRequest
//     делает для /api/generate (`req.X == nil && Options.X != 0`).
//
// Prompt передаётся уже собранным вызывающим (buildChatPrompt).
// System/Raw намеренно НЕ прокидываются: /api/chat собирает system из
// messages[] и не должен повторно префиксовать его к готовому промпту.
func buildGenerateRequestFromChat(req chatRequest, prompt string) generateRequest {
	genReq := generateRequest{
		Model:   req.Model,
		Prompt:  prompt,
		Stream:  req.Stream,
		Options: req.Options,
	}

	// Шаг 1: разворачиваем Options.* в top-level поля generateRequest.
	normalizeGenerateRequest(&genReq)

	// Шаг 2: top-level поля /api/chat имеют приоритет над Options.*
	if req.Temperature != nil {
		v := *req.Temperature
		genReq.Temperature = &v
	}
	if req.MaxTokens != nil {
		genReq.MaxTokens = *req.MaxTokens
	}
	if req.NumCtx != nil && *req.NumCtx > 0 {
		genReq.NumCtx = *req.NumCtx
	} else if genReq.NumCtx == 0 && genReq.Options.NumCtx > 0 {
		genReq.NumCtx = genReq.Options.NumCtx
	}

	// Шаг 3: keep_alive — до R65d /api/chat его не принимал вообще, поэтому
	// клиент не мог ни выгрузить модель, ни продлить её жизнь в VRAM.
	genReq.KeepAlive = keepAliveFromRaw(req.KeepAlive)
	genReq._keepAliveDuration = parseKeepAlive(genReq.KeepAlive)

	return genReq
}

// keepAliveFromRaw нормализует Ollama keep_alive к строке.
//
// Ollama допускает как строку ("5m", "0", "-1"), так и число (секунды, -1).
func keepAliveFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if f, ferr := n.Float64(); ferr == nil {
			if f < 0 {
				return "-1"
			}
			return (time.Duration(f) * time.Second).String()
		}
	}
	return ""
}

// formatWantsJSON — true если клиент запросил JSON-вывод (format:"json"
// или объект JSON-схемы). Фактическое ограничение вывода выполняет chat
// template / грамматика в C-bridge; здесь значение используется для
// диагностики и для будущего проброса в params.
func formatWantsJSON(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.EqualFold(strings.TrimSpace(s), "json")
	}
	trimmed := strings.TrimSpace(string(raw))
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

// thinkEnabledFromRaw — true/false/"low"/"medium"/"high" → (enabled, explicit).
// explicit=false означает «клиент не задавал think» (использовать конфиг).
func thinkEnabledFromRaw(raw json.RawMessage) (enabled bool, explicit bool) {
	if len(raw) == 0 {
		return false, false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "", "false", "none", "off":
			return false, true
		default:
			// "true" / "low" / "medium" / "high" — reasoning включён.
			return true, true
		}
	}
	return false, false
}

// buildChatPrompt ?????????? ApplyChatTemplate ?? GGUF (???????????????) ???
// ?????? ?????? (fallback). ?????????? GGUF chat template ???????????
// ?????????? ??????????? ? ???????? segfault ??? ?????? ??????.
func buildChatPrompt(msgs []chatMessage, modelName string) (string, error) {
	// Извлекаем system промпт (если есть)
	system := extractSystemFromMessages(msgs)

	// Round 11 (2026-07-28) SOFT PROMPT Injection для EnableReasoning:
	// Если config.EnableReasoning=true, добавляем "thinking instruction"
	// к system промпту. Это работает универсально для ЛЮБОЙ модели
	// (включая gemma-4-it, который не эмитит нативные <think> блоки).
	//
	// Модели с native thinking (Qwen3-thinking, DeepSeek-R1) игнорируют
	// soft prompt и продолжают эмитить <think> блоки — парсер
	// (cmd/cppworker/reasoning_content.go) корректно их извлекает.
	//
	// Native C-bridge enable_thinking требует common::chat миграцию
	// (см. docs/CHANGELOG.md v0.4.6 P.14) — отложено.
	//
	// Round 14b (2026-07-28): native enable_thinking через
	// common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
	// Если native path работает И template поддерживает thinking —
	// soft prompt НЕ нужен, template сам эмитит <think> блоки.
	// Иначе fallback на soft prompt.
	if currentConfig != nil && currentConfig.EnableReasoning {
		// Round 35 (2026-08-12) CRITICAL bugfix: wrap C++ native chat template
		// in recover() to catch cgo SIGSEGV. The native path calls
		// common_chat_templates_apply (c/bridge/csrc/chat_thinking.cpp) which
		// periodically SIGSEGV'ится в cgo execution (signal arrived during cgo),
		// leading to crashloop (exit code 2). Confirmed live: 5+ crashes in 1h
		// on gemma-4 with Cline 65K requests after async reload.
		//
		// SIGSEGV в cgo конвертируется в Go panic через runtime.sigpanic, и
		// этот panic CATCHABLE в том же goroutine через recover(). После catch
		// fallback на простой C API (line 320) — llama_chat_apply_template, не
		// использует C++ common::chat и стабилен.
		//
		// Trade-off: recover() в той же goroutine что и cgo — но Go runtime
		// гарантирует что panic от cgo SIGSEGV всегда recoverable. C-state
		// после SIGSEGV может быть corrupted, но следующий cgo-вызов (если он
		// будет) — уже будет проверять handles через Go-уровневые проверки.
		var nativePrompt string
		var supportsThinking bool
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Get().Errorw("buildChatPrompt: cgo SIGSEGV in ApplyChatTemplateWithThinking — recovered, falling back to C API",
						"model", modelName, "panic", fmt.Sprintf("%v", r))
					err = fmt.Errorf("native cgo panic: %v", r)
				}
			}()
			nativePrompt, supportsThinking, err = backend.ApplyChatTemplateWithThinking(
				modelName, "" /* override */, msgsToBridge(msgs),
				true /* enableThinking */, true, /* addGenerationPrompt */
			)
		}()
		if err == nil && nativePrompt != "" {
			if supportsThinking {
				logger.Get().Debugw("buildChatPrompt: used native enable_thinking path",
					"model", modelName, "prompt_len", len(nativePrompt))
				return nativePrompt, nil
			}
			// Native API works but template doesn't support enable_thinking.
			// Fallback: re-apply legacy + soft prompt in system.
			// Round 32 #12 (2026-08-11): use language-aware instruction
			// чтобы модель не отвечала на языке инструкции (mixed RU/EN).
			logger.Get().Debugw("buildChatPrompt: native enable_thinking not supported by template, using soft prompt",
				"model", modelName, "prompt_len", len(nativePrompt), "lang", detectPrimaryLanguage(msgs).String())
			system = injectThinkingInstructionWithLang(system, detectPrimaryLanguage(msgs))
			prompt2, err2 := backend.ApplyChatTemplate(modelName, system, msgsToBridge(msgs), true)
			if err2 == nil && prompt2 != "" {
				return prompt2, nil
			}
			// Both native + legacy GGUF template failed — fall through to manual assembly.
		} else if err != nil && err != bridge.ErrNoChatTemplate {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplateWithThinking failed, falling back",
				"model", modelName, "error", err)
		}
	}

	// Round 11 (2026-07-28) SOFT PROMPT PATH (для моделей без native support
	// или когда EnableReasoning=false): добавляем "thinking instruction"
	// к system промпту. Работает универсально для ЛЮБОЙ instruction-tuned
	// модели (включая gemma-4-it, который не эмитит нативные <think> блоки).
	// Round 32 #12 (2026-08-11): use language-aware version.
	if currentConfig != nil && currentConfig.EnableReasoning {
		system = injectThinkingInstructionWithLang(system, detectPrimaryLanguage(msgs))
	}

	// Применяем chat template из GGUF
	prompt, err := backend.ApplyChatTemplate(modelName, system, msgsToBridge(msgs), true)
	if err == nil && prompt != "" {
		logger.Get().Debugw("buildChatPrompt: used GGUF chat template",
			"model", modelName, "prompt_len", len(prompt))
		return prompt, nil
	}
	if err != nil && err != bridge.ErrNoChatTemplate {
		// For gemma models (gemma4 tokenizer), chat template is not supported by bridge yet.
		// Downgrade to Debug since manual prompt assembly handles it correctly.
		if isGemmaModel(modelName) {
			logger.Get().Debugw("buildChatPrompt: GGUF chat template unavailable for gemma model, using manual fallback",
				"model", modelName, "error", err)
		} else {
			logger.Get().Warnw("buildChatPrompt: ApplyChatTemplate failed, falling back to manual",
				"model", modelName, "error", err)
		}
	}

	// Fallback: ?????? ??????
	logger.Get().Debugw("buildChatPrompt: using manual prompt assembly (no GGUF chat template)",
		"model", modelName)
	// Round 11: для fallback path — НЕ модифицируем msgs (chat template
	// здесь не применяется, только ручная сборка). Чтобы thinking
	// instruction сработал, модель должна получить system промпт через
	// buildChatPromptFromMessages. Это делает prepended system message
	// в msgs (если её ещё нет). См. injectThinkingIntoMessages.
	// Round 32 #12 (2026-08-11): use language-aware version.
	if currentConfig != nil && currentConfig.EnableReasoning {
		msgs = injectThinkingIntoMessagesWithLang(msgs, detectPrimaryLanguage(msgs))
	}
	return buildChatPromptFromMessages(msgs, modelName), nil
}

// Round 11 (2026-07-28) Soft Prompt Injection — две helper-функции.
//
// enable_thinking native C-bridge support требует common::chat
// миграцию (отложено). В soft mode добавляем "thinking instruction"
// к system промпту, что работает для ЛЮБОЙ instruction-tuned модели.

// injectThinkingInstruction возвращает system промпт с prepended
// thinking instruction. Если исходный system пустой — возвращает
// только instruction. Если есть — instruction + "\n\n" + original.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in `<reasoning>...</reasoning>` tags". Без этого модели без нативного thinking
// (qwen3-instruct, gemma-4-it, custom fine-tunes типа
// Qwen3.6-35B-A3B-Uncensored-HauhauCS-Aggressive) эмитят reasoning
// КАК ОБЫЧНЫЙ ТЕКСТ — парсер SplitReasoningContent не находит теги,
// кладёт reasoning в `content` вместо `reasoning_content`.
// OpenWebUI показывает reasoning как ответ — выглядит как "неполный ответ".
//
// Round 17.1 update: live test показал, что qwen3-instruct при prompt "use
// `<think>` tags" реально использует `<reasoning>...</reasoning>` (свой
// convention). Поэтому:
//   - Soft prompt просит `<reasoning>` напрямую (более широкая совместимость)
//   - Parser (cmd/cppworker/reasoning_content.go) поддерживает ОБА варианта
//     (плюс `<thinking>`, `<analysis>`) для других custom fine-tunes
// Round 32 #12 (2026-08-11): DEPRECATED thin wrapper — use
// injectThinkingInstructionWithLang instead. This keeps backward
// compat for external callers; all internal cppworker callsites
// have been switched to the language-aware version.
func injectThinkingInstruction(system string) string {
	return injectThinkingInstructionWithLang(system, LangEN)
}

// injectThinkingIntoMessages prepended system message с thinking
// instruction к msgs. Если system message уже есть — конкатенирует.
// Используется для fallback path (buildChatPromptFromMessages) где
// system промпт собирается руками, а не через chat template.
//
// Round 17.1 fix (2026-07-31): добавлена инструкция "Wrap your reasoning
// in `<reasoning>...</reasoning>` tags" (см. injectThinkingInstruction).
// Round 32 #12 (2026-08-11): DEPRECATED thin wrapper — use
// injectThinkingIntoMessagesWithLang instead. Backward compat
// для external callers; internal callsites переведены на WithLang.
func injectThinkingIntoMessages(msgs []chatMessage) []chatMessage {
	return injectThinkingIntoMessagesWithLang(msgs, LangEN)
}

// extractSystemFromMessages ????????? system ????????? ?? ?????? ?????????.
func extractSystemFromMessages(msgs []chatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// msgsToBridge ???????????? []chatMessage ? []bridge.ChatMessage.
// ??? assistant-????????? ? ToolCalls ??????????? tool_calls ? content,
// ??? ??? bridge.ApplyChatTemplate ?? ????? ?????????? ???? ??? tool_calls.
// ??? tool-????????? content ?????????? ??? ???? (??? ???????? ????????? ??????).
func msgsToBridge(msgs []chatMessage) []bridge.ChatMessage {
	result := make([]bridge.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		// ???? assistant ????????? ???????? tool_calls ? ??????????? ?? ? content
		// Round 6 #2: wrap tool_calls as JSON array [{...},{...}] in Hermes/Qwen
		// format, which llama.cpp chat_template recognizes as structured
		// tool_call (not plain-JSON). bridge.ChatMessage has no native ToolCalls
		// field (C-bridge limitation), so we serialize into Content.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			tcJSON, err := json.Marshal(m.ToolCalls)
			if err == nil && len(tcJSON) > 0 {
				toolCallsBlock := "[" + string(tcJSON) + "]"
				if content != "" {
					content = content + "\n" + toolCallsBlock
				} else {
					content = toolCallsBlock
				}
			}
		}

		result = append(result, bridge.ChatMessage{
			Role:    m.Role,
			Content: content,
		})
	}
	return result
}

// buildChatPromptFromMessages ? ???????? prompt ?? ??????? ?????????.
// ???????????? tool-????????? ? assistant-????????? ? ToolCalls ??? function calling.
func buildChatPromptFromMessages(msgs []chatMessage, modelName string) string {
	var promptBuilder strings.Builder
	isGemma := isGemmaModel(modelName)

	for _, msg := range msgs {
		switch msg.Role {
		case "system":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>system\n")
			} else {
				promptBuilder.WriteString("<|system|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "user":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>user\n")
			} else {
				promptBuilder.WriteString("<|user|>\n")
			}
			promptBuilder.WriteString(msg.Content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "assistant":
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>model\n")
			} else {
				promptBuilder.WriteString("<|assistant|>\n")
			}
			// ???? ???? tool_calls, ??????????? ?? ? content
			// Round 6 #2: wrap tool_calls as JSON array in Hermes/Qwen format
			// (see msgsToBridge comment for rationale).
			content := msg.Content
			if len(msg.ToolCalls) > 0 {
				tcJSON, err := json.Marshal(msg.ToolCalls)
				if err == nil && len(tcJSON) > 0 {
					toolCallsBlock := "[" + string(tcJSON) + "]"
					if content != "" {
						content = content + "\n" + toolCallsBlock
					} else {
						content = toolCallsBlock
					}
				}
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		case "tool":
			// ??????????? tool-?????????: ??? ??????????? : ?????????
			content := msg.Content
			if msg.Name != "" {
				content = "tool_call_result(" + msg.Name + "): " + content
			} else if msg.ToolCallID != "" {
				content = "tool_call_result(" + msg.ToolCallID + "): " + content
			}
			if isGemma {
				promptBuilder.WriteString("<start_of_turn>tool\n")
			} else {
				promptBuilder.WriteString("<|tool|>\n")
			}
			promptBuilder.WriteString(content)
			if isGemma {
				promptBuilder.WriteString("<end_of_turn>\n")
			} else {
				promptBuilder.WriteString("<|end|>\n")
			}
		}
	}
	// ????????? ?????? ??? ?????? ??????????
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}

	return promptBuilder.String()
}

// writeChatStreamResponse ? streaming ????? ? ??????? NDJSON ??? /api/chat.
func writeChatStreamResponse(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Round 32 (2026-08-09): emit immediate "prefill" heartbeat перед inference.
	// C-bridge prefill занимает 5-30s на reasoning моделях. Без heartbeat клиент
	// не видит никаких данных в течение всего prefill ("зависший спиннер").
	// С heartbeat клиент получает валидный NDJSON {"done":false} event мгновенно —
	// connection "alive" с точки зрения EventSource, и пользователь видит
	// что запрос "пошёл". Heartbeat совместим с OpenWebUI (игнорирует строки
	// без "message" поля) и aiohttp/Cline (NDJSON парсер tolerates done:false).
	prefillStartTime := time.Now()
	prefillHB := map[string]interface{}{"done": false}
	prefillHBJSON, _ := json.Marshal(prefillHB)
	fmt.Fprintf(w, "%s\n", prefillHBJSON)
	flusher.Flush()
	_ = prefillStartTime // для future logging/debugging
	ctx := r.Context()
	// R63 (2026-09-15): per-infer AbortWatcher создаётся внутри GenerateStream
	// (inference.go:521), там где доступен abortFlag от C-bridge. Удаляем
	// старый per-model watcher (R60.58) — был race condition для
	// параллельных infers одной модели.
	var outputBuf strings.Builder
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// 2026-06-25: ?????? snapshot ????? streaming (????? ??????????? ?????????).
	var streamErr error
	defer func() {
		// Round 5 Fix 3: пробрасываем r для детекции client_disconnect.
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, false, streamErr, r)
	}()

	// 2026-07-01: reasoning-?????? (??? reasoning-???????).
	rcParser := NewReasoningStreamState()
	rcIsReasoning := IsReasoningModel(modelName)

	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)

		// 2026-07-01: ??? reasoning-??????? ????? ????? ?? (reasoning, content) ?
		// ?????? ??????????? NDJSON-????? ? message.reasoning / message.content.
		if rcIsReasoning {
			reasoningDelta, contentDelta := rcParser.Feed(token)
			// Round 32 #18 (2026-08-11): workaround для gemma-4 Q4_K_M detokenizer.
			// C-bridge detokenize'ит токены line break (`\n`) как literal `\\n`
			// (2 chars: backslash + n) вместо LF (1 char). Без этой замены
			// content приходит в JSON output как escaped `\\\\n` (4 source chars),
			// что после parsing даёт literal backslash + n (НЕ real newline).
			// В UI это видно как "весь markdown идёт сплошной линией".
			// Post-process: заменяем literal `\\n` на real `\n` ДО json.Marshal.
			// Безопасно — real newlines остаются нетронутыми (только 2-char
			// backslash+n конвертируются).
			reasoningDelta = strings.ReplaceAll(reasoningDelta, `\\n`, "\n")
			contentDelta = strings.ReplaceAll(contentDelta, `\\n`, "\n")
			if reasoningDelta != "" {
				rcChunk := map[string]interface{}{
					"model":      modelName,
					"created_at": createdAt,
					"message": map[string]interface{}{
						"role":      "assistant",
						"reasoning": reasoningDelta,
					},
					"done": false,
				}
				rcJSON, _ := json.Marshal(rcChunk)
				fmt.Fprintf(w, "%s\n", rcJSON)
				flusher.Flush()
			}
			if contentDelta != "" {
				ccChunk := map[string]interface{}{
					"model":      modelName,
					"created_at": createdAt,
					"message": map[string]string{
						"role":    "assistant",
						"content": contentDelta,
					},
					"done": false,
				}
				ccJSON, _ := json.Marshal(ccChunk)
				fmt.Fprintf(w, "%s\n", ccJSON)
				flusher.Flush()
			}
			return true
		}

		// Round 32 #18 (2026-08-11): workaround для gemma-4 Q4_K_M detokenizer.
		// Заменяем literal `\\n` на real `\n` для content чанков.
		token = strings.ReplaceAll(token, `\\n`, "\n")
		chunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message": map[string]string{
				"role":    "assistant",
				"content": token,
			},
			"done": false,
		}
		jsonData, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "%s\n", jsonData)
		flusher.Flush()
		return true
	}

	// /api/chat endpoint: tools ?? ?????????????? (Ollama-???), reload ???????? ??? n_ctx overflow.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// Round 31 #6 (2026-08-09): если streamErr — это ErrAborted (отменено через
		// bridge.RequestAbort), выдаём done_reason="cancelled" + cancelled: true
		// вместо "error" чтобы клиент мог отличить cancel от failure. DECISION Q3.
		if errors.Is(streamErr, bridge.ErrAborted) {
			cancelledChunk := map[string]interface{}{
				"model":       modelName,
				"created_at":  createdAt,
				"message":     map[string]string{"role": "assistant", "content": ""},
				"done":        true,
				"done_reason": "cancelled",
				"cancelled":   true,
			}
			cancelledJSON, _ := json.Marshal(cancelledChunk)
			fmt.Fprintf(w, "%s\n", cancelledJSON)
			flusher.Flush()
			return
		}
		// ??????????? ????????? reload-loop-limit (HTTP 413 ? NDJSON-?????).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"model":           modelName,
				"created_at":      createdAt,
				"message":         map[string]string{"role": "assistant", "content": ""},
				"done":            true,
				"done_reason":     "error",
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
				"http_status":     http.StatusRequestEntityTooLarge,
			}
			errJSON, _ := json.Marshal(errChunk)
			w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
			w.Header().Set("X-HTTP-Status", "413")
			fmt.Fprintf(w, "%s\n", errJSON)
			flusher.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"model":      modelName,
			"created_at": createdAt,
			"message":    map[string]string{"role": "assistant", "content": ""},
			"done":       true,
			"error":      streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	cleanedOutput := cleanFinalContent(fullOutput)
	// R65d (2026-09-20): реальные token counts вместо eval_count:0.
	//
	// До R65d финальный чанк нативного /api/chat отдавал eval_count=0 и
	// eval_duration == total_duration (включая prefill). Клиенты (OpenWebUI)
	// считали tokens/sec от этих полей, поэтому показывали N/A или заниженное
	// значение. Считаем так же, как OpenAI-путь cppworker
	// (writeOpenAIUsageChunk → countTokensSafe, handlers_openai.go:2058-2075),
	// чтобы метрики не расходились между нативным и OpenAI маршрутами.
	evalTokens := countTokensSafe(modelName, cleanedOutput)
	promptTokens := countTokensSafe(modelName, prompt)
	evalNs := duration.Nanoseconds()
	promptEvalNs := int64(0)
	// Если зафиксировано время первого контентного чанка — отделяем prefill (TTFT)
	// от генерации, чтобы eval_duration не включал prefill.
	if !prefillStartTime.IsZero() && duration > 0 {
		ttft := time.Since(prefillStartTime).Nanoseconds()
		if ttft >= 0 && ttft <= evalNs {
			promptEvalNs = ttft
		}
	}
	doneChunk := map[string]interface{}{
		"model":                modelName,
		"created_at":           createdAt,
		"message":              map[string]string{"role": "assistant", "content": cleanedOutput},
		"done":                 true,
		"done_reason":          "stop",
		"total_duration":       evalNs,
		"prompt_eval_count":    promptTokens,
		"prompt_eval_duration": promptEvalNs,
		"eval_count":           evalTokens,
		"eval_duration":        evalNs - promptEvalNs,
	}
	doneJSON, _ := json.Marshal(doneChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}

// writeChatStreamResponseWithTools ? streaming-????? ? ?????????? tool calling.
//
// ? ??????? ?? writeChatStreamResponse, ??? ???????:
//  1. ??????? ?????? ??? ?????? ????? generateStreamWithRamFallback (real-time feedback).
//  2. ??????????? ??????????? ?????? output ? ?????.
//  3. ????? ?????????? ????????? ????????? tool_calls ????? parseToolCallsFromOutput.
//  4. ???? ???? tool_calls ? ??????? content ? ?????? ????????? ???? ? done:true,
//     done_reason:"tool_calls" ? message.tool_calls.
//  5. ???? ??? ? ?????? ??????? done:true ????.
//
// ??? ?????? ???, ??? ??????? OpenWebUI (stream=true + tools) ??????? non-streaming
// JSON-????? ? ?? ??? ????????? ??? ?????????? (????? "???????? ???????????" ??? ??????).
func writeChatStreamResponseWithTools(w http.ResponseWriter, r *http.Request, modelName, prompt string, params bridge.GenerationParams) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	// 2026-06-24: preflight n_ctx check BEFORE flushing headers.
	if err := clampNPredictToFitContext(modelName, prompt, &params); err != nil {
		var perr *PromptExceedsNCtxError
		if errors.As(err, &perr) {
			writePromptExceedsNCtxResponse(w, perr)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Round 6 Fix 5: do NOT flush headers immediately — buffer the response.
	// Previously every prose token was sent on the wire and then the final
	// chunk tried to set content="" with tool_calls — clients got garbage
	// (raw JSON in chat). New approach: callback ONLY writes to outputBuf,
	// no flushes. After inference, parse tool_calls and emit one final
	// chunk (either tool_calls or content). Heartbeat goroutine keeps
	// the connection alive during long generations.
	flusher.Flush()
	ctx := r.Context()
	// R63 (2026-09-15): per-infer AbortWatcher создаётся внутри GenerateStream
	// (inference.go:521), там где доступен abortFlag от C-bridge.
	start := time.Now()
	createdAt := time.Now().UTC().Format(time.RFC3339)

	// Buffer full output for final tool_calls detection.
	var outputBuf strings.Builder

	// Round 6 Fix 5: heartbeat goroutine prevents idle-timeout during
	// long generations.
	//
	// Round 27 (v0.5.14 follow-up): /api/chat (Ollama native) is NDJSON,
	// not SSE. SSE comment ": keepalive\n\n" was being parsed as JSON
	// by Cline and other strict NDJSON clients, causing "invalid json"
	// spam. Use a valid NDJSON object `{"keepalive":true}` instead —
	// NDJSON parsers parse it as a no-content chunk and continue.
	// Also bumped interval from 100ms to 15s default (same as OpenAI
	// SSE handler) — 100ms × N seconds of generation = thousands of
	// useless chunks. 15s × N still keeps TCP alive (typical proxy
	// idle-timeout is 30-60s).
	keepaliveInterval := getHeartbeatInterval(15 * time.Second)
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(keepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatStop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := fmt.Fprintf(w, "{\"keepalive\":true}\n"); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	}()
	defer func() {
		close(heartbeatStop)
		select {
		case <-heartbeatDone:
		case <-time.After(50 * time.Millisecond):
		}
	}()

	// Snapshot for chat streaming (debug endpoint).
	var streamErr error
	defer func() {
		recordLastPromptFromErrorWithContext(modelName, "/api/chat", prompt, &params, true, streamErr, r)
	}()

	// Round 6 Fix 5: callback ONLY buffers. Prose is NEVER sent to the wire
	// because it might be part of a tool_call (Gemma-4 / Qwen3 stream prose
	// before tool_call). Without buffering, clients see raw JSON in chat.
	callback := func(token string) bool {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		outputBuf.WriteString(token)
		return true
	}

	// /api/chat streaming with tools: full buffer, single final response.
	streamErr = generateStreamWithRamFallback(ctx, modelName, prompt, params, callback, false)

	// ???? ?? ????? streaming ????????? ?????? ? ?????? ????????? chunk ? error,
	// ??? ? writeChatStreamResponse. Tool calls ? ???? ?????? ?? ???????????.
	if streamErr != nil {
		maybeRestartOnMemorySlotError(streamErr, modelName)
		// ??????????? ????????? reload-loop-limit: HTTP-?????? 413 ? NDJSON-?????
		// ? error+code+model+attempts. OpenWebUI ?????? error ? ???????? ??????
		// ? ???????? ?????????? (?????? ???????????? ????? unload+reload).
		if rllErr, ok := streamErr.(*ReloadLoopLimitError); ok {
			errChunk := map[string]interface{}{
				"model":           modelName,
				"created_at":      createdAt,
				"message":         map[string]string{"role": "assistant", "content": ""},
				"done":            true,
				"done_reason":     "error",
				"error":           rllErr.Error(),
				"code":            "reload_loop_limit",
				"attempts":        rllErr.Count,
				"elapsed_seconds": rllErr.Elapsed.Seconds(),
				"http_status":     http.StatusRequestEntityTooLarge,
			}
			errJSON, _ := json.Marshal(errChunk)
			// ????? ?????? HTTP-?????? ? response (best-effort: ???? streaming ??? ????? ? http.status ????? ?????????????? ????????, ?? ??? ? NDJSON ??? ????? ????? ???????).
			w.Header().Set("X-CppWorker-Error", "reload_loop_limit")
			w.Header().Set("X-HTTP-Status", "413")
			fmt.Fprintf(w, "%s\n", errJSON)
			flusher.Flush()
			return
		}
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       streamErr.Error(),
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	fullOutput := outputBuf.String()
	duration := time.Since(start)

	// ???????? ?????????? tool_calls ?? ?????????? ??????.
	toolCalls := parseToolCallsFromOutput(fullOutput)

	// 2026-06-24: ???????? ?? ?????? ????? ????? cleanFinalContent (gemma antiprompt).
	if len(toolCalls) == 0 && strings.TrimSpace(cleanFinalContent(fullOutput)) == "" {
		errChunk := map[string]interface{}{
			"model":       modelName,
			"created_at":  createdAt,
			"message":     map[string]string{"role": "assistant", "content": ""},
			"done":        true,
			"done_reason": "error",
			"error":       "model produced an empty response (inference succeeded but output is empty). This may indicate n_ctx too small, prompt too long, or model issue.",
		}
		errJSON, _ := json.Marshal(errChunk)
		fmt.Fprintf(w, "%s\n", errJSON)
		flusher.Flush()
		return
	}

	// R65d (2026-09-20): реальные token counts вместо eval_count:0
	// (та же причина, что и в writeChatStreamResponse — OpenWebUI считает
	// tokens/sec из этих полей и показывал N/A).
	fullOutputCount := countTokensSafe(modelName, fullOutput)
	promptTokensTools := countTokensSafe(modelName, prompt)
	finalChunk := map[string]interface{}{
		"model":             modelName,
		"created_at":        createdAt,
		"message":           map[string]interface{}{"role": "assistant"},
		"done":              true,
		"total_duration":    duration.Nanoseconds(),
		"prompt_eval_count": promptTokensTools,
		"eval_count":        fullOutputCount,
		"eval_duration":     duration.Nanoseconds(),
	}

	if len(toolCalls) > 0 {
		// Tool calls ?????????? ? ?????? message.tool_calls ? ??????? content.
		// ??? ????????: OpenWebUI ??? `done_reason: "tool_calls"` ???????
		// ??? content ??????, ? tool_calls ???????? ?????? ???????.
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = ""
		msg["tool_calls"] = toolCalls
		finalChunk["done_reason"] = "tool_calls"
		logger.Get().Infow("writeChatStreamResponseWithTools: detected tool_calls in streamed output",
			"model", modelName, "tool_calls_count", len(toolCalls))
	} else {
		// ??? tool calls ? ??????? ????????? ?????. ????????: ??????????
		// ?????? output ? ????????? ?????, ????? OpenWebUI ????? ??????
		// content ? ????????? ?????? ("???? ???????? ??????, ?????? ???").
		//
		// ?????? ????? ??? bug: ????????? ???? ???? content: "", ????
		// ????? ??? ??? ????????? ? streaming chunks. OpenWebUI ?????
		// done:true ????? content ?? ?????????? ????? (?? ??????????
		// ?? streaming chunks), ??????? ????? ?????? ?????.
		//
		// ?????? ??????????? ???????: ??????? ????????? ????????? ??????,
		// ??????? ?????? ????? ??????? (</tool_call>, [TOOL_CALLS], ? ?.?.).
		msg := finalChunk["message"].(map[string]interface{})
		msg["content"] = cleanFinalContent(fullOutput)
		finalChunk["done_reason"] = "stop"
		logger.Get().Debugw("writeChatStreamResponseWithTools: text response (no tool_calls)",
			"model", modelName, "content_len", len(msg["content"].(string)))
	}

	doneJSON, _ := json.Marshal(finalChunk)
	fmt.Fprintf(w, "%s\n", doneJSON)
	flusher.Flush()
}
