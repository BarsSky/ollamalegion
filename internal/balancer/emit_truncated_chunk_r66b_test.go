//go:build llama_stub

// emit_truncated_chunk_r66b_test.go — R66b (2026-09-22): терминальный чанк при
// обрыве стрима должен нести ПРИЧИНУ (timeout / client_cancel).
//
// До R66b функция emitTruncatedChunk не логировала событие и не различала
// таймаут и отмену клиентом: сообщение было общим "timeout or cancellation",
// а вызывающий код возвращал nil (чтобы handleChat не дописал второй error-чанк),
// из-за чего обрыв стрима не был виден ни в логах, ни в теле ответа.

package balancer

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEmitTruncatedChunk_OllamaNDJSON_CarriesReason(t *testing.T) {
	const model = "qwen-test"
	for _, tc := range []struct {
		reason   string
		wantText string
	}{
		{"timeout", "stream timeout"},
		{"client_cancel", "client cancelled"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			rec := httptest.NewRecorder()
			emitTruncatedChunk(rec, "/api/chat", model, 1500*time.Millisecond, tc.reason)

			body := strings.TrimSpace(rec.Body.String())
			if body == "" {
				t.Fatal("пустое тело: терминальный чанк не отправлен")
			}
			var chunk map[string]interface{}
			if err := json.Unmarshal([]byte(body), &chunk); err != nil {
				t.Fatalf("NDJSON-чанк не парсится (%v): %s", err, body)
			}
			if done, _ := chunk["done"].(bool); !done {
				t.Errorf("done != true: %v", chunk["done"])
			}
			if reason, _ := chunk["done_reason"].(string); reason != "truncated" {
				t.Errorf("done_reason = %q, ожидалось truncated", reason)
			}
			if got, _ := chunk["reason"].(string); got != tc.reason {
				t.Errorf("reason = %q, ожидалось %q", got, tc.reason)
			}
			errText, _ := chunk["error"].(string)
			if !strings.Contains(errText, tc.wantText) {
				t.Errorf("error = %q, ожидалось упоминание %q", errText, tc.wantText)
			}
			if m, _ := chunk["model"].(string); m != model {
				t.Errorf("model = %q, ожидалось %q", m, model)
			}
		})
	}
}

func TestEmitTruncatedChunk_OpenAISSE_CarriesReason(t *testing.T) {
	rec := httptest.NewRecorder()
	emitTruncatedChunk(rec, "/v1/chat/completions", "qwen-test", 2*time.Second, "timeout")

	body := rec.Body.String()
	if !strings.Contains(body, "data: ") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("ожидался SSE-формат с [DONE], получено: %s", body)
	}
	// Первая data:-строка — JSON-чанк с finish_reason=truncated и reason.
	first := strings.SplitN(strings.TrimPrefix(body, "data: "), "\n", 2)[0]
	var chunk map[string]interface{}
	if err := json.Unmarshal([]byte(first), &chunk); err != nil {
		t.Fatalf("SSE-чанк не парсится (%v): %s", err, first)
	}
	if reason, _ := chunk["reason"].(string); reason != "timeout" {
		t.Errorf("reason = %q, ожидалось timeout", reason)
	}
	errObj, _ := chunk["error"].(map[string]interface{})
	if errObj == nil {
		t.Fatalf("нет error-объекта: %s", first)
	}
	if r, _ := errObj["reason"].(string); r != "timeout" {
		t.Errorf("error.reason = %q, ожидалось timeout", r)
	}
	if t2, _ := errObj["type"].(string); t2 != "stream_truncated" {
		t.Errorf("error.type = %q, ожидалось stream_truncated", t2)
	}
}
