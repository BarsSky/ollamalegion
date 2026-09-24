// admission_keepalive_r70_test.go — R70 (2026-09-24): keepalive для
// streaming-клиента, который ждёт свободный слот.
//
// ПРОБЛЕМА: пока запрос стоит в admission-очереди (до LB_ADMISSION_WAIT_SEC,
// default 300 c), балансер не отправлял клиенту ни одного байта. OpenWebUI/Cline
// с таймаутом 300 c отваливались, и пользователь видел ошибку вместо ответа.
//
// ФИКС: для streaming-запросов заголовки коммитятся сразу (200 + Content-Type
// формата) и во время ожидания шлются keepalive'ы:
//
//   - SSE (/v1/*)     — комментарий `: keepalive`;
//   - NDJSON (/api/*) — пустая строка (построчные парсеры её пропускают).
//
// Non-stream запросы keepalive не получают: статус ещё может стать 503.
package balancer

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAdmissionStreamKind_R70 — какой keepalive безопасен для какого пути.
func TestAdmissionStreamKind_R70(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		body        string
		wantCT      string
		wantPayload string
		wantOK      bool
	}{
		{
			name: "OpenAI SSE", path: "/v1/chat/completions",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			wantCT: "text/event-stream", wantPayload: ": keepalive\n\n", wantOK: true,
		},
		{
			name: "Ollama NDJSON", path: "/api/chat",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			wantCT: "application/x-ndjson", wantPayload: "\n", wantOK: true,
		},
		{
			name: "Ollama generate NDJSON", path: "/api/generate",
			body:   `{"model":"m","prompt":"hi","stream":true}`,
			wantCT: "application/x-ndjson", wantPayload: "\n", wantOK: true,
		},
		{
			name: "non-stream", path: "/api/chat",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`,
			wantOK: false,
		},
		{
			name: "non-stream OpenAI", path: "/v1/chat/completions",
			body:   `{"model":"m","messages":[],"stream":false}`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, payload, ok := admissionStreamKind(tc.path, []byte(tc.body))
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, ожидалось %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if ct != tc.wantCT {
				t.Errorf("Content-Type = %q, ожидался %q", ct, tc.wantCT)
			}
			if string(payload) != tc.wantPayload {
				t.Errorf("keepalive = %q, ожидался %q", payload, tc.wantPayload)
			}
		})
	}
}

// TestAdmissionKeepaliveInterval_R70 — LB_ADMISSION_KEEPALIVE_SEC.
func TestAdmissionKeepaliveInterval_R70(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", 5 * time.Second},
		{"0", 0},
		{"2", 2 * time.Second},
		{"-1", 5 * time.Second}, // отрицательное → default
		{"abc", 5 * time.Second},
	}
	for _, tc := range cases {
		t.Run("env="+tc.env, func(t *testing.T) {
			t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", tc.env)
			if got := AdmissionKeepaliveInterval(); got != tc.want {
				t.Errorf("AdmissionKeepaliveInterval() = %v, ожидалось %v", got, tc.want)
			}
		})
	}
}

// TestHandleChat_KeepaliveWhileWaiting_R70 — HTTP-уровень: пока слот занят,
// streaming-клиент получает keepalive'ы (пустые строки NDJSON), затем обычный
// ответ; заголовок X-Queue-Keepalive сигнализирует, что стрим уже начат.
func TestHandleChat_KeepaliveWhileWaiting_R70(t *testing.T) {
	t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", "1") // 1 c — чтобы не ждать долго
	var received []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &received, &mu)
	defer upstream.Close()

	p, router, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()
	p.setAdmissionWait(5 * time.Second)
	p.mu.RLock()
	state := p.backends["llama_test"]
	p.mu.RUnlock()
	state.mu.Lock()
	state.Backend.MaxConcurrentReqs = 1
	state.mu.Unlock()
	if !p.tryAcquireSlot("llama_test") {
		t.Fatal("не удалось занять слот")
	}

	body := `{"model":"qwen2.5-coder","messages":[{"role":"user","content":"hi"}],"stream":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-Id", "keepalive-user")

	done := make(chan struct{})
	go func() {
		defer close(done)
		router.handleChat(rec, req)
	}()

	// Держим слот дольше периода keepalive (1 c), затем отпускаем.
	time.Sleep(1200 * time.Millisecond)
	p.releaseSlot("llama_test")

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleChat не завершился: запрос завис при ожидании слота")
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("ожидался 200, получено %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("X-Queue-Keepalive"); got != "1" {
		t.Errorf("X-Queue-Keepalive = %q, ожидалось \"1\" (стрим начат до выдачи слота)", got)
	}
	// Keepalive — пустые строки NDJSON в начале тела (до реальных чанков).
	if !strings.HasPrefix(rec.Body.String(), "\n") {
		t.Errorf("тело должно начинаться с keepalive-строк, получено: %q", firstBytes(rec.Body.String(), 40))
	}
	t.Logf("✅ keepalive: тело начинается с %d пустых строк, размер ответа %d",
		countLeadingNewlines(rec.Body.String()), rec.Body.Len())
}

// TestHandleChat_NoKeepaliveForNonStream_R70 — non-stream запрос keepalive не
// получает: заголовки не коммитятся раньше времени (иначе нельзя было бы
// честно ответить 503 при таймауте очереди).
func TestHandleChat_NoKeepaliveForNonStream_R70(t *testing.T) {
	t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", "1")
	var received []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &received, &mu)
	defer upstream.Close()

	p, router, cleanup := makeTestLlamaProxy(t, upstream.URL)
	defer cleanup()
	p.setAdmissionWait(2 * time.Second) // короткое ожидание → 503
	p.mu.RLock()
	state := p.backends["llama_test"]
	p.mu.RUnlock()
	state.mu.Lock()
	state.Backend.MaxConcurrentReqs = 1
	state.mu.Unlock()
	if !p.tryAcquireSlot("llama_test") {
		t.Fatal("не удалось занять слот")
	}
	defer p.releaseSlot("llama_test")

	body := `{"model":"qwen2.5-coder","messages":[{"role":"user","content":"hi"}],"stream":false}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	router.handleChat(rec, req)

	if got := rec.Header().Get("X-Queue-Keepalive"); got != "" {
		t.Errorf("non-stream запрос не должен получать keepalive, X-Queue-Keepalive=%q", got)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ожидался 503 (ожидание истекло), получено %d: %s", rec.Code, rec.Body.String())
	}
	if retry := rec.Header().Get("Retry-After"); retry == "" {
		t.Error("при 503 ожидался Retry-After")
	}
}

func firstBytes(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func countLeadingNewlines(s string) int {
	n := 0
	for _, r := range s {
		if r != '\n' {
			break
		}
		n++
	}
	return n
}
