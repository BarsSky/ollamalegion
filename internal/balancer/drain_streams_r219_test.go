//go:build llama_stub

// drain_streams_r219_test.go — R219 (2026-10-21).
//
// Почему тест появился. Живой дефект R217: рестарт балансера рвал активный
// SSE-ответ клиенту, а в логе было мгновенное «all active streams completed».
// Причина: учёт активных стримов (`p.activeStreams`) был только в
// handleStreamingResponse (путь Ollama), а /v1 стримит через hijack и legacy
// пути. Тест фиксирует учёт в ОБОИХ путях, чтобы будущий рефакторинг не снял
// его снова.
package balancer

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// slowSSEReader отдаёт данные порциями с задержкой — имитация живого стрима.
type slowSSEReader struct {
	chunks []string
	delay  time.Duration
	i      int
}

func (r *slowSSEReader) Read(p []byte) (int, error) {
	if r.i >= len(r.chunks) {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	n := copy(p, r.chunks[r.i])
	r.i++
	return n, nil
}

func r219Proxy(t *testing.T) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{}
	cfg.Backends = []types.Backend{{
		ID: "b1", Type: types.BackendTypeLlamaCpp, Host: "h", CppWorkerPort: 18092, Status: types.StatusHealthy,
	}}
	return newProxyWithCleanup(t, cfg)
}

// TestR219_LegacySSERegistersActiveStream — пока стрим идёт, Wait не должен
// завершаться: иначе Shutdown не ждёт ответ и рвёт его при рестарте.
func TestR219_LegacySSERegistersActiveStream(t *testing.T) {
	p := r219Proxy(t)

	body := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
		"data: [DONE]\n\n"
	chunks := []string{body[:len(body)/3], body[len(body)/3 : 2*len(body)/3], body[2*len(body)/3:]}
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&slowSSEReader{chunks: chunks, delay: 120 * time.Millisecond}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = p.proxyRequestOpenAIStreamingLegacy(rec, req, resp, "b1")
	}()

	// Даём стриму начаться (первый Read уже в полёте).
	time.Sleep(60 * time.Millisecond)

	waited := make(chan struct{})
	go func() {
		p.activeStreams.Wait()
		close(waited)
	}()

	select {
	case <-waited:
		t.Fatal("activeStreams.Wait() завершился, пока стрим активен: Shutdown не станет ждать ответ")
	case <-time.After(150 * time.Millisecond):
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("стрим не завершился за 5с")
	}

	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("activeStreams не отпущен после завершения стрима (утечка счётчика)")
	}
}

// TestR219_StreamAccountingWiredInBothPaths — структурная страховка: оба
// /v1-пути (hijack и legacy) обязаны регистрировать стрим ДО цикла записи.
func TestR219_StreamAccountingWiredInBothPaths(t *testing.T) {
	check := func(path, anchor string) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("не читается %s: %v", path, err)
		}
		src := string(raw)
		addIdx := strings.Index(src, "p.activeStreams.Add(1)")
		if addIdx < 0 {
			t.Fatalf("%s: нет регистрации activeStreams — рестарт снова оборвёт стрим", path)
		}
		doneIdx := strings.Index(src, "p.activeStreams.Done()")
		if doneIdx < 0 || doneIdx < addIdx {
			t.Fatalf("%s: нет defer Done() после Add(1)", path)
		}
		if !strings.Contains(src, anchor) {
			t.Fatalf("%s: не найден ожидаемый контекст %q", path, anchor)
		}
	}
	check("proxy_request_hijack.go", "hijack path active")
	check("proxy_request.go", "proxyRequestOpenAIStreamingLegacy")
}
