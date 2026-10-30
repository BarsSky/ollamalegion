// done_reason_registry_r328_test.go — R328 (2026-10-30).
//
// Аудит маршрутов: done_reason присваивается в ≥50 точках и принимает 7 разных
// значений, а контракт Ollama знает четыре (stop, length, load, unload) плюс
// content_filter для OpenAI. Наши внутренние коды ("truncated" — обрыв по НАШЕМУ
// таймауту, R297) клиентам не документированы: клиент, разбирающий done_reason,
// видит незнакомое значение и закрывает ход как «модель закончила» — отсюда часть
// жалоб Cline на незавершённые задачи.
//
// Тесты фиксируют: (1) реестр знает ровно контрактные значения; (2) нормализация
// переводит наш "truncated" в "length", пустое — в "stop"; (3) настоящие причины
// воркера (tool_calls/error/cancelled) не подменяются; (4) нативный passthrough
// действительно нормализует кадр.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR328_RegistryMatchesContract — закрытый список значений по поверхностям.
func TestR328_RegistryMatchesContract(t *testing.T) {
	// Ollama: контракт знает stop/length; tool_calls/error/cancelled используются
	// нативной поверхностью воркера и уже поддерживаются клиентами.
	for _, want := range []DoneReason{DoneReasonStop, DoneReasonLength, DoneReasonToolCalls, DoneReasonError, DoneReasonCancelled} {
		if !doneReasonsOllama[want] {
			t.Errorf("Ollama-реестр не содержит %q — клиент получит неконтрактное значение", want)
		}
	}
	// Наш внутренний код НЕ должен быть в списке разрешённых клиенту.
	if doneReasonsOllama[DoneReasonTruncated] {
		t.Error("внутренний код \"truncated\" попал в список для клиента: он не документирован в Ollama API")
	}
	// OpenAI: content_filter — контрактное значение для /v1.
	if !doneReasonsOpenAI[DoneReasonContentFilter] {
		t.Error("OpenAI-реестр не содержит content_filter")
	}
}

// TestR328_Normalize — таблица переводов.
func TestR328_Normalize(t *testing.T) {
	cases := []struct {
		surface string
		in      string
		want    string
		changed bool
	}{
		{"ollama", "stop", "stop", false},
		{"ollama", "length", "length", false},
		{"ollama", "tool_calls", "tool_calls", false},
		{"ollama", "error", "error", false},
		{"ollama", "cancelled", "cancelled", false},
		// Наш внутренний код → контрактное «ответ не дописан».
		{"ollama", "truncated", "length", true},
		// Пустая причина → безопасный stop.
		{"ollama", "", "stop", true},
		// Незнакомый код не отдаём клиенту.
		{"ollama", "some_new_code", "stop", true},
		// OpenAI-поверхность: content_filter разрешён.
		{"openai", "content_filter", "content_filter", false},
		// На OpenAI-поверхности наш truncated тоже становится length.
		{"openai", "truncated", "length", true},
	}
	for _, tc := range cases {
		got, changed := normalizeDoneReasonForSurface(tc.surface, tc.in)
		if got != tc.want || changed != tc.changed {
			t.Errorf("normalize(%q, %q) = (%q, %v), want (%q, %v)",
				tc.surface, tc.in, got, changed, tc.want, tc.changed)
		}
	}
}

// TestR328_NativeFrameNormalizesTruncated — живой кадр нашего обрыва.
func TestR328_NativeFrameNormalizesTruncated(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"truncated",` +
		`"message":{"role":"assistant","content":"Всего 15 яблок."}}`
	out, changed := nativeDoneHygieneR321(line, 0, 42)
	if !changed {
		t.Fatal("кадр с неконтрактным done_reason=\"truncated\" не нормализован")
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if m["done_reason"] != "length" {
		t.Errorf("done_reason = %v, want length (truncated — наш внутренний код)", m["done_reason"])
	}
	// Ответ не должен потеряться при нормализации причины.
	msg, _ := m["message"].(map[string]interface{})
	if c, _ := msg["content"].(string); !strings.Contains(c, "15 яблок") {
		t.Errorf("content потерян при нормализации: %q", c)
	}
}

// TestR328_WorkerReasonsPreserved — причины воркера не подменяются.
func TestR328_WorkerReasonsPreserved(t *testing.T) {
	for _, reason := range []string{"stop", "length", "tool_calls", "error", "cancelled"} {
		line := `{"model":"gemma","done":true,"done_reason":"` + reason + `",` +
			`"message":{"role":"assistant","content":"ответ"}}`
		out, changed := nativeDoneHygieneR321(line, 0, 5)
		if changed {
			t.Errorf("done_reason=%q переписан: %q", reason, out)
		}
		if out != line {
			t.Errorf("строка изменена без причины для done_reason=%q", reason)
		}
	}
}
