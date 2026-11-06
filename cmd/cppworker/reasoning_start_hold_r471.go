// reasoning_start_hold_r471.go — R471 (2026-11-06): начало вывода не уходит
// клиенту как ответ, если модель может рассуждать, а клиент размышления не
// просил.
//
// ЖИВОЙ ДЕФЕКТ (прогон Cline, gemma-4-E4B, /api/chat + tools). Сводка воркера:
//
//	streamed_content_bytes=5363  (= 3323 ответа + 2040 размышлений)
//	thinking_len=2040, content_len=3323, reasoning_echo=false
//
// Клиент получил ОБА текста в одном content («The user is asking to continue…»
// + русский анализ), хотя итоговая классификация считала английский часть
// размышлениями. Воспроизведено на уровне парсера: если в выводе есть
// ЗАКРЫВАЮЩИЙ маркер, а ОТКРЫВАЮЩЕГО нет (канал открыт шаблоном, модель его не
// повторяет), то поток отдаёт английский как content, а финальный разбор — как
// reasoning. Клиент видит склейку.
//
// ПРАВИЛО: если клиент размышления не просил, а модель их умеет и в промпте
// размышления включены — начало вывода ДЕРЖИМ до закрывающего маркера. Тогда:
//   - есть маркер → всё до него не отдаётся (это размышления), content начинается
//     после него;
//   - маркера нет вовсе → текст уходит клиенту в финальном чанке (не теряется,
//     просто без поточной отдачи).
package main

import "strings"

// reasoningStartHoldR471 — держать ли начало вывода до закрывающего маркера.
//
// Условия намеренно узкие, чтобы не задерживать обычные ответы:
//   - клиент НЕ просил размышления (echo=false): если просил — он их и получит;
//   - модель reasoning-архитектуры (иначе рассуждений и не будет);
//   - в промпте размышления ВКЛЮЧЕНЫ: там стоит служебный токен включения
//     (`<|think|>` у gemma, `enable_thinking` у qwen и т.п.). Если размышления
//     выключены, задерживать нечего.
func reasoningStartHoldR471(prompt, modelName string, echo bool) bool {
	if echo {
		return false
	}
	if !IsReasoningModel(modelName) {
		return false
	}
	return promptEnablesThinkingR471(prompt)
}

// promptEnablesThinkingR471 — включены ли размышления в собранном промпте.
//
// Смотрим ТОЛЬКО служебные маркеры включения, которые сами шаблоны ставят в
// системную часть. Это не эвристика по словам: список закрытый.
func promptEnablesThinkingR471(prompt string) bool {
	if prompt == "" {
		return false
	}
	for _, m := range thinkingEnableMarkersR471 {
		if strings.Contains(prompt, m) {
			return true
		}
	}
	return false
}

// thinkingEnableMarkersR471 — маркеры «размышления включены» из chat-шаблонов.
var thinkingEnableMarkersR471 = []string{
	"<|think|>",         // gemma-4
	"<|channel>thought", // gemma-4: канал размышлений
	"enable_thinking",   // qwen3 и совместимые (в тексте шаблона/системы)
	"thinking_budget",   // qwen3: бюджет размышлений
	"<|thinking|>",      // прочие шаблоны
}
