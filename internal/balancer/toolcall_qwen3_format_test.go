package balancer

import (
	"encoding/json"
	"testing"
)

// TestDetector_Qwen3ToolCallsPrefix — ЖИВОЙ ДЕФЕКТ (2026-10-06): Qwen3 через
// Open WebUI вернул вызов инструмента ТЕКСТОМ в content:
//
//	[TOOL_CALLS][{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"{...}"}}]
//
// Клиент получил это как обычный текст, картинка не нарисовалась. Тест фиксирует,
// распознаёт ли это существующий детектор форм и извлекает ли имя инструмента.
func TestDetector_Qwen3FormatNestedFunction(t *testing.T) {
	args := `{\"prompt\":\"Сказочный лес с светящимися деревьями\",\"model\":\"stable-diffusion.cpp\",\"negative_prompt\":\"чёрные тени, ужасы\",\"steps\":30,\"width\":768,\"height\":512,\"seed\":42}`
	content := `[TOOL_CALLS][{"id":"call_generate_image","type":"function","function":{"name":"generate_image","arguments":"` + args + `"}}]`

	calls, remaining, found := detectAndExtractToolCallsFromContent(content)
	t.Logf("found=%v remaining=%q calls=%d", found, remaining, len(calls))
	for i, c := range calls {
		entry, _ := c.(map[string]interface{})
		fn, _ := entry["function"].(map[string]interface{})
		name, _ := fn["name"].(string)
		rawArgs, _ := json.Marshal(fn["arguments"])
		t.Logf("  call[%d]: id=%v name=%q args=%s", i, entry["id"], name, rawArgs)
	}

	// Через тот же путь, которым пользуется цикл: message с content.
	msg := map[string]interface{}{"role": "assistant", "content": content}
	imgCalls := extractImageToolCalls(msg)
	t.Logf("extractImageToolCalls -> %d вызов(ов)", len(imgCalls))
	for _, c := range imgCalls {
		t.Logf("  name=%q args=%q", c.Name, c.Arguments)
	}
	if len(imgCalls) == 0 {
		t.Fatalf("вызов модели не распознан — именно это увидел клиент как текст")
	}
	if imgCalls[0].Name != "generate_image" {
		t.Errorf("имя инструмента = %q", imgCalls[0].Name)
	}
	args2, err := parseImageToolArgs(imgCalls[0].Arguments)
	if err != nil {
		t.Fatalf("аргументы не разобраны: %v (%q)", err, imgCalls[0].Arguments)
	}
	if args2.Prompt == "" || args2.Width != 768 || args2.Height != 512 || args2.Steps != 30 {
		t.Errorf("аргументы разобраны неверно: %+v", args2)
	}
	if args2.Model != "stable-diffusion.cpp" {
		t.Errorf("model=%q", args2.Model)
	}
}
