//go:build llama_stub

// empty_output_retry_r83_test.go — R83/v59 (2026-10-01): повтор генерации, когда
// модель не выдала ни одного байта.
//
// Живой факт: пробник debug/streamprobe на запросе Cline (gemma-4-E4B-it-Q4_K_M,
// 18 tools) иногда получал мгновенный EOG — в логе cppworker
// «inference вернула пустой вывод — отдаём error-чанк, raw_len=0», а клиент видел
// «model produced an empty response» (первый сбой Cline из журнала сессии).
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// postToolsChatStream — /api/chat с tools, stream=true (путь
// writeChatStreamResponseWithTools).
func postToolsChatStream(t *testing.T, url string) []map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"model":    "gemma-4-E4B-it-Q4_K_M",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"tools": []map[string]interface{}{
			{"type": "function", "function": map[string]interface{}{"name": "read_file", "parameters": map[string]interface{}{"type": "object"}}},
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

// TestR83v59_EmptyOutputRetriesOnce — первый проход пустой, второй даёт ответ:
// клиент получает нормальный ответ, а не «empty response».
func TestR83v59_EmptyOutputRetriesOnce(t *testing.T) {
	os.Setenv("CPPWORKER_EMPTY_OUTPUT_RETRY", "true")
	defer os.Unsetenv("CPPWORKER_EMPTY_OUTPUT_RETRY")
	bridge.ResetStubInferCalls()
	defer bridge.SetStubEmptyFirstCall(false)
	defer withStubEmitTokens(t, []string{"Ответ после повтора."})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	// Флаг ставим ПОСЛЕ инициализации сервера: setup делает прогрев/cканы,
	// которые сами дёргают stub-стриминг.
	bridge.SetStubEmptyFirstCall(true)
	chunks := postToolsChatStream(t, srv.URL)

	final := findFinalDoneChunk(t, chunks)
	if got, _ := final["done_reason"].(string); got != "stop" {
		t.Errorf("done_reason = %q, want stop (повтор должен был дать ответ); final=%+v", got, final)
	}
	var content strings.Builder
	for _, c := range chunks {
		content.WriteString(getContentFromMessage(c))
	}
	if !strings.Contains(content.String(), "Ответ после повтора.") {
		t.Errorf("ответ после повтора не доехал до клиента: %q", content.String())
	}
	if calls := bridge.StubInferCalls(); calls < 2 {
		t.Errorf("вызовов генерации %d, want >= 2 (повтор обязателен при raw_len=0)", calls)
	}
}

// TestR83v59_RetryCanBeDisabled — рубильник: при выключенном повторе клиент
// получает прежний error-чанк, а генерация вызывается один раз.
func TestR83v59_RetryCanBeDisabled(t *testing.T) {
	os.Setenv("CPPWORKER_EMPTY_OUTPUT_RETRY", "false")
	defer os.Unsetenv("CPPWORKER_EMPTY_OUTPUT_RETRY")
	bridge.ResetStubInferCalls()
	defer bridge.SetStubEmptyFirstCall(false)
	defer withStubEmitTokens(t, []string{"Ответ после повтора."})()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	bridge.SetStubEmptyFirstCall(true)
	chunks := postToolsChatStream(t, srv.URL)
	final := findFinalDoneChunk(t, chunks)
	if got, _ := final["done_reason"].(string); got != "error" {
		t.Errorf("done_reason = %q, want error (повтор выключен)", got)
	}
	if calls := bridge.StubInferCalls(); calls != 1 {
		t.Errorf("вызовов генерации %d, want ровно 1", calls)
	}
}

// TestR83v59_HelperReadsEnv — разбор переменной окружения.
func TestR83v59_HelperReadsEnv(t *testing.T) {
	for _, v := range []string{"false", "0", "no", "OFF"} {
		os.Setenv("CPPWORKER_EMPTY_OUTPUT_RETRY", v)
		if emptyOutputRetryEnabled() {
			t.Errorf("значение %q должно выключать повтор", v)
		}
	}
	for _, v := range []string{"", "true", "1", "yes"} {
		os.Setenv("CPPWORKER_EMPTY_OUTPUT_RETRY", v)
		if !emptyOutputRetryEnabled() {
			t.Errorf("значение %q должно оставлять повтор включённым", v)
		}
	}
	os.Unsetenv("CPPWORKER_EMPTY_OUTPUT_RETRY")
}
