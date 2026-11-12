// reasoning_prefilled_hold_r493_test.go — живой дефект 2026-11-12 (архив стенда
// A10 ×2, Qwen3.8-27B): «мысли модели пишутся в контент».
//
// ЧТО В АРХИВЕ (дословно):
//
//	prompt-last.json: prompt_tail = "...<|im_start|>assistant\n<think>\n"
//	  — шаблон qwen35 открывает канал размышлений В ПРОМПТЕ;
//	failures.json: kind=hold_released, detail="hold_timeout held_ms=20061
//	  held_bytes=1137" — удержание снято по таймауту, накопленное ушло в content;
//	last-stream.json: tokens_sent=0, write_errors=1, "context canceled" через
//	  28.8 с — клиент не дождался вывода и отменил запрос.
//
// ПРИЧИНА: reasoningStartHoldR471 проверял только список маркеров включения
// (thinkingEnableMarkersR471), а `<think>` в промпте — открытый блок, не маркер
// включения из списка. Удержание не включалось → R482 снимал его по таймауту →
// размышления уходили клиенту как content.
//
// ЧТО ФИКСИРУЕМ: промпт, заканчивающийся ВНУТРИ открытого блока размышлений,
// обязан включать удержание, и R485 не должен считать это удержание «не про
// размышления» (иначе он его снимет).
package main

import (
	"strings"
	"testing"
)

// qwen35PromptTailR493 — точный хвост промпта из архива стенда.
const qwen35PromptTailR493 = "<|im_start|>user\nhi<|im_end|>\n<|im_start|>assistant\n<think>\n"

// TestR493_PrefilledThinkPromptHoldsReasoning — главный случай из архива.
func TestR493_PrefilledThinkPromptHoldsReasoning(t *testing.T) {
	prompt := "system\nReasoning effort is set to xhigh.\n" + qwen35PromptTailR493
	model := "Qwen3.8-27B-UD-Q4_K_M"

	// Парсер обязан видеть незакрытый блок (иначе и резать нечего).
	if !PromptOpensReasoning(prompt) {
		t.Fatalf("PromptOpensReasoning(prompt) = false — промпт заканчивается на <think>, " +
			"парсер обязан считать блок открытым")
	}
	// Удержание должно включаться: клиент размышления не просил.
	if !reasoningStartHoldR471(prompt, model, false) {
		t.Fatal("reasoningStartHoldR471 = false для промпта с открытым <think> — " +
			"именно из-за этого в архиве размышления уехали в content")
	}
	// R485: удержание про размышления → снимать по таймауту нельзя.
	if !toolsHoldReasoningR485(true, "", prompt, model, false, 0) {
		t.Fatal("toolsHoldReasoningR485 = false при reasoningHold=true — R485 обязан " +
			"держать удержание, иначе таймаут снова сольёт размышления в content")
	}
}

// TestR493_ClosedPromptDoesNotHold — обратная сторона: блок закрыт в промпте
// (модель уже ответила) или размышления не включены — держать нечего.
func TestR493_ClosedPromptDoesNotHold(t *testing.T) {
	closed := "…<|im_start|>assistant\n<think>\nмысли\n</think>\n"
	if PromptOpensReasoning(closed) {
		t.Fatal("блок закрыт в промпте — PromptOpensReasoning должен быть false")
	}
	if reasoningStartHoldR471(closed, "Qwen3.8-27B-UD-Q4_K_M", false) {
		t.Fatal("закрытый блок не должен включать удержание")
	}
	plain := "…<|im_start|>assistant\n"
	if reasoningStartHoldR471(plain, "Qwen3.8-27B-UD-Q4_K_M", false) {
		t.Fatal("промпт без включённых размышлений не должен держать вывод")
	}
}

// TestR493_ClientAskedReasoningNoHold — если клиент просил размышления (echo),
// он получает их в своём поле; удержание не нужно (прежнее поведение сохранено).
func TestR493_ClientAskedReasoningNoHold(t *testing.T) {
	prompt := "system\n" + qwen35PromptTailR493
	if reasoningStartHoldR471(prompt, "Qwen3.8-27B-UD-Q4_K_M", true) {
		t.Fatal("echo=true: удержание не нужно, клиент сам просил размышления")
	}
}

// TestR493_ExistingEnableMarkersStillWork — прежние маркеры включения не сломаны
// (gemma `<|think|>`, qwen `enable_thinking`): здесь менялось только добавление.
func TestR493_ExistingEnableMarkersStillWork(t *testing.T) {
	cases := []struct{ name, prompt, model string }{
		{"gemma <|think|>", "system <|think|>\nuser hi\nassistant\n", "gemma-4-E4B"},
		{"qwen enable_thinking", "system enable_thinking=true\nuser hi\nassistant\n", "Qwen3.8-27B-UD-Q4_K_M"},
	}
	for _, c := range cases {
		if !reasoningStartHoldR471(c.prompt, c.model, false) {
			t.Errorf("%s: прежний маркер включения перестал включать удержание", c.name)
		}
	}
}

// TestR493_HoldTriggerIsNotWordBased — guard против «починки по словам»:
// упоминание слова thinking в инструкции не должно включать удержание.
func TestR493_HoldTriggerIsNotWordBased(t *testing.T) {
	prompt := "system: think step by step, then answer\n<|im_start|>assistant\n"
	if strings.Contains(prompt, "<think>") {
		t.Fatal("условие теста не выполнено")
	}
	if reasoningStartHoldR471(prompt, "Qwen3.8-27B-UD-Q4_K_M", false) {
		t.Fatal("обычная инструкция «think step by step» не должна включать удержание — " +
			"это была бы эвристика по словам")
	}
}
