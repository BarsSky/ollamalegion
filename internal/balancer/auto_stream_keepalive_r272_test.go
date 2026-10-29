// auto_stream_keepalive_r272_test.go — R272 (2026-10-29): тело не-стрим ответа
// обязано «дышать», пока копим генерацию.
//
// ЖИВОЙ ДЕФЕКТ (второй стенд): клиент на OpenAI-порту (18079) отваливался по
// своему таймауту, хотя модель генерировала — потому что после заголовков
// 200/chunked балансер не отправлял в тело НИ БАЙТА до конца генерации
// (27B-модель на CPU-offload пишет минутами). Тесты фиксируют:
//   - пока копим, в тело периодически уходит ПРОБЕЛЬНЫЙ байт;
//   - итоговый JSON после ведущих пробелов остаётся валидным (RFC 8259: ws value ws);
//   - keepalive выключается переменной окружения и не пишет ПОСЛЕ Stop();
//   - writer без http.Flusher не оборачивается (keepalive бессмыслен).
package balancer

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// r272Writer — ResponseWriter, который умеет Flush и записывает каждый вызов
// отдельно: так видно, что ПРОБЕЛЬНЫЕ байты ушли РАНЬШЕ итогового JSON, а не
// слились с ним в один Write (ровно это и отличает «клиент видит жизнь» от
// «клиент молчит до конца»).
type r272Writer struct {
	mu     sync.Mutex
	writes []string
	header http.Header
	status int
	flush  int
}

func newR272Writer() *r272Writer { return &r272Writer{header: http.Header{}} }

func (w *r272Writer) Header() http.Header { return w.header }

func (w *r272Writer) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.status = code
	}
}

func (w *r272Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, string(p))
	return len(p), nil
}

func (w *r272Writer) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flush++
}

func (w *r272Writer) snapshot() (writes []string, flushes int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.writes...), w.flush
}

// r272OnlyWriter — writer БЕЗ Flush: keepalive должен быть отключён.
type r272OnlyWriter struct{ n int }

func (w *r272OnlyWriter) Header() http.Header       { return http.Header{} }
func (w *r272OnlyWriter) Write([]byte) (int, error) { w.n++; return 0, nil }
func (w *r272OnlyWriter) WriteHeader(int)           {}

// TestR272_IntervalFromEnv — период читается из env, мусор не выключает защиту.
func TestR272_IntervalFromEnv(t *testing.T) {
	cases := []struct {
		env  string
		set  bool
		want time.Duration
		why  string
	}{
		{"", false, 10 * time.Second, "по умолчанию 10 с"},
		{"0", true, 0, "0 = выключено осознанно"},
		{"15", true, 15 * time.Second, "явное значение"},
		{"1", true, 2 * time.Second, "ниже минимума поднимаем до 2 с"},
		{"abc", true, 10 * time.Second, "мусор = дефолт (опечатка не возвращает дефект)"},
		{"-5", true, 0, "отрицательное = выключено"},
		{" 7 ", true, 7 * time.Second, "пробелы вокруг значения"},
	}
	for _, c := range cases {
		if c.set {
			t.Setenv("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC", c.env)
		} else {
			t.Setenv("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC", "")
		}
		if got := autoStreamKeepaliveInterval(); got != c.want {
			t.Errorf("env=%q: got %v, want %v (%s)", c.env, got, c.want, c.why)
		}
	}
}

// TestR272_StopIsIdempotentAndStopsWrites — после Stop() писать в тело нельзя:
// финальный JSON должен быть последним, иначе клиент получит мусор в конце.
func TestR272_StopIsIdempotentAndStopsWrites(t *testing.T) {
	w := newR272Writer()
	k := startBodyKeepalive(w, 10*time.Millisecond, "bk-1", "m1")
	if k == nil {
		t.Fatal("keepalive не запустился при период>0")
	}
	time.Sleep(45 * time.Millisecond)
	k.Stop()
	writes, flushes := w.snapshot()
	if len(writes) < 2 {
		t.Fatalf("ожидались минимум 2 пробельных записи, получено %d", len(writes))
	}
	if flushes == 0 {
		t.Error("keepalive обязан флашить: иначе байты останутся в буфере")
	}
	for _, s := range writes {
		if strings.TrimSpace(s) != "" {
			t.Errorf("keepalive писал не-пробельные данные: %q", s)
		}
	}
	// Повторный Stop безопасен и не «оживляет» горутину.
	k.Stop()
	k.Stop()
	time.Sleep(30 * time.Millisecond)
	writes2, _ := w.snapshot()
	if len(writes2) != len(writes) {
		t.Errorf("после Stop() продолжилась запись: было %d, стало %d", len(writes), len(writes2))
	}
}

