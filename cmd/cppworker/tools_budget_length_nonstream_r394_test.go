//go:build llama_stub

// tools_budget_length_nonstream_r394_test.go — R394 (2026-11-04): тот же
// «флаг завершения врёт», но на НЕ-стриминговом /api/chat.
//
// До R394 ответ собирался с жёстким done_reason:"stop" (handlers_chat.go,
// handleChat), хотя комментарий R314 рядом описывает ровно обратный живой факт:
// num_predict=160, 592 символа размышлений, видимого ответа нет — и клиент
// получал «stop». Агентский клиент по «stop» закрывает ход.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// postChatNonStreamR394 — /api/chat без stream, с клиентским лимитом.
func postChatNonStreamR394(t *testing.T, url string, numPredict int, think bool) map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"stream":   false,
		"think":    think,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"options":  map[string]interface{}{"num_predict": numPredict},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat (non-stream): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("тело не JSON (%q): %v", string(raw), err)
	}
	return out
}

// TestR394_NonStreamBudgetSpent_IsLength — исчерпан n_predict: причина length.
func TestR394_NonStreamBudgetSpent_IsLength(t *testing.T) {
	tokens := []string{
		"<think>долго разбираем задачу и расходуем весь бюджет</think>",
		"Начало ответа, который обрывается",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	out := postChatNonStreamR394(t, srv.URL, len(tokens), false)
	if got, _ := out["done_reason"].(string); got != "length" {
		t.Errorf("done_reason = %q, want length: ответ оборван клиентским n_predict (R394)", got)
	}
}

// TestR394_NonStreamBudgetNotSpent_StaysStop — лимит не исчерпан: stop.
func TestR394_NonStreamBudgetNotSpent_StaysStop(t *testing.T) {
	tokens := []string{
		"<think>коротко</think>",
		"Законченный ответ модели.",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	out := postChatNonStreamR394(t, srv.URL, 512, false)
	if got, _ := out["done_reason"].(string); got != "stop" {
		t.Errorf("done_reason = %q, want stop: бюджет не исчерпан", got)
	}
}

// TestR394_NonStreamReasoningOnlyThinkFalse_NoticeAndLength — размышления
// скрыты think=false, видимого ответа нет: пояснение + length, а не пустой stop.
func TestR394_NonStreamReasoningOnlyThinkFalse_NoticeAndLength(t *testing.T) {
	tokens := []string{"<think>все размышления ушли в бюджет</think>"}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	out := postChatNonStreamR394(t, srv.URL, len(tokens), false)
	if got, _ := out["done_reason"].(string); got != "length" {
		t.Errorf("done_reason = %q, want length", got)
	}
	msg, _ := out["message"].(map[string]interface{})
	content, _ := msg["content"].(string)
	if content == "" {
		t.Error("content пуст при скрытых размышлениях: клиент получил молчание вместо пояснения")
	}
}
