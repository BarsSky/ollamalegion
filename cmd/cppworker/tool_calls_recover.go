// tool_calls_recover.go — R638 (2026-11-19): чистка финального текста и восстановление вызовов обрезкой префикса.
//
// ЗАЧЕМ ВЫНЕСЕНО. `tool_calls.go` держал 1988 строк: кэш промпта, разбор
// девяти семейств форматов вызова, нормализацию и сборку системного промпта.
// Здесь — один из слоёв; поведение не менялось, код перенесён как есть.
package main

import (
	"encoding/json"
	"strings"
)

// cleanFinalContent выполняет минимальную очистку текстового ответа модели
// перед отдачей клиенту в финальном NDJSON-чанке /api/chat (streaming, tools!=nil).
//
// Зачем нужно:
//  1. Хвостовые служебные токены: модель иногда добавляет </tool_call>, [TOOL_CALLS],
//     <|python_tag|> и т.п. в конец plain-text ответа (когда модель "думала" про tool calls,
//     но не решила их вызывать — артефакт prompt'а с tool definitions).
//  2. Лишние пробелы / переносы строк, которые портят отображение в OpenWebUI.
//
// Не делаем агрессивной очистки (как cleanContentAfterToolCallExtraction в balancer),
// чтобы не повредить нормальный текст ответа.
//
// Используется ТОЛЬКО для случая, когда parseToolCallsFromOutput вернул пустой
// результат (модель не сгенерировала tool_calls), но text response всё равно
// содержит подозрительные токены.
func cleanFinalContent(output string) string {
	if output == "" {
		return ""
	}
	s := output
	// Удаляем хвостовые служебные токены (могут быть артефактом из prompt с tools).
	// Идём в цикле, потому что их может быть несколько подряд.
	//
	// R108 (2026-10-13): перед проверкой суффикса срезаем ПРОБЕЛЬНЫЕ символы.
	// Живой дефект: gemma-4 отдала «ок<end_of_turn> » (токен, за которым остался
	// пробел/перевод строки) — HasSuffix не срабатывал ни на «<end_of_turn>», ни
	// на «<end_of_turn>\n», и клиент (через балансер :18080) увидел в content
	// «ок<end_of_turn>» — тот же класс дефекта, что «ок<end_of_turn>» в R106c.
	for {
		trimmed := false
		s = strings.TrimRight(s, " \t\r\n")
		// Прямое вхождение + пробельные символы вокруг.
		for _, tok := range trailingToolTokens {
			if strings.HasSuffix(s, tok) {
				s = strings.TrimSuffix(s, tok)
				trimmed = true
			}
		}
		if !trimmed {
			break
		}
	}
	// R114b (2026-10-13): вырезаем одиночные маркеры размышлений, оставшиеся в
	// ВИДИМОМ тексте. Парный разбор (SplitReasoningContent) снимает блоки
	// open…close; тег без пары (начало блока было в промпте шаблона, обрыв по
	// max_tokens, чужой шаблон) оставался в content — живая жалоба оператора:
	// «в ответе перемешка с ответом и маркером </think>».
	s = stripOrphanReasoningTags(s)
	// R418 (2026-11-03): токены шаблона в СЕРЕДИНЕ видимого текста
	// (`<|user|>`, `<|im_start|>` …). `trailingToolTokens` выше чистит только
	// хвост; живой случай Qwen3.8-9B: «ок\n<|user|>\nОтветь…» уехало клиенту.
	s = stripTemplateTurnTokensR418(s)
	// Trim whitespace и trailing newlines.
	return strings.TrimSpace(s)
}

