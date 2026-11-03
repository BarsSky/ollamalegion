// tools_stream_reasoning_r404.go — R404 (2026-11-03): размышления на tools-пути
// отдаются ДЕЛЬТАМИ, а не одним куском в конце.
//
// ЖИВОЙ ДЕФЕКТ (A10, Cline, Qwen3.8-27B, /api/chat + tools). Лог воркера:
//
//	writeChatStreamResponseWithTools: reasoning отделён...
//	  reasoning_len=16647  content_len=12  reasoning_sent=true
//	R188 повтор ограничен ... n_predict=4096
//	/api/chat stream response summary ... done_reason=length
//	  thinking_len=16647  final_content_len=10  generated_tokens=4096  budget_exhausted=true
//	  elapsed_ms=362261
//	AbortWatcher: per-infer abort set ... context canceled
//
// Почему клиент «ничего не получал». На tools-пути удержание срабатывало на
// САМОМ НАЧАЛЕ блока размышлений (`startsWithReasoningMarker` →
// reasoningHold=true, toolsHold=true), после чего в callback'е не отдавалось
// ни одной дельты, пока блок не ЗАКРОЕТСЯ. Если бюджет вывода заканчивался
// ВНУТРИ размышлений (а на 4096 токенах это нормальный исход), блок не
// закрывался никогда: клиент минутами не получал ни байта (только
// keepalive-кадры), затем — финальный чанк с 10 байтами content, done_reason
// length и повтор генерации R188 с тем же бюджетом (ещё минуты). Cline в этот
// момент показывает «нет ответа» / обрывает ход, а часть клиентов падает на
// разборе финального чанка («text part … not found»), потому что thinking
// пришёл только в done-кадре, без дельт.
//
// ПРАВИЛО. Размышления НЕ могут быть вызовом инструмента, поэтому их можно
// отдавать сразу: каждый токен проходит через ReasoningStreamState, готовая
// thinking-дельта уходит клиенту отдельным NDJSON-кадром (валидный Ollama-
// стрим: message.thinking + done:false). Удержание остаётся только для
// content — там действительно может начаться разметка tool call.
//
// Проверка: tools_stream_reasoning_r404_test.go (дельта из парсера, чанк,
// отсутствие дубля в финале).
package main

import "strings"

// toolsThinkingChunkR404 — NDJSON-кадр reasoning-дельты для tools-стрима.
//
// Форма — как у остальных дельт нативного /api/chat: message.thinking,
// done:false. Клиенты (Cline/OpenWebUI) читают именно это поле и помечают
// размышления как Think.
func toolsThinkingChunkR404(modelName, createdAt, delta string) map[string]interface{} {
	return map[string]interface{}{
		"model":      modelName,
		"created_at": createdAt,
		"message": map[string]string{
			"role":     "assistant",
			"thinking": delta,
		},
		"done": false,
	}
}

// toolsFeedThinkingR404 — скормить токен reasoning-парсеру и вернуть готовую
// thinking-дельту ("" = парсер ещё удерживает возможный неполный маркер).
func toolsFeedThinkingR404(st *ReasoningStreamState, token string) string {
	if st == nil || token == "" {
		return ""
	}
	thinkingDelta, _ := st.Feed(token)
	return thinkingDelta
}

// toolsShouldRetryWithoutThinkingR406 — стоит ли повторить генерацию БЕЗ
// размышлений, когда клиентский бюджет вывода уже израсходован на thinking.
//
// ЖИВОЙ ДЕФЕКТ (R406, Cline). Клиент задаёт max_tokens (например 4096), модель
// уходит в размышления на весь бюджет и не выдаёт видимого ответа. Мы честно
// отдаём done_reason=length, но Cline трактует это как «output token limit» и
// запускает свою overflow-recovery: compacting → compacted, пересказ истории и
// повтор. В результате пропадает часть переписки и «text part … not found».
//
// Повтор с выключенным thinking даёт модели шанс ответить в ТОМ ЖЕ бюджете:
// скрытая работа больше не съедает его целиком. Условия строгие, чтобы не
// дублировать уже отданный ответ:
//   - ничего видимого не стримилось (contentStreamed=false);
//   - вызова инструмента не распознано;
//   - размышления реально были, и бюджет исчерпан;
//   - альтернативный промпт построен, и повтора ещё не было.
func toolsShouldRetryWithoutThinkingR406(nPredict, generatedTokens int, fullOutput, visible, promptNoThink string,
	contentStreamed bool, toolCalls int, retried bool) bool {
	if retried || contentStreamed || toolCalls > 0 {
		return false
	}
	if strings.TrimSpace(promptNoThink) == "" {
		return false
	}
	if strings.TrimSpace(visible) != "" {
		return false
	}
	if reasoningCharsRawR394(fullOutput) <= 0 {
		return false
	}
	return toolsBudgetExhaustedR394(nPredict, generatedTokens)
}

// buildChatPromptNoThinkingR406 — промпт с enable_thinking=false, не меняя
// остальную структуру запроса (та же tool-история).
//
// ПОЧЕМУ ОТДЕЛЬНАЯ ФУНКЦИЯ, А НЕ buildChatPromptWithOptions(ReasoningOverride=
// false): тот путь при выключенном reasoning идёт в LEGACY-шаблон без
// enable_thinking, а шаблон Qwen3 при отсутствии переменной оставляет thinking
// включённым по умолчанию. Повтор тогда снова уйдёт в размышления. Нам нужен
// именно native common_chat_templates_apply с enable_thinking=false.
func buildChatPromptNoThinkingR406(msgs []chatMessage, modelName string) string {
	caps := templateToolCapsForModel(modelName)
	if p, _, err := safeApplyChatTemplateWithThinking(
		modelName, "", buildNativeToolHistory(msgs, caps), false, true); err == nil && p != "" {
		return p
	}
	if usesStructuredToolHistory(caps) {
		// Тот же fallback, что в buildChatPrompt: шаблон отказался рисовать
		// структурные tool_calls — пробуем текстовую форму.
		if p, _, err := safeApplyChatTemplateWithThinking(
			modelName, "", buildLegacyToolHistory(msgs), false, true); err == nil && p != "" {
			return p
		}
	}
	return ""
}
