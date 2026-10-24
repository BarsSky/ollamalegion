// toolcall_angle_form_test.go — TOOLCALL-ANGLE: живой дефект (скриншот оператора).
//
// DeepSeek Harness через балансер (OpenAI-поверхность /v1/chat/completions,
// модель gemma-4) получил в ТЕКСТЕ ответа сырую разметку вызова инструмента:
//
//	<tool_call> <function=bash> <parameter=command> pwd && ls -la ... </parameter>
//	<parameter=description> ... </parameter> </function> </tool_call>
//
// Инструмент не вызвался: детекторы знали Hermes только как `<tool_call>` + JSON,
// а angle-форму (`<function=NAME><parameter=KEY>VALUE</parameter>`) не разбирал
// никто. findHermesToolCallBlocks после `<tool_call>` искал `{`/`[`, видел `<`
// и делал break → found=false.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// angleFormScreenshot — ТОЧНАЯ строка из тела ответа в живой жалобе: модель
// склеила angle-разметку вызова пробелами в одну строку и отдала её как content.
const angleFormScreenshot = `<tool_call> <function=bash> <parameter=command> pwd && ls -la </parameter> <parameter=description> Показать содержимое рабочего каталога </parameter> </function> </tool_call>`

// angleFormNewlines — та же форма, но с переводами строк (как модель выводит
// обычно): обрамляющие переводы строк внутри <parameter> должны быть сняты.
const angleFormNewlines = "Сейчас посмотрю каталог.\n<tool_call>\n<function=bash>\n" +
	"<parameter=command>\npwd && ls -la\n</parameter>\n" +
	"<parameter=description>\nПоказать содержимое рабочего каталога\n</parameter>\n" +
	"</function>\n</tool_call>"

func TestDetectAndExtractToolCallsFromContent_AngleForm(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		wantFound     bool
		wantCount     int
		wantNames     []string
		wantName      string
		wantArgs      map[string]string
		wantRemaining string
	}{
		{
			name:          "screenshot: точная строка angle-формы",
			content:       angleFormScreenshot,
			wantFound:     true,
			wantCount:     1,
			wantName:      "bash",
			wantArgs:      map[string]string{"command": "pwd && ls -la", "description": "Показать содержимое рабочего каталога"},
			wantRemaining: "",
		},
		{
			name:          "angle-форма с переводами строк и прозой вокруг",
			content:       angleFormNewlines,
			wantFound:     true,
			wantCount:     1,
			wantName:      "bash",
			wantArgs:      map[string]string{"command": "pwd && ls -la", "description": "Показать содержимое рабочего каталога"},
			wantRemaining: "Сейчас посмотрю каталог.",
		},
		{
			name: "два angle-вызова подряд",
			content: "<tool_call> <function=bash> <parameter=command> pwd </parameter> </function> </tool_call>" +
				"<tool_call> <function=read_file> <parameter=path> /etc/hosts </parameter> </function> </tool_call>",
			wantFound:     true,
			wantCount:     2,
			wantNames:     []string{"bash", "read_file"},
			wantName:      "bash",
			wantArgs:      map[string]string{"command": "pwd"},
			wantRemaining: "",
		},
		{
			name:          "оборванный angle-блок без </tool_call>",
			content:       `<tool_call> <function=bash> <parameter=command> pwd </parameter> </function>`,
			wantFound:     true,
			wantCount:     1,
			wantName:      "bash",
			wantArgs:      map[string]string{"command": "pwd"},
			wantRemaining: "",
		},
		{
			name:          "обычный текст — не вызов",
			content:       "Просто ответ модели без вызова инструмента",
			wantFound:     false,
			wantCount:     0,
			wantRemaining: "Просто ответ модели без вызова инструмента",
		},
		{
			name:          "angle-подобный текст внутри JSON-ответа не съедается",
			content:       `Ответ: {"note":"<function=x>"}`,
			wantFound:     false,
			wantCount:     0,
			wantRemaining: `Ответ: {"note":"<function=x>"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			toolCalls, remaining, found := detectAndExtractToolCallsFromContent(tt.content)
			if found != tt.wantFound {
				t.Fatalf("found = %v, ожидалось %v (остаток %q)", found, tt.wantFound, remaining)
			}
			if !found {
				if remaining != tt.wantRemaining {
					t.Errorf("remaining = %q, ожидалось %q", remaining, tt.wantRemaining)
				}
				return
			}
			if len(toolCalls) != tt.wantCount {
				t.Fatalf("вызовов %d, ожидалось %d", len(toolCalls), tt.wantCount)
			}
			tcMap, ok := toolCalls[0].(map[string]interface{})
			if !ok {
				t.Fatalf("toolCalls[0] не map: %T", toolCalls[0])
			}
			if tcMap["type"] != "function" {
				t.Errorf("type = %v, ожидалось function", tcMap["type"])
			}
			fn, ok := tcMap["function"].(map[string]interface{})
			if !ok {
				t.Fatalf("toolCalls[0].function не map: %T", tcMap["function"])
			}
			if fn["name"] != tt.wantName {
				t.Errorf("name = %v, ожидалось %q", fn["name"], tt.wantName)
			}
			// arguments обязан быть JSON-СТРОКОЙ (требование OpenAI API).
			argsStr, ok := fn["arguments"].(string)
			if !ok {
				t.Fatalf("arguments не строка: %T", fn["arguments"])
			}
			var args map[string]interface{}
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				t.Fatalf("arguments не валидный JSON: %v (%q)", err, argsStr)
			}
			for k, want := range tt.wantArgs {
				if got, _ := args[k].(string); got != want {
					t.Errorf("arguments[%q] = %v, ожидалось %q", k, args[k], want)
				}
			}
			if len(args) != len(tt.wantArgs) {
				t.Errorf("arguments = %v, ожидалось ровно %d ключей", args, len(tt.wantArgs))
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, ожидалось %q", remaining, tt.wantRemaining)
			}
			// Разметки вызова в остатке быть не должно.
			for _, marker := range []string{"<tool_call>", "<function=", "<parameter=", "</function>"} {
				if strings.Contains(remaining, marker) {
					t.Errorf("в remaining осталась разметка %q: %q", marker, remaining)
				}
			}
			// Все вызовы разобраны по порядку (важно для нескольких блоков).
			for i, wantName := range tt.wantNames {
				if i >= len(toolCalls) {
					break
				}
				m, _ := toolCalls[i].(map[string]interface{})
				f, _ := m["function"].(map[string]interface{})
				if f == nil || f["name"] != wantName {
					t.Errorf("toolCalls[%d].function.name = %v, ожидалось %q", i, f["name"], wantName)
				}
			}
		})
	}
}
