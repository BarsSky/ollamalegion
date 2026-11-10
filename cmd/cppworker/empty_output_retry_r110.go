// empty_output_retry_r110.go — R110 (2026-10-13): повтор генерации, когда
// видимого ответа не получилось.
//
// ЗАЧЕМ ОТДЕЛЬНЫЙ ФАЙЛ. Условие повтора — это контракт безопасности (что уже
// отправлено клиенту, что считается «пустым» выводом), и оно должно быть в одном
// месте: до R110 оно было продублировано в двух потоковых путях и расходилось
// (`len(fullOutput) == 0` против «после чистки пусто»).
package main

import (
	"context"
	"strings"

	"ollama-loadbalancer/pkg/logger"
)

// maxEmptyOutputRetries — сколько раз повторять генерацию при «пустом» выводе.
//
// Один повтор был в R83/v59. Живая статистика (gemma-4-E4B-it-Q4_K_M через
// Cline CLI, 2026-10-13): примерно 2 запроса из 4 заканчивались тем, что модель
// не давала ни ответа, ни вызова инструмента, и клиент показывал свою заглушку
// «ERROR: EMPTY CONTENT». Один повтор оставлял половину таких запросов
// неудачными. Два повтора при пустом выводе безопасны: клиенту не отправлено ни
// байта, а на медленных моделях повод для повтора — только пустой ответ, не
// обычная генерация.
const maxEmptyOutputRetries = 2

