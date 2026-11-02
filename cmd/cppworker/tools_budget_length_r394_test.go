//go:build llama_stub

// tools_budget_length_r394_test.go — R394 (2026-11-04): живой сценарий A10 через
// stub-мост.
//
// ЧТО БЫЛО (Cline через балансер, /api/chat + tools):
//
//	streamed_content_bytes=456  ✔ клиент получил начало ответа
//	raw_len=15812               ✔ модель израсходовала бюджет ~4096 токенов
//	done_reason=stop            ✘ «модель закончила», хотя ответ оборван лимитом
//
// R320 считал длину только по «незакрытому блоку размышлений», поэтому закрытый
// блок давал stop. Cline закрывал ход — продолжения не было, и оператор видел
// «флаг завершения раньше, чем приходит настоящий ответ».
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// postChatBudgetR394 — /api/chat с tools и клиентским лимитом num_predict.
// think передан явно (Cline умеет выключать размышления).
func postChatBudgetR394(t *testing.T, url string, numPredict int, think bool) []map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"think":  think,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"options": map[string]interface{}{"num_predict": numPredict},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "read_file",
					"description": "read a file",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(url+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	return parseNDJSONStream(t, resp.Body)
}

// finalChunkR394 — терминальный чанк (done=true).
func finalChunkR394(t *testing.T, chunks []map[string]interface{}) map[string]interface{} {
	t.Helper()
	for i := len(chunks) - 1; i >= 0; i-- {
		if done, _ := chunks[i]["done"].(bool); done {
			return chunks[i]
		}
	}
	t.Fatalf("нет терминального чанка (done=true) среди %d чанков", len(chunks))
	return nil
}

// streamedContentR394 — склейка content всех не-терминальных чанков.
func streamedContentR394(chunks []map[string]interface{}) string {
	var out string
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			continue
		}
		if ka, _ := c["keepalive"].(bool); ka {
			continue
		}
		out += getContentFromMessage(c)
	}
	return out
}

// TestR394_ClosedReasoningBudgetSpent_IsLength — регрессия живого случая: блок
// размышлений ЗАКРЫТ, ответ начался и упал на клиентском лимите. До R394 здесь
// был done_reason=stop (R320 видел закрытый блок и объявлял «модель закончила»).
func TestR394_ClosedReasoningBudgetSpent_IsLength(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "true")

	tokens := []string{
		"<think>долго разбираем задачу и расходуем бюджет</think>",
		"Начало ответа, который обрывается на лимите",
		" и продолжения уже не будет",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Клиент дал ровно 3 токена бюджета, и модель их потратила — это усечение.
	chunks := postChatBudgetR394(t, srv.URL, len(tokens), false)
	final := finalChunkR394(t, chunks)
	if got, _ := final["done_reason"].(string); got != "length" {
		t.Errorf("done_reason = %q, want length: ответ оборван клиентским n_predict, "+
			"stop заставляет агентский клиент закрыть ход (R394)", got)
	}
	if streamedContentR394(chunks) == "" {
		t.Error("видимый ответ не отдан ни одной дельтой: клиент увидел бы пустой ход")
	}
}

// TestR394_BudgetNotSpent_StaysStop — обратная сторона: если лимит не исчерпан,
// причина обязана остаться stop (нельзя превращать нормальный ответ в length).
func TestR394_BudgetNotSpent_StaysStop(t *testing.T) {
	t.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "true")

	tokens := []string{
		"<think>коротко подумали</think>",
		"Законченный ответ модели.",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatBudgetR394(t, srv.URL, 256, false)
	final := finalChunkR394(t, chunks)
	if got, _ := final["done_reason"].(string); got != "stop" {
		t.Errorf("done_reason = %q, want stop: бюджет не исчерпан, ответ закончен", got)
	}
}

// TestR394_ReasoningOnlyThinkFalse_GetsNotice — при think=false размышления
// клиенту не отдаются (thinking_len=0 в живом логе), и раньше такой ход
// закрывался либо пустым content, либо ошибкой «empty response». Теперь клиент
// получает пояснение и честную причину length.
func TestR394_ReasoningOnlyThinkFalse_GetsNotice(t *testing.T) {
	tokens := []string{
		"<think>все размышления ушли в бюджет</think>",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatBudgetR394(t, srv.URL, len(tokens), false)
	final := finalChunkR394(t, chunks)

	if got, _ := final["done_reason"].(string); got != "length" {
		t.Errorf("done_reason = %q, want length (бюджет израсходован размышлениями)", got)
	}
	if got := getContentFromMessage(final); got == "" {
		t.Error("content пуст, размышления скрыты think=false: клиент получил молчание вместо пояснения (R394.1)")
	}
	if msg, ok := final["message"].(map[string]interface{}); ok {
		if th, _ := msg["thinking"].(string); th != "" {
			t.Errorf("при think=false размышления не должны уезжать в thinking: %q", th)
		}
	}
}
