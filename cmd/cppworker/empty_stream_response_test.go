//go:build llama_stub

// empty_stream_response_test.go — регрессионные тесты для бага
// "клиент получает пустой content с done_reason=stop" в cppworker.
//
// Исторический контекст (2026-06-24):
// Пользователь сообщил, что Cline на модели gemma-4-E4B-it-Q4_K_M получает
// от cppworker NDJSON-чанк финального ответа /api/chat вида:
//
//	{"created_at":"2026-06-24T11:24:52Z","done":true,"done_reason":"stop",
//	 "message":{"content":"","role":"assistant"},"model":"gemma-4-E4B-it-Q4_K_M"}
//
// Cline трактует это как валидный (хоть и пустой) ответ и показывает
// «пустой ответ». Аналогично ведут себя OpenWebUI и Roo Code.
//
// Корневая причина (первая итерация фикса):
// streaming-функции cppworker (writeChatStreamResponseWithTools, writeStreamResponse,
// writeOllamaStream) до этого фикса не проверяли, что модель сгенерировала
// хотя бы один токен, и при отсутствии токенов (например, antiprompt
// "<end_of_turn>" сработал мгновенно) отдавали клиенту done:true +
// done_reason:"stop" + content:"".
//
// Корневая причина (вторая итерация фикса — реальный случай пользователя):
// Модель ЭМИТИТ один токен "<end_of_turn>" (gemma antiprompt), после чего
// C-bridge останавливается. В raw outputBuf оказывается "<end_of_turn>"
// (не пусто), но cleanFinalContent("<end_of_turn>") возвращает "" (т.к.
// "<end_of_turn>" есть в trailingToolTokens). Финальный чанк уходит с
// content="" + done_reason="stop" — клиент видит пустой успех.
//
// Решение второй итерации: проверять empty на CLEANED output, а не на raw.
// То есть if cleanFinalContent(fullOutput) == "" → error-чанк.
//
// В stub-режиме `bridge.SetStubEmptyOutput(true)` имитирует «0 токенов»,
// а `bridge.SetStubEmitTokens([]string{"<end_of_turn>"})` имитирует
// «1 токен = antiprompt» — точный сценарий пользователя.
//
// Build tag: llama_stub.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// parseNDJSONStream разбирает NDJSON-стрим (как от /api/chat и /api/generate
// в Ollama-формате) на отдельные JSON-объекты. Возвращает список чанков
// в порядке поступления.
func parseNDJSONStream(t *testing.T, body io.Reader) []map[string]interface{} {
	t.Helper()
	var chunks []map[string]interface{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			t.Fatalf("parseNDJSONStream: invalid JSON %q: %v", line, err)
		}
		chunks = append(chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("parseNDJSONStream: scanner error: %v", err)
	}
	return chunks
}

// findFinalDoneChunk возвращает последний NDJSON-чанк с done=true.
func findFinalDoneChunk(t *testing.T, chunks []map[string]interface{}) map[string]interface{} {
	t.Helper()
	for i := len(chunks) - 1; i >= 0; i-- {
		if done, ok := chunks[i]["done"].(bool); ok && done {
			return chunks[i]
		}
	}
	t.Fatalf("findFinalDoneChunk: no chunk with done=true in %d chunks", len(chunks))
	return nil
}

// getContentFromMessage извлекает content из message.* финального чанка
// (поддерживает формат /api/chat: message.content string).
func getContentFromMessage(chunk map[string]interface{}) string {
	msg, ok := chunk["message"].(map[string]interface{})
	if !ok {
		return ""
	}
	if c, ok := msg["content"].(string); ok {
		return c
	}
	return ""
}

// withEmptyStubOutput включает режим «пустой output» для stub bridge и
// возвращает cleanup-функцию для восстановления. Использует defer:
//
//	defer withEmptyStubOutput(t)()
func withEmptyStubOutput(t *testing.T) func() {
	t.Helper()
	prev := bridge.SetStubEmptyOutput(true)
	return func() {
		bridge.SetStubEmptyOutput(prev)
	}
}

// withStubEmitTokens устанавливает специфические токены для эмитации в stub
// bridge и возвращает cleanup-функцию для восстановления. Использует defer:
//
//	defer withStubEmitTokens(t, []string{"<end_of_turn>"})()
//
// Это второй тип инъекции: имитирует «модель эмитит конкретные токены и
// останавливается» (например, gemma эмитит "<end_of_turn>" первым токеном,
// после чего antiprompt срабатывает). В отличие от withEmptyStubOutput,
// outputBuf будет НЕ пустой, но cleanFinalContent(outputBuf) станет пустым
// (т.к. "<end_of_turn>" есть в trailingToolTokens).
func withStubEmitTokens(t *testing.T, tokens []string) func() {
	t.Helper()
	prev := bridge.SetStubEmitTokens(tokens)
	return func() {
		bridge.SetStubEmitTokens(prev)
	}
}

