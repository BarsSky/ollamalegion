// tool_history_callformat_r475.go — R475 (2026-11-07): история вызовов
// показывается модели в ТОЙ ЖЕ форме, которой требует её шаблон.
//
// ЖИВОЙ ДЕФЕКТ (Cline → балансер → cppworker → Qwen3.8-9B, /api/chat,
// 6 инструментов). В запросе 13 assistant-сообщений с вызовами. Промпт,
// снятый с /app/diag/prompt-last.txt, показывал их ТАК:
//
//	<|im_start|>assistant
//	[[{"id":"call_editor","type":"function","function":{"name":"editor",…}}]]<|im_end|>
//	<|im_start|>user
//	tool result (editor): {"query":"insert:…","result":"Inserted content at line 29 …"}<|im_end|>
//
// А собственный шаблон этой модели (tokenizer.chat_template из GGUF, ветка
// assistant) предписывает совсем другую форму и прямо требует её в <IMPORTANT>:
//
//	<tool_call>
//	<function=editor>
//	<parameter=path>
//	…
//	</parameter>
//	</function>
//	</tool_call>
//
// Модель учится на истории: 12 из 12 вызовов в промпте были JSON-дампом в
// тексте, поэтому она и сама начала выдавать вызовы текстом — сначала всем
// блоком `[[{…}]]` (7 КБ кода уехали клиенту ответом), затем «командой в
// fence» (`node --check public/app.js` текстом вместо вызова run_commands).
// Класс дефекта закрыт по краям (R474 отдаёт пояснение вместо кода), но
// ПРИЧИНУ убирает эта правка: история больше не противоречит инструкции.
//
// ЧЕГО НЕ МЕНЯЕМ: канал результата инструмента (role="tool" → текстовый
// user-префикс) и структурный путь (R246) — там своя логика по возможностям
// шаблона. Меняется только ФОРМА показа вызова в текстовой истории.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// functionTagCallsR475 — шаблон описывает вызовы формой `<function=NAME>` +
// `<parameter=имя>` (Qwen3/chatml-семейство). Именно эту форму и надо
// показывать в истории: она же стоит в инструкции шаблона.
func functionTagCallsR475(templateSrc string) bool {
	if templateSrc == "" {
		return false
	}
	return strings.Contains(templateSrc, "<function=") && strings.Contains(templateSrc, "<parameter=")
}

// legacyToolCallsXMLR475 — блок вызовов в форме шаблона.
//
// Возвращает "" если аргументы не разбираются: тогда вызывающий код остаётся
// на прежней форме `[[…]]` (лучше показать старую форму, чем битую новую).
func legacyToolCallsXMLR475(calls []ollamaToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	for _, call := range calls {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			return ""
		}
		args, ok := orderedArgsR475(toolCallArgsString(call.Function.Arguments))
		if !ok {
			return ""
		}
		b.WriteString("<tool_call>\n<function=")
		b.WriteString(name)
		b.WriteString(">\n")
		for _, kv := range args {
			b.WriteString("<parameter=")
			b.WriteString(kv[0])
			b.WriteString(">\n")
			b.WriteString(kv[1])
			b.WriteString("\n</parameter>\n")
		}
		b.WriteString("</function>\n</tool_call>\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// orderedArgsR475 — аргументы вызова как пары «ключ → текст значения» В ТОМ
// ПОРЯДКЕ, в котором их прислал клиент. Порядок важен: шаблон рисует параметры
// по порядку, и модель повторяет его в своих вызовах.
func orderedArgsR475(argsJSON string) ([][2]string, bool) {
	trimmed := strings.TrimSpace(argsJSON)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return nil, true
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	out := make([][2]string, 0, 8)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, false
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		out = append(out, [2]string{key, paramValueTextR475(raw)})
	}
	if _, err := dec.Token(); err != nil {
		return nil, false
	}
	return out, true
}

// paramValueTextR475 — значение параметра так, как его ждёт шаблон: строка —
// как есть (без кавычек и без экранирования), всё остальное — компактным JSON.
func paramValueTextR475(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return compact.String()
}
