// synthesize_chat_sse_r91_test.go — R91 (2026-10-08): молчаливый пустой ответ
// больше не отдаётся как успех.
//
// ЖИВОЙ ЗАМЕР. Под нагрузкой (6 клиентов, 60 с) 11 из 30 ответов через балансер
// приходили как HTTP 200 с телом вида:
//
//	data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}
//	data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
//	data: [DONE]
//
// то есть ни content, ни tool_calls — «успешный» ответ без текста. Причина в том,
// что synthesizeChatSSE переизлучает буферизованный ответ апстрима как SSE и
// просто пропускал пустой message. Клиент не мог отличить «модель промолчала» от
// «запрос не дошёл».
package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSynthesizeChatSSE_EmptyUpstreamIsError — пустой апстрим → error-чанк.
func TestSynthesizeChatSSE_EmptyUpstreamIsError(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-1","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	rec := httptest.NewRecorder()
	synthesizeChatSSE(rec, raw, "m", 200)

	if !strings.Contains(rec.Body.String(), `"finish_reason":"error"`) {
		t.Fatalf("пустой ответ не превращён в ошибку: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"finish_reason":"stop"`) {
		t.Errorf("клиент снова получил успешную остановку без текста: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "empty completion") {
		t.Errorf("в ошибке нет причины: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Error("поток не закрыт маркером [DONE]")
	}
}

// TestSynthesizeChatSSE_ContentStillPassesThrough — обычный ответ не ломается.
func TestSynthesizeChatSSE_ContentStillPassesThrough(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-2","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"привет"},"finish_reason":"stop"}]}`)
	rec := httptest.NewRecorder()
	synthesizeChatSSE(rec, raw, "m", 200)

	body := rec.Body.String()
	if !strings.Contains(body, `"content":"привет"`) {
		t.Fatalf("контент потерялся: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Errorf("нормальный финал должен остаться stop: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"error"`) {
		t.Errorf("нормальный ответ помечен ошибкой: %s", body)
	}
}

// TestSynthesizeChatSSE_ToolCallsStillPassThrough — ответ с tool_calls без текста
// (нормальный случай вызова инструмента) ошибкой НЕ считается.
func TestSynthesizeChatSSE_ToolCallsStillPassThrough(t *testing.T) {
	raw := []byte(`{"id":"chatcmpl-3","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"generate_image","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	rec := httptest.NewRecorder()
	synthesizeChatSSE(rec, raw, "m", 200)

	body := rec.Body.String()
	if !strings.Contains(body, "generate_image") {
		t.Fatalf("tool_calls потерялись: %s", body)
	}
	if strings.Contains(body, `"finish_reason":"error"`) {
		t.Errorf("вызов инструмента ошибочно признан пустым ответом: %s", body)
	}
}

// TestSynthesizeChatSSE_EmptyDocIsError — даже без choices вовсе (совсем пустой
// документ) клиент должен увидеть ошибку, а не тишину.
func TestSynthesizeChatSSE_EmptyDocIsError(t *testing.T) {
	rec := httptest.NewRecorder()
	synthesizeChatSSE(rec, []byte(`{}`), "m", 200)

	var sawError bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			continue
		}
		ch, _ := chunk["choices"].([]interface{})
		if len(ch) == 0 {
			continue
		}
		first, _ := ch[0].(map[string]interface{})
		if fr, _ := first["finish_reason"].(string); fr == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("пустой документ отдан как успех: %s", rec.Body.String())
	}
}
