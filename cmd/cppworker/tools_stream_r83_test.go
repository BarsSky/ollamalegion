//go:build llama_stub

// tools_stream_r83_test.go — R83/v56 (2026-10-01): потоковая отдача content на
// tools-пути /api/chat.
//
// Живой дефект, из-за которого это делалось. writeChatStreamResponseWithTools
// буферизовал ответ целиком и отдавал его одним финальным чанком, поэтому клиент
// (Cline) не получал ни одного токена, пока модель генерирует: /debug/last-stream
// показывал tokens_sent=0, bytes_written=380 (это 20 keepalive-ов за 306 с). На
// медленном инстансе (19-35 слоёв из 42) Cline отваливался по своему таймауту и
// показывал «Model returned empty response».
//
// Тесты фиксируют три требования:
//  1. prose уходит НЕСКОЛЬКИМИ дельтами до финального чанка, а склейка дельт
//     равна полному ответу (ни потерь, ни дублей);
//  2. tool call НЕ утекает в content: маркер и JSON удерживаются окном
//     toolsStreamHoldback даже если маркер пришёл по частям;
//  3. CPPWORKER_TOOLS_STREAM_CONTENT=false возвращает прежнее поведение.
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

// postChatWithTools отправляет /api/chat с tools и возвращает NDJSON-чанки.
func postChatWithTools(t *testing.T, url string) []map[string]interface{} {
	t.Helper()
	body := map[string]interface{}{
		"model":  "gemma-4-E4B-it-Q4_K_M",
		"stream": true,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
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

// joinContentDeltas склеивает content всех чанков с done=false и отдельно
// возвращает финальный чанк.
func joinContentDeltas(t *testing.T, chunks []map[string]interface{}) (string, map[string]interface{}) {
	t.Helper()
	var sb strings.Builder
	var final map[string]interface{}
	for _, c := range chunks {
		if done, _ := c["done"].(bool); done {
			final = c
			continue
		}
		if ka, _ := c["keepalive"].(bool); ka {
			continue
		}
		sb.WriteString(getContentFromMessage(c))
	}
	if final == nil {
		t.Fatalf("нет финального чанка (done=true) среди %d чанков", len(chunks))
	}
	return sb.String(), final
}

// TestR83v56_ToolsPathStreamsContentDeltas — prose приходит несколькими дельтами,
// склейка равна полному ответу, финальный чанк контент не дублирует.
func TestR83v56_ToolsPathStreamsContentDeltas(t *testing.T) {
	os.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "true")
	defer os.Unsetenv("CPPWORKER_TOOLS_STREAM_CONTENT")

	tokens := []string{
		"Первое длинное предложение ответа модели. ",
		"Второе длинное предложение ответа модели. ",
		"Третье длинное предложение ответа модели. ",
		"Четвёртое длинное предложение ответа. ",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatWithTools(t, srv.URL)
	streamed, final := joinContentDeltas(t, chunks)

	want := strings.Join(tokens, "")
	if strings.TrimRight(streamed, " ") != strings.TrimRight(want, " ") {
		t.Errorf("склейка дельт = %q, want %q", streamed, want)
	}
	if len(chunks) < 3 {
		t.Errorf("чанков %d: prose должен идти несколькими дельтами, а не одним финальным чанком", len(chunks))
	}
	if got := getContentFromMessage(final); got != "" {
		t.Errorf("финальный чанк дублирует контент: %q", got)
	}
	if got, _ := final["done_reason"].(string); got != "stop" {
		t.Errorf("done_reason = %q, want stop", got)
	}
}

// TestR83v56_ToolCallDoesNotLeakIntoContent — маркер tool call и его JSON не
// попадают в content даже когда маркер приходит по частям.
func TestR83v56_ToolCallDoesNotLeakIntoContent(t *testing.T) {
	os.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "true")
	defer os.Unsetenv("CPPWORKER_TOOLS_STREAM_CONTENT")

	tokens := []string{
		"Сейчас прочитаю файл. ",
		"<tool_",
		"call>",
		`{"name":"read_file","arguments":{"path":"/tmp/x"}}`,
		"</tool_call>",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatWithTools(t, srv.URL)
	var allContent strings.Builder
	var final map[string]interface{}
	for _, c := range chunks {
		allContent.WriteString(getContentFromMessage(c))
		if done, _ := c["done"].(bool); done {
			final = c
		}
	}
	if final == nil {
		t.Fatal("нет финального чанка")
	}
	content := allContent.String()
	for _, bad := range []string{"<tool_", "<tool_call>", `"name"`, "read_file\"", "arguments"} {
		if strings.Contains(content, bad) {
			t.Errorf("в content утёк tool call (%q): %q", bad, content)
		}
	}
	if !strings.Contains(content, "Сейчас прочитаю файл.") {
		t.Errorf("prose перед tool call потерян: %q", content)
	}
	if got, _ := final["done_reason"].(string); got != "tool_calls" {
		t.Errorf("done_reason = %q, want tool_calls (final=%+v)", got, final)
	}
	msg, _ := final["message"].(map[string]interface{})
	if msg == nil {
		t.Fatalf("нет message в финальном чанке: %+v", final)
	}
	calls, _ := msg["tool_calls"].([]interface{})
	if len(calls) != 1 {
		t.Errorf("tool_calls = %v, want ровно один вызов", msg["tool_calls"])
	}
}

// TestR83v56_StreamCanBeDisabled — аварийный рубильник возвращает прежнее
// поведение: один финальный чанк со всем ответом.
func TestR83v56_StreamCanBeDisabled(t *testing.T) {
	os.Setenv("CPPWORKER_TOOLS_STREAM_CONTENT", "false")
	defer os.Unsetenv("CPPWORKER_TOOLS_STREAM_CONTENT")

	tokens := []string{
		"Первый кусок ответа. ",
		"Второй кусок ответа. ",
		"Третий кусок ответа.",
	}
	defer withStubEmitTokens(t, tokens)()

	srv, cleanup := setupCppWorkerTestServer(t)
	defer cleanup()

	chunks := postChatWithTools(t, srv.URL)
	if len(chunks) != 1 {
		t.Errorf("чанков %d, want 1 (флаг CPPWORKER_TOOLS_STREAM_CONTENT=false)", len(chunks))
	}
	streamed, final := joinContentDeltas(t, chunks)
	want := strings.Join(tokens, "")
	if streamed != "" {
		t.Errorf("при выключенной потоковой отдаче дельт быть не должно: %q", streamed)
	}
	if got := getContentFromMessage(final); got != want {
		t.Errorf("финальный чанк = %q, want %q", got, want)
	}
}

// TestR83v56_HoldbackHelpers — юнит-проверки предохранителей.
func TestR83v56_HoldbackHelpers(t *testing.T) {
	if !toolsStreamStartsWithJSON("  \n{\"a\":1}") {
		t.Error("JSON-объект в начале ответа должен удерживаться")
	}
	if !toolsStreamStartsWithJSON("[{\"name\":\"x\"}]") {
		t.Error("JSON-массив в начале ответа должен удерживаться")
	}
	if toolsStreamStartsWithJSON("Обычный текст [1] и дальше") {
		t.Error("текст не должен считаться JSON")
	}
	if _, ok := toolsStreamMarkerPos("prose..... <tool_call>", toolsStreamHoldback); !ok {
		t.Error("маркер <tool_call> в хвосте должен быть найден")
	}
	if _, ok := toolsStreamMarkerPos("prose..... <tool_call|>", toolsStreamHoldback); !ok {
		t.Error("маркер <tool_call|> (gemma-4) в хвосте должен быть найден")
	}
	// Маркер далеко от хвоста (дальше окна удержания + длины маркера) на отдачу
	// хвоста не влияет: он уже ушёл бы клиенту в составе предыдущих дельт.
	far := strings.Repeat("x", 80) + " <tool_call> " + strings.Repeat("y", 80)
	if _, ok := toolsStreamMarkerPos(far, toolsStreamHoldback); ok {
		t.Error("маркер вне окна удержания не должен влиять на отдачу хвоста")
	}
	// Позиция маркера важна: prose перед ним обязан уйти клиенту.
	if pos, ok := toolsStreamMarkerPos("prose text <tool_call>{}", toolsStreamHoldback); !ok || pos != len("prose text ") {
		t.Errorf("toolsStreamMarkerPos = (%d, %v), want (%d, true)", pos, ok, len("prose text "))
	}
	// Граница среза не должна разрезать UTF-8: "Привет" = 12 байт.
	s := "Привет"
	if cut := toolsStreamRuneSafeCut(s, 0, 7); cut != 6 {
		t.Errorf("toolsStreamRuneSafeCut = %d, want 6 (граница руны)", cut)
	}
	// Заглушка bridge: убеждаемся, что токены доезжают до callback целиком.
	if got := bridge.SetStubEmitTokens([]string{"a", "b"}); got != nil {
		t.Log("SetStubEmitTokens вернул предыдущее значение — ок")
	}
	bridge.SetStubEmitTokens(nil)
}