// shouldRetryEmptyStreamingOutput — повторять ли генерацию.
//
// Контракт (унаследован от R83/v59, сохранён дословно):
//   - повтор разрешён ТОЛЬКО если клиенту не отправлено ни байта content
//     (emittedContentBytes == 0) — иначе он получит два ответа подряд;
//   - повтор разрешён только если клиент ещё ждёт (ctx не отменён): иначе
//     генерация мгновенно падает на взведённом abort-флаге, и в логе появляется
//     пугающее «повторная генерация не удалась: BRIDGE_ERR_ABORTED»;
//   - выключатель CPPWORKER_EMPTY_OUTPUT_RETRY=off уважается.
//
// R83/v59 повторял только при raw_len == 0. Живой случай (Cline + gemma-4,
// tools-путь, 2026-10-13): модель отвечает ОДНИМ служебным токеном шаблона
// (`<end_of_turn>`) — raw_len != 0, но дельт клиенту не отправлено (токен короче
// окна удержания), а после cleanFinalContent видимого текста нет. Клиент получал
// «ERROR: EMPTY CONTENT» вместо ответа. Теперь такой вывод тоже считается пустым.
func shouldRetryEmptyStreamingOutput(rawOutput string, emittedContentBytes int, ctx context.Context) bool {
	if !emptyOutputRetryEnabled() {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	// Уже что-то отдали — повтор дал бы дубль в ответе.
	if emittedContentBytes > 0 {
		return false
	}
	return strings.TrimSpace(cleanFinalContent(rawOutput)) == ""
}

// shouldRetryUnparsedToolMarkup — R113 (2026-10-13): повторять ли генерацию,
// когда модель выдала РАЗМЕТКУ вызова инструмента, но распознать её не удалось.
//
// ЗАЧЕМ. Раньше такой вывод уходил клиенту как content («отдаём сырой вывод»), и
// оператор видел инструмент в теле ответа, а агентский клиент (Cline) — финальный
// ответ вместо вызова: задача помечалась выполненной после первой итерации.
// Разметку клиенту отдавать нельзя, а лучший выход — попросить модель ещё раз:
// сэмплирование стохастично, и повтор обычно даёт корректный вызов.
//
// Контракт безопасности тот же, что у shouldRetryEmptyStreamingOutput: повтор
// разрешён ТОЛЬКО пока клиенту не отправлено ни байта и он ещё ждёт.
func shouldRetryUnparsedToolMarkup(rawOutput string, emittedContentBytes int, ctx context.Context) bool {
	if !emptyOutputRetryEnabled() {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	if emittedContentBytes > 0 {
		return false
	}
	return looksLikeToolCallMarkup(rawOutput)
}

// retryReasonLabel — короткая причина повтора для лога (видно, что именно
// случилось: пустой вывод или нераспознанная разметка вызова).
func retryReasonLabel(emptyOutput, unparsedMarkup bool) string {
	switch {
	case emptyOutput && unparsedMarkup:
		return "пустой вывод + нераспознанная разметка вызова"
	case unparsedMarkup:
		return "нераспознанная разметка вызова"
	default:
		return "пустой вывод"
	}
}

// toolsStreamStartsWithServiceToken — вывод начинается со служебного токена
// шаблона (`<end_of_turn>`, `<|end|>`, `<start_of_turn>model` и т.п.).
//
// ЗАЧЕМ. Дельты на tools-пути удерживаются, только если ответ начинается с
// размышлений, JSON или фенса. Живой случай (gemma-4 через Cline): модель
// начинала вывод со `<end_of_turn>`, дальше шли размышления — и когда буфер
// перерастал окно удержания, `cut = len(buf) - toolsStreamHoldback` попадал УЖЕ
// ПОСЛЕ служебного токена, поэтому клиент получал `<end_of_turn>` как обычный
// текст ответа (в записи Cline: «ERROR: EMPTY CONTENT<end_of_turn>»).
//
// Такие ответы тоже держим целиком: служебный токен — не текст ответа.
func toolsStreamStartsWithServiceToken(buf string) bool {
	trimmed := strings.TrimLeft(buf, " \t\r\n")
	if trimmed == "" {
		return false
	}
	for _, tok := range trailingToolTokens {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		if strings.HasPrefix(trimmed, tok) {
			return true
		}
	}
	return false
}

// splitToolsOutput — отделить размышления от ответа и распарсить вызовы
// инструмента в потоковом tools-пути.
//
// Возвращает:
//   - reasoningOut    — размышления для отдельного поля (пусто, если клиент
//     попросил think=false или модель их не эмитила);
//   - outputForParse  — текст без размышлений: по нему ищутся tool_calls и
//     строится видимый ответ;
//   - toolCalls       — распознанные вызовы;
//   - hasReasoning    — были ли размышления в выводе ВООБЩЕ (для лога: видно и
//     случай think=false, когда reasoningOut пуст).
//
// Вынесено из writeChatStreamResponseWithTools, потому что после автоповтора
// разбор нужно выполнить ПОВТОРНО на новом выводе модели — с той же семантикой.
//
// R113 (2026-10-13): добавлен tools — схема инструментов ЗАПРОСА. По ней
// arguments вызова приводится к объекту (run_commands.commands, read_files.files,
// editor.path, …). Без схемы модель отдаёт массив или голую строку, клиент такой
// вызов выполнить не может, и задача помечается выполненной (Cline: COMPLETE)
// после первой же итерации.
func splitToolsOutput(fullOutput string, thinkOverride *bool, tools []openAITool) (
	reasoningOut, outputForParse string, toolCalls []openAIToolCall, hasReasoning bool,
) {
	reasoningOut, outputForParse, toolCalls, hasReasoning, _ = splitToolsOutputR482(fullOutput, thinkOverride, tools)
	return reasoningOut, outputForParse, toolCalls, hasReasoning
}

// splitToolsOutputR482 — то же, плюс признак «вызов найден ВНУТРИ размышлений».
//
// ЖИВОЙ ДЕФЕКТ (второй стенд, Qwen3.8-27B через Cline): модель кладёт вызов
// инструмента внутрь блока размышлений (`<think>… {"name":"read_files",…}`) и
// либо не закрывает блок до конца хода, либо закрывает уже после вызова.
// Прежний разбор смотрел ТОЛЬКО видимую часть (outputForParse): при незакрытом
// блоке она пуста — и вызов исчезал («инструменты не применяются»), а текст
// затем уходил клиенту одним куском как «сырой ответ».
//
// Правило: если в видимой части вызова нет, ищем вызов в тексте размышлений.
// reasoningOut при этом по-прежнему отдаётся клиенту только по его просьбе
// (R470) — меняется только распознавание вызова.
func splitToolsOutputR482(fullOutput string, thinkOverride *bool, tools []openAITool) (
	reasoningOut, outputForParse string, toolCalls []openAIToolCall, hasReasoning bool, fromReasoning bool,
) {
	outputForParse = fullOutput
	rsn, cnt, has := SplitReasoningContent(fullOutput)
	if !has {
		return "", outputForParse, parseToolCallsForRequest(outputForParse, tools), false, false
	}
	if ReasoningWantedByClient(thinkOverride) {
		reasoningOut = rsn
	}
	outputForParse = cnt
	toolCalls = parseToolCallsForRequest(outputForParse, tools)
	if len(toolCalls) == 0 && strings.TrimSpace(rsn) != "" && len(tools) > 0 {
		if rc := parseToolCallsForRequest(rsn, tools); len(rc) > 0 {
			logger.Get().Warnw("R482: tool call найден внутри блока размышлений",
				"reasoning_chars", len(rsn),
				"visible_chars", len(strings.TrimSpace(cnt)),
				"tool_calls", len(rc),
				"note", "модель положила вызов в reasoning; вызов применён, размышления клиенту не отдаются без think=true")
			return reasoningOut, outputForParse, rc, true, true
		}
	}
	return reasoningOut, outputForParse, toolCalls, true, false
}
