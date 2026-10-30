// native_done_frame_r331_test.go — R331 (2026-10-30).
//
// Пять правил решали судьбу message.content в завершающем кадре; на нативном пути
// по проводу кадр правили ДВА слоя подряд (stripNativeDoneContent и
// nativeDoneHygieneR321), и каждый не знал о другом. Живая регрессия: strip
// очищал content, hygiene видела «пустой ответ после размышлений» и вставляла
// пояснение поверх нормального завершения — оператор увидел это как «в конце
// каждого ответа предупреждение про размышления».
//
// Тесты фиксируют единственного владельца: finalizeNativeDoneFrameR331.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestR331_DuplicateStrippedWithoutNotice — штатное завершение: дубль снят,
// пояснения НЕТ (ровно то, что ломалось в R321).
func TestR331_DuplicateStrippedWithoutNotice(t *testing.T) {
	answer := "Всего 15 яблок. Ответ: 15."
	line := `{"model":"gemma","done":true,"done_reason":"stop","message":{"role":"assistant","content":"` +
		answer + `"}}`

	out, changed := finalizeNativeDoneFrameR331(line, answer, 340, true)
	if !changed {
		t.Fatal("дубль уже отданного текста должен быть снят")
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	msg, _ := m["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if strings.TrimSpace(content) != "" {
		t.Errorf("content = %q, want пусто: текст уже уехал дельтами", content)
	}
	if strings.Contains(content, "только размышления") {
		t.Error("вставлено пояснение о размышлениях поверх нормального завершения — это регрессия R321")
	}
	if m["done_reason"] != "stop" {
		t.Errorf("done_reason = %v, want stop (ответ пришёл целиком)", m["done_reason"])
	}
}

// TestR331_ReasoningOnlyGetsReasonAndNotice — клиент не видел ничего: причина и
// пояснение появляются, и это единственная копия текста.
func TestR331_ReasoningOnlyGetsReasonAndNotice(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"stop",` +
		`"message":{"role":"assistant","content":"","thinking":"…"}}`

	out, changed := finalizeNativeDoneFrameR331(line, "", 340, true)
	if !changed {
		t.Fatal("пустое завершение после размышлений обязано получить причину и пояснение")
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("результат не JSON: %v", err)
	}
	if m["done_reason"] != "length" {
		t.Errorf("done_reason = %v, want length", m["done_reason"])
	}
	msg, _ := m["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if !strings.Contains(content, "340") {
		t.Errorf("в пояснении нет числа символов размышлений: %q", content)
	}
}

// TestR331_Idempotent — второй проход ничего не меняет (кадр может пройти
// обработку дважды при ретраях).
func TestR331_Idempotent(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"stop",` +
		`"message":{"role":"assistant","content":"","thinking":"…"}}`
	once, changed := finalizeNativeDoneFrameR331(line, "", 340, true)
	if !changed {
		t.Fatal("первый проход не сработал")
	}
	twice, changed2 := finalizeNativeDoneFrameR331(once, "", 340, true)
	if changed2 || twice != once {
		t.Errorf("второй проход изменил кадр:\n once %q\n twice %q", once, twice)
	}
}

// TestR331_StrippingDisabledKeepsWorkerText — LB_OLLAMA_DONE_CONTENT=keep:
// текст воркера не трогаем (только причина, если она неконтрактная).
func TestR331_StrippingDisabledKeepsWorkerText(t *testing.T) {
	answer := "Всего 15 яблок."
	line := `{"model":"gemma","done":true,"done_reason":"stop","message":{"role":"assistant","content":"` +
		answer + `"}}`

	out, changed := finalizeNativeDoneFrameR331(line, answer, 0, false)
	if changed || out != line {
		t.Errorf("при выключенном strip кадр изменён: %q", out)
	}
}

// TestR331_ToolCallsUntouched — вызов инструмента не переписывается.
func TestR331_ToolCallsUntouched(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"tool_calls",` +
		`"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"read_file"}}]}}`
	out, changed := finalizeNativeDoneFrameR331(line, "", 500, true)
	if changed || out != line {
		t.Errorf("кадр с tool_calls изменён: %q", out)
	}
}
