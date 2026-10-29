// marker_hold_ndjson_r266_1_test.go — R266.1 (2026-10-28): утечка служебных
// меток шаблона в `message.content` на пути SSE→NDJSON (`/api/chat`).
//
// ЖИВОЙ ФАКТ (Cline, qwen3.8): в видимом ответе пришли `<|assistant|>` (он был
// РАЗРЕЗАН на четыре чанка) и `</think>` (его не было в списке меток вовсе).
// Контроль: тот же запрос на `/v1/chat/completions` метки не пропустил —
// значит дефект именно в NDJSON-ветке, а не в общем egress-фильтре.
//
// Тесты фиксируют три вещи: (1) удержание хвоста склеивает разрезанную метку;
// (2) `</think>` и родственные маркеры вычищаются, обычная проза со `<` и `|`
// не страдает; (3) сквозной поток `/api/chat` не содержит меток и заканчивается
// ровно одним `done:true`.
package balancer

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

// TestR266_1_MarkerHoldGluesSplitMarker — метка, разрезанная на четыре чанка,
// не должна доехать до клиента ни одной своей частью.
func TestR266_1_MarkerHoldGluesSplitMarker(t *testing.T) {
	var hold markerHold
	// Живой разрез из лога оператора: `<|as` + `sista` + `nt` + `|>`.
	parts := []string{"<|as", "sista", "nt", "|>"}
	for i, part := range parts {
		if got := hold.Apply(part); got != "" {
			t.Fatalf("чанк %d (%q) отдал %q клиенту: половина метки утекла", i, part, got)
		}
	}
	if tail := hold.Flush(); tail != "" {
		t.Fatalf("Flush вернул %q — обрывок метки ушёл клиенту", tail)
	}
	// После метки обычный текст проходит без задержки.
	if got := hold.Apply("The file has ~348 lines"); got != "The file has ~348 lines" {
		t.Fatalf("текст после метки искажён: %q", got)
	}
}

// TestR266_1_ThinkMarkersStrippedAndProseKept — маркеры размышлений вычищаются,
// а обычная проза со `<` и `|` не режется (иначе сломаем код и разметку).
func TestR266_1_ThinkMarkersStrippedAndProseKept(t *testing.T) {
	var hold markerHold
	cases := []struct {
		in   string
		want string
	}{
		{"</think>Ответ", "Ответ"},
		{"<think>план</think>Ответ", "планОтвет"},
		{"<|/think|>Ответ", "Ответ"},
		// Проза: сравнение, pipe, html-тег — трогать нельзя.
		{"a < b && c | d", "a < b && c | d"},
		{"<div>ok</div>", "<div>ok</div>"},
	}
	for _, c := range cases {
		if got := hold.Apply(c.in); got != c.want {
			t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if tail := hold.Flush(); tail != "" {
		t.Fatalf("Flush вернул %q, ожидалось пусто", tail)
	}
}

// TestR266_1_SSEDataFilterOnlyTouchesContent — переписывается только чанк с
// content; чанки без content (role/finish/usage) остаются байт-в-байт.
func TestR266_1_SSEDataFilterOnlyTouchesContent(t *testing.T) {
	var hold markerHold
	roleChunk := `{"choices":[{"index":0,"delta":{"role":"assistant"}}]}`
	if got := filterSSEDeltaContentMarkers(roleChunk, &hold); got != roleChunk {
		t.Fatalf("чанк без content изменён: %q -> %q", roleChunk, got)
	}
	in := `{"choices":[{"index":0,"delta":{"content":"Ответ</think>"}}]}`
	out := filterSSEDeltaContentMarkers(in, &hold)
	if strings.Contains(out, "</think>") {
		t.Fatalf("метка осталась в данных: %q", out)
	}
	var obj map[string]interface{}
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("переписанный чанк не валидный JSON: %v (%q)", err, out)
	}
	delta := obj["choices"].([]interface{})[0].(map[string]interface{})["delta"].(map[string]interface{})
	if got := delta["content"]; got != "Ответ" {
		t.Fatalf("content=%v, want %q", got, "Ответ")
	}
}

// TestR266_1_ChatStreamHasNoServiceMarkers — сквозная проверка пути /api/chat:
// разрезанная метка + `</think>` + проза, затем finish_reason и [DONE].
func TestR266_1_ChatStreamHasNoServiceMarkers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"qwen3.8","state":"loaded","context_size":12288}]}`))
		case strings.HasPrefix(r.URL.Path, "/api/chat"):
			// R266.1: живой путь Cline на /api/chat — cppworker сам отдаёт NDJSON
			// (нативная Ollama-форма), и балансер проксирует строки как есть.
			// Именно здесь метки и утекали.
			w.Header().Set("Content-Type", "application/x-ndjson")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			write := func(s string) {
				_, _ = fmt.Fprint(w, s)
				if flusher != nil {
					flusher.Flush()
				}
			}
			line := func(content string) string {
				return `{"model":"qwen3.8","created_at":"2026-10-28T00:00:00Z","done":false,"message":{"role":"assistant","content":"` + content + `"}}` + "\n"
			}
			write(line(""))
			// Метка разрезана ровно как в живом логе: `<|as`+`sista`+`nt`+`|>`.
			for _, part := range []string{"<|as", "sista", "nt", "|>"} {
				write(line(part))
			}
			write(line("The file has ~348 lines"))
			write(line("</think> Let me read it"))
			write(`{"model":"qwen3.8","created_at":"2026-10-28T00:00:01Z","done":true,"done_reason":"stop","message":{"role":"assistant","content":""}}` + "\n")
		default:
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.URL.Path, "models") {
				_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"qwen3.8","state":"loaded","context_size":12288}]}`))
				return
			}
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	body := []byte(`{"model":"qwen3.8","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Cline/DSH просят стрим именно так: без Accept транспорт может уйти в
	// не-стриминговый путь и вернуть один собранный чанк вместо потока.
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp вернул ошибку: %v", err)
	}

	chunks := parseNDJSONResponse(t, bytes.NewReader(rec.Body.Bytes()))
	if len(chunks) == 0 {
		t.Fatalf("пустой ответ: %q", rec.Body.String())
	}

	var content strings.Builder
	doneCount := 0
	for _, obj := range chunks {
		if done, _ := obj["done"].(bool); done {
			doneCount++
		}
		msg, _ := obj["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		if c, ok := msg["content"].(string); ok {
			content.WriteString(c)
		}
	}

	got := content.String()
	for _, marker := range []string{"<|assistant|>", "<|tool|>", "</think>", "<|as", "sista|>"} {
		if strings.Contains(got, marker) {
			t.Fatalf("в message.content остался служебный маркер %q: %q (raw body: %s)", marker, got, rec.Body.String())
		}
	}
	if !strings.Contains(got, "The file has ~348 lines") || !strings.Contains(got, "Let me read it") {
		t.Fatalf("видимый текст потерян: %q (raw body: %s)", got, rec.Body.String())
	}
	if doneCount != 1 {
		t.Fatalf("done:true встретился %d раз(а), want ровно 1 (иначе клиент рвёт сессию)", doneCount)
	}
}
