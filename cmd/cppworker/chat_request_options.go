// chat_request_options.go — R66b (2026-09-22): per-request обработка полей
// Ollama /api/chat, которые раньше принимались, но ни на что не влияли:
//
//   - think   ("true"/"false"/"low"/"medium"/"high") — управление reasoning
//     для КОНКРЕТНОГО запроса (раньше смотрели только глобальный
//     config.EnableReasoning → клиент не мог ни включить, ни выключить
//     thinking на один запрос);
//   - format  ("json" | JSON-схема) — структурированный вывод (раньше
//     принимался и молча игнорировался: модель могла вернуть markdown-обёртку
//     ```json ... ``` или обычный текст).
//
// ОГРАНИЧЕНИЕ (честно): C-bridge не экспонирует ни JSON-грамматику, ни
// json_schema-constrained sampling (см. c/bridge/bridge.h — доступен только
// SampleToken(logits, temperature, seed)). Поэтому format реализован на уровне
// инструкции в system-промпте + пост-обработки (снятие markdown-обёртки), а не
// грамматикой. Это заметно надёжнее, чем полное игнорирование поля, и не
// ломает модели без JSON-режима.
package main

import (
	"strings"
)

// chatPromptOptions — per-request переопределения сборки промпта.
type chatPromptOptions struct {
	// ReasoningOverride — результат thinkEnabledFromRaw(req.Think).
	// nil  → использовать глобальный config.EnableReasoning (прежнее поведение).
	// &true/&false → явное требование клиента для ЭТОГО запроса.
	ReasoningOverride *bool

	// JSONMode — клиент запросил format:"json" или JSON-схему.
	JSONMode bool
	// JSONSchema — исходный текст схемы (если format был объектом).
	JSONSchema string
}

// upsertSystemMessage — R66b: записывает итоговый system-промпт (с JSON/
// reasoning-инструкциями) в messages.
//
// Зачем: Jinja-путь (Backend.ApplyChatTemplateWithThinking →
// common_chat_templates_apply) НЕ принимает system отдельным аргументом и
// читает его ТОЛЬКО из messages. Без этой записи инструкции не доходили до
// модели на native-thinking пути (проверено на живом стенде:
// /api/v1/cppworker/debug/last-prompt показывал prompt_head без system-блока).
//
// Legacy C-API путь (bridge ApplyChatTemplate) роль "system" в messages
// пропускает и использует одноимённый аргумент — дублирования нет.
func upsertSystemMessage(msgs []chatMessage, system string) []chatMessage {
	if strings.TrimSpace(system) == "" {
		return msgs
	}
	out := make([]chatMessage, len(msgs))
	copy(out, msgs)
	for i := range out {
		if out[i].Role == "system" {
			out[i].Content = system
			return out
		}
	}
	return append([]chatMessage{{Role: "system", Content: system}}, out...)
}

// effectiveReasoningEnabled — итоговый режим reasoning для запроса:
// явный think от клиента (ReasoningOverride) поверх глобального
// config.EnableReasoning. R66b: до этого поле think на /api/chat
// принималось, но ни на что не влияло.
func effectiveReasoningEnabled(opts chatPromptOptions, configEnabled bool) bool {
	if opts.ReasoningOverride != nil {
		return *opts.ReasoningOverride
	}
	return configEnabled
}

// jsonInstructionFor — инструкция структурированного вывода на языке диалога.
func jsonInstructionFor(lang LangCode, schema string) string {
	var sb strings.Builder
	switch lang {
	case LangRU:
		sb.WriteString("Отвечай ТОЛЬКО валидным JSON и ничем больше. ")
		sb.WriteString("Не добавляй пояснений, приветствий и markdown-обёрток (```json). ")
	default:
		sb.WriteString("Respond with VALID JSON only and nothing else. ")
		sb.WriteString("Do not add explanations, greetings or markdown fences (```json). ")
	}
	if strings.TrimSpace(schema) != "" {
		switch lang {
		case LangRU:
			sb.WriteString("Ответ должен соответствовать этой JSON-схеме: ")
		default:
			sb.WriteString("The response must match this JSON schema: ")
		}
		sb.WriteString(strings.TrimSpace(schema))
		sb.WriteString(" ")
	}
	switch lang {
	case LangRU:
		sb.WriteString("Первым символом ответа должна быть { или [.")
	default:
		sb.WriteString("The first character of the response must be { or [.")
	}
	return sb.String()
}

// injectJSONInstruction — добавляет инструкцию JSON-вывода в system-промпт.
func injectJSONInstruction(system string, lang LangCode, schema string) string {
	instruction := jsonInstructionFor(lang, schema)
	if strings.TrimSpace(system) == "" {
		return instruction
	}
	return instruction + "\n\n" + system
}

// stripJSONFence снимает markdown-обёртку вокруг JSON-ответа:
//
//	```json
//	{"a":1}
//	```
//	→ {"a":1}
//
// Работает и с незакрытым блоком (модель часто не дописывает закрывающие
// бэктики) и с префиксом вида "Вот JSON: ```json ...". Если после снятия
// обёртки текст пуст — возвращает исходный (лучше отдать как есть, чем пустоту).
func stripJSONFence(s string) string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return s
	}

	// Ищем открывающий fence в начале текста или сразу после короткого
	// пояснения ("Вот результат: ```json").
	openIdx := strings.Index(trimmed, "```")
	if openIdx >= 0 {
		// Пояснение перед fence допускаем только короткое — иначе это не
		// JSON-обёртка, а markdown-ответ с кодом внутри.
		prefix := strings.TrimSpace(trimmed[:openIdx])
		if len(prefix) <= 32 {
			rest := trimmed[openIdx+3:]
			// Срезаем тег языка после открывающего fence (json / JSON / пусто).
			// Два варианта: многострочный ("```json\n{...}") и однострочный
			// ("```json {...}```").
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				langTag := strings.TrimSpace(rest[:nl])
				if langTag == "" || strings.EqualFold(langTag, "json") {
					rest = rest[nl+1:]
				}
			} else {
				oneLine := strings.TrimLeft(rest, " \t")
				if strings.HasPrefix(strings.ToLower(oneLine), "json") {
					if after := strings.TrimLeft(oneLine[len("json"):], " \t"); after != "" {
						rest = after
					}
				}
			}
			// Срезаем закрывающий fence.
			if closeIdx := strings.LastIndex(rest, "```"); closeIdx >= 0 {
				rest = rest[:closeIdx]
			}
			rest = strings.TrimSpace(rest)
			if rest != "" {
				return rest
			}
		}
	}

	// Незакрытый fence без перевода строки: "```json {...}" одной строкой.
	if strings.HasPrefix(strings.ToLower(trimmed), "```json") {
		body := strings.TrimSpace(trimmed[len("```json"):])
		body = strings.TrimSpace(strings.TrimSuffix(body, "```"))
		if body != "" {
			return body
		}
	}
	return s
}