// =====================================================================
// 1. Главный регрессионный тест: воспроизводит баг из лога Cline.
//    /api/chat, stream=true, tools != nil, модель не выдаёт токенов.
//    ДО фикса: финальный чанк имеет done_reason="stop" + content=""
//    (клиент видит «пустой ответ»).
//    ПОСЛЕ фикса: финальный чанк имеет done_reason="error" + error="..."
//    (клиент видит ошибку инференса).
// =====================================================================
func TestOllamaChatStreamWithTools_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "test_tool",
					"description": "test",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("REGRESSION: final chunk done_reason=%q, want \"error\". "+
			"Client (Cline/OpenWebUI) will see empty success and show no answer. "+
			"finalChunk=%+v", doneReason, final)
	}
	if _, ok := final["error"]; !ok {
		t.Errorf("REGRESSION: final chunk has no error field. finalChunk=%+v", final)
	}

	errMsg, _ := final["error"].(string)
	if errMsg == "" || !strings.Contains(errMsg, "empty response") {
		t.Errorf("expected error to contain 'empty response', got %q", errMsg)
	}

	t.Logf("Final chunk: done_reason=%s, error=%q", doneReason, errMsg)
}

// =====================================================================
// 2. /api/chat, stream=true, без tools, модель не выдаёт токенов.
// =====================================================================
func TestOllamaChatStream_NoTools_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("final done_reason=%q, want \"error\"", doneReason)
	}
}

// =====================================================================
// 3. /api/generate, stream=true, модель не выдаёт токенов.
// =====================================================================
func TestOllamaGenerateStream_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"prompt": "hi",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/generate", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/generate failed: %v", err)
	}
	defer resp.Body.Close()

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("final done_reason=%q, want \"error\"", doneReason)
	}
}

// =====================================================================
// 4. /v1/completions (OpenAI legacy), stream=true, модель не выдаёт токенов.
// =====================================================================
func TestOpenAICompletionStream_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"prompt": "hi",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, "data:") {
		t.Fatalf("response is not SSE: %s", bodyStr)
	}
	// Должен быть error-чанк + [DONE].
	if !strings.Contains(bodyStr, "\"finish_reason\":\"error\"") {
		t.Errorf("response missing finish_reason=error: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("response missing [DONE]: %s", bodyStr)
	}
}

// =====================================================================
// 5. /v1/chat/completions (OpenAI chat), stream=true, модель не выдаёт токенов.
// =====================================================================
func TestOpenAIChatStream_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, "data:") {
		t.Fatalf("response is not SSE: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "\"finish_reason\":\"error\"") {
		t.Errorf("response missing finish_reason=error: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("response missing [DONE]: %s", bodyStr)
	}
}

// =====================================================================
// 6. Sanity: при нормальном (не пустом) ответе — done_reason="stop".
// =====================================================================
func TestOllamaChatStreamWithTools_NonEmptyOutput_DoneReasonStop(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Обычный stub (без SetStubEmptyOutput) выдаёт "[llama_stub] Stub mode — no real llama.cpp"
	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
		"tools": []map[string]interface{}{
			{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "test_tool",
					"description": "test",
					"parameters":  map[string]interface{}{"type": "object"},
				},
			},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "stop" {
		t.Errorf("expected done_reason=stop (non-empty output), got %q", doneReason)
	}
}

// =====================================================================
// 7. Главный регресс-тест для gemma-4 (точное имя модели из лога):
//    /api/chat, stream=true, model="gemma-4-E4B-it-Q4_K_M",
//    модель не выдаёт токенов → error-чанк.
// =====================================================================
func TestOllamaChatStream_GemmaModel_EmptyOutput_ReturnsErrorChunk(t *testing.T) {
	defer withEmptyStubOutput(t)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("REGRESSION (gemma-4): final chunk done_reason=%q, want \"error\". "+
			"This is the exact scenario from the user report (2026-06-24). "+
			"finalChunk=%+v", doneReason, final)
	}

	errMsg, _ := final["error"].(string)
	if errMsg == "" || !strings.Contains(errMsg, "empty response") {
		t.Errorf("gemma-4 empty output: expected error containing 'empty response', got %q", errMsg)
	}
}

