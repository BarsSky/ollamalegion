package main

// declared_tool_filter_r428.go — R428 (2026-11-04): вызов инструмента, которого
// КЛИЕНТ НЕ ОБЪЯВЛЯЛ, не уезжает как tool_calls.
//
// ЖИВОЙ ДЕФЕКТ (реплей сессии Cline на Qwen3.8-9B, 25 инструментов). Модель
// позвала `write_file(path=…, text=…)` — инструмента с таким именем у Cline НЕТ
// (объявлен `editor(path, new_text)`). cppworker отдавал вызов как есть, и клиент
// получал tool_call на несуществующий инструмент: выполнить его нельзя в
// принципе, Cline сообщает об ошибке инструмента, ход тратится впустую.
//
// РЕШЕНИЕ ДВУХСТУПЕНЧАТОЕ:
//  1. известные СИНОНИМЫ записи файла (`write_file`, `create_file`, `write`, …)
//     отображаются на объявленный инструмент записи (обычно `editor`), аргументы
//     переименовываются под его схему (`text`/`content`/`data` → `new_text`,
//     `file`/`filename` → `path`). Это оживляет вызывающий код, который модель
//     усвоила под другими именами;
//  2. всё остальное с именем не из `tools[]` отбрасывается (с WARN в лог) —
//     лучше текст ответа, чем заведомо невыполнимый вызов.
//
// ГРАНИЦЫ: фильтр работает только когда клиент передал непустой `tools[]`
// (tools-free чат не меняется), и alias применяется только к известным ролям
// записи файла — произвольные имена не угадываются.

import (
	"encoding/json"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// writerToolAliases — имена, которыми модели зовут «записать/создать файл».
var writerToolAliases = map[string]bool{
	"write_file": true, "writefile": true, "create_file": true, "createfile": true,
	"file_write": true, "writefiletool": true, "save_file": true, "savefile": true,
	"write": true, "create": true,
}

// pathArgKeys / contentArgKeys — как модели называют аргументы записи файла.
var (
	pathArgKeys    = []string{"path", "file", "filename", "file_path", "filepath", "target"}
	contentArgKeys = []string{"new_text", "text", "content", "contents", "data", "body"}
)

// filterUndeclaredToolCalls — оставить только вызовы объявленных инструментов
// (с преобразованием известных синонимов).
func filterUndeclaredToolCalls(calls []openAIToolCall, tools []openAITool) []openAIToolCall {
	if len(calls) == 0 {
		return calls
	}
	// Без tools[] фильтровать нечего: клиент не объявлял инструменты, значит и
	// сверять не с чем (tools-free чат).
	if len(tools) == 0 {
		return calls
	}
	declared := make(map[string]openAITool, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			declared[t.Function.Name] = t
		}
	}
	out := make([]openAIToolCall, 0, len(calls))
	for _, call := range calls {
		if _, ok := declared[call.Function.Name]; ok {
			out = append(out, call)
			continue
		}
		if mapped, ok := mapAliasToDeclaredWriter(call, declared); ok {
			if logger.Get() != nil {
				logger.Get().Infow("R428: tool call alias mapped to declared tool",
					"from", call.Function.Name, "to", mapped.Function.Name)
			}
			out = append(out, mapped)
			continue
		}
		if logger.Get() != nil {
			logger.Get().Warnw("R428: tool call dropped — name is not declared by the client",
				"name", call.Function.Name, "declared_count", len(declared))
		}
	}
	return out
}

// mapAliasToDeclaredWriter — синоним записи файла → объявленный инструмент записи.
func mapAliasToDeclaredWriter(call openAIToolCall, declared map[string]openAITool) (openAIToolCall, bool) {
	if !writerToolAliases[strings.ToLower(strings.TrimSpace(call.Function.Name))] {
		return openAIToolCall{}, false
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		// Аргументы не объект — преобразовывать нечего.
		return openAIToolCall{}, false
	}
	target, targetProps, ok := pickDeclaredWriterTool(declared)
	if !ok {
		return openAIToolCall{}, false
	}
	newArgs := map[string]interface{}{}
	// Путь.
	if v, ok := pickArg(args, pathArgKeys); ok {
		if key := pickTargetKey(targetProps, pathArgKeys...); key != "" {
			newArgs[key] = v
		}
	}
	// Содержимое.
	if v, ok := pickArg(args, contentArgKeys); ok {
		if key := pickTargetKey(targetProps, contentArgKeys...); key != "" {
			newArgs[key] = v
		}
	}
	// Остальные аргументы переносим, если целевая схема их знает.
	for k, v := range args {
		if _, exists := newArgs[k]; exists {
			continue
		}
		if len(targetProps) == 0 || targetProps[k] {
			newArgs[k] = v
		}
	}
	if _, ok := newArgs["path"]; !ok {
		if len(targetProps) == 0 || targetProps["path"] {
			// пути нет — операция записи бессмысленна
			return openAIToolCall{}, false
		}
	}
	data, err := json.Marshal(newArgs)
	if err != nil {
		return openAIToolCall{}, false
	}
	mapped := call
	mapped.Function.Name = target.Function.Name
	mapped.Function.Arguments = string(data)
	if mapped.ID == "" || strings.HasPrefix(mapped.ID, "call_"+call.Function.Name) {
		mapped.ID = "call_" + target.Function.Name
	}
	return mapped, true
}

// pickDeclaredWriterTool — объявленный инструмент, который пишет файлы.
// Приоритет: имя `editor`; иначе единственный инструмент со свойствами и пути,
// и содержимого. Если такой не один — не угадываем.
func pickDeclaredWriterTool(declared map[string]openAITool) (openAITool, map[string]bool, bool) {
	if t, ok := declared["editor"]; ok {
		return t, toolParamProps(t), true
	}
	var found []openAITool
	for _, t := range declared {
		props := toolParamProps(t)
		if props == nil {
			continue
		}
		hasPath := false
		for _, k := range pathArgKeys {
			if props[k] {
				hasPath = true
				break
			}
		}
		hasContent := false
		for _, k := range contentArgKeys {
			if props[k] {
				hasContent = true
				break
			}
		}
		if hasPath && hasContent {
			found = append(found, t)
		}
	}
	if len(found) == 1 {
		return found[0], toolParamProps(found[0]), true
	}
	return openAITool{}, nil, false
}

// toolParamProps — имена свойств схемы параметров инструмента (nil, если схема
// не объект/не разобралась).
func toolParamProps(t openAITool) map[string]bool {
	if len(t.Function.Parameters) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(t.Function.Parameters, &schema); err != nil {
		return nil
	}
	if len(schema.Properties) == 0 {
		return nil
	}
	out := make(map[string]bool, len(schema.Properties))
	for k := range schema.Properties {
		out[k] = true
	}
	return out
}

func pickArg(args map[string]interface{}, keys []string) (interface{}, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			return v, true
		}
	}
	return nil, false
}

// pickTargetKey — первое имя из списка, которое есть в схеме цели ("" — нет).
func pickTargetKey(props map[string]bool, keys ...string) string {
	for _, k := range keys {
		if props == nil || props[k] {
			return k
		}
	}
	return ""
}
