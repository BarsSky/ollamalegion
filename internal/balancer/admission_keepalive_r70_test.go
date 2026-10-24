// admission_keepalive_r70_test.go — R70 (2026-09-24): keepalive для
// streaming-клиента, который ждёт свободный слот.
//
// ПРОБЛЕМА (R70): пока запрос стоял в admission-очереди (до
// LB_ADMISSION_WAIT_SEC, default 300 c), балансер не отправлял клиенту ни
// одного байта. OpenWebUI/Cline с таймаутом 300 c отваливались, и пользователь
// видел ошибку вместо ответа.
//
// R70-ФИКС (ОТМЕНЁН, см. ADMISSION-HONEST ниже): для streaming-запросов
// заголовки коммитились сразу (200 + Content-Type формата) и во время ожидания
// слались keepalive'ы. Это оказалось ЖИВЫМ ДЕФЕКТОМ: второй клиент OpenWebUI на
// /v1/chat/completions мгновенно получал 200 + `X-Queue-Keepalive: 1` и 30-50 c
// пустого тела, а честные X-Queue-Wait-Ms/X-Queue-Position и 503+Retry-After при
// исчерпании ожидания были недостижимы (статус уже отправлен).
//
// ADMISSION-HONEST (R118.20, 2026-10-15): преждевременная коммита и keepalive во
// время ожидания УБРАНЫ (см. admission_queue.go + admission_honest_test.go).
// Здесь остаются только два теста классификатора путей и env-ручки периода —
// они описывают контракт LB_ADMISSION_KEEPALIVE_SEC, который сохранён.
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

// TestHandleChat_NoEarlyCommitWhileWaiting_AdmissionHonest — HTTP-уровень:
// пока слот занят, streaming-клиент не получает НИ БАЙТА и никакого
// X-Queue-Keepalive; после освобождения слота приходит обычный ответ, а ожидание
// видно в честных заголовках X-Queue-Wait-Ms/X-Queue-Position.
//
// Заменяет R70-тест TestHandleChat_KeepaliveWhileWaiting_R70, который закреплял
// обратное (200 + keepalive-тело до выдачи слота) — именно это и было живым
// дефектом «OpenWebUI висит на пустом стриме» (ADMISSION-HONEST, R118.20).
func TestHandleChat_NoEarlyCommitWhileWaiting_AdmissionHonest(t *testing.T) {
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

	// Держим слот дольше периода keepalive (1 c), затем отпускаем: если бы
	// keepalive-во-время-ожидания остался, клиент получил бы 200 и keepalive-тело
	// ещё до выдачи слота (гонку чтения ответа до `<-done` не создаём — сам факт
	// виден в финальном ответе: X-Queue-Keepalive и ведущие пустые строки).
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
	if got := rec.Header().Get("X-Queue-Keepalive"); got != "" {
		t.Errorf("X-Queue-Keepalive = %q, ожидалось отсутствие заголовка (стрим не начат раньше слота)", got)
	}
	if wait := rec.Header().Get("X-Queue-Wait-Ms"); wait == "" || wait == "0" {
		t.Errorf("X-Queue-Wait-Ms = %q, ожидалось значение > 0 (ожидание видно клиенту)", wait)
	}
	if n := countLeadingNewlines(rec.Body.String()); n != 0 {
		t.Errorf("тело начинается с %d keepalive-строк, ожидался настоящий ответ", n)
	}
	t.Logf("✅ honest: во время ожидания ни байта, после слота X-Queue-Wait-Ms=%s",
		rec.Header().Get("X-Queue-Wait-Ms"))
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
