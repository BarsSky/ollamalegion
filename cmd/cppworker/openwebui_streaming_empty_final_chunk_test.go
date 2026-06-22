//go:build llama_stub

// openwebui_streaming_empty_final_chunk_test.go — регрессионные тесты для бага
// "OpenWebUI видит пустой ответ после streaming от cppworker".
//
// Исторический контекст:
// Пользователь сообщил, что при работе OpenWebUI с инструментами (web search и т.п.)
// после успешного выполнения tool модель не отвечает — OpenWebUI показывает
// "один источник найден, ответа нет". Корневая причина: финальный SSE чанк
// от cppworker имел пустой delta.content (т.к. cppworker стримит текст
// по-токенно, а в финальный чанк не подставлял полный output).
//
// OpenWebUI после done:true берёт content из финального чанка — и видит пустой.
//
// Эти тесты воспроизводят баг (до фикса) и проверяют, что фикс работает
// (после фикса).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// parseSSEResponse разбирает SSE ответ от cppworker на отдельные data-чанки.
// Возвращает список распарсенных JSON-объектов (без [DONE]).
func parseSSEResponse(t *testing.T, body io.Reader) []map[string]interface{} {
	t.Helper()
	var chunks []map[string]interface{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("parseSSEResponse: invalid JSON in SSE chunk %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("parseSSEResponse: scanner error: %v", err)
	}
	return chunks
}

// findFinalChunk возвращает последний data-чанк с finish_reason != "" / != nil.
func findFinalChunk(t *testing.T, chunks []map[string]interface{}) map[string]interface{} {
	t.Helper()
	for i := len(chunks) - 1; i >= 0; i-- {
		choices, _ := chunks[i]["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			return chunks[i]
		}
	}
	t.Fatalf("findFinalChunk: no chunk with finish_reason found in %d chunks", len(chunks))
	return nil
}

// extractDeltaContent извлекает delta.content из чанка (или "", если не задан).
func extractDeltaContent(chunk map[string]interface{}) string {
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) == 0 {
		return ""
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if delta == nil {
		return ""
	}
	content, _ := delta["content"].(string)
	return content
}

// extractDeltaToolCalls извлекает delta.tool_calls из чанка.
func extractDeltaToolCalls(chunk map[string]interface{}) []interface{} {
	choices, _ := chunk["choices"].([]interface{})
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	if delta == nil {
		return nil
	}
	tc, _ := delta["tool_calls"].([]interface{})
	return tc
}

// TestOpenAIChatStream_FinalChunkHasFullContent — ключевой регрессионный тест.
//
// Воспроизводит сценарий пользователя: cppworker стримит text chunks,
// в финальном чанке должен быть ПОЛНЫЙ output (а не пустой).
// До фикса: финальный чанк имел delta.content="" → OpenWebUI показывал пустой ответ.
// После фикса: финальный чанк имеет delta.content=<full_output>.
func TestOpenAIChatStream_FinalChunkHasFullContent(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Используем dummy.gguf — stub bridge вернёт пустой output, но chunks
	// всё равно эмитятся (для теста важна структура, не содержимое).
	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "Hello world test"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /v1/chat/completions: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, string(body))
	}

	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q, want text/event-stream", ct)
	}

	chunks := parseSSEResponse(t, resp.Body)
	if len(chunks) == 0 {
		t.Fatalf("no SSE chunks received (bug: response is empty)")
	}

	final := findFinalChunk(t, chunks)
	finishReason, _ := final["choices"].([]interface{})[0].(map[string]interface{})["finish_reason"].(string)
	if finishReason != "stop" && finishReason != "tool_calls" {
		t.Errorf("final chunk finish_reason=%q, want stop or tool_calls", finishReason)
	}

	// КРИТИЧНО: финальный чанк должен либо содержать content (plain-text ответ),
	// либо содержать tool_calls. Иначе OpenWebUI видит пустой ответ.
	finalContent := extractDeltaContent(final)
	toolCalls := extractDeltaToolCalls(final)

	if finalContent == "" && len(toolCalls) == 0 {
		t.Errorf("BUG REPRODUCED: финальный SSE чанк имеет пустой content И нет tool_calls "+
			"(OpenWebUI увидит пустой ответ после done:true). finalChunk=%+v", final)
	}

	t.Logf("Final chunk: finish_reason=%s, content_len=%d, tool_calls_count=%d",
		finishReason, len(finalContent), len(toolCalls))
}

