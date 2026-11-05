// template_tools_r434.go — R434 (2026-11-05): инструменты запроса уходят в САМ
// CHAT TEMPLATE модели, а не только в текстовую преамбулу.
//
// ПОЧЕМУ. В c/bridge/csrc/chat_thinking.cpp вызов common_chat_templates_apply
// всегда шёл с `inputs.tools = empty`: модель НЕ видела нативную секцию
// инструментов своего шаблона. Единственным источником знаний об инструментах
// была наша текстовая преамбула (buildToolsSystemPrompt), то есть формат вызова
// задавали мы, а не обучение модели. Следствия (живые дефекты, снятые со стенда):
//   - gemma-4 копировала придуманный нами гибрид «[TOOL_CALLS] + OpenAI-объект» и
//     теряла name/arguments → Cline: COMPLETED без созданного файла;
//   - 9B писала содержимое файла в ответ вместо вызова;
//   - приходилось чинить последствия парсером (R428/R431/R432/R433).
//
// КАК ТЕПЕРЬ. Инструменты маршалятся в OpenAI-массив (тот же, что прислал
// клиент) и передаются в шаблон. llama.cpp подставляет их в jinja-переменную
// `tools`, и модель получает ровно тот синтаксис вызова, которому обучена
// (gemma — `<|tool_call>call:…`, Qwen — Hermes XML, Mistral — [TOOL_CALLS]).
//
// ПРЕАМБУЛА ОСТАЁТСЯ ТОЛЬКО FALLBACK'ом: если шаблон инструменты НЕ отрисовал
// (в нём нет секции tools / он сломан / модель грузилась со своим шаблоном),
// отдаём прежний текстовый список — иначе модель о инструментах не узнает вовсе.
// Решение принимается по факту: C-мост рендерит промпт дважды (с tools и без) и
// сообщает, изменился ли результат, — никаких догадок по именам моделей.
package main

import (
	"encoding/json"
	"os"
	"strings"

	"ollama-loadbalancer/c/bridge"
	"ollama-loadbalancer/pkg/logger"
)

// templateToolsModeR434 — куда передавать инструменты.
type templateToolsModeR434 int

const (
	// templateToolsNative — в шаблон, преамбула только если шаблон не отрисовал.
	templateToolsNative templateToolsModeR434 = iota
	// templateToolsPrompt — прежнее поведение: только текстовая преамбула.
	// Аварийный рубильник для случая, когда чей-то шаблон от инструментов портит
	// промпт (например, падает на нестандартной схеме параметров).
	templateToolsPrompt
)

// templateToolsMode — режим из CPPWORKER_TEMPLATE_TOOLS ("native" по умолчанию,
// "prompt" — прежнее поведение; неизвестное значение = native).
func templateToolsMode() templateToolsModeR434 {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_TEMPLATE_TOOLS"))) {
	case "prompt", "off", "0", "false", "no", "legacy":
		return templateToolsPrompt
	}
	return templateToolsNative
}

// tryRenderToolsPromptR434 — попытка собрать промпт с инструментами В ШАБЛОНЕ.
//
// Возвращает (prompt, true), если шаблон нарисовал инструменты. Пустой prompt и
// false — вызывающий обязан включить текстовую преамбулу (fallback).
//
// reasoningEnabled: если шаблон сам thinking не поддерживает, а размышления
// включены, добавляем soft-инструкцию и рендерим ещё раз — иначе поведение
// thinking на этом пути отличалось бы от остальных.
func tryRenderToolsPromptR434(
	modelName string,
	msgs []chatMessage,
	tools []openAITool,
	lang LangCode,
	reasoningEnabled bool,
) (string, bool) {
	if len(tools) == 0 {
		return "", false
	}
	toolsJSON, err := json.Marshal(tools)
	if err != nil {
		logger.Get().Warnw("R434: tools[] не сериализуются, остаётся текстовая преамбула",
			"model", modelName, "error", err)
		return "", false
	}
	caps := templateToolCapsForModel(modelName)

	render := func(history []bridge.ChatMessage, enableThinking bool) (string, bool, bool, int, error) {
		return safeApplyChatTemplateWithTools(
			modelName, "" /* chatTemplateOverride */, history, string(toolsJSON),
			enableThinking, true /* addGenerationPrompt */)
	}

	history := buildNativeToolHistory(msgs, caps)
	prompt, supportsThinking, rendered, toolFormat, err := render(history, reasoningEnabled)
	if err != nil && usesStructuredToolHistory(caps) {
		// Шаблон заявил структурные tool_calls, но отрендерить их отказался
		// (например, требует arguments объектом). Один повтор текстовой формой —
		// модель обязана увидеть и историю, и инструменты.
		logger.Get().Warnw("R434: structured tool history rejected by template, retrying as text",
			"model", modelName, "error", err)
		history = buildLegacyToolHistory(msgs)
		prompt, supportsThinking, rendered, toolFormat, err = render(history, reasoningEnabled)
	}
	if err != nil || prompt == "" {
		if err != nil {
			logger.Get().Debugw("R434: template render with tools failed, using text preamble",
				"model", modelName, "error", err)
		}
		return "", false
	}
	if !rendered {
		logger.Get().Debugw("R434: chat template did not render tools, using text preamble",
			"model", modelName, "tools", len(tools), "prompt_len", len(prompt))
		return "", false
	}

	// Шаблон инструменты нарисовал. Если размышления включены, а native
	// thinking шаблон не поддерживает — добавляем soft-инструкцию и рендерим
	// ещё раз (enable_thinking уже не нужен: инструкция в тексте system).
	if reasoningEnabled && !supportsThinking {
		msgs2 := upsertSystemMessage(msgs, injectThinkingInstructionWithLang(
			extractSystemFromMessages(msgs), lang))
		prompt2, _, rendered2, format2, err2 := render(buildNativeToolHistory(msgs2, caps), false)
		if err2 == nil && prompt2 != "" && rendered2 {
			logger.Get().Infow("R434: prompt rendered with tools in template + soft thinking instruction",
				"model", modelName, "tools", len(tools), "prompt_len", len(prompt2),
				"tool_format", format2)
			return prompt2, true
		}
	}

	logger.Get().Infow("R434: prompt rendered with tools in template",
		"model", modelName, "tools", len(tools), "prompt_len", len(prompt),
		"tool_format", toolFormat, "supports_thinking", supportsThinking)
	return prompt, true
}
