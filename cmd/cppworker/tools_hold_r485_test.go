// tools_hold_r485_test.go — R485 (2026-11-10): сторож R482 не должен снимать
// удержание РАЗМЫШЛЕНИЙ в content.
//
// Живой архив второго стенда (A10 ×2, Qwen3.8-27B, клиент ai-sdk с 16 tools):
// в истории запроса assistant-сообщения несут content_len=65109 / 18720 / 12113
// ВМЕСТЕ с tool_calls, и текст начинается как планирование («Alright. Let me
// check the target name…»). Это размышления, снятые сторожем R482 и уехавшие в
// content: клиент сохранил их как прошлый ответ модели, и следующий ход начался
// с этого загрязнения («замыкается в себя», инструменты всё хуже).
package main

import "testing"

// TestR485_ReasoningHoldKept — открытый блок размышлений: удержание не снимаем.
func TestR485_ReasoningHoldKept(t *testing.T) {
	if !toolsHoldReasoningR485(true, "любой текст", "", "qwen3.8-27b", false, 0) {
		t.Fatal("открытый reasoning-блок — снимать удержание нельзя (размышления уйдут в content)")
	}
}

// TestR485_ProseReleased — обычная проза без размышлений: сторож работает как в R482.
func TestR485_ProseReleased(t *testing.T) {
	if toolsHoldReasoningR485(false, "Готово. Файл создан.", "простой промпт", "llama-3.1-8b", false, 0) {
		t.Fatal("обычная проза не должна считаться размышлениями — ход обязан снова стать потоковым")
	}
}

// TestR485_MarkerAtStart — модель сама открыла канал размышлений в начале вывода.
func TestR485_MarkerAtStart(t *testing.T) {
	buf := "  \n<|channel>thought I should read TASK.md"
	if !toolsHoldReasoningR485(false, buf, "", "qwen3.8-27b", false, 0) {
		t.Fatal("открывающий маркер размышлений в начале вывода — удержание")
	}
	if !toolsHoldReasoningR485(false, "  <think>reasoning", "", "some-model", false, 0) {
		t.Fatal("<think> в начале вывода — удержание")
	}
}

// TestR485_ImpliedOpenR471 — маркера нет, но канал открыт шаблоном (R471).
func TestR485_ImpliedOpenR471(t *testing.T) {
	prompt := "<|im_start|>system\nenable_thinking=true<|im_end|>"
	if !toolsHoldReasoningR485(false, "Alright. Let me check the target name", prompt, "qwen3.8-27B-UD-Q4_K_M", false, 0) {
		t.Fatal("reasoning-модель с включёнными в промпте размышлениями — начало вывода держим")
	}
}

// TestR485_EchoRequested — клиент сам просил размышления: отдельного удержания
// (и отдельного решения) не нужно, размышления и так уходят в поле thinking.
func TestR485_EchoRequested(t *testing.T) {
	prompt := "<|im_start|>system\nenable_thinking=true<|im_end|>"
	if toolsHoldReasoningR485(false, "проза", prompt, "qwen3.8-27b", true, 0) {
		t.Fatal("think=true: клиент просил размышления — удержание не наш случай")
	}
}

// TestR485_EmittedAlready — клиенту уже ушло начало ответа: удержание начинать поздно.
func TestR485_EmittedAlready(t *testing.T) {
	if toolsHoldReasoningR485(false, "<|channel>thought дальнейший текст", "", "qwen3.8-27b", false, 10) {
		t.Fatal("после отданных байт маркер в середине вывода — уже не «начало размышлений»")
	}
}

// TestR485_ReleaseSwitchOffByDefault — аварийный тумблер по умолчанию выключен.
func TestR485_ReleaseSwitchOffByDefault(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_HOLD_RELEASE_REASONING", "")
	if toolsHoldReleaseReasoningR485() {
		t.Fatal("по умолчанию reasoning-удержание снимать нельзя (R485)")
	}
	for _, v := range []string{"1", "on", "TRUE", "yes"} {
		t.Setenv("CPPWORKER_TOOLS_HOLD_RELEASE_REASONING", v)
		if !toolsHoldReleaseReasoningR485() {
			t.Fatalf("значение %q должно включать аварийный тумблер", v)
		}
	}
	for _, v := range []string{"0", "off", "false", "no"} {
		t.Setenv("CPPWORKER_TOOLS_HOLD_RELEASE_REASONING", v)
		if toolsHoldReleaseReasoningR485() {
			t.Fatalf("значение %q не должно включать тумблер", v)
		}
	}
}
