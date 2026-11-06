// empty_answer_r461.go — R461 (2026-11-06): пояснение «только размышления»
// больше не врёт про исчерпанный бюджет.
//
// ЖИВОЙ ДЕФЕКТ (A10 ×2, Qwen3.8-27B, Cline/DSH). Клиент постоянно показывал
// «модель сгенерировала только размышления, увеличьте max_tokens», хотя окно
// было 131072 и бюджет ответа выбран не был. Причина: R314 писал в текст
// «(бюджет ответа исчерпан)» ВСЕГДА — по одному признаку «видимого текста нет,
// а размышления были». Этот признак не говорит, кто остановил модель: она могла
// закончить сама (done_reason=stop), а размышления остались последним, что она
// сказала.
//
// Оператор читает такой текст как факт и идёт увеличивать max_tokens — при том
// что настоящая причина в другом. Для «докажи, что отдано правильно» это худший
// вид ошибки: не потеря данных, а ложное объяснение.
//
// ПРАВИЛО: где числа ИЗВЕСТНЫ (n_predict и фактически сгенерированные токены),
// формулировка называет, исчерпан бюджет или нет. Где неизвестны — нейтральный
// текст без утверждения (emptyAnswerNoticeForNativeR314).
package main

import "ollama-loadbalancer/internal/i18n"

// emptyAnswerNoticeWithBudgetR461 — то же пояснение, но с проверенным фактом о
// бюджете: «исчерпан (сгенерировано X из Y)» или «не исчерпан, модель
// остановилась сама».
//
// Возвращает пустую строку, если подменять нечего (есть видимый текст или не
// было размышлений) — правило R314/R394/R417 сохраняется, чужие ответы не
// трогаем.
func emptyAnswerNoticeWithBudgetR461(content string, reasoningChars, nPredict, generatedTokens int) string {
	base := emptyAnswerNoticeForNativeR314(content, reasoningChars)
	if base == "" {
		return ""
	}
	// nPredict<=0 — бюджет неизвестен (модель грузилась с дефолтом воркера и он
	// не дошёл до этого места): утверждать нечего, отдаём нейтральный текст.
	if nPredict <= 0 {
		return base
	}
	if generatedTokens < 0 {
		generatedTokens = 0
	}
	if toolsBudgetExhaustedR394(nPredict, generatedTokens) {
		return i18n.T("cppworker.handlers_chat.empty_answer_reasoning_only_budget_hit",
			reasoningChars, generatedTokens, nPredict)
	}
	return i18n.T("cppworker.handlers_chat.empty_answer_reasoning_only_budget_left",
		reasoningChars, generatedTokens, nPredict)
}
