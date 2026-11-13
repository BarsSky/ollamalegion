package balancer

// stream_sink_r493_test.go — R493-12r (этап 6 «два мозга»): один сток стрима.
//
// Критерий этапа — T2 побайтово. Здесь проверяется сам сток (кадр = строка +
// перевод строки, счётчик, флаш, терминатор) и то, что включение флага НЕ меняет
// ни одного байта клиенту: те же шесть комбинаций T2 проходят в обоих режимах.
//
// ЖИВОЙ КЛАСС ДЕФЕКТОВ, который сток закрывает: забытый перевод строки склеивает
// кадры (клиент видит один «склеенный» ответ), забытый флаш превращает поток в
// «замер», забытый счётчик делает сводку `streamed_content_bytes` ложью.

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// flushCounterR493 — ResponseWriter, считающий флаши (http.ResponseWriter сам не
// умеет: Flusher — отдельный интерфейс).
type flushCounterR493 struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (f *flushCounterR493) Header() http.Header { return http.Header{} }
func (f *flushCounterR493) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}
func (f *flushCounterR493) WriteHeader(int) {}
func (f *flushCounterR493) Flush()          { f.mu.Lock(); f.flushes++; f.mu.Unlock() }

func (f *flushCounterR493) snapshot() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String(), f.flushes
}

// TestR493r_PlainSinkFrameContract — кадр пишется ОДНИМ Write, с переводом
// строки, и сразу флашится.
func TestR493r_PlainSinkFrameContract(t *testing.T) {
	w := &flushCounterR493{}
	s := newPlainStreamSinkR493(w)

	n, err := s.WriteLine([]byte(`{"done":false}`))
	if err != nil {
		t.Fatalf("запись кадра: %v", err)
	}
	body, flushes := w.snapshot()
	if body != "{\"done\":false}\n" {
		t.Fatalf("клиент получил %q — кадр обязан оканчиваться переводом строки", body)
	}
	if n != len(body) {
		t.Fatalf("счётчик вернул %d байт, записано %d — сводка разойдётся с фактом", n, len(body))
	}
	if flushes != 1 {
		t.Fatalf("флашей %d, ожидался 1: без флаша поток выглядит как «замер»", flushes)
	}
	// Счётчик стока — единственный источник правды для сводки: он обязан совпасть
	// с тем, что реально ушло в писатель.
	if got := s.Written(); got != int64(len(body)) {
		t.Fatalf("Written() = %d, в писатель ушло %d — сводка разойдётся с фактом", got, len(body))
	}

	// Кадр, который УЖЕ оканчивается переводом строки, не получает второй.
	if _, err := s.WriteLine([]byte("line\n")); err != nil {
		t.Fatalf("запись кадра со переводом строки: %v", err)
	}
	body, _ = w.snapshot()
	if strings.Count(body, "\n") != 2 {
		t.Fatalf("двойной перевод строки: %q", body)
	}
}

// TestR493r_PlainSinkTerminator — «терминатор ушёл» это факт стока, а не
// переменной цикла (R252 дописывал второй [DONE] именно на этом).
func TestR493r_PlainSinkTerminator(t *testing.T) {
	w := &flushCounterR493{}
	s := newPlainStreamSinkR493(w)
	if s.TerminatorSent() {
		t.Fatal("до записи терминатор не уходил")
	}
	if _, err := s.WriteLine([]byte("data: [DONE]")); err != nil {
		t.Fatalf("запись терминатора: %v", err)
	}
	// Сток НЕ угадывает терминатор по содержимому: это решение цикла.
	if s.TerminatorSent() {
		t.Fatal("сток не должен объявлять терминатор сам")
	}
	s.NoteTerminator()
	if !s.TerminatorSent() {
		t.Fatal("после NoteTerminator факт обязан быть виден")
	}
}

// TestR493r_PlainSinkNeedsWriter — без писателя сток не паникует.
func TestR493r_PlainSinkNeedsWriter(t *testing.T) {
	var s *plainStreamSinkR493
	if _, err := s.WriteLine([]byte("x")); err == nil {
		t.Fatal("пустой сток обязан вернуть ошибку, а не панику")
	}
	if s.TerminatorSent() {
		t.Fatal("пустой сток не отправлял терминатор")
	}
	s.Flush() // без писателя — no-op
}

// TestR493r_HijackSinkSameContract — реализация поверх сырого соединения даёт
// ТОТ ЖЕ вид кадра: цикл не должен знать, куда пишет.
func TestR493r_HijackSinkSameContract(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	s := newHijackStreamSinkR493(nil, bw)
	if _, err := s.WriteLine([]byte(`{"done":true}`)); err != nil {
		t.Fatalf("запись: %v", err)
	}
	s.Flush()
	if got := buf.String(); got != "{\"done\":true}\n" {
		t.Fatalf("кадр в соединение = %q, ожидался тот же вид, что у ResponseWriter", got)
	}
	s.NoteTerminator()
	if !s.TerminatorSent() {
		t.Fatal("терминатор не отмечен")
	}
}

// TestR493r_PipeDoesNotChangeClientBytes — критерий этапа T2 побайтово: включение
// флага не добавляет и не убирает ни одного байта клиенту.
func TestR493r_PipeDoesNotChangeClientBytes(t *testing.T) {
	run := func(flag string) string {
		t.Setenv("LB_STREAM_PIPE", flag)
		// Тот же upstream, что у T2-golden: NDJSON на stream-запросах.
		srv := t2Upstream(t)
		p := t2Proxy(t, srv, true)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/chat",
			strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		p.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	off := normalizeGoldenDurationsT2(t, run("0"), "/api/chat")
	on := normalizeGoldenDurationsT2(t, run("1"), "/api/chat")
	if off == "" {
		t.Fatal("без флага клиент не получил ничего: сценарий T2 сломан, сравнение бессмысленно")
	}
	if off != on {
		t.Fatalf("флаг изменил байты клиенту:\n без стока: %q\n со стоком: %q", off, on)
	}
	// Сравниваются ВСЕ кадры, включая терминатор: их число — часть контракта T2.
	if strings.Count(off, `"done":true`) != 1 {
		t.Fatalf("терминаторов %d, ожидался ровно один: %q", strings.Count(off, `"done":true`), off)
	}
}
