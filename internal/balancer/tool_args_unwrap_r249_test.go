package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// R249: живой детерминированный дефект gemma-4 (6/6 битых при temperature=0):
// модель завернула весь объект аргументов write в поле content, добавила лишнюю
// ';' и неэкранированные кавычки C++-кода. Клиент такой вызов применить не мог.
func TestR249_UnwrapWriteWrappedInContent(t *testing.T) {
	schema := &toolParamSchema{
		Type:     "object",
		Required: []string{"path", "content"},
		Properties: map[string]*toolParamSchema{
			"path":    {Type: "string"},
			"content": {Type: "string"},
		},
	}
	garbled := `{"content":"{\"content\": \"#include \u003ciostream\u003e\\n\\nint main() {\\n    std::cout \u003c\u003c \\\"hello\\\" \u003c\u003c std::endl;\\n    return 0;\\n}\";\\n\", \"path\": \"C:/work/hello.cpp\"}"}`

	fixed, changed := repairToolArguments(garbled, schema)
	if !changed {
		t.Fatalf("аргументы не восстановлены: %s", garbled)
	}
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(fixed), &got); err != nil {
		t.Fatalf("post-repair не JSON: %v (%s)", err, fixed)
	}
	if got["path"] != "C:/work/hello.cpp" {
		t.Fatalf("path = %v (args=%s)", got["path"], fixed)
	}
	content, _ := got["content"].(string)
	if !strings.Contains(content, "int main()") || !strings.Contains(content, "iostream") {
		t.Fatalf("content потерян или искажён: %q", content)
	}
	if strings.Contains(content, `\"`) {
		t.Fatalf("в content осталось экранирование: %q", content)
	}
	if strings.HasSuffix(strings.TrimSpace(content), ";") {
		t.Fatalf("в content осталась мусорная ';': %q", content)
	}
}

// R249: валидные аргументы не трогаем и не переворачиваем.
func TestR249_UnwrapKeepsValidWriteArgs(t *testing.T) {
	schema := &toolParamSchema{
		Type:     "object",
		Required: []string{"path", "content"},
		Properties: map[string]*toolParamSchema{
			"path":    {Type: "string"},
			"content": {Type: "string"},
		},
	}
	valid := `{"path":"C:/work/hello.cpp","content":"int main(){return 0;}"}`
	fixed, changed := repairToolArguments(valid, schema)
	if changed && fixed != valid {
		t.Fatalf("валидные аргументы изменены: %s -> %s", valid, fixed)
	}
}
