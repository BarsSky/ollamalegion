package balancer

// qwen_tool_markers_r266_test.go — R266 (2026-10-28): служебные метки qwen3.8
// не должны попадать в видимый ответ клиенту, а обрыв маркера между чанками не
// должен оставлять половину метки в тексте.
import (
	"strings"
	"testing"
)

func TestR266_StripsQwenServiceMarkers(t *testing.T) {
	in := "</think> <|tool|> tool_call_result(run_commands): [{\"query\":\"grep\"}]<|assistant|> The file has ~348 lines."
	got := stripQwenServiceMarkers(in)
	for _, bad := range []string{"<|tool|>", "<|assistant|>"} {
		if strings.Contains(got, bad) {
			t.Fatalf("метка %q осталась в тексте: %q", bad, got)
		}
	}
	// Содержательный текст обязан сохраниться (не «вырезали всё»).
	if !strings.Contains(got, "tool_call_result") || !strings.Contains(got, "The file has ~348 lines") {
		t.Fatalf("потерян содержательный текст: %q", got)
	}
}

func TestR266_KeepsProseWithAngleBrackets(t *testing.T) {
	for _, in := range []string{
		"if (a < b && c > d) return;",
		"vector<int> v; // шаблон C++",
		"текст со < скобкой",
	} {
		if got := stripQwenServiceMarkers(in); got != in {
			t.Fatalf("проза изменена: %q -> %q", in, got)
		}
	}
}

func TestR266_SplitMarkerSuffixDetection(t *testing.T) {
	cases := map[string]bool{
		"<|tool_":        true,
		"text <|tool_ca": true,
		"<|assis":        true,
		"обычный текст":  false,
		"x < y":          false,
		"<|tool|>":       false, // целая метка — не «хвост»
	}
	for in, want := range cases {
		if got := splitMarkerSuffixLen(in) > 0; got != want {
			t.Errorf("splitMarkerSuffixLen(%q) > 0 = %v, want %v", in, got, want)
		}
	}
}

func TestR266_ToolCallBufferLimitEnv(t *testing.T) {
	t.Setenv("LB_TOOL_CALL_BUFFER_BYTES", "")
	if got := toolCallBufferLimit(); got != 64*1024 {
		t.Fatalf("дефолт = %d, want 64 КиБ", got)
	}
	t.Setenv("LB_TOOL_CALL_BUFFER_BYTES", "131072")
	if got := toolCallBufferLimit(); got != 131072 {
		t.Fatalf("env не применён: %d", got)
	}
	t.Setenv("LB_TOOL_CALL_BUFFER_BYTES", "1024") // меньше минимума — дефолт
	if got := toolCallBufferLimit(); got != 64*1024 {
		t.Fatalf("мусорное значение должно давать дефолт, got %d", got)
	}
}
