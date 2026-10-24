//go:build llama_stub

// generate_context_r119_test.go — R119 (H1): /api/generate не отдавал `context`
// и usage-счётчики.
//
// Клиент (Ollama-совместимый клиент, Cline/OpenWebUI) ожидает в финальном
// NDJSON-чанке стрима и в негстрим-ответе ключ `context` (по конвенции
// `[]int{}`) — реальная Ollama всегда его отдаёт. Без него клиент не может
// продолжить беседу / корректно посчитать usage.
//
// Проверяются ТРИ поверхности cppworker (все видны клиенту напрямую, т.к.
// балансер проксирует Ollama-пути нативно, LB_OLLAMA_NATIVE_PATH=1):
//
//	/api/generate         stream=true  → финальный done-чанк
//	/api/generate         stream=false → generateResponse
//	/api/ollama/generate  stream=true  → финальный done-чанк
//
// Инвариант R51.3/R53.1: в стриме РОВНО ОДИН чанк с done=true — проверяется
// отдельно, чтобы добавление полей не породило второй done-чанк.
//
// Build tag: llama_stub (см. c/bridge/bridge_stub.go).
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// postGenerateJSON отправляет запрос и возвращает тело ответа.
func postGenerateJSON(t *testing.T, srvURL, path string, body map[string]interface{}) (int, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(srvURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s failed: %v", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// countDoneTrue считает чанки с done=true.
func countDoneTrue(chunks []map[string]interface{}) int {
	n := 0
	for _, c := range chunks {
		if d, ok := c["done"].(bool); ok && d {
			n++
		}
	}
	return n
}

// assertContextPresent проверяет наличие ключа `context` и (если он массив)
// отсутствие в нём мусора. По конвенции cppworker это []int{}.
func assertContextPresent(t *testing.T, label string, obj map[string]interface{}) {
	t.Helper()
	raw, ok := obj["context"]
	if !ok {
		t.Errorf("%s: нет ключа `context` (Ollama-клиент без него не может продолжить беседу): %v", label, obj)
		return
	}
	if raw == nil {
		t.Errorf("%s: `context` = null, ожидалось []int{} (по конвенции Ollama): %v", label, obj)
		return
	}
	arr, ok := raw.([]interface{})
	if !ok {
		t.Errorf("%s: `context` не массив, а %T: %v", label, raw, raw)
		return
	}
	for i, v := range arr {
		if _, ok := v.(float64); !ok {
			t.Errorf("%s: `context[%d]` не число, а %T", label, i, v)
		}
	}
}

// =====================================================================
// 1. /api/generate, stream=true: финальный done-чанк содержит `context`.
// =====================================================================
func TestR119_GenerateStream_FinalChunkHasContext(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, raw := postGenerateJSON(t, srv.URL, "/api/generate", map[string]interface{}{
		"model":  "dummy",
		"prompt": "hi",
		"stream": true,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(raw))
	}

	chunks := parseNDJSONStream(t, bytes.NewReader(raw))
	final := findFinalDoneChunk(t, chunks)

	if got := countDoneTrue(chunks); got != 1 {
		t.Errorf("R51.3/R53.1: в стриме %d чанков с done=true, ожидался РОВНО 1", got)
	}
	assertContextPresent(t, "/api/generate stream final chunk", final)

	if _, ok := final["eval_count"]; !ok {
		t.Errorf("/api/generate stream final chunk: нет eval_count: %v", final)
	}
}

// =====================================================================
// 2. /api/generate, stream=false: generateResponse содержит `context`.
// =====================================================================
func TestR119_GenerateNonStream_HasContext(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, raw := postGenerateJSON(t, srv.URL, "/api/generate", map[string]interface{}{
		"model":  "dummy",
		"prompt": "hi",
		"stream": false,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(raw))
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("non-stream response is not JSON: %v (%s)", err, string(raw))
	}
	assertContextPresent(t, "/api/generate non-stream", obj)

	if _, ok := obj["prompt_eval_count"]; !ok {
		t.Errorf("/api/generate non-stream: нет prompt_eval_count (usage-счётчик): %v", obj)
	}
}

// =====================================================================
// 3. /api/ollama/generate, stream=true: финальный done-чанк содержит `context`.
// =====================================================================
func TestR119_OllamaGenerateStream_FinalChunkHasContext(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, raw := postGenerateJSON(t, srv.URL, "/api/ollama/generate", map[string]interface{}{
		"model":  "dummy",
		"prompt": "hi",
		"stream": true,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(raw))
	}

	chunks := parseNDJSONStream(t, bytes.NewReader(raw))
	final := findFinalDoneChunk(t, chunks)

	if got := countDoneTrue(chunks); got != 1 {
		t.Errorf("R51.3/R53.1: в стриме %d чанков с done=true, ожидался РОВНО 1", got)
	}
	assertContextPresent(t, "/api/ollama/generate stream final chunk", final)
}

// =====================================================================
//  4. Sanity: /api/ollama/generate, stream=false — `context` был и раньше,
//     не должен пропасть.
//
// =====================================================================
func TestR119_OllamaGenerateNonStream_StillHasContext(t *testing.T) {
	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	status, raw := postGenerateJSON(t, srv.URL, "/api/ollama/generate", map[string]interface{}{
		"model":  "dummy",
		"prompt": "hi",
		"stream": false,
	})
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, string(raw))
	}

	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("non-stream response is not JSON: %v (%s)", err, string(raw))
	}
	assertContextPresent(t, "/api/ollama/generate non-stream", obj)
}
