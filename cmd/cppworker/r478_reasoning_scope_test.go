//go:build llama_stub

// r478_reasoning_scope_test.go — R478 (2026-11-09): R470 не должен гасить
// размышления на ОБЫЧНОМ /api/chat (без tools).
//
// ЖИВОЙ ДЕФЕКТ (OpenWebUI): «модель генерирует токены, ответ не доходит».
// Гейт R470 (`rcSendReasoning && reasoningEcho`) стоял в общей функции
// writeChatStreamResponse, хотя задуман для tools-пути. У reasoning-моделей
// парсер стартует ВНУТРИ размышлений (шаблон открыл блок в промпте), поэтому
// весь видимый ответ до закрывающего маркера отбрасывался: клиент получал
// пустые кадры и keepalive, а в конце — пояснение вместо текста.
//
// Тест — сторож по исходникам: важно не «как выглядит», а ГДЕ стоит вызов.
package main

import (
	"os"
	"strings"
	"testing"
)

// funcBodyR478 — тело функции по имени (от строки объявления до закрывающей
// скобки на нулевом отступе). Грубо, но для сторожа достаточно и не тянет
// go/ast в пакет main.
func funcBodyR478(t *testing.T, src, header string) string {
	t.Helper()
	idx := strings.Index(src, header)
	if idx < 0 {
		t.Fatalf("в handlers_chat.go нет функции %q", header)
	}
	rest := src[idx:]
	lines := strings.Split(rest, "\n")
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		b.WriteString("\n")
		if strings.HasPrefix(line, "}") {
			break
		}
	}
	return b.String()
}

// TestR478_NonToolsChatDoesNotUseToolsEchoGate — в writeChatStreamResponse не
// должно быть гейта toolsThinkingEchoR470.
func TestR478_NonToolsChatDoesNotUseToolsEchoGate(t *testing.T) {
	data, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatalf("чтение handlers_chat.go: %v", err)
	}
	src := string(data)
	body := funcBodyR478(t, src, "func writeChatStreamResponse(w http.ResponseWriter")

	if strings.Contains(body, "toolsThinkingEchoR470") {
		t.Fatal("в writeChatStreamResponse снова стоит гейт R470: на обычном /api/chat у reasoning-моделей пропадёт видимый ответ")
	}
	if !strings.Contains(body, "ReasoningWantedByClient") {
		t.Fatal("в writeChatStreamResponse нет ReasoningWantedByClient — политика отдачи размышлений не применяется")
	}
}

// TestR478_ToolsChatKeepsToolsEchoGate — на tools-пути гейт обязан остаться:
// там клиент-агент складывает thinking в текст ответа (это отдельный дефект).
func TestR478_ToolsChatKeepsToolsEchoGate(t *testing.T) {
	data, err := os.ReadFile("handlers_chat.go")
	if err != nil {
		t.Fatalf("чтение handlers_chat.go: %v", err)
	}
	src := string(data)
	body := funcBodyR478(t, src, "func writeChatStreamResponseWithTools(")

	if !strings.Contains(body, "toolsThinkingEchoR470") {
		t.Fatal("на tools-пути пропал гейт R470 — Cline снова будет получать размышления как текст ответа")
	}
}

// TestR478_AbsentThinkIsNotThinkFalse — «поле think не прислано» не то же
// самое, что «think=false»: по умолчанию размышления клиенту нужны.
func TestR478_AbsentThinkIsNotThinkFalse(t *testing.T) {
	if !ReasoningWantedByClient(nil) {
		t.Fatal("ReasoningWantedByClient(nil) = false: отсутствие поля think стало запретом")
	}
	no := false
	if ReasoningWantedByClient(&no) {
		t.Fatal("ReasoningWantedByClient(think=false) = true")
	}
	yes := true
	if !ReasoningWantedByClient(&yes) {
		t.Fatal("ReasoningWantedByClient(think=true) = false")
	}
}