// TestR272_NoFlusherNoKeepalive — без http.Flusher периодическая запись
// бессмысленна (данные останутся в буфере), поэтому помогать нечем.
func TestR272_NoFlusherNoKeepalive(t *testing.T) {
	if k := startBodyKeepalive(&r272OnlyWriter{}, time.Second, "bk-1", "m1"); k != nil {
		t.Fatal("keepalive не должен запускаться на writer'е без Flusher")
	}
}

// TestR272_BufferedResponseStaysAliveAndValid — сквозная проверка: клиент не
// просит SSE, апстрим тянет с ответом, а тело всё равно «дышит», и итоговый
// JSON парсится (ведущие пробелы допустимы по RFC 8259).
func TestR272_BufferedResponseStaysAliveAndValid(t *testing.T) {
	t.Setenv("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC", "2")
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_SEC", "0") // лимит по времени не мешает
	t.Setenv("LB_OPENAI_AUTO_STREAM_MAX_BYTES", "0")

	up := r272SlowUpstream(t, 2600*time.Millisecond)
	p := r258Proxy()
	w := newR272Writer()
	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	started := time.Now()
	if err := p.proxyRequestOpenAIStreamAsNonStream(w, r258Request(t, "application/json"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	if time.Since(started) < 2500*time.Millisecond {
		t.Fatalf("апстрим должен был тянуть ответ, тест не воспроизводит дефект")
	}

	writes, flushes := w.snapshot()
	if flushes == 0 {
		t.Error("заголовки/keepalive обязаны флашиться, иначе клиент ничего не увидит")
	}
	whitespace := 0
	var payload strings.Builder
	for _, s := range writes {
		if strings.TrimSpace(s) == "" {
			whitespace++
			continue
		}
		payload.WriteString(s)
	}
	if whitespace == 0 {
		t.Fatalf("не было ни одного пробельного keepalive: клиент молчал всю генерацию (%d записей)", len(writes))
	}
	raw := payload.String()
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		t.Fatalf("итоговое тело не JSON: %q", raw)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &parsed); err != nil {
		t.Fatalf("интегральное тело не парсится как JSON: %v", err)
	}
	if _, ok := parsed["choices"]; !ok {
		t.Errorf("в ответе нет choices: %s", raw)
	}
}

// TestR272_KeepaliveDisabledLeavesNoPadding — при выключенном keepalive тело
// начинается сразу с '{': контракт для строгих клиентов не меняем.
func TestR272_KeepaliveDisabledLeavesNoPadding(t *testing.T) {
	t.Setenv("LB_OPENAI_AUTO_STREAM_KEEPALIVE_SEC", "0")
	up := r258Upstream(t)
	p := r258Proxy()
	w := newR272Writer()
	body := []byte(`{"model":"m1","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if err := p.proxyRequestOpenAIStreamAsNonStream(w, r258Request(t, "application/json"), body, up.URL, "bk-1"); err != nil {
		t.Fatalf("auto-stream: %v", err)
	}
	writes, _ := w.snapshot()
	var all strings.Builder
	for _, s := range writes {
		if strings.TrimSpace(s) == "" {
			t.Fatalf("при выключенном keepalive пробельных записей быть не должно: %q", s)
		}
		all.WriteString(s)
	}
	if !strings.HasPrefix(all.String(), "{") {
		t.Errorf("тело должно начинаться с '{': %q", all.String())
	}
}

// r272SlowUpstream — SSE-апстрим, который отдаёт первый кадр сразу, а финал —
// через delay: ровно та ситуация «модель генерирует долго, клиент ждёт».
func r272SlowUpstream(t *testing.T, delay time.Duration) *httptest.Server {
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
		write(`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n")
		time.Sleep(delay)
		write(`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{"content":"Hello"}}]}` + "\n\n")
		write(`data: {"id":"c1","object":"chat.completion.chunk","model":"m1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
		write("data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}
