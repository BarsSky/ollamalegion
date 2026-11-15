// auto_stream_parity_r493_test.go — R493, этап 6: авто-стрим (R258) на ОБЩЕМ
// решении по строке.
//
// ЧТО БЫЛО. `streamUpstreamSSEToClient` — четвёртая копия того же конвейера
// (фильтр размышлений → придержанный finish → [DONE] → граница события), и она
// успела разойтись с основным путём в двух местах:
//
//   - НЕТ нормализации формы вызова (R186): нативный tool_call без `index` уходил
//     клиенту как есть, и Cline/Continue не могли склеить фрагменты;
//   - НЕТ раннего закрытия по кадру-ошибке воркера (R294): клиент получал причину
//     и продолжал ждать конца потока.
//
// Тесты фиксируют ТРИ вещи: байты обычного потока не меняются (golden), форма
// вызова приводится (паритет R186), кадр-ошибка закрывает поток (паритет R294).
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// r493AutoStreamUpstream — SSE-апстрим с произвольными кадрами и опцией «не
// закрывать поток» (нужна, чтобы отличить раннее закрытие по кадру-ошибке от
// обычного EOF).
func r493AutoStreamUpstream(t *testing.T, chunks []string, holdOpen time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for _, c := range chunks {
			_, _ = fmt.Fprint(w, c)
			if fl != nil {
				fl.Flush()
			}
		}
		if holdOpen > 0 {
			select {
			case <-r.Context().Done():
			case <-time.After(holdOpen):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestR258Parity_NormalStreamBytesUnchanged — golden байтов обычного потока:
// сведение на общий план не должно изменить ни одного байта.
func TestR258Parity_NormalStreamBytesUnchanged(t *testing.T) {
	content := `data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"}}]}`
	finish := `data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	done := `data: [DONE]`
	up := r493AutoStreamUpstream(t, []string{content + "\n\n", finish + "\n\n", done + "\n\n"}, 0)
	p := r258Proxy()
	rec := httptest.NewRecorder()

	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if err := p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "text/event-stream"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	want := content + "\n\n" + finish + "\n\n" + done + "\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("байты потока изменились.\nполучено: %q\nожидалось: %q", got, want)
	}
	if n := strings.Count(rec.Body.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("терминаторов %d, ожидался ровно один", n)
	}
}

// TestR258Parity_ToolCallGetsIndex — паритет R186: нативный tool_call без `index`
// обязан получить индекс, иначе клиент не склеит фрагменты вызова.
func TestR258Parity_ToolCallGetsIndex(t *testing.T) {
	// Форма «нативный воркер без index» — то, что R186 приводит к форме OpenAI.
	tool := `data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Moscow\"}"}}]}}]}`
	up := r493AutoStreamUpstream(t, []string{
		tool + "\n\n",
		`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}, 0)
	p := r258Proxy()
	rec := httptest.NewRecorder()

	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if err := p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "text/event-stream"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	// Разбираем кадр как клиент: важен индекс у САМОГО вызова, а не у choices[].
	var found bool
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index *int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		for _, ch := range frame.Choices {
			for _, tc := range ch.Delta.ToolCalls {
				if tc.Index == nil {
					t.Fatalf("у tool_call нет своего \"index\" — клиент не склеит фрагменты вызова (R186): %q", line)
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("tool_call не дошёл до клиента: %q", rec.Body.String())
	}
}

// TestR258Parity_UpstreamErrorFrameClosesStream — паритет R294: кадр-ошибка
// воркера закрывает поток, а не оставляет клиента ждать конца.
func TestR258Parity_UpstreamErrorFrameClosesStream(t *testing.T) {
	up := r493AutoStreamUpstream(t, []string{
		`data: {"error":{"message":"requested n_ctx exceeds max","code":2}}` + "\n\n",
	}, 10*time.Second) // поток НЕ закрывается: без раннего закрытия тест ждал бы
	p := r258Proxy()
	rec := httptest.NewRecorder()

	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- p.proxyRequestOpenAIStreamAsNonStream(rec, r258Request(t, "text/event-stream"), body, up.URL, "bk-1")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("поток не закрылся по кадру-ошибке (R294): клиент ждал бы конца потока")
	}
	elapsed := time.Since(start)
	out := rec.Body.String()
	if !strings.Contains(out, "exceeds max") {
		t.Fatalf("кадр-ошибка не дошёл до клиента: %q", out)
	}
	if n := strings.Count(out, "data: [DONE]"); n != 1 {
		t.Fatalf("терминаторов %d, ожидался ровно один: %q", n, out)
	}
	t.Logf("поток закрыт по кадру-ошибке за %.2f с", elapsed.Seconds())
}
