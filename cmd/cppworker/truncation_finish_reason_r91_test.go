//go:build llama_stub

// truncation_finish_reason_r91_test.go — R91 (2026-10-09): finish_reason честно
// сообщает об обрыве по капу max_tokens, а незакрытый reasoning-блок не доезжает
// до клиента пустым ответом.
//
// Живой факт на стенде (gemma-4-E4B-it-Q4_K_M, max_tokens=96): модель тратит
// весь бюджет внутри незакрытого <reasoning>, воркер отдаёт content="", а
// finish_reason="stop" — клиент считает ответ полным. При этом авто-продолжение
// балансера (LB_AUTO_CONTINUE_ON_TRUNCATION=1) ждёт именно "length".
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// postV1ChatNonStream — /v1/chat/completions с stream=false.
func postV1ChatNonStream(t *testing.T, url string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(url+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, string(raw))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("ответ не JSON: %v (%s)", err, string(raw))
	}
	return out
}

func firstChoice(t *testing.T, resp map[string]interface{}) map[string]interface{} {
	t.Helper()
	choices, _ := resp["choices"].([]interface{})
	if len(choices) == 0 {
		t.Fatalf("в ответе нет choices: %v", resp)
	}
	c, _ := choices[0].(map[string]interface{})
	return c
}

// TestR91_NonStream_TruncatedByMaxTokens_ReportsLength — кап исчерпан → length.
func TestR91_NonStream_TruncatedByMaxTokens_ReportsLength(t *testing.T) {
	defer withStubEmitTokens(t, []string{"Это длинный ответ, который заведомо не помещается в лимит из трёх токенов."})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	resp := postV1ChatNonStream(t, srv.URL, map[string]interface{}{
		"model":      "gemma-4-E4B-it-Q4_K_M",
		"stream":     false,
		"max_tokens": 3,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})

	choice := firstChoice(t, resp)
	if got, _ := choice["finish_reason"].(string); got != "length" {
		t.Fatalf("finish_reason = %q, want length (ответ обрезан капом max_tokens); choice=%+v", got, choice)
	}
	// Текст обязан остаться: length — это сигнал об обрыве, а не замена ответа.
	msg, _ := choice["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content == "" {
		t.Error("content пуст: клиент не должен получать «обрезанный» ответ без текста")
	}
}

// TestR91_NonStream_NormalAnswerStaysStop — без упора в кап причина прежняя.
func TestR91_NonStream_NormalAnswerStaysStop(t *testing.T) {
	defer withStubEmitTokens(t, []string{"Коротко."})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	resp := postV1ChatNonStream(t, srv.URL, map[string]interface{}{
		"model":      "gemma-4-E4B-it-Q4_K_M",
		"stream":     false,
		"max_tokens": 512,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})

	if got, _ := firstChoice(t, resp)["finish_reason"].(string); got != "stop" {
		t.Fatalf("finish_reason = %q, want stop (кап не достигнут)", got)
	}
}

// TestR91_NonStream_UnclosedReasoningIsNotAnEmptyAnswer — если модель целиком
// ушла в незакрытый reasoning, клиент получает текст, а не пустой успех.
func TestR91_NonStream_UnclosedReasoningIsNotAnEmptyAnswer(t *testing.T) {
	defer withStubEmitTokens(t, []string{"<reasoning>Думаю про балансировку нагрузки и про то, как её объяснить."})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	resp := postV1ChatNonStream(t, srv.URL, map[string]interface{}{
		"model":      "gemma-4-E4B-it-Q4_K_M",
		"stream":     false,
		"max_tokens": 512,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})

	msg, _ := firstChoice(t, resp)["message"].(map[string]interface{})
	if content, _ := msg["content"].(string); content == "" {
		t.Fatalf("content пуст при незакрытом reasoning — клиент видит пустой успех (message=%+v)", msg)
	}
}
