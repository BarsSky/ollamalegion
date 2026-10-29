package balancer

// R266: после вычищения метки текстовый вызов qwen3.8 обязан распознаваться
// штатным детектором (иначе вызов уедет клиенту текстом).
import (
	"strings"
	"testing"
)

func TestR266_QwenToolCallConvertsAfterMarkerStrip(t *testing.T) {
	raw := `<|tool_call|>{"name":"run_commands","arguments":{"query":"grep -n main file.cpp"}}`
	stripped := stripQwenServiceMarkers(raw)
	if strings.Contains(stripped, "<|tool_call|>") {
		t.Fatalf("метка не вычищена: %q", stripped)
	}
	calls, remaining, found := detectAndExtractToolCallsFromContent(stripped)
	if !found || len(calls) == 0 {
		t.Fatalf("вызов не распознан после вычищения метки: stripped=%q remaining=%q", stripped, remaining)
	}
	tc, ok := calls[0].(map[string]interface{})
	if !ok {
		t.Fatalf("неожиданный тип вызова: %T", calls[0])
	}
	fn, _ := tc["function"].(map[string]interface{})
	if name, _ := fn["name"].(string); name != "run_commands" {
		t.Fatalf("имя функции = %q, want run_commands", name)
	}
}
