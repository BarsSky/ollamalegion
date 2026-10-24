package balancer

// cline_sse_session_r231_test.go — R231 (2026-10-24): сквозная проверка
// СИТУАЦИИ обмена Cline → балансер → cppworker для стрима OpenAI.
//
// Живая жалоба: «модель иногда возвращает в основном теле ответа </think>,
// есть дублирование, и где-то затесался символ конца стрима чата, от чего
// Cline закончил принимать ответ, разорвав сессию».
//
// Проверяются три инварианта клиентского контракта:
//  1. В видимом content НЕТ служебных маркеров размышлений (`</think>`) и
//     токенов конца хода (`<end_of_turn>`) — даже если воркер их прислал.
//  2. reasoning_content не смешивается с content (нет дубля текста).
//  3. `[DONE]` ровно один и он — ПОСЛЕДНЕЕ событие потока (маркер конца стрима
//     не «затесался» в тело и не приходит дважды).

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

func TestR231_SSEPassthrough_StripsMarkersAndEndTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"gemma-4-E4B-it-Q4_K_M","state":"loaded","context_size":12288}]}`))
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("upstream: no flusher")
			}
			write := func(s string) {
				_, _ = fmt.Fprint(w, s)
				flusher.Flush()
			}
			// Воркер (как в живой жалобе) присылает:
			//  - reasoning отдельным каналом,
			//  - видимый content с дублем `</think>`,
			//  - токен конца хода `<end_of_turn>` в теле,
			//  - финальный чанк и [DONE].
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"план"},"finish_reason":null}]}` + "\n\n")
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Ответ</think>"},"finish_reason":null}]}` + "\n\n")
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" готово<end_of_turn>"},"finish_reason":null}]}` + "\n\n")
			write(`data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
			write("data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	body := []byte(`{"model":"gemma-4-E4B-it-Q4_K_M","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp вернул ошибку: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP status=%d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	chunks := parseSSEResponse(t, bytes.NewReader(rec.Body.Bytes()))
	if len(chunks) == 0 {
		t.Fatalf("пустой ответ: %q", rec.Body.String())
	}

	var content, reasoning strings.Builder
	doneCount := 0
	lastChunk := ""
	finishedSeen := false
	for _, data := range chunks {
		if data == "[DONE]" {
			doneCount++
			lastChunk = "[DONE]"
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			t.Fatalf("невалидный SSE JSON: %v (%s)", err, data)
		}
		choices, _ := obj["choices"].([]interface{})
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]interface{})
		if fr, _ := choice["finish_reason"].(string); fr != "" {
			finishedSeen = true
		}
		delta, _ := choice["delta"].(map[string]interface{})
		if c, ok := delta["content"].(string); ok {
			content.WriteString(c)
		}
		if rc, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(rc)
		}
	}

	// 1) Маркеры размышлений и токен конца хода не доехали до клиента.
	if got := content.String(); got != "Ответ готово" {
		t.Fatalf("content=%q, want %q (маркеры/токены конца должны быть вырезаны)", got, "Ответ готово")
	}
	for _, marker := range []string{"</think>", "<think>", "<end_of_turn>", "<|im_end|>"} {
		if strings.Contains(content.String(), marker) {
			t.Fatalf("в content остался служебный маркер %q: %q", marker, content.String())
		}
	}

	// 2) reasoning отдельным каналом, без смешения с content.
	if got := reasoning.String(); got != "план" {
		t.Fatalf("reasoning=%q, want %q", got, "план")
	}
	if strings.Contains(reasoning.String(), "Ответ") {
		t.Fatalf("reasoning и content смешаны: %q", reasoning.String())
	}

	// 3) Ровно один [DONE] и он последний; finish_reason пришёл ДО него.
	if doneCount != 1 {
		t.Fatalf("[DONE] встретился %d раз(а), want ровно 1 (иначе клиент рвёт сессию)", doneCount)
	}
	if lastChunk != "[DONE]" {
		t.Fatalf("последнее событие=%q, want [DONE]", lastChunk)
	}
	if !finishedSeen {
		t.Fatal("finish_reason не пришёл до [DONE] — клиент не узнает причину остановки")
	}
}
