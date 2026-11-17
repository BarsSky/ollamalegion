package balancer

// exact_prompt_tokens_r493_test.go — дефект 4 из разбора 12c: молчаливый
// таймаут токенизации.
//
// ЖИВОЙ ФАКТ (второй стенд, архив 2026-11-16): 18 из 58 запросов остались без
// точного числа токенов; из-за этого решение о росте окна считалось по оценке,
// которая завышает в 1.917…2.010 раза (40 измерений). Цена одного такого
// решения: 503 + перезагрузка 27B на A10 (needed=184100 против фактических
// 97987 при загруженных 180224).
//
// Здесь проверяется: при таймауте делается ОДИН повтор с большим лимитом, а
// отказ, о котором нельзя ничего сказать (500/не-токенизатор), повтора не
// заслуживает. Таймауты подменяются пакетными переменными — тест без «спим 6 с».

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func withExactTimeoutsR493(t *testing.T, base, retry time.Duration) {
	t.Helper()
	oldBase, oldRetry := exactTokensTimeoutR473, exactTokensRetryTimeoutR473
	exactTokensTimeoutR473, exactTokensRetryTimeoutR473 = base, retry
	t.Cleanup(func() { exactTokensTimeoutR473, exactTokensRetryTimeoutR473 = oldBase, oldRetry })
}

// TestR493_ExactTokenizeRetriesAfterTimeout — первый вызов «зависает», повтор
// обязан дождаться и вернуть точное число.
func TestR493_ExactTokenizeRetriesAfterTimeout(t *testing.T) {
	withExactTimeoutsR493(t, 20*time.Millisecond, 300*time.Millisecond)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			time.Sleep(120 * time.Millisecond) // дольше базового таймаута
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"source":"tokenizer","tokens":4242,"model_loaded":true}`))
	}))
	defer srv.Close()

	p := &Proxy{}
	n, ok, why := p.exactTokenizeWithRetryR493(srv.URL, "m", "text")
	if !ok || n != 4242 {
		t.Fatalf("после повтора ожидалось (4242,true), got (%d,%v,%q)", n, ok, why)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("вызовов ожидалось 2 (первый таймаут + повтор), got %d", got)
	}
}

// TestR493_ExactTokenizeReportsFinalTimeout — если и повтор не успел, причина
// обязана быть названа: молчаливое «остались с оценкой» и было дефектом.
func TestR493_ExactTokenizeReportsFinalTimeout(t *testing.T) {
	withExactTimeoutsR493(t, 10*time.Millisecond, 10*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{"source":"tokenizer","tokens":1}`))
	}))
	defer srv.Close()

	p := &Proxy{}
	_, ok, why := p.exactTokenizeWithRetryR493(srv.URL, "m", "text")
	if ok {
		t.Fatal("ожидался отказ токенизации")
	}
	if !strings.HasPrefix(why, "timeout_after_retry") {
		t.Fatalf("причина = %q, ожидалась timeout_after_retry:...", why)
	}
}

// TestR493_ExactTokenizeDoesNotRetryOnServerError — 500 повтора не заслуживает:
// это не медленная токенизация, а неисправность, и ждать её бессмысленно.
func TestR493_ExactTokenizeDoesNotRetryOnServerError(t *testing.T) {
	withExactTimeoutsR493(t, 20*time.Millisecond, 300*time.Millisecond)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &Proxy{}
	_, ok, why := p.exactTokenizeWithRetryR493(srv.URL, "m", "text")
	if ok || why != "status_500" {
		t.Fatalf("ожидался отказ status_500, got ok=%v why=%q", ok, why)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("повторять 500 нельзя: вызовов %d, ожидался 1", got)
	}
}

// TestR493_ExactTokenizeNoTokenizerSource — воркер ответил 200, но сказал, что
// токенизатора нет (source != tokenizer): это не таймаут, повтора нет.
func TestR493_ExactTokenizeNoTokenizerSource(t *testing.T) {
	withExactTimeoutsR493(t, 20*time.Millisecond, 300*time.Millisecond)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"source":"estimate","tokens":99}`))
	}))
	defer srv.Close()

	p := &Proxy{}
	_, ok, why := p.exactTokenizeWithRetryR493(srv.URL, "m", "text")
	if ok || why != "no_tokenizer" {
		t.Fatalf("ожидался no_tokenizer, got ok=%v why=%q", ok, why)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("повторять «нет токенизатора» нельзя: вызовов %d", got)
	}
}
