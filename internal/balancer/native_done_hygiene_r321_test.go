// native_done_hygiene_r321_test.go — R321 (2026-10-30).
//
// ЖИВАЯ ЖАЛОБА (Cline через балансер): поток заканчивается «прямо посреди
// рассуждений», клиент получает done:true + done_reason:"stop" с пустым
// message.content и считает ход полноценно завершённым — остаток ответа теряется,
// а следующий запрос «продолжи» показывается как ответ.
//
// Правило контракта (Ollama): "stop" = модель закончила; остановка по бюджету
// вывода = "length". Эти тесты фиксируют нормализацию на стороне балансера —
// она нужна, пока в кластере может стоять образ воркера СТАРШЕ правки R320.
package balancer

import (
	"encoding/json"
	"strings"
	"testing"
)

// decodeR321 — разобрать NDJSON-строку в map для проверок.
func decodeR321(t *testing.T, line string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("строка не JSON: %v (%q)", err, line)
	}
	return m
}

// TestR321_DoneReasonStopWithReasoningBecomesLength — главный живой случай.
func TestR321_DoneReasonStopWithReasoningBecomesLength(t *testing.T) {
	line := `{"model":"gemma","created_at":"2026-10-30T10:00:00Z","done":true,` +
		`"done_reason":"stop","message":{"role":"assistant","content":""}}`

	out, changed := nativeDoneHygieneR321(line, 459)
	if !changed {
		t.Fatal("пустое завершение после размышлений обязано быть исправлено: клиент иначе считает ход законченным")
	}
	m := decodeR321(t, out)
	if m["done_reason"] != "length" {
		t.Errorf("done_reason = %v, want length (контракт Ollama: обрезано по бюджету)", m["done_reason"])
	}
	msg := m["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if strings.TrimSpace(content) == "" {
		t.Error("content остался пустым — клиент снова увидит «Model returned empty response» без причины")
	}
	if !strings.Contains(content, "459") {
		t.Errorf("в пояснении нет числа символов размышлений: %q", content)
	}
	if msg["role"] != "assistant" {
		t.Errorf("role = %v, want assistant (клиенты строго читают это поле)", msg["role"])
	}
}

// TestR321_IdempotentWhenWorkerAlreadyFixed — исправленный воркер (R320) ничего
// не требует: слой не должен переписывать честное "length".
func TestR321_IdempotentWhenWorkerAlreadyFixed(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"length",` +
		`"message":{"role":"assistant","content":"⚠️ Модель сгенерировала только размышления (459 симв.)"}}`
	out, changed := nativeDoneHygieneR321(line, 459)
	if changed {
		t.Fatalf("честный done_reason=length переписан: %q", out)
	}
	if out != line {
		t.Errorf("строка изменилась без причины:\n got %q\nwant %q", out, line)
	}
}

// TestR321_LeavesRealAnswersAlone — чужой ответ не трогаем.
func TestR321_LeavesRealAnswersAlone(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"stop",` +
		`"message":{"role":"assistant","content":"Всего 15 яблок."}}`
	if out, changed := nativeDoneHygieneR321(line, 459); changed {
		t.Errorf("непустой ответ переписан: %q", out)
	}
}

// TestR321_IgnoresStreamsWithoutReasoning — пустой ответ БЕЗ размышлений — это
// другая история (R261/R314), и здесь её решать нельзя.
func TestR321_IgnoresStreamsWithoutReasoning(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"stop","message":{"role":"assistant","content":""}}`
	if _, changed := nativeDoneHygieneR321(line, 0); changed {
		t.Error("без размышлений подменять причину нельзя: это не «бюджет ушёл в reasoning»")
	}
}

// TestR321_HandlesGenerateResponseField — /api/generate держит текст в "response".
func TestR321_HandlesGenerateResponseField(t *testing.T) {
	line := `{"model":"gemma","done":true,"done_reason":"stop","response":""}`
	out, changed := nativeDoneHygieneR321(line, 120)
	if !changed {
		t.Fatal("/api/generate: пустой response после размышлений обязан получить причину")
	}
	m := decodeR321(t, out)
	if m["done_reason"] != "length" {
		t.Errorf("done_reason = %v, want length", m["done_reason"])
	}
	resp, _ := m["response"].(string)
	if strings.TrimSpace(resp) == "" {
		t.Error("response остался пустым — для /api/generate это и есть «пустой ответ»")
	}
}

// TestR321_PreservesOtherDoneReasons — tool_calls/error не подменяются.
func TestR321_PreservesOtherDoneReasons(t *testing.T) {
	for _, reason := range []string{"tool_calls", "error", "cancelled"} {
		line := `{"model":"gemma","done":true,"done_reason":"` + reason + `",` +
			`"message":{"role":"assistant","content":""}}`
		if out, changed := nativeDoneHygieneR321(line, 300); changed {
			t.Errorf("done_reason=%q переписан на %q: причина воркера — факт, её нельзя подменять", reason, out)
		}
	}
}

// TestR321_NonDoneAndBrokenLinesUntouched — промежуточные кадры и мусор.
func TestR321_NonDoneAndBrokenLinesUntouched(t *testing.T) {
	cases := []string{
		`{"model":"gemma","done":false,"message":{"role":"assistant","content":""}}`,
		`{"keepalive":true}`,
		`not json at all`,
		``,
	}
	for _, line := range cases {
		if out, changed := nativeDoneHygieneR321(line, 500); changed {
			t.Errorf("строка %q изменена, хотя это не пустое завершение: %q", line, out)
		}
	}
}
