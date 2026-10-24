package balancer

// admission_honest_test.go — ADMISSION-HONEST (R118.20, 2026-10-15).
//
// ЖИВОЙ ДЕФЕКТ (стенд, «не запускается нормально параллельная работа с
// OpenWebUI»): когда слот занят, второй клиент на OpenAI-поверхности
// (/v1/chat/completions) получал НЕМЕДЛЕННО `200 OK` +
// `Content-Type: text/event-stream` + `X-Queue-Keepalive: 1`, а в теле — только
// строки `: keepalive` 30-50 c. Честные X-Queue-Wait-Ms/X-Queue-Position и
// 503+Retry-After при исчерпании ожидания были недостижимы: заголовки
// коммитились ДО получения слота (200 + Flush перед acquireInferenceSlot),
// поэтому 503-ветка уже не могла изменить статус.
//
// КОНТРАКТ, который проверяет этот файл (одинаковый для /api/chat и
// /v1/chat/completions):
//
//	при занятом слоте недопустим исход «200 + тело без полезной нагрузки и без
//	X-Queue-заголовков». Допустимы ровно два исхода:
//	  1) слот выдан после ожидания → 200 + X-Queue-Wait-Ms>0 (+ X-Queue-Position)
//	     и настоящий ответ в теле;
//	  2) ожидание исчерпано → 503 + Retry-After + X-Queue-Position.
//
// Тест обязан падать ДО правки admission_queue.go (200 + keepalive-тело).

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// admissionHonestSurfaces — обе клиентские поверхности в одном наборе: нативная
// Ollama (/api/chat) и OpenAI (/v1/chat/completions). Поведение обязано совпадать.
var admissionHonestSurfaces = []struct {
	name string
	path string
	body string
}{
	{
		name: "ollama /api/chat",
		path: "/api/chat",
		body: `{"model":"qwen2.5-coder","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	},
	{
		name: "openai /v1/chat/completions",
		path: "/v1/chat/completions",
		body: `{"model":"qwen2.5-coder","messages":[{"role":"user","content":"hi"}],"stream":true}`,
	},
}

// dispatchInferenceSurface — вызвать тот же хендлер, который обслуживает путь.
func dispatchInferenceSurface(router *LlamaCppRouter, path string, w http.ResponseWriter, r *http.Request) {
	if path == "/v1/chat/completions" {
		router.handleOpenAIChatCompletions(w, r)
		return
	}
	router.handleChat(w, r)
}

// admissionHonestRig — прокси с единственным слотом, УЖЕ занятым «другим
// клиентом»; возвращает функцию освобождения слота.
func admissionHonestRig(t *testing.T) (*Proxy, *LlamaCppRouter, func()) {
	t.Helper()
	// Именно keepalive-режим включал преждевременную коммиту 200+Flush до
	// получения слота (живой дефект стенда).
	t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", "1")

	var received []byte
	var mu sync.Mutex
	upstream := makeUpstreamForOllamaTest(t, &received, &mu)
	t.Cleanup(upstream.Close)

	p, router, cleanup := makeTestLlamaProxy(t, upstream.URL)
	t.Cleanup(cleanup)

	p.mu.RLock()
	state := p.backends["llama_test"]
	p.mu.RUnlock()
	state.mu.Lock()
	state.Backend.MaxConcurrentReqs = 1
	state.mu.Unlock()

	if !p.tryAcquireSlot("llama_test") {
		t.Fatal("не удалось занять единственный слот")
	}
	return p, router, func() { p.releaseSlot("llama_test") }
}

// hasUsefulAdmissionPayload — есть ли в теле настоящий ответ, а не только
// keepalive-строки (SSE `: keepalive` / пустые NDJSON-строки).
func hasUsefulAdmissionPayload(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.Contains(line, "{") {
			return true
		}
	}
	return false
}

// syncRecorder — httptest.ResponseRecorder под мьютексом.
//
// Нужен тестам, которые читают состояние ответа, ПОКА хендлер ещё работает
// («до выдачи слота клиенту не ушло ни байта»): без синхронизации это гонка —
// хендлер пишет заголовки/тело в своей горутине, и `-race` её ловит.
type syncRecorder struct {
	mu      sync.Mutex
	rec     *httptest.ResponseRecorder
	written int
	header  bool
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{rec: httptest.NewRecorder()}
}

func (s *syncRecorder) Header() http.Header { return s.rec.Header() }

func (s *syncRecorder) WriteHeader(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header = true
	s.rec.WriteHeader(code)
}

func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.header = true
	s.written += len(b)
	return s.rec.Write(b)
}

func (s *syncRecorder) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Flush()
}

// sentBeforeSlot — ушло ли клиенту что-нибудь (статус/заголовки или байты).
func (s *syncRecorder) sentBeforeSlot() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header || s.written > 0, s.written
}

// assertNotDishonestAdmission — запрещает ровно тот исход, из-за которого клиент
// OpenWebUI «висел» на пустом стриме: 200 без полезной нагрузки и без честных
// X-Queue-заголовков.
func assertNotDishonestAdmission(t *testing.T, rec *httptest.ResponseRecorder, when string) {
	t.Helper()
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		return
	}
	if rec.Header().Get("X-Queue-Wait-Ms") != "" || hasUsefulAdmissionPayload(body) {
		return
	}
	t.Fatalf("ADMISSION-HONEST (%s): запрещённый исход — 200 без полезной нагрузки и без "+
		"X-Queue-заголовков (Content-Type=%q, X-Queue-Keepalive=%q, X-Queue-Position=%q, body=%q)",
		when, rec.Header().Get("Content-Type"), rec.Header().Get("X-Queue-Keepalive"),
		rec.Header().Get("X-Queue-Position"), firstBytes(body, 80))
}

// TestAdmission_HonestGrantAfterWait — слот освобождается во время ожидания:
// клиент обязан получить 200 с X-Queue-Wait-Ms>0 (ожидание видно в заголовках) и
// настоящий ответ, а не keepalive-тело. До правки тест падает: заголовки уже
// скоммичены 200+Keepalive, X-Queue-Wait-Ms выставить невозможно.
func TestAdmission_HonestGrantAfterWait(t *testing.T) {
	for _, tc := range admissionHonestSurfaces {
		t.Run(tc.name, func(t *testing.T) {
			p, router, releaseSlot := admissionHonestRig(t)
			p.setAdmissionWait(5 * time.Second)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", "honest-user-1")

			done := make(chan struct{})
			go func() {
				defer close(done)
				dispatchInferenceSurface(router, tc.path, rec, req)
			}()

			// Держим слот дольше, чем нужно для постановки в очередь, затем отдаём.
			time.Sleep(400 * time.Millisecond)
			releaseSlot()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("хендлер не завершился: запрос завис на ожидании слота")
			}

			assertNotDishonestAdmission(t, rec, "слот выдан после ожидания")
			if rec.Code != http.StatusOK {
				t.Fatalf("ожидался 200 после выдачи слота, получено %d: %s",
					rec.Code, firstBytes(rec.Body.String(), 200))
			}
			if wait := rec.Header().Get("X-Queue-Wait-Ms"); wait == "" || wait == "0" {
				t.Errorf("X-Queue-Wait-Ms = %q, ожидалось значение > 0 (клиент не видит ожидание)",
					wait)
			}
			if pos := rec.Header().Get("X-Queue-Position"); pos != "1" {
				t.Errorf("X-Queue-Position = %q, ожидалось \"1\"", pos)
			}
			if !hasUsefulAdmissionPayload(rec.Body.String()) {
				t.Errorf("в теле нет полезной нагрузки: %q", firstBytes(rec.Body.String(), 80))
			}
			t.Logf("✅ %s: слот выдан после ожидания, X-Queue-Wait-Ms=%s, ответ=%d байт",
				tc.path, rec.Header().Get("X-Queue-Wait-Ms"), rec.Body.Len())
		})
	}
}

// TestAdmission_Honest503OnWaitTimeout — ожидание исчерпано, слот так и не
// освободился: клиент обязан получить 503 + Retry-After + X-Queue-Position.
// До правки здесь был 200 с keepalive-телом (503-ветка не могла изменить уже
// отправленный статус) — это и есть живой дефект стенда.
func TestAdmission_Honest503OnWaitTimeout(t *testing.T) {
	for _, tc := range admissionHonestSurfaces {
		t.Run(tc.name, func(t *testing.T) {
			p, router, releaseSlot := admissionHonestRig(t)
			defer releaseSlot() // слот держим весь тест: ожидание обязано истечь
			p.setAdmissionWait(700 * time.Millisecond)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-User-Id", "honest-user-2")

			dispatchInferenceSurface(router, tc.path, rec, req)

			assertNotDishonestAdmission(t, rec, "ожидание исчерпано")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("ожидался честный 503 при исчерпании ожидания, получено %d: %s",
					rec.Code, firstBytes(rec.Body.String(), 200))
			}
			if retry := rec.Header().Get("Retry-After"); retry == "" {
				t.Error("при 503 ожидался Retry-After")
			}
			if pos := rec.Header().Get("X-Queue-Position"); pos == "" {
				t.Error("при 503 ожидался X-Queue-Position")
			}
			if rec.Header().Get("X-Queue-Wait-Ms") == "" {
				t.Error("при 503 ожидался X-Queue-Wait-Ms (сколько клиент прождал)")
			}
			t.Logf("✅ %s: ожидание исчерпано → 503, Retry-After=%s, X-Queue-Position=%s",
				tc.path, rec.Header().Get("Retry-After"), rec.Header().Get("X-Queue-Position"))
		})
	}
}

// TestAdmission_HonestNoKeepaliveBodyBeforeSlot — keepalive-строки не должны
// появляться в теле ДО выдачи слота: именно они превращали ожидание в «200 с
// пустым стримом» для OpenWebUI.
func TestAdmission_HonestNoKeepaliveBodyBeforeSlot(t *testing.T) {
	for _, tc := range admissionHonestSurfaces {
		t.Run(tc.name, func(t *testing.T) {
			p, router, releaseSlot := admissionHonestRig(t)
			p.setAdmissionWait(2 * time.Second)

			rec := newSyncRecorder()
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")

			done := make(chan struct{})
			go func() {
				defer close(done)
				dispatchInferenceSurface(router, tc.path, rec, req)
			}()

			// Сначала дожидаемся постановки в очередь (это миллисекунды), затем
			// гарантируем окно больше периода keepalive (1 c): если бы
			// keepalive-во-время-ожидания остался, он бы уже отправился.
			enqueuedAt := time.Now()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) &&
				p.admission.stats(p.admissionWaitTimeout()).Waiting == 0 {
				time.Sleep(10 * time.Millisecond)
			}
			if p.admission.stats(p.admissionWaitTimeout()).Waiting == 0 {
				releaseSlot()
				<-done
				t.Fatal("запрос не встал в admission-очередь (слот не был занят?) — " +
					"проверка «ни байта до выдачи слота» недостоверна")
			}
			if rest := time.Until(enqueuedAt.Add(1200 * time.Millisecond)); rest > 0 {
				time.Sleep(rest)
			}
			if sent, bytesSent := rec.sentBeforeSlot(); sent {
				releaseSlot()
				<-done
				t.Fatalf("до выдачи слота клиенту отправлен ответ: статус/заголовки "+
					"закоммичены или %d байт keepalive отправлено", bytesSent)
			}

			releaseSlot()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("хендлер не завершился после освобождения слота")
			}
			if rec.Header().Get("X-Queue-Keepalive") != "" {
				t.Errorf("X-Queue-Keepalive = %q, ожидалось отсутствие заголовка",
					rec.Header().Get("X-Queue-Keepalive"))
			}
			assertNotDishonestAdmission(t, rec.rec, "keepalive до выдачи слота")
			t.Logf("✅ %s: во время ожидания слота клиенту не отправлено ни байта", tc.path)
		})
	}
}