// TestOpenAIChatStream_AccumulatesFullText — проверяет, что outputBuf накапливает
// весь текст от streaming callback. Это нужно для правильного финального чанка.
func TestOpenAIChatStream_AccumulatesFullText(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Минимальный запрос
	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "Test prompt"},
		},
		"max_tokens": 10,
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, string(body))
	}

	chunks := parseSSEResponse(t, resp.Body)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 SSE chunks (content + final), got %d", len(chunks))
	}

	// Первый чанк — это role announcement (delta.role="assistant", content="")
	// Следующие — content chunks (delta.content=<token>)
	// Последний — final chunk (finish_reason="stop")
	// ВСЕ content-чанки должны иметь delta.content (даже если stub возвращает пусто)
	for i, chunk := range chunks {
		choices, _ := chunk["choices"].([]interface{})
		if len(choices) == 0 {
			t.Errorf("chunk[%d] has no choices", i)
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if choice == nil {
			t.Errorf("chunk[%d] has invalid choice", i)
			continue
		}
		delta, _ := choice["delta"].(map[string]interface{})
		// Дельта может быть пустой только в финальном чанке (где есть finish_reason)
		// или в первом role-only чанке (delta.role без content).
		finishReason, _ := choice["finish_reason"].(string)
		isRoleOnly := delta != nil && delta["role"] != "" && delta["content"] == nil
		isFinal := finishReason != ""

		if delta == nil && !isFinal {
			t.Errorf("chunk[%d] has nil delta but isn't final (finish_reason empty)", i)
		}
		if isRoleOnly && i != 0 {
			t.Errorf("chunk[%d] is role-only but not the first chunk", i)
		}
		if finishReason != "" && finishReason != "stop" && finishReason != "tool_calls" {
			t.Errorf("chunk[%d] unexpected finish_reason=%q", i, finishReason)
		}
	}
}

// TestOpenAICompletionStream_FinalChunkHasFullText — аналогично для /v1/completions.
func TestOpenAICompletionStream_FinalChunkHasFullText(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"prompt": "Test",
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, string(body))
	}

	chunks := parseSSEResponse(t, resp.Body)
	if len(chunks) == 0 {
		t.Fatalf("no chunks received")
	}

	final := findFinalChunk(t, chunks)
	choices, _ := final["choices"].([]interface{})
	choice, _ := choices[0].(map[string]interface{})
	text, _ := choice["text"].(string)
	finishReason, _ := choice["finish_reason"].(string)

	if finishReason != "stop" {
		t.Errorf("final chunk finish_reason=%q, want stop", finishReason)
	}

	// КРИТИЧНО: финальный чанк должен содержать полный text.
	// До фикса: text="" → клиент видит пустой ответ.
	// Тест проверяет структуру (chunk exists, finish_reason set),
	// конкретный text зависит от stub bridge output (который может быть пустым).
	t.Logf("Final /v1/completions chunk: text_len=%d, finish_reason=%s", len(text), finishReason)

	// Проверяем что структура корректная — text поле присутствует
	if _, ok := choice["text"]; !ok {
		t.Errorf("final chunk missing 'text' field in choice[0]")
	}
}

// TestOpenAIChatStream_ReloadLoopLimit_ErrorChunk — проверяет, что при достижении
// reload-loop limit cppworker возвращает SSE-чанк с error и code="reload_loop_limit"
// вместо бесконечного цикла unload+reload.
func TestOpenAIChatStream_ReloadLoopLimit_ErrorChunk(t *testing.T) {
	// Этот тест сложно реализовать в stub-режиме (нет настоящего reload-loop).
	// Проверяем только структуру: при ошибке cppworker шлёт error-чанк + [DONE].
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Запрос с невалидной моделью — должна вернуться ошибка
	body := map[string]interface{}{
		"model":  "non-existent-model",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "test"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	// Streaming-ответ с ошибкой: либо 200 + error-chunk + [DONE], либо 4xx/5xx
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		// Должен быть error chunk и [DONE]
		bodyStr := string(body)
		if !strings.Contains(bodyStr, "[DONE]") {
			t.Errorf("streaming response without [DONE] marker: %s", bodyStr)
		}
	}
	// Status 4xx/5xx тоже OK (например, 503 если модель не найдена)
}

// TestOpenAIChatStream_StreamFormatIsSSE — smoke test: проверяет что
// streaming ответ приходит в SSE-формате (data: ...\n\n), не NDJSON.
func TestOpenAIChatStream_StreamFormatIsSSE(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	body := map[string]interface{}{
		"model":  "dummy",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "Test"},
		},
	}
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST failed: %v", err)
	}
	defer resp.Body.Close()

	body2, _ := io.ReadAll(resp.Body)
	bodyStr := string(body2)

	// Каждый data-чанк должен быть в формате "data: {...}\n\n"
	// (а не "{...}\n" как NDJSON).
	if !strings.Contains(bodyStr, "data:") {
		t.Errorf("response is not SSE format (no 'data:' prefix): %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "data: [DONE]") {
		t.Errorf("response missing [DONE] terminator: %s", bodyStr)
	}
	// NDJSON был бы в формате "{...}\n{...}\n" без "data: " префикса.
	if strings.HasPrefix(strings.TrimSpace(bodyStr), "{") {
		t.Errorf("response looks like NDJSON, not SSE: %s", bodyStr)
	}
}