// handlers_chat_prompt.go — R625 (2026-11-19): сборка промпта и параметров из чата.
//
// ЗАЧЕМ ВЫНЕСЕНО. `handlers_chat.go` держал 3529 строк: в нём и HTTP-поверхность чата,
// и стрим-писатели, и — отдельным слоем — сборка запроса для моста (промпт из
// сообщений, thinking-инструкции, извлечение system, keep-alive, JSON-схема).
// Этот слой не зависит от `http.ResponseWriter`, поэтому вынесен первым: он
// самодостаточен и его легко читать отдельно от стриминга.
//
// ПОВЕДЕНИЕ НЕ МЕНЯЛОСЬ: код перенесён как есть; тесты `cmd/cppworker` (наборы про
// tools-совместимость, thinking и prompt-сборку) зелёные до и после.
package main

import (
	"encoding/json"
	"strings"
	"time"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/internal/i18n"
	"ollama-loadbalancer/pkg/logger"
)
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
	// R69/R83-v67 (2026-10-02): max_output_tokens (Cline) — ВЕРХНЯЯ ГРАНИЦА, а не
	// длина ответа. Раньше значение подставлялось в genReq.MaxTokens, то есть
	// «клиент примет до 64000» превращалось в «сгенерируй до 64000» (десятки
	// минут на слабой карте, обрыв по клиентскому таймауту). Теперь граница
	// только уменьшает финальный num_predict.
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		genReq._upperBoundNumPredict = *req.MaxOutputTokens
		logger.Get().Infow(i18n.T("cppworker.handlers_chat.max_output_tokens_upper_bound"),
			"model", req.Model, "max_output_tokens", *req.MaxOutputTokens)
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
	// R66b: `"think": null` — клиент поле не задавал (а не «выключить»).
	if len(raw) == 0 || strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
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

// jsonSchemaText — текст JSON-схемы из format-поля ("json" → "" ; объект/массив
// → его компактный JSON). Используется только для инструкции в system-промпте.
func jsonSchemaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed
	}
	return ""
}

// buildChatPrompt — прежняя сигнатура (используется OpenAI-путём и тестами):
// без per-request переопределений.
func buildChatPrompt(msgs []chatMessage, modelName string) (string, error) {
	return buildChatPromptWithOptions(msgs, modelName, chatPromptOptions{})
}

// buildChatPromptWithOptions — сборка промпта с per-request опциями (R66b):
// think (reasoning on/off для одного запроса) и format:"json".
func buildChatPromptWithOptions(msgs []chatMessage, modelName string, opts chatPromptOptions) (string, error) {
	// Извлекаем system промпт (если есть)
	system := extractSystemFromMessages(msgs)
	lang := detectPrimaryLanguage(msgs)

	// R66b: эффективный режим reasoning = per-request think (если клиент задал)
	// поверх глобального config.EnableReasoning.
	//
	// R83/v67 (2026-10-02): fallback — НЕ только глобальный config.
	//
	// ЖИВОЙ ДЕФЕКТ (жалоба оператора: «флаг включить размышление был активен,
	// однако при ответе размышление не применилось»). Галочка «размышления» в
	// WebUI пишется в профиль модели (enableReasoning=true) и честно доезжает до
	// загрузки — /api/models показывает reasoning_enabled=true. Но сборка промпта
	// смотрела ТОЛЬКО на глобальный config.EnableReasoning (по умолчанию false) и
	// per-model состояние (inst.reasoningEnabled, на которое влияет галочка) не
	// читала вовсе: ни native enable_thinking, ни soft-инструкция не применялись.
	// Проверено на стенде через /api/v1/cppworker/debug/last-prompt: промпт
	// приходил без thinking-инструкции вообще.
	//
	// Порядок приоритета: явный think клиента > per-model (WebUI/профиль) >
	// глобальный config.EnableReasoning.
	configReasoning := PerModelReasoningEnabled(modelName)
	if currentConfig != nil && currentConfig.EnableReasoning {
		configReasoning = true
	}
	reasoningEnabled := effectiveReasoningEnabled(opts, configReasoning)

	// R66b: format:"json"/схема → инструкция структурированного вывода.
	// Грамматику C-bridge не поддерживает, поэтому ограничиваем промптом, а
	// markdown-обёртку снимаем пост-обработкой (stripJSONFence).
	if opts.JSONMode {
		system = injectJSONInstruction(system, lang, opts.JSONSchema)
	}

	// R66b (2026-09-22): инструкции (JSON/thinking) должны попасть и в
	// Jinja-шаблон. ApplyChatTemplateWithThinking НЕ принимает system отдельным
	// аргументом — он берёт его ТОЛЬКО из messages. До этого фикса format:"json"
	// не доходил до модели на native-thinking пути: system собирался, но в
	// messages не попадал (проверено через /api/v1/cppworker/debug/last-prompt —
	// prompt_head содержал только user-сообщение без system-блока).
	//
	// Legacy C-API путь (ApplyChatTemplate) роль `system` в messages игнорирует
	// и использует одноимённый аргумент, поэтому дублирования нет.
	msgs = upsertSystemMessage(msgs, system)

	// thinkingInjected защищает от повторной вставки soft-prompt: native-путь
	// может поддержать thinking сам, и тогда инструкция не нужна.
	thinkingInjected := false
	injectThinking := func() {
		if thinkingInjected {
			return
		}
		system = injectThinkingInstructionWithLang(system, lang)
		msgs = upsertSystemMessage(msgs, system)
		thinkingInjected = true
	}

	// R434 (2026-11-05): ИНСТРУМЕНТЫ — В ШАБЛОН, ПРЕАМБУЛА — ТОЛЬКО FALLBACK.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline в VS Code + gemma-4): преамбула задавала единственный
	// формат вызова, придуманный нами, модель его копировала с потерей полей, и
	// ход закрывался как «COMPLETED» без созданного файла. Причина — инструменты
	// вообще не попадали в chat template (C-мост звал
	// common_chat_templates_apply с пустым inputs.tools), поэтому нативный
	// синтаксис модели молчал, а формат задавали мы.
	//
	// Теперь: сначала пробуем отрисовать tools[] самим шаблоном. Шаблон нарисовал
	// (toolsRendered) — преамбулы нет вообще. Не нарисовал (нет секции tools в
	// шаблоне/шаблон сломан/рубильник CPPWORKER_TEMPLATE_TOOLS=prompt) — прежний
	// текстовый список как fallback.
	// R445 (2026-11-05): если в истории есть ОТКАЗЫ клиента (файл существует без
	// old_text, new_text больше лимита), добавляем точную инструкцию в system.
	// Текст ошибки модель видит и так (role:"tool"), но слабые модели его не
	// применяют: без подсказки цикл повторяется до остановки задачей.
	if hint := clientErrorHintsR445(msgs); hint != "" {
		system = strings.TrimSpace(system + "\n\n" + hint)
		msgs = upsertSystemMessage(msgs, system)
		logger.Get().Infow("client error hints injected (R445)",
			"model", modelName, "hint_len", len(hint))
	}

	// R455/R456/R457 (2026-11-06): три класса, на которых модель теряет
	// инструменты и уходит в текст:
	//   - последние команды падали с ненулевым кодом (R455);
	//   - один и тот же вызов повторён с теми же аргументами (R456);
	//   - предыдущий ответ выдал содержимое файла текстом вместо вызова (R457).
	// Инструкция добавляется в system, данные клиента не трогаем.
	if hint := clientToolGuidanceHint(msgs); hint != "" {
		system = strings.TrimSpace(system + "\n\n" + hint)
		msgs = upsertSystemMessage(msgs, system)
		logger.Get().Infow("client tool guidance injected (R455/R456/R457)",
			"model", modelName, "hint_len", len(hint),
			"command_failed", commandFailureHintR455(msgs) != "",
			"repeated_call", repeatedToolCallHintR456(msgs) != "",
			"file_body_answer", fileBodyAnswerHintR457(msgs) != "")
	}

	if len(opts.Tools) > 0 {
		if mode := templateToolsMode(); mode == templateToolsNative {
			if prompt, ok := tryRenderToolsPromptR434(modelName, msgs, opts.Tools, lang, reasoningEnabled); ok {
				return prompt, nil
			}
		}
		msgs = augmentSystemWithToolsForChoice(msgs, opts.Tools, opts.ToolChoice)
		system = extractSystemFromMessages(msgs)
	}

	// Round 11 (2026-07-28) SOFT PROMPT Injection для EnableReasoning:
	// Если config.EnableReasoning=true, добавляем "thinking instruction"
	// к system промпту. Это работает универсально для ЛЮБОЙ модели
	// (включая gemma-4-it, который не эмитит нативные <think> блоки).
	//
	// Модели с native thinking (Qwen3-thinking, DeepSeek-R1) игнорируют
	// soft prompt и продолжают эмитить <think> блоки — парсер
	// (cmd/cppworker/reasoning_content.go) корректно их извлекает.
	//
	// Round 14b (2026-07-28): native enable_thinking через
	// common_chat_templates_apply (см. c/bridge/csrc/chat_thinking.cpp).
	// Если native path работает И template поддерживает thinking —
	// soft prompt НЕ нужен, template сам эмитит <think> блоки.
	// Иначе fallback на soft prompt.
	// R475 (2026-11-07): возможности шаблона нужны не только native-пути — от них
	// зависит и ФОРМА вызова в текстовой истории (`<function=…>` вместо `[[…]]`).
	caps := templateToolCapsForModel(modelName)

	if reasoningEnabled {
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
		// R91 (2026-10-08): recover() переехал внутрь
		// safeApplyChatTemplateWithThinking — там же, где помечается «сломанная»
		// модель, чтобы ВСЕ точки входа в C имели одну защиту (см.
		// chat_template_guard.go).
		//
		// R246 (2026-10-26): форма tool-истории выбирается по шаблону модели
		// (см. tool_history.go). Структурные вызовы + role="tool" с
		// tool_call_id — единственный вид, из которого шаблон gemma-4 рисует и
		// вызов, и результат инструмента; на паре role+content результат
		// терялся целиком и модель просила «подождать вывод».
		nativePrompt, supportsThinking, err = safeApplyChatTemplateWithThinking(
			modelName, "" /* chatTemplateOverride */, buildNativeToolHistory(msgs, caps),
			true /* enableThinking */, true, /* addGenerationPrompt */
		)
		if err != nil && err != bridge.ErrNoChatTemplate && usesStructuredToolHistory(caps) {
			// Шаблон заявил структурные tool_calls, но отрендерить их отказался
			// (например, требует arguments объектом, как Qwen3.8). Один повтор
			// текстовой формой — модель обязана увидеть результат инструмента.
			logger.Get().Warnw("buildChatPrompt: structured tool history rejected by template, retrying as text",
				"model", modelName, "error", err)
			nativePrompt, supportsThinking, err = safeApplyChatTemplateWithThinking(
				modelName, "", buildLegacyToolHistory(msgs, caps), true, true,
			)
		}
		if err == nil && nativePrompt != "" {
			if supportsThinking {
				logger.Get().Debugw("buildChatPrompt: used native enable_thinking path",
					"model", modelName, "prompt_len", len(nativePrompt), "tool_caps", caps)
				return nativePrompt, nil
			}
			// Native API works but template doesn't support enable_thinking.
			// Fallback: re-apply legacy + soft prompt in system.
			// Round 32 #12 (2026-08-11): use language-aware instruction
			// чтобы модель не отвечала на языке инструкции (mixed RU/EN).
			logger.Get().Debugw("buildChatPrompt: native enable_thinking not supported by template, using soft prompt",
				"model", modelName, "prompt_len", len(nativePrompt), "lang", lang.String())
			injectThinking()
			prompt2, err2 := safeApplyChatTemplate(modelName, system, buildLegacyToolHistory(msgs, caps), true)
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
	if reasoningEnabled {
		injectThinking()
	}

	// Применяем chat template из GGUF.
	//
	// R91 (2026-10-08): через safeApplyChatTemplate, а НЕ напрямую. Это тот самый
	// вызов, на котором cppworker падал с SIGSEGV (bridge_apply_chat_template →
	// SIGSEGV) и терял все запросы узла: recover() тогда стоял только вокруг
	// native-пути выше, а этот остался без защиты.
	prompt, err := safeApplyChatTemplate(modelName, system, buildLegacyToolHistory(msgs, caps), true)
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

	// Fallback: ручная сборка промпта.
	logger.Get().Debugw("buildChatPrompt: using manual prompt assembly (no GGUF chat template)",
		"model", modelName)
	// R66b: system-сообщение (с JSON/thinking инструкциями) уже вставлено в msgs
	// выше через upsertSystemMessage — дополнительная инъекция не нужна и
	// привела бы к дублированию инструкции в промпте.
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
//
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

// extractSystemFromMessages извлекает system сообщение из списка сообщений.
func extractSystemFromMessages(msgs []chatMessage) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// msgsToBridge конвертирует []chatMessage в []bridge.ChatMessage.
// Для assistant-сообщений с ToolCalls сериализует tool_calls в content,
// так как bridge.ApplyChatTemplate не имеет отдельного поля для tool_calls.
// Для tool-сообщений content передаётся как есть (уже содержит результат вызова).
func msgsToBridge(msgs []chatMessage) []bridge.ChatMessage {
	result := make([]bridge.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		content := m.Content
		// Если assistant сообщение содержит tool_calls — сериализуем их в content
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

// buildChatPromptFromMessages — собирает prompt из массива сообщений.
// Поддерживает tool-сообщения и assistant-сообщения с ToolCalls для function calling.
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
			// Если есть tool_calls, сериализуем их в content
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
			// Форматируем tool-сообщение: имя инструмента : результат
			content := msg.Content
			// R66b: Ollama-клиенты кладут имя инструмента в `tool_name`,
			// OpenAI-совместимые — в `name`; поддерживаем оба.
			if toolName := msg.effectiveToolName(); toolName != "" {
				content = "tool_call_result(" + toolName + "): " + content
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
	// Добавляем маркер для ответа ассистента
	if isGemma {
		promptBuilder.WriteString("<start_of_turn>model\n")
	} else {
		promptBuilder.WriteString("<|assistant|>\n")
	}

	return promptBuilder.String()
}

// writeChatStreamResponse — streaming ответ в формате NDJSON для /api/chat.
//
// thinkOverride — R108 (2026-10-13): явный think клиента (nil = не задавал).
// Разбор ответа теперь БЕЗУСЛОВНЫЙ и тего-зависимый (см. блок «Политика отдачи
// reasoning» в reasoning_content.go), поэтому имя модели здесь не участвует:
// при think=false размышления не отдаются вовсе (ни в content, ни в отдельном
// поле) — та же семантика, что у non-stream /api/chat.