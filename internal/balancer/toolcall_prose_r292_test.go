package balancer

// toolcall_prose_r292_test.go — R292 (2026-10-29): текст, который лишь похож на
// начало tool-вызова, не должен «замирать» в буфере до его заполнения.
//
// ЖИВАЯ ЖАЛОБА (второй стенд): «последний вывод обернулся в план вместе с
// предстоящими размышлениями и ждёт, пока буфер заполнится, чтобы выдать
// результат целиком — не печатает потоково».
//
// Причина: кросс-чанковый детектор текстового tool-вызова держит content в
// буфере, пока не решит, вызов это или нет. Триггер — ПЕРВЫЙ символ чанка
// (`[`, `{name`, `<tool_call>`, …). Ответ, начавшийся с `[` (маркдаун-список,
// массив в тексте — обычное дело у qwen/gemma), копился до 64 КиБ, и клиент
// получал всё одним куском в конце.
//
// Тесты фиксируют:
//   - проза, начавшаяся как JSON, отдаётся клиенту ПОТОКОМ (несколько кадров), и
//     текст не теряется и не дублируется;
//   - настоящий текстовый tool-вызов по-прежнему распознаётся как вызов, а не
//     уезжает клиенту текстом (это был бы регресс R266);
//   - политика сброса: признаки вызова держим, без признаков — не дольше
//     grace/объёма.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestR292_ProseFlushPolicy — таблица политики сброса буфера.
func TestR292_ProseFlushPolicy(t *testing.T) {
	t.Setenv("LB_TOOLCALL_PROSE_MAX_BYTES", "128")
	t.Setenv("LB_TOOLCALL_PROSE_GRACE_MS", "300")

	fresh := time.Now()
	old := time.Now().Add(-time.Second)

	cases := []struct {
		name  string
		buf   string
		since time.Time
		want  bool
	}{
		{"пусто — решать нечего", "", old, false},
		{"мало и свежо — ещё держим", "[1. пункт", fresh, false},
		{"мало, но пересидело grace — отдаём", "[1. пункт", old, true},
		{"объём превышен — отдаём", "[" + strings.Repeat("x", 200), fresh, true},
		{"настоящий вызов по ключу JSON — держим", `[{"name":"write_file","arguments":{}}`, old, false},
		{"настоящий вызов по метке — держим", `<tool_call>{"name":"x"`, old, false},
		{"qwen-метка — держим", `<|tool|>{"name":"x"`, old, false},
		{"[TOOL_CALLS] — держим", `[TOOL_CALLS][{"name":"x"`, old, false},
	}
	for _, c := range cases {
		if got := shouldFlushToolBufferAsProseR292(c.buf, c.since); got != c.want {
			t.Errorf("%s: shouldFlush(%q) = %v, want %v", c.name, c.buf, got, c.want)
		}
	}
}

// TestR292_ProseFlushGraceDisabled — grace=0 означает «отдавать сразу».
func TestR292_ProseFlushGraceDisabled(t *testing.T) {
	t.Setenv("LB_TOOLCALL_PROSE_GRACE_MS", "0")
	if got := shouldFlushToolBufferAsProseR292("[1. пункт", time.Now()); !got {
		t.Error("при grace=0 подозрительный текст должен отдаваться сразу")
	}
}

// streamProseUpstream — подставной cppworker: отдаёт SSE, где content начинается
// с «[» и содержит прозу без признаков tool-вызова.
func startProseUpstream(t *testing.T, chunks []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"qwen3.8","state":"loaded","context_size":8192}]}`))
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions") || strings.HasSuffix(r.URL.Path, "/api/chat"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fl, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("upstream: no flusher")
			}
			write := func(s string) { _, _ = fmt.Fprint(w, s); fl.Flush() }
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
			for _, c := range chunks {
				payload, _ := json.Marshal(map[string]interface{}{
					"id": "x", "object": "chat.completion.chunk",
					"choices": []interface{}{map[string]interface{}{
						"index": 0,
						"delta": map[string]interface{}{"content": c},
					}},
				})
				write("data: " + string(payload) + "\n\n")
			}
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
			write("data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
}

// ndjsonContents — собрать message.content из NDJSON-ответа и число кадров с текстом.
func ndjsonContents(t *testing.T, raw []byte) (string, int, int) {
	t.Helper()
	var text strings.Builder
	contentFrames := 0
	toolCalls := 0
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		msg, _ := chunk["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		if c, ok := msg["content"].(string); ok && c != "" {
			text.WriteString(c)
			contentFrames++
		}
		if tc, ok := msg["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls += len(tc)
		}
	}
	return text.String(), contentFrames, toolCalls
}

// TestR292_ProseIsStreamedNotBuffered — главный тест: ответ, начавшийся с «[» и
// не являющийся вызовом, приходит клиенту НЕСКОЛЬКИМИ кадрами (потоком), текст цел.
func TestR292_ProseIsStreamedNotBuffered(t *testing.T) {
	t.Setenv("LB_TOOLCALL_PROSE_MAX_BYTES", "128")
	t.Setenv("LB_TOOLCALL_PROSE_GRACE_MS", "50")

	// Достаточно текста, чтобы сработал объёмный порог (128 байт).
	chunks := []string{
		"[1. Первый пункт плана", " — это объяснение, а не вызов инструмента.",
		" 2. Второй пункт: правки в файле.", " 3. Третий пункт: проверка результата.",
		" Итог: план, а не tool call.",
	}
	upstream := startProseUpstream(t, chunks)
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	body := []byte(`{"model":"qwen3.8","stream":true,"messages":[{"role":"user","content":"напиши план"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}

	text, frames, toolCalls := ndjsonContents(t, rec.Body.Bytes())
	if toolCalls != 0 {
		t.Fatalf("прозу приняли за tool call (%d вызовов) — регресс R266", toolCalls)
	}
	want := strings.Join(chunks, "")
	if got := strings.Join(strings.Fields(text), " "); got != strings.Join(strings.Fields(want), " ") {
		t.Fatalf("текст потерян/искажён:\n got: %q\nwant: %q", text, want)
	}
	// Главное: текст ушёл НЕСКОЛЬКИМИ кадрами, а не одним куском в конце. Пока
	// буфер держал всё, кадр был ровно один (самый конец стрима).
	if frames < 2 {
		t.Fatalf("текст пришёл %d кадр(ами) — значит буфер держал его до конца стрима", frames)
	}
}

// TestR292_RealToolCallStillDetected — настоящий текстовый tool-вызов (с меткой и
// JSON аргументами) обязан распознаваться как вызов, а не уезжать клиенту текстом.
func TestR292_RealToolCallStillDetected(t *testing.T) {
	t.Setenv("LB_TOOLCALL_PROSE_MAX_BYTES", "128")
	t.Setenv("LB_TOOLCALL_PROSE_GRACE_MS", "50")

	chunks := []string{
		`<tool_call>{"name":"write_file",`,
		`"arguments":{"path":"/tmp/a.txt","content":"hello"}}`,
		`</tool_call>`,
	}
	upstream := startProseUpstream(t, chunks)
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	body := []byte(`{"model":"qwen3.8","stream":true,"messages":[{"role":"user","content":"создай файл"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp: %v", err)
	}

	text, _, toolCalls := ndjsonContents(t, rec.Body.Bytes())
	if toolCalls == 0 {
		t.Fatalf("настоящий вызов не распознан как tool_calls; content=%q", text)
	}
	if strings.Contains(text, "write_file") {
		t.Fatalf("вызов утёк клиенту как текст: %q", text)
	}
}

// TestR292_ContentBufferLooksLikeToolCall — грубая проверка предиката на прозе.
func TestR292_ContentBufferLooksLikeToolCall(t *testing.T) {
	prose := []string{
		"[1. проверить конфиг 2. перезапустить сервис]",
		"{\"note\": \"это не вызов, а пример в объяснении\"",
		"План: сначала прочитать, потом править.",
	}
	for _, p := range prose {
		if contentBufferLooksLikeToolCallR292(p) {
			t.Errorf("проза принята за вызов: %q", p)
		}
	}
	calls := []string{
		`{"name":"write_file","arguments":{}}`,
		`<|tool|>{"name":"run_commands"}`,
		`<tool_call>{"name":"x"}`,
		`[TOOL_CALLS][{"name":"x","arguments":{}}]`,
	}
	for _, c := range calls {
		if !contentBufferLooksLikeToolCallR292(c) {
			t.Errorf("вызов не распознан: %q", c)
		}
	}
}
