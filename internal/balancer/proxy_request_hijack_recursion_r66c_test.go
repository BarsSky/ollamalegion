package balancer

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// failingHijackWriter — ResponseWriter, который РЕАЛИЗУЕТ http.Hijacker, но
// Hijack() всегда возвращает ошибку. Именно так выглядит writer, у которого
// соединение уже hijack'нуто, либо hijack недоступен по другой причине.
//
// Ключевой момент: динамический тип writer'а не меняется, поэтому диспетчер
// proxyRequestOpenAIStreaming при повторном вызове снова увидит http.Hijacker.
type failingHijackWriter struct {
	*httptest.ResponseRecorder
}

func (f *failingHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("hijack not possible: connection already hijacked (test)")
}

// TestProxyRequestOpenAIStreaming_HijackFailure_NoRecursion — R66c регресс-тест.
//
// БАГ (найден 2026-09-22 в `go test -short ./tests/...`): при ОШИБКЕ hijack'а
// hijack-реализация звала обратно диспетчер —
//
//	proxyRequestOpenAIStreamingHijacked (proxy_request_hijack.go:119)
//	  → proxyRequestOpenAIStreaming (dispatcher, видит http.Hijacker)
//	    → proxyRequestOpenAIStreamingHijacked (снова Hijack → снова ошибка)
//	      → ... бесконечно
//
// Результат — не паника в горутине, а `fatal error: stack overflow` и смерть
// всего процесса балансера. Тест требует, чтобы вызов (а) завершился и
// (б) отдал тело клиенту через legacy-путь (w.Write), а не завис/упал.
func TestProxyRequestOpenAIStreaming_HijackFailure_NoRecursion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)

		w.Write([]byte(`data: {"id":"chat-1","choices":[{"index":0,"delta":{"content":"fallback-ok"},"finish_reason":null}]}` + "\n\n"))
		w.Write([]byte(`data: {"id":"chat-1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	req, err := http.NewRequestWithContext(context.Background(), "POST", upstream.URL+"/v1/chat/completions", nil)
	if err != nil {
		t.Fatalf("build upstream request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("upstream request failed: %v", err)
	}

	w := &failingHijackWriter{ResponseRecorder: httptest.NewRecorder()}
	clientReq := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	p := makeTestProxy(15)

	// Отдельная горутина + таймаут: если регрессия вернётся в виде зависания,
	// тест упадёт по таймауту, а не повиснет до общего -timeout пакета.
	done := make(chan error, 1)
	go func() {
		done <- p.proxyRequestOpenAIStreaming(w, clientReq, resp, "test-backend")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("legacy fallback after failed hijack returned error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timed out: hijack-failure fallback neither returned nor streamed (recursion/hang regression)")
	}

	body := w.Body.String()
	if !strings.Contains(body, "fallback-ok") {
		t.Errorf("legacy fallback did not write SSE body to the client writer; got %q", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("legacy fallback did not terminate the SSE stream with [DONE]; got %q", body)
	}
}
