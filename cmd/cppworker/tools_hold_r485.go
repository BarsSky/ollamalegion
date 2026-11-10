// tools_hold_r485.go — R485 (2026-11-10): удержание РАЗМЫШЛЕНИЙ не снимается в
// content. Найдено по живому архиву второго стенда (A10 ×2, Qwen3.8-27B).
//
// ЧТО В АРХИВЕ. В истории запроса клиента assistant-сообщения несут
// content_len=65109 / 18720 / 12113 вместе с tool_calls, и текст начинается как
// планирование («Alright. Let me check the target name…»). Это размышления,
// которые ушли в content. Причина — сторож R482: по таймауту он снимал
// удержание и ВСЁ ДАЛЬНЕЙШЕЕ выводил как content. Клиент сохранил это как ответ
// ассистента, и на следующем ходу модель видит свои же размышления как прошлый
// ответ — «замыкается в себя», инструменты применяются всё хуже.
//
// ПРАВИЛО R485. Сторож снимает удержание только для НЕ-reasoning вывода (проза,
// разметка вызова). Если удержание вызвано открытым блоком размышлений
// (reasoningHold, явный маркер в начале или «канал открыт шаблоном», R471) —
// удержание остаётся: накопленное в content не отдаётся, и последующие токены
// тоже. Видимость при этом даёт `CPPWORKER_TOOLS_THINKING=on` (размышления в
// поле thinking, отдельно от content) или `CPPWORKER_TOOLS_HOLD_MAX_SEC=0`
// (прежнее поведение R190/R471: держать до закрывающего маркера).
//
// Аварийный выход для оператора: CPPWORKER_TOOLS_HOLD_RELEASE_REASONING=1 —
// вернуть поведение R482 (снимать и reasoning), понимая риск загрязнения
// истории клиента.
package main

import (
	"os"
	"strings"
)

// toolsHoldReasoningR485 — вызвано ли удержание размышлениями, а не разметкой
// вызова. Все аргументы — то, что уже есть в callback tools-пути.
func toolsHoldReasoningR485(reasoningHold bool, buf, prompt, modelName string, reasonEcho bool, emittedLen int) bool {
	if reasoningHold {
		return true
	}
	if emittedLen != 0 {
		return false
	}
	// Начало вывода — маркер размышлений (модель сама открыла канал).
	if startsWithReasoningMarker(strings.TrimLeft(buf, " \t\r\n")) {
		return true
	}
	// Маркера нет, но канал открыт шаблоном (R471): модель reasoning, клиент
	// размышления не просил, в промпте они включены.
	return reasoningStartHoldR471(prompt, modelName, reasonEcho)
}

// toolsHoldReleaseReasoningR485 — аварийный тумблер (по умолчанию выключен).
func toolsHoldReleaseReasoningR485() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("CPPWORKER_TOOLS_HOLD_RELEASE_REASONING")))
	switch v {
	case "1", "on", "true", "yes":
		return true
	default:
		return false
	}
}
