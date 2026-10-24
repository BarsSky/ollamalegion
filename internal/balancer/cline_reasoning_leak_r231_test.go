// cline_reasoning_leak_r231_test.go — R231 (2026-10-24): вторая линия обороны
// балансера против маркеров размышлений в ВИДИМОМ канале.
//
// Живая жалоба Cline: «модель иногда возвращает в основном теле ответа
// </think>». cppworker чистит маркеры сам (R114b/R231), но клиент не должен
// зависеть от версии воркера: балансер обязан не пропускать служебные маркеры
// в delta.content/message.content, сохраняя текст и пробелы (клиент склеивает
// дельты).
//
// ВАЖНО: filterOpenAIStreamingLine принимает SSE-строку "data: {...}" и
// возвращает (строка, modified). Именно на этой развилке была живая ошибка
// вызова в llamacpp_transport.go (передавали голый JSON → фильтр был no-op;
// modified трактовался как shouldSkip → чанк выбрасывался).
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decodeSSEContent(t *testing.T, line []byte) map[string]interface{} {
	t.Helper()
	trimmed := strings.TrimSpace(string(line))
	trimmed = strings.TrimPrefix(trimmed, "data:")
	trimmed = strings.TrimSpace(trimmed)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		t.Fatalf("невалидный JSON после фильтра: %v (%s)", err, line)
	}
	return parsed
}

// TestR231_FilterStripsReasoningMarkersFromContent — маркеры вырезаются из
// delta.content, остальные поля чанка сохраняются.
func TestR231_FilterStripsReasoningMarkersFromContent(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		wantContent string
	}{
		{"закрывающий маркер", "ответ</think>", "ответ"},
		{"маркер в конце строки", "строка 1</think>\nстрока 2", "строка 1\nстрока 2"},
		{"дубль закрытия", "ответ</think></think>", "ответ"},
		{"gemma-4 канал", "<|channel>thought\nплан<channel|>ответ", "планответ"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			chunk := "data: " + `{"id":"x","choices":[{"index":0,"delta":{"content":` +
				mustJSONString(c.content) + `},"finish_reason":null}]}`
			filtered, modified := filterOpenAIStreamingLine([]byte(chunk))
			if !modified {
				t.Fatalf("фильтр обязан отметить изменение (маркер был в content)")
			}
			if filtered == nil {
				t.Fatalf("чанк потерян")
			}
			got := decodeSSEContent(t, filtered)
			choices := got["choices"].([]interface{})
			delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
			content, _ := delta["content"].(string)
			if content != c.wantContent {
				t.Fatalf("content=%q, want %q", content, c.wantContent)
			}
			if strings.Contains(content, "</think>") || strings.Contains(content, "<channel|>") {
				t.Fatalf("маркер остался в content: %q", content)
			}
		})
	}
}

// TestR231_FilterKeepsReasoningChannelUntouched — reasoning_content не трогаем:
// это отдельный канал, и чистить его должен воркер, а не граница клиента.
func TestR231_FilterKeepsReasoningChannelUntouched(t *testing.T) {
	chunk := "data: " + `{"choices":[{"delta":{"reasoning_content":"шаг <think>внутри</think>"}}]}`
	filtered, modified := filterOpenAIStreamingLine([]byte(chunk))
	if modified {
		t.Fatalf("reasoning-чанк не должен изменяться")
	}
	if string(filtered) != chunk {
		t.Fatalf("reasoning-чанк изменён:\n in: %s\nout: %s", chunk, filtered)
	}
}

// TestR231_FilterPreservesDoneMarker — терминатор [DONE] проходит байт-в-байт
// (иначе клиент не узнает о конце потока).
func TestR231_FilterPreservesDoneMarker(t *testing.T) {
	line := []byte("data: [DONE]")
	filtered, modified := filterOpenAIStreamingLine(line)
	if modified {
		t.Fatal("[DONE] не должен модифицироваться")
	}
	if string(filtered) != string(line) {
		t.Fatalf("[DONE] изменён: %q", filtered)
	}
}

// TestR231_FilterLeavesPlainContentAlone — чанк без служебных токенов
// возвращается без изменений (байт-в-байт).
func TestR231_FilterLeavesPlainContentAlone(t *testing.T) {
	chunk := "data: " + `{"choices":[{"delta":{"content":"hello world"}}]}`
	filtered, modified := filterOpenAIStreamingLine([]byte(chunk))
	if modified {
		t.Fatal("обычный чанк не должен модифицироваться")
	}
	if string(filtered) != chunk {
		t.Fatalf("обычный чанк изменён: %q", filtered)
	}
}

// TestR231_TranslateNonStreamStripsReasoningMarkers — Ollama-перевод
// (OpenAI→Ollama) тоже не отдаёт маркеры в message.content.
func TestR231_TranslateNonStreamStripsReasoningMarkers(t *testing.T) {
	chunk := `{"choices":[{"delta":{"content":"ответ</think>"}}]}`
	seenReasoning := false
	out := translateOpenAISSEDataToOllama("/api/chat", []byte(chunk), "gemma-4", &seenReasoning, time.Time{}, time.Time{}, "")
	if out == nil {
		t.Fatal("перевод вернул nil")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("невалидный NDJSON: %v (%s)", err, out)
	}
	msg, _ := parsed["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if content != "ответ" {
		t.Fatalf("content=%q, want %q", content, "ответ")
	}
}

func mustJSONString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