// =====================================================================
// 8. КРИТИЧЕСКИЙ РЕГРЕСС-ТЕСТ: модель эмитит ОДИН токен "<end_of_turn>"
//    (gemma antiprompt), raw outputBuf = "<end_of_turn>" (не пустой),
//    но cleanFinalContent(outputBuf) = "" (т.к. "<end_of_turn>" есть в
//    trailingToolTokens). ДО второй итерации фикса: финальный чанк
//    имел content="" + done_reason="stop" (точный сценарий пользователя
//    с gemma-4-E4B-it-Q4_K_M). ПОСЛЕ фикса: error-чанк.
// =====================================================================

// TestOllamaChatStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk —
// точный сценарий пользователя: gemma-4 эмитит <end_of_turn> первым токеном.
// Тест проверяет, что check должен быть на CLEANED output, а не на raw.
func TestOllamaChatStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk(t *testing.T) {
	defer withStubEmitTokens(t, []string{"<end_of_turn>"})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("CRITICAL REGRESSION (gemma antiprompt → cleanFinalContent → empty): "+
			"final chunk done_reason=%q, want \"error\". "+
			"Это точный сценарий пользователя (2026-06-24, gemma-4-E4B-it-Q4_K_M): "+
			"модель эмитит <end_of_turn> первым токеном, cleanFinalContent strips it to empty, "+
			"клиент видит {content:'', done_reason:'stop'}. "+
			"finalChunk=%+v", doneReason, final)
	}

	errMsg, _ := final["error"].(string)
	if errMsg == "" || !strings.Contains(errMsg, "empty response") {
		t.Errorf("gemma antiprompt cleaned-to-empty: expected error containing 'empty response', got %q", errMsg)
	}

	// Sanity: финальный content должен быть пустым (т.к. это error-чанк).
	content := getContentFromMessage(final)
	if content != "" {
		t.Errorf("expected empty content in error chunk, got %q", content)
	}
}

// TestOllamaGenerateStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk —
// тот же сценарий для /api/generate (OpenAI-compat path тоже использует
// cleanFinalContent).
func TestOllamaGenerateStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk(t *testing.T) {
	defer withStubEmitTokens(t, []string{"<end_of_turn>"})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"prompt": "hi",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/generate", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/generate failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "error" {
		t.Errorf("CRITICAL REGRESSION (gemma antiprompt in /api/generate): "+
			"final done_reason=%q, want \"error\". finalChunk=%+v", doneReason, final)
	}
}

// TestOpenAIChatStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk —
// тот же сценарий для /v1/chat/completions (SSE).
func TestOpenAIChatStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk(t *testing.T) {
	defer withStubEmitTokens(t, []string{"<end_of_turn>"})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, "data:") {
		t.Fatalf("response is not SSE: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "\"finish_reason\":\"error\"") {
		t.Errorf("CRITICAL REGRESSION (gemma antiprompt in /v1/chat/completions): "+
			"response missing finish_reason=error: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("response missing [DONE]: %s", bodyStr)
	}
}

// TestOpenAICompletionStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk —
// тот же сценарий для /v1/completions (SSE).
func TestOpenAICompletionStream_GemmaAntiprompt_CleanedToEmpty_ReturnsErrorChunk(t *testing.T) {
	defer withStubEmitTokens(t, []string{"<end_of_turn>"})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"prompt": "hi",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/completions failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	bodyStr := string(bodyBytes)
	if !strings.Contains(bodyStr, "data:") {
		t.Fatalf("response is not SSE: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "\"finish_reason\":\"error\"") {
		t.Errorf("CRITICAL REGRESSION (gemma antiprompt in /v1/completions): "+
			"response missing finish_reason=error: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Errorf("response missing [DONE]: %s", bodyStr)
	}
}

// Sanity test: при непустом ответе (raw output содержит полезный текст +
// <end_of_turn> в конце) — cleanFinalContent strips <end_of_turn>, остаётся
// полезный текст, done_reason="stop" (НЕ error).
func TestOllamaChatStream_GemmaAntipromptAfterText_DoneReasonStop(t *testing.T) {
	defer withStubEmitTokens(t, []string{"Hello world!", "<end_of_turn>"})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/api/chat", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /api/chat failed: %v", err)
	}
	defer resp.Body.Close()

	chunks := parseNDJSONStream(t, resp.Body)
	final := findFinalDoneChunk(t, chunks)

	doneReason, _ := final["done_reason"].(string)
	if doneReason != "stop" {
		t.Errorf("expected done_reason=stop (text before antiprompt), got %q", doneReason)
	}

	content := getContentFromMessage(final)
	if !strings.Contains(content, "Hello world!") {
		t.Errorf("expected content to contain 'Hello world!', got %q", content)
	}
	// <end_of_turn> должен быть очищен (есть в trailingToolTokens).
	if strings.Contains(content, "<end_of_turn>") {
		t.Errorf("content should not contain <end_of_turn> after cleanFinalContent, got %q", content)
	}
}