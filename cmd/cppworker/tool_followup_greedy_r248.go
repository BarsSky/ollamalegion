// tool_followup_greedy_r248.go — R248 (2026-10-27): детерминированный ход
// после результата инструмента.
//
// ЖИВОЙ ДЕФЕКТ (стенд, Qwen3.8/gemma-4, DeepSeek Harness). Один и тот же
// двухходовой запрос «вызов → результат → ответ» вёл себя по-разному: иногда
// модель использовала результат, а иногда СНОВА выдавала тот же tool_call с
// пустым content. При дефолтной температуре (0.7) это лотерея, а клиент в цикле
// исполняет повторный вызов, получает его опять и уходит в таймауты — снаружи
// «бесконечные запросы, а модель продолжает работать».
//
// ПОЛИТИКА. Если в истории есть результат инструмента для последнего вызова
// ассистента, ход обязан быть детерминированным: greedy (temperature=0, top_p=1).
// Это не отнимает у клиента управление: если он ЯВНО прислал temperature, его
// значение сохраняется.
package main

import (
	"strings"

	"ollama-loadbalancer/c/bridge"
)

// hasToolResultForLastCall — есть ли в истории сообщение role="tool", которое
// относится к последнему вызову ассистента (по tool_call_id, либо безусловно,
// когда клиент прислал результат без id — так делает часть клиентов).
func hasToolResultForLastCall(msgs []chatMessage) bool {
	var lastCallIDs []string
	sawToolAfterLastCall := false
	for _, m := range msgs {
		switch strings.TrimSpace(m.Role) {
		case "assistant":
			lastCallIDs = lastCallIDs[:0]
			sawToolAfterLastCall = false
			for _, c := range m.ToolCalls {
				if id := strings.TrimSpace(c.ID); id != "" {
					lastCallIDs = append(lastCallIDs, id)
				}
			}
		case "tool":
			if len(lastCallIDs) == 0 {
				// Вызова в истории не видно, но результат есть — тоже повод
				// ответить детерминированно, иначе модель «додумывает» вызов.
				sawToolAfterLastCall = true
				continue
			}
			id := strings.TrimSpace(m.ToolCallID)
			if id == "" {
				sawToolAfterLastCall = true
				continue
			}
			for _, cid := range lastCallIDs {
				if cid == id {
					sawToolAfterLastCall = true
					break
				}
			}
		}
	}
	return sawToolAfterLastCall
}

// applyToolFollowupDeterminism — включает greedy для хода с результатом
// инструмента, если клиент не задал температуру явно. Возвращает true, если
// политика применена (для лога/тестов).
func applyToolFollowupDeterminism(msgs []chatMessage, params *bridge.GenerationParams, clientSetTemperature bool) bool {
	if params == nil {
		return false
	}
	if !hasToolResultForLastCall(msgs) {
		return false
	}
	// Клиент явно управляет температурой — не вмешиваемся.
	if clientSetTemperature {
		return false
	}
	params.Temperature = 0
	params.TopP = 1
	params.TopK = 1
	return true
}
