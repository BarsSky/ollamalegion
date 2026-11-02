package balancer

// stream_terminal_once_r384_test.go — R384 (2026-11-01): после обрыва по сторожу
// тишины клиент получает РОВНО ОДИН терминатор.
//
// ЖИВОЙ ДЕФЕКТ, из которого вырос тест (стенд): апстрим присылал заголовки
// `text/event-stream` и замолкал. Сторож закрывал тело, abortFramesR296 отдавал
// кадр-причину upstream_stall и терминатор `{"done":true,"done_reason":"error"}`,
// а затем исполнение доходило до блока «lastData == ""» и писало ВТОРОЙ
// терминатор: `{"done":true,"done_reason":"stop"}`. Клиент видел два `done` —
// ошибку и «успешное завершение» в одном потоке.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestR384_StallAbortEmitsExactlyOneTerminator(t *testing.T) {
	// Порог тишины — 1 c: тест не должен ждать минуты.
	t.Setenv("LB_STREAM_FIRST_FRAME_ABORT_SEC", "1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"count":1,"models":[{"name":"stall-model","state":"loaded","context_size":4096}]}`))
		case strings.HasSuffix(r.URL.Path, "/api/chat"):
			// ЗАГОЛОВКИ ОТДАЁМ, ТЕЛО — НИКОГДА: ровно поведение зависшего воркера.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	p, _, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()

	body := []byte(`{"model":"stall-model","stream":true,"messages":[{"role":"user","content":"привет"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := p.proxyRequestLlamaCpp(rec, req.WithContext(ctx), "llama_test", body); err != nil {
		t.Fatalf("proxyRequestLlamaCpp вернул ошибку: %v", err)
	}

	frames := make([]map[string]interface{}, 0, 4)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		frames = append(frames, obj)
	}
	if len(frames) == 0 {
		t.Fatalf("клиент не получил ни одного кадра: %q", rec.Body.String())
	}

	doneCount := 0
	lastReason := ""
	for _, f := range frames {
		if done, _ := f["done"].(bool); done {
			doneCount++
			lastReason, _ = f["done_reason"].(string)
		}
	}
	if doneCount != 1 {
		t.Fatalf("клиент получил %d кадров done:true, ожидался РОВНО ОДИН (тело: %q)",
			doneCount, rec.Body.String())
	}
	// Единственный терминатор обязан называть причину обрыва, а не «stop»:
	// иначе ошибка выглядит как успешное завершение.
	if lastReason == "stop" {
		t.Errorf("единственный терминатор несёт done_reason=stop при обрыве по сторожу (тело: %q)",
			rec.Body.String())
	}
	// И причина должна быть названа в кадре-причине.
	if !strings.Contains(rec.Body.String(), "upstream_stall") {
		t.Errorf("кадра-причины upstream_stall нет в ответе: %q", rec.Body.String())
	}
}
