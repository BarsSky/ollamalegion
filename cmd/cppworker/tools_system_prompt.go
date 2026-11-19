// tools_system_prompt.go — R638 (2026-11-19): системный промпт инструментов: buildToolsSystemPrompt и подпись схемы.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"encoding/json"
	"sort"
	"strings"
)

// buildToolsSystemPrompt создаёт system prompt augmentation на основе определений инструментов.
// Этот prompt добавляется к существующему system prompt, чтобы модель знала о доступных инструментах.
//
// Особенности (Шаг 2 — фикс n_ctx overflow с tools):
//  1. Компактный формат: только имя + обрезанное описание + сигнатура параметров
//     (раньше выводился полный JSON Schema — тысячи токенов на 5-10 tools).
//  2. Кеш по fingerprint: для повторяющихся запросов OpenWebUI (типичный случай)
//     возвращается уже собранный промпт — нет затрат на Marshal/Unmarshal + strings.Builder.
//  3. Лимит на суммарный размер: toolsPromptMaxTotalChars — защита от переполнения n_ctx
//     при большом количестве tools.
//
// Возвращаемая строка стабильно детерминирована для одного и того же набора tools —
// это гарантирует попадание в кеш.
func buildToolsSystemPrompt(tools []openAITool) string {
	if len(tools) == 0 {
		return ""
	}

	// Кеш-проверка: если fingerprint совпадает и запись свежая — возвращаем как есть.
	fp := toolsPromptFingerprint(tools)
	if cached := getCachedToolsPrompt(fp); cached != "" {
		toolsPromptCacheHits.Add(1)
		return cached
	}
	toolsPromptCacheMisses.Add(1)

	// Сначала собираем в буфер без жёсткого ограничения, чтобы понять реальный размер.
	// Если превышает toolsPromptMaxTotalChars — обрезаем описания пропорционально.
	//
	// R432 (2026-11-05): НЕ НАВЯЗЫВАЕМ ОДИН ПРИДУМАННЫЙ ФОРМАТ.
	//
	// ЖИВОЙ ДЕФЕКТ (Cline в VS Code + gemma-4, скриншот оператора): преамбула R426
	// требовала «ровно одну строку»
	//
	//	[TOOL_CALLS][{"id":"call_<tool>","type":"function","function":{"name":"<tool>","arguments":{…}}}]
	//
	// — гибрид маркера Mistral и OpenAI-объекта, которого НЕТ ни в одном формате,
	// на котором модели обучены. Маленькая модель копировала скелет и теряла
	// поля. Снятый живьём сырой вывод:
	//
	//	[TOOL_CALLS][{"id":"call_read_files","type":"function","function":{"files":["TASK.md"]}}]
	//
	// `name` и `arguments` пропали, аргументы вложились прямо в `function` →
	// разбор не находил вызова, Cline показывал COMPLETED, файл не создавался
	// (R431 научил это восстанавливать, но причина — эта инструкция).
	//
	// Теперь перечислены РЕАЛЬНЫЕ формы, которые понимает наш разбор, и модель
	// вправе использовать ту, что задаёт её шаблон (gemma — `call:NAME(...)`,
	// Qwen/Hermes — XML, Mistral — [TOOL_CALLS]). Поведенческие правила R426,
	// которые заставили 9B звать инструменты вместо копирования списка,
	// сохранены.
	var sb strings.Builder
	sb.WriteString("You can call tools. Use the call format your chat template defines; these forms are accepted:\n")
	sb.WriteString(`<tool_call>{"name":"<tool>","arguments":{...}}</tool_call>` + "\n")
	sb.WriteString(`[TOOL_CALLS][{"name":"<tool>","arguments":{...}}]` + "\n")
	sb.WriteString("call:<tool>(<arg>: <value>, ...)\n")
	sb.WriteString("Rules for tool calls:\n")
	sb.WriteString("1. \"name\" must be one of the tools listed below; arguments must be a JSON object.\n")
	sb.WriteString("2. Put the call in the reply itself. Never write a call as prose like name(args) or [name(...)], and do not wrap the call in other markers.\n")
	sb.WriteString("3. When a tool performs the action (e.g. writing a file), call the tool instead of pasting the artifact or its contents into the answer.\n")
	sb.WriteString("4. If no tool is needed, answer normally in text; a text-only answer means the task is finished.\n\n")
	sb.WriteString("Available tools (signature: description):\n")

	perToolLimit := toolsPromptMaxDescriptionChars
	// Адаптивное уменьшение лимита, если tools слишком много.
	if len(tools) > 5 {
		perToolLimit = toolsPromptMaxTotalChars / len(tools)
		if perToolLimit < 80 {
			perToolLimit = 80 // жёсткий минимум, чтобы сигнатура помещалась
		}
		if perToolLimit > toolsPromptMaxDescriptionChars {
			perToolLimit = toolsPromptMaxDescriptionChars
		}
	}

	for _, tool := range tools {
		if tool.Type != "function" {
			continue
		}
		// Формат: "- name(args): desc"
		sb.WriteString("- ")
		sb.WriteString(tool.Function.Name)
		sb.WriteString("(")
		sig := toolSignature(tool.Function.Parameters)
		sb.WriteString(sig)
		sb.WriteString(")")
		desc := trimDescription(tool.Function.Description, perToolLimit)
		if desc != "" {
			sb.WriteString(": ")
			sb.WriteString(desc)
		}
		sb.WriteString("\n")
	}

	result := sb.String()

	// Пост-проверка: если превышаем жёсткий лимит — обрезаем хвост.
	if len(result) > toolsPromptMaxTotalChars {
		result = result[:toolsPromptMaxTotalChars]
		// Обрезаем по последнему переводу строки, чтобы не разрывать строку tool'а.
		if idx := strings.LastIndex(result, "\n"); idx > toolsPromptMaxTotalChars/2 {
			result = result[:idx]
		}
		result += "\n[truncated]"
	}

	putCachedToolsPrompt(fp, result)
	return result
}

// toolSignature возвращает компактную сигнатуру параметров: "q:str, limit?:int".
// required помечаются без "?", опциональные — с "?".
// Если парсинг не удался — возвращает "..." как fallback (модель попробует угадать).
func toolSignature(paramsRaw json.RawMessage) string {
	if len(paramsRaw) == 0 {
		return ""
	}
	var params map[string]interface{}
	if err := json.Unmarshal(paramsRaw, &params); err != nil {
		return "..."
	}
	props, _ := params["properties"].(map[string]interface{})
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	if req, ok := params["required"].([]interface{}); ok {
		for _, r := range req {
			if name, ok := r.(string); ok {
				required[name] = true
			}
		}
	}
	// Стабильный порядок: сортируем ключи properties.
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)

	var parts []string
	for _, name := range names {
		propMap, _ := props[name].(map[string]interface{})
		propType, _ := propMap["type"].(string)
		if propType == "" {
			// array, oneOf и т.п. — компактно представляем как "any".
			propType = "any"
		}
		// Компактные алиасы типов.
		switch propType {
		case "string":
			propType = "str"
		case "integer":
			propType = "int"
		case "number":
			propType = "num"
		case "boolean":
			propType = "bool"
		case "array":
			propType = "arr"
		case "object":
			propType = "obj"
		}
		part := name + ":" + propType
		if !required[name] {
			part += "?"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}
