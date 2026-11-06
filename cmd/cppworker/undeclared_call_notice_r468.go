// undeclared_call_notice_r468.go — R468 (2026-11-06): честная причина «вызов не
// выполнен», когда вызов РАСПОЗНАН, но инструмент клиентом не объявлен.
//
// ЖИВОЙ ДЕФЕКТ (артефакт /app/diag/RAW-20261106-153057.913-markup_stripped.txt,
// Cline + gemma-4-E4B, /api/chat). Модель выдала полноценный нативный вызов
//
//	<|tool_call>call:editor{new_text:<|"|>…код…<|"|>,path:<|"|>d:\cline-e2e-web\app.js<|"|>}<tool_call|>
//
// Парсер его РАЗОБРАЛ (проверено на этом самом тексте: 1 вызов, name=editor,
// аргументы валидны). Но в запросе было объявлено 5 инструментов —
// read_files, search_codebase, fetch_web_content, ask_question, run_commands —
// БЕЗ editor. R428 правильно отбросил вызов: инструмент не объявлен, выполнять
// его нельзя. А клиенту мы сказали:
//
//	«Модель вызвала инструмент в нераспознанном формате — вызов не выполнен».
//
// Это ЛОЖЬ: формат распознан, отказ — по составу объявленных инструментов.
// Оператор читает «нераспознанный формат» и идёт чинить парсер, которого чинить
// не надо; модель на следующем ходу получает в истории неверную причину и
// повторяет то же самое. Класс тот же, что R461: нельзя называть причину,
// которую не проверял.
//
// ПРАВИЛО: причина в тексте — либо «не распознали разметку», либо «распознали,
// но инструмент не объявлен (вот он, вот что объявлено)». Диагностика (R427)
// получает отдельный reason `call_undeclared` и имена отброшенных вызовов,
// чтобы вопрос «а editor вообще объявляли?» решался по артефакту, а не
// разбором исходников.
package main

import (
	"fmt"
	"strings"

	"ollama-loadbalancer/internal/i18n"
)

// undeclaredCallNamesR468 — имена РАСПОЗНАННЫХ вызовов, которые не объявлены в
// запросе (ровно те, что отбросит filterUndeclaredToolCalls).
//
// Логика повторяет фильтр НАМЕРЕННО: если фильтр умеет переводить синоним на
// объявленный инструмент записи (mapAliasToDeclaredWriter), такой вызов не
// отброшен — и в список попасть не должен. Иначе текст снова разойдётся с
// поведением.
func undeclaredCallNamesR468(outputForParse string, tools []openAITool) []string {
	if outputForParse == "" || len(tools) == 0 {
		// Без tools[] фильтровать нечего — всё объявлено по определению.
		return nil
	}
	calls := parseToolCallsFromOutputRaw(outputForParse)
	if len(calls) == 0 {
		return nil
	}
	declared := make(map[string]openAITool, len(tools))
	for _, t := range tools {
		if t.Function.Name != "" {
			declared[t.Function.Name] = t
		}
	}
	var out []string
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Function.Name)
		if name == "" {
			continue
		}
		if _, ok := declared[name]; ok {
			continue
		}
		if _, ok := mapAliasToDeclaredWriter(call, declared); ok {
			continue
		}
		key := strings.ToLower(name)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, name)
	}
	return out
}

// undeclaredCallNoticeR468 — понятный текст вместо «нераспознанного формата».
//
// Текст читает и оператор (в UI), и модель на следующем ходу (он попадает в
// историю как content ассистента), поэтому в нём: конкретный инструмент, что
// было объявлено, и что сделать. Ограничение по числу имён — чтобы сообщение
// оставалось сообщением, а не списком на экран.
func undeclaredCallNoticeR468(names []string, tools []openAITool) string {
	if len(names) == 0 {
		return ""
	}
	declared := declaredToolNamesR468(tools, 8)
	return i18n.T("cppworker.handlers_chat.undeclared_call_notice",
		strings.Join(names, ", "), declared)
}

// declaredToolNamesR468 — имена объявленных инструментов через запятую (до
// limit), плюс «…» если их больше. Нужно для текста: «объявлены: …».
func declaredToolNamesR468(tools []openAITool, limit int) string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Function.Name) != "" {
			names = append(names, t.Function.Name)
		}
	}
	if len(names) == 0 {
		return i18n.T("cppworker.handlers_chat.undeclared_call_none_declared")
	}
	if limit > 0 && len(names) > limit {
		return fmt.Sprintf("%s …", strings.Join(names[:limit], ", "))
	}
	return strings.Join(names, ", ")
}
