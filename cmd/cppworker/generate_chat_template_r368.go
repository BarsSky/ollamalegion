// generate_chat_template_r368.go — R368 (2026-10-31): /api/generate применяет
// чат-шаблон модели, как это делает настоящая Ollama.
//
// ДЕФЕКТ (замер стенда, docs/plans/2026-10-31-generate-raw-prompt-degeneracy.md).
// `/api/generate` у нас отправлял в llama.cpp «сырой» prompt: ни шаблона модели,
// ни служебных токенов начала реплики. Instruction-tuned модель на таком входе
// иногда завершает ответ вырожденно — живой случай: `response:"***"`,
// `tokens:3`, `durationMs:146` (модель выдала три токена и EOS).
//
// Замеры на gemma-4-E4B (промпт «напиши абзац о тестах», num_predict=120):
//
//	прямой /api/generate           1 короткий из 12
//	через балансер (→ completions) 3 коротких из 12
//	/api/chat (шаблон применяется) 0 коротких из 8
//
// Настоящая Ollama для /api/generate шаблон ПРИМЕНЯЕТ (и только `raw:true`
// отключает это). То есть расхождение с Ollama было не косметическим: клиент,
// который ждёт ollama-поведение, получал другой промпт и иногда мусор.
//
// ЧТО ДЕЛАЕМ. После загрузки модели строим prompt шаблоном
// (system → user, с enable_thinking из конфига). Если шаблона у модели нет или
// он сломан (см. chat_template_guard.go) — падаем на ПРЕЖНЕЕ поведение: system
// и текстовая инструкция подмешиваются в prompt. Так ни один клиент не получает
// регресс: у моделей без шаблона всё как раньше, у моделей с шаблоном — как в
// Ollama. `raw:true` отключает и шаблон, и подмешивание.
//
// ЧЕГО НЕ ТРОГАЕМ. OpenAI-совместимый `/v1/completions` остаётся сырым
// completion-эндпоинтом (это его семантика), а `/v1/chat/completions` и
// `/api/chat` шаблон применяли и раньше.
package main

import (
	"ollama-loadbalancer/c/bridge"
)

// generateThinkingInstructionR368 — текстовая инструкция для случая БЕЗ шаблона.
// Дословно прежняя (Round 11/17.1): модели без нативного thinking эмитят
// рассуждения как обычный текст, и без тегов парсер не отделит их от ответа.
const generateThinkingInstructionR368 = "Before answering, use detailed step-by-step thinking. " +
	"Reason about the problem carefully, consider different angles, " +
	"show your work, then provide a clear final answer. " +
	"Structure your response: first explain your reasoning, then give the answer. " +
	"IMPORTANT: Wrap your step-by-step reasoning inside <reasoning>...</reasoning> tags. " +
	"Your final answer (the user-facing response) should be OUTSIDE the </reasoning> tag."

// promptSourcesR368 — откуда взялся итоговый prompt (для логов и тестов).
const (
	promptSourceRawR368      = "raw"      // клиент попросил raw:true
	promptSourceTemplateR368 = "template" // собрано шаблоном модели
	promptSourceFallbackR368 = "fallback" // шаблона нет/сломан — прежнее поведение
)

// generatePromptFallbackR368 — прежнее поведение /api/generate: system и
// (если включены размышления) текстовая инструкция подмешиваются в prompt.
//
// Вынесено в чистую функцию, чтобы правило можно было проверить без загрузки
// модели: ветка исполняется, когда шаблона нет, а это как раз те модели, на
// которых раньше и держалось всё поведение эндпоинта.
func generatePromptFallbackR368(req generateRequest, thinking bool) string {
	prompt := req.Prompt
	// raw:true — клиент просит ровно свои токены. promptForGenerateR368 выходит
	// по raw раньше, но проверка остаётся здесь же: функция чистая и не должна
	// зависеть от того, кто её позвал.
	if req.Raw {
		return prompt
	}
	if req.System != "" {
		prompt = req.System + "\n" + prompt
	}
	if thinking {
		prompt = generateThinkingInstructionR368 + "\n\n" + prompt
	}
	return prompt
}

// promptForGenerateR368 — итоговый prompt для /api/generate и источник, из
// которого он получен.
//
// modelName — ФАКТИЧЕСКОЕ имя загруженной модели (после ensureModelLoaded):
// шаблон ищется у неё, а не у алиаса из запроса.
func promptForGenerateR368(modelName string, req generateRequest, thinking bool) (string, string) {
	if req.Raw {
		return req.Prompt, promptSourceRawR368
	}
	msgs := make([]bridge.ChatMessage, 0, 2)
	if req.System != "" {
		msgs = append(msgs, bridge.ChatMessage{Role: "system", Content: req.System})
	}
	msgs = append(msgs, bridge.ChatMessage{Role: "user", Content: req.Prompt})

	// Тот же защищённый вход в C, что и у чата: recover() + пометка «шаблон
	// сломан» живут в chat_template_guard.go. Прямой bridge-вызов здесь завёл бы
	// вторую точку входа без защиты — ровно так уже теряли узел (R91).
	if prompt, _, err := safeApplyChatTemplateWithThinking(modelName, "", msgs, thinking, true); err == nil && prompt != "" {
		return prompt, promptSourceTemplateR368
	}
	return generatePromptFallbackR368(req, thinking), promptSourceFallbackR368
}