// trailingToolTokens — служебные токены, которые модель может эмитить в конце
// plain-text ответа при наличии tools в prompt (артефакты chat template'ов).
var trailingToolTokens = []string{
	"</tool_call>",
	"</tool_call>\n",
	"<|python_tag|>",
	"<|python_tag|>\n",
	"<|eom_id|>",
	"<|eot_id|>",
	"<|end|>",
	"<|end|>\n",
	"[TOOL_CALLS]",
	"[TOOL_CALLS]\n",
	"[TOOL_RESULTS]",
	"[TOOL_RESULTS]\n",
	"<start_of_turn>model\n",
	"<start_of_turn>model",
	"<end_of_turn>",
	"<end_of_turn>\n",
	// R110 (2026-10-13): обёртки НАТИВНОГО вызова gemma-4. Сам вызов парсер уже
	// распознаёт (normalizeGemmaNativeToolCallSyntax), но если модель оставила в
	// хвосте только обёртку (частичный вызов, обрезка по max_tokens), без этих
	// токенов она уезжала клиенту как видимый текст.
	"<tool_call|>",
	"<|tool_call>",
	"<|tool_call|>",
	// R246: хвост диалекта gemma tool_code (обрыв генерации по max_tokens —
	// иначе обёртка уезжает клиенту видимым текстом).
	"[tool_code]",
	"[/tool_code]",
	"```tool_code",
	// R432 (2026-11-05): спец-токены кавычек нативного вызова gemma
	// (`call:read_files(files:[<|"|>TASK.md<|"|>])`). Если вызов всё же не
	// распознан, эти токены не должны попадать в видимый ответ.
	`<|"|>`,
	`<|'|>`,
}

// recoverToolCallsByPrefixTrimming — Round 16 (2026-07-30) multi-tool recovery.
//
// Symptom: Qwen3-4B-Instruct и аналогичные instruct-модели при multi-tool
// запросах иногда выдают "почти-валидный" JSON с лишним trailing garbage
// (см. live test TestLiveParser_TrailingBraceBug, 228 байт, последний `}`
// лишний). Чистый json.Unmarshal падает.
//
// Алгоритм: пробуем серию простых фиксов, после каждого — парсим. Фиксы:
//  1. Trim trailing non-JSON символов (`}`, whitespace, etc.)
//  2. Trim trailing `}` после `]` (самый частый случай)
//  3. Strip extra `}` INSIDE string values (Qwen3-4B quirk: model пишет
//     `"arguments":"{...}}"}` вместо `"arguments":"{...}"}` — лишний `}` внутри строки)
//  4. Prefix trimming (last resort: отрезаем по 1 символу с конца)
func recoverToolCallsByPrefixTrimming(output string) []openAIToolCall {
	if len(output) < 4 {
		return nil
	}
	if !strings.ContainsAny(output, "[{") {
		return nil
	}

	// Helper: try to parse the given string as tool_calls.
	tryParse := func(s string) []openAIToolCall {
		s = strings.TrimSpace(s)
		if !strings.ContainsAny(s, "[{") {
			return nil
		}
		var arr []openAIToolCall
		if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
			if normalizeToolCalls(arr) {
				return arr
			}
		}
		var single openAIToolCall
		if err := json.Unmarshal([]byte(s), &single); err == nil && single.Function.Name != "" {
			if normalizeToolCalls([]openAIToolCall{single}) {
				return []openAIToolCall{single}
			}
		}
		return nil
	}

	// 1. Trim trailing non-JSON chars (whitespace, stray `}` after `]`).
	candidate := strings.TrimRight(output, " \t\r\n}")
	if calls := tryParse(candidate); len(calls) > 0 {
		return calls
	}

	// 2. Strip extra `}` INSIDE string values. Типичный кейс:
	//    `"arguments":"{\"city\":\"Paris\"}}"}` → `"arguments":"{\"city\":\"Paris\"}\""`
	//    Модель добавляет лишний `}` перед закрывающей кавычкой string value.
	//    Паттерн: `}` followed by `"` followed by `}` — внутри string это лишний brace.
	//    Ищем `}}"` где первая `}` — лишний brace в string, вторая `}` — закрытие объекта.
	//    Простой regex: заменяем `}}` на `}` если после `}}` идёт `,` или `}` или `]`.
	fixed := fixTrailingBraceMisorder(output)
	if fixed != output {
		if calls := tryParse(fixed); len(calls) > 0 {
			return calls
		}
		// Также попробуем после trimRight
		if calls := tryParse(strings.TrimRight(fixed, " \t\r\n}")); len(calls) > 0 {
			return calls
		}
	}

	// 3. Prefix trimming (last resort): отрезаем 1-2 символа с конца.
	for trim := 1; trim <= 4; trim++ {
		if len(output) <= trim {
			break
		}
		if calls := tryParse(output[:len(output)-trim]); len(calls) > 0 {
			return calls
		}
	}
	return nil
}
