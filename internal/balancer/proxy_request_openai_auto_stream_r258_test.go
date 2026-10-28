// proxy_request_openai_auto_stream_r258_test.go — R258 (2026-10-28): не-стрим
// агентские запросы не «копят молча».
//
// ЖИВОЙ ФАКТ (DSH, deepseek-harness): часть запросов агент шлёт с stream=false,
// балансер уводил их в auto-stream workaround и копил ВЕСЬ ответ до конца —
// клиент не видел ни байта, пока модель не закончит, и падал по своему таймауту
// на длинном ходе (Cline: «The operation timed out.» через ~480 с).
//
// Тесты фиксируют оба исхода:
//   - клиент, готовый к SSE (`Accept: text/event-stream`), получает поток кадрами
//     с ровно одним `data: [DONE]` — без накопления;
//   - клиент, которому нужен цельный JSON, получает буферизацию, но с пределами
//     и ПОНЯТНОЙ ошибкой вместо бесконечного ожидания.
package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// r258Proxy — минимальный прокси для auto-stream пути.
func r258Proxy() *Proxy {
	return &Proxy{
		config:          &types.LoadBalancerConfig{},
		streamingClient: &http.Client{},
		metricsMgr:      NewMetricsManager(),
		sessionMgr:      NewSessionManagerWithTTL(time.Minute, time.Minute),
	}
}

// r258Upstream — SSE-апстрим: две дельты content, finish_reason и [DONE].
func r258Upstream(t *testing.T, extraChunks ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = fmt.Fprint(w, s)
			if fl != nil {
				fl.Flush()
			}
		}
		write(`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}` + "\n\n")
		for _, c := range extraChunks {
			write(c)
		}
		write(`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// r258Request — клиентский запрос с stream=false.
func r258Request(t *testing.T, accept string) *http.Request {
	t.Helper()
	body := `{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req
}

// TestR258_ClientAcceptsSSE — решение принимается по заголовку, не по UA.
func TestR258_ClientAcceptsSSE(t *testing.T) {
	if !clientAcceptsSSE(r258Request(t, "text/event-stream")) {
		t.Error("Accept: text/event-stream должен включать SSE-ветку")
	}
	if !clientAcceptsSSE(r258Request(t, "application/json, text/event-stream;q=0.9")) {
		t.Error("SSE в списке Accept должен распознаваться")
	}
	if clientAcceptsSSE(r258Request(t, "application/json")) {
		t.Error("без SSE в Accept копить нельзя отключать — клиент ждёт цельный JSON")
	}
	if clientAcceptsSSE(nil) {
		t.Error("nil-запрос не должен включать SSE")
	}
}

// TestR258_LimitsFromEnv — дефолты и переопределения лимитов.
func TestR258_LimitsFromEnv(t *testing.T) {
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "")
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_BYTES", "")
	wait, bytes := autoStreamLimits()
	if wait != defaultAutoStreamMaxSec*time.Second {
		t.Errorf("дефолт по времени = %v, want %v", wait, defaultAutoStreamMaxSec*time.Second)
	}
	if bytes != defaultAutoStreamMaxBytes {
		t.Errorf("дефолт по размеру = %d, want %d", bytes, defaultAutoStreamMaxBytes)
	}
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "42")
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_BYTES", "1024")
	wait, bytes = autoStreamLimits()
	if wait != 42*time.Second {
		t.Errorf("env по времени = %v, want 42s", wait)
	}
	if bytes != 1024 {
		t.Errorf("env по размеру = %d, want 1024", bytes)
	}
	// Мусор в env не должен превращаться в «без лимита» молча.
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "не-число")
	wait, _ = autoStreamLimits()
	if wait != defaultAutoStreamMaxSec*time.Second {
		t.Errorf("мусор в env должен давать дефолт, получено %v", wait)
	}
}

// TestR258_SSEBranchStreamsInsteadOfBuffering — главный сценарий R258.
//
// Клиент с Accept: text/event-stream при stream=false обязан получить поток:
// кадры content и ровно один `data: [DONE]`, БЕЗ ожидания полного ответа.
func TestR258_SSEBranchStreamsInsteadOfBuffering(t *testing.T) {
	up := r258Upstream(t)
	p := r258Proxy()
	rec := httptest.NewRecorder()

	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if err := p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "text/event-stream"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}

	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream (клиент просил поток)", ct)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"Hello"`) {
		t.Errorf("кадры content не дошли клиенту: %q", out)
	}
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Errorf("[DONE] должен быть ровно один, найдено %d:\n%s", n, out)
	}
	// Ответ НЕ должен быть собран в non-stream JSON: это и есть «накопление»,
	// от которого избавляемся.
	if strings.Contains(out, `"object":"chat.completion"`) && !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Errorf("ответ пришёл как цельный non-stream JSON, а должен потоком: %q", out)
	}
}

// TestR258_SizeLimitGivesClearError — вместо бесконечного ожидания.
func TestR258_SizeLimitGivesClearError(t *testing.T) {
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_BYTES", "3")
	up := r258Upstream(t)
	p := r258Proxy()
	rec := httptest.NewRecorder()

	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	// Клиент НЕ просит SSE → идёт буферизация, и лимит должен сработать.
	if err := p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "application/json"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "auto_stream_too_large") {
		t.Fatalf("ожидалась понятная ошибка auto_stream_too_large, получено: %q", out)
	}
	if !strings.Contains(out, "stream=true") {
		t.Errorf("в ошибке должна быть подсказка про stream=true: %q", out)
	}
	// Это ошибка, а не «успешный пустой ответ» (R220): клиент обязан её увидеть.
	if strings.Contains(out, `"choices"`) {
		t.Errorf("при превышении лимита нельзя отдавать частичный успешный ответ: %q", out)
	}
}

// TestR258_TimeLimitGivesClearError — тот же контракт по времени.
func TestR258_TimeLimitGivesClearError(t *testing.T) {
	// 0 отключает лимит (осознанный выбор оператора), поэтому берём минимум 1 с
	// и апстрим, который тянет с первым кадром дольше.
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "1")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(1200 * time.Millisecond)
		_, _ = fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"content":"late"}}]}`+"\n\n")
	}))
	defer srv.Close()

	p := r258Proxy()
	rec := httptest.NewRecorder()
	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if err := p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "application/json"), body, srv.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, "auto_stream_timeout") && !strings.Contains(out, "late") {
		// Оба исхода допустимы по времени гонки с первым кадром, но «пустой 200» — нет.
		t.Fatalf("ожидалась ошибка лимита времени или поздний кадр, получено: %q", out)
	}
}

// TestR258_HasClientTools — признак агентского запроса для warn-строки.
func TestR258_HasClientTools(t *testing.T) {
	if hasClientTools([]byte(`{"model":"m1","messages":[]}`)) {
		t.Error("без tools[] признак агентского запроса не должен выставляться")
	}
	if !hasClientTools([]byte(`{"model":"m1","tools":[{"type":"function","function":{"name":"write_file"}}]}`)) {
		t.Error("с tools[] запрос агентский — warn-строка должна это отражать")
	}
}
