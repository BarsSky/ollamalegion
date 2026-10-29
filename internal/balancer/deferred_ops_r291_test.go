// deferred_ops_r291_test.go — R291 (2026-10-29): «кнопка думает две минуты»
// и разные форматы ответа при отложенных операциях.
//
// ЖИВЫЕ ФАКТЫ, которые здесь закреплены:
//   - ручной reload при активном стриме ждал 120 с, хотя решение известно
//     сразу (LB_RELOAD_DEFER_MAX_SEC), и оператор видел «кнопка не реагирует»;
//   - `/api/chat` ждал 122 с и лишь потом отдавал 503 без чисел, `/v1` в том
//     же сценарии вёл себя иначе — один сценарий, три разных ответа.
//
// Тесты проверяют ПОВЕДЕНИЕ (решение и тело ответа), а не собственные моки:
// решение принимается настоящей функцией, тело разбирается как JSON клиента.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// r291TestProxy — прокси с учётом стримов и без тяжёлой инициализации.
func r291TestProxy() *Proxy {
	return &Proxy{
		metricsMgr: NewMetricsManager(),
		sessionMgr: NewSessionManager(),
		backends:   map[string]*BackendState{},
		config:     nil,
	}
}

// TestR291_DeferDecision — таблица правила «ждать или отложить».
func TestR291_DeferDecision(t *testing.T) {
	cases := []struct {
		name        string
		maxWaitSec  string // LB_RELOAD_DEFER_MAX_SEC
		fastFailSec string // LB_RELOAD_DEFER_FAST_FAIL_SEC
		streams     int
		wantWait    time.Duration
		wantFailFst bool
	}{
		{
			name:    "стримов нет — выполняем сразу",
			streams: 0, wantWait: 0, wantFailFst: false,
		},
		{
			name:       "ЖИВОЙ СЛУЧАЙ: бюджет 120 с при активном стриме — отвечаем сразу",
			maxWaitSec: "120", fastFailSec: "5", streams: 1,
			wantWait: 0, wantFailFst: true,
		},
		{
			name:       "короткий бюджет (3 с) — ждём его, а не отвечаем сразу",
			maxWaitSec: "3", fastFailSec: "5", streams: 1,
			wantWait: 3 * time.Second, wantFailFst: false,
		},
		{
			name:       "бюджет 0 — не ждём вовсе",
			maxWaitSec: "0", fastFailSec: "5", streams: 2,
			wantWait: 0, wantFailFst: true,
		},
		{
			name:       "fast-fail 0 (оператор запретил ждать) — сразу отказ",
			maxWaitSec: "120", fastFailSec: "0", streams: 1,
			wantWait: 0, wantFailFst: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.maxWaitSec != "" {
				t.Setenv("LB_RELOAD_DEFER_MAX_SEC", c.maxWaitSec)
			}
			if c.fastFailSec != "" {
				t.Setenv("LB_RELOAD_DEFER_FAST_FAIL_SEC", c.fastFailSec)
			}
			p := r291TestProxy()
			var release func()
			if c.streams > 0 {
				for i := 0; i < c.streams; i++ {
					release = beginClientStreamR271(p, "bk-1", "m-1")
				}
				defer release()
			}

			active, wait, failFast := p.deferDecisionR291("bk-1")
			if active != c.streams {
				t.Fatalf("activeStreams = %d, want %d", active, c.streams)
			}
			if wait != c.wantWait {
				t.Fatalf("wait = %v, want %v", wait, c.wantWait)
			}
			if failFast != c.wantFailFst {
				t.Fatalf("failFast = %v, want %v", failFast, c.wantFailFst)
			}
		})
	}
}

// TestR291_OperatorReloadAnswersImmediately — главный живой симптом: ответ НЕ
// ждёт 120 секунд, когда ждать бессмысленно.
func TestR291_OperatorReloadAnswersImmediately(t *testing.T) {
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "120")
	t.Setenv("LB_RELOAD_DEFER_FAST_FAIL_SEC", "5")

	p := r291TestProxy()
	release := beginClientStreamR271(p, "bk-1", "gemma")
	defer release()

	start := time.Now()
	ok, active := p.deferDestructiveOpR291(context.Background(), "bk-1", "gemma", "operator-reload")
	elapsed := time.Since(start)

	if ok {
		t.Fatal("разрушительная операция разрешена при живом стриме — это оборвало бы чужую генерацию")
	}
	if active != 1 {
		t.Fatalf("activeStreams = %d, want 1 (в тексте отказа нужно число)", active)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("решение заняло %v — это и есть «кнопка думает две минуты»", elapsed)
	}
}

// TestR291_ShortBudgetStillWaits — обратная сторона: если бюджет меньше
// fast-fail, короткое ожидание сохраняется (шанс выполнить операцию сразу).
func TestR291_ShortBudgetStillWaits(t *testing.T) {
	t.Setenv("LB_RELOAD_DEFER_MAX_SEC", "2")
	t.Setenv("LB_RELOAD_DEFER_FAST_FAIL_SEC", "5")

	p := r291TestProxy()
	release := beginClientStreamR271(p, "bk-1", "gemma")

	// Освобождаем стрим через 300 мс: операция обязана дождаться и выполниться.
	go func() {
		time.Sleep(300 * time.Millisecond)
		release()
	}()

	start := time.Now()
	ok, _ := p.deferDestructiveOpR291(context.Background(), "bk-1", "gemma", "operator-reload")
	elapsed := time.Since(start)

	if !ok {
		t.Fatalf("короткий бюджет должен был дождаться освобождения стрима (%v)", elapsed)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("операция выполнилась за %v — ожидание не сработало", elapsed)
	}
}

// TestR291_DeferredBodyIsUnified — один формат для ручного reload и для
// context-growth: клиент проверяет status/deferredReason, а не текст.
func TestR291_DeferredBodyIsUnified(t *testing.T) {
	cases := []struct {
		name        string
		write       func(w http.ResponseWriter)
		wantReason  string
		wantNumbers bool
	}{
		{
			name:       "reload при активных стримах",
			write:      func(w http.ResponseWriter) { writeReloadDeferredR291(w, "gemma", "operator-reload", 2) },
			wantReason: "active streams",
		},
		{
			name: "нужно больше окна",
			write: func(w http.ResponseWriter) {
				// R299: те же два «мешающих» факта, что и на живом пути.
				writeContextGrowthDeferred(w, "gemma", 24096, 8192, 1, true)
			},
			wantReason:  "context_growth_pending",
			wantNumbers: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.write(rec)

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("код = %d, want 503", rec.Code)
			}
			if got := rec.Header().Get("Retry-After"); got == "" {
				t.Fatal("нет Retry-After: клиент не знает, когда повторять")
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("тело не JSON: %v (%s)", err, rec.Body.String())
			}
			// Общий контракт «отложено» — одинаковый на всех путях.
			if body["status"] != "deferred" {
				t.Fatalf("status = %v, want deferred (единый формат)", body["status"])
			}
			if body["deferred"] != true {
				t.Fatalf("deferred = %v, want true", body["deferred"])
			}
			if body["deferredReason"] != c.wantReason {
				t.Fatalf("deferredReason = %v, want %q", body["deferredReason"], c.wantReason)
			}
			if body["reason"] != c.wantReason {
				t.Fatalf("reason = %v, want %q (совместимость с прежним полем)", body["reason"], c.wantReason)
			}
			// Никаких полей успеха в отложенном ответе.
			for _, forbidden := range []string{"reloaded", "newContext", "previousContext", "changedParams"} {
				if _, ok := body[forbidden]; ok {
					t.Fatalf("в отложенном ответе не должно быть поля успеха %q: %v", forbidden, body)
				}
			}
			if c.wantNumbers {
				if body["requiredTokens"] == nil || body["loadedWindowTokens"] == nil {
					t.Fatalf("нужны числа «сколько нужно/сколько есть»: %v", body)
				}
			}
			if rec.Header().Get("Content-Type") == "" || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
				t.Fatalf("Content-Type = %q, want JSON", rec.Header().Get("Content-Type"))
			}
		})
	}
}

// TestR291_ContextGrowthNumbers — текст отказа preflight'а получает числа
// (сколько нужно/сколько есть), а не «reload already in progress».
func TestR291_ContextGrowthNumbers(t *testing.T) {
	p := r291TestProxy()
	p.metricsMgr = NewMetricsManager()

	body := []byte(`{"model":"gemma","stream":true,"messages":[{"role":"user","content":"` +
		strings.Repeat("word ", 2000) + `"}]}`)

	estimated, loaded, needed := p.contextGrowthNumbers("bk-1", "gemma", body, "/api/chat")
	if estimated <= 0 {
		t.Fatalf("оценка промпта должна быть > 0, got %d", estimated)
	}
	if loaded != 0 {
		t.Fatalf("окно неизвестно (бэкенд не в карте) — ожидали 0, got %d", loaded)
	}
	if needed != 0 {
		t.Fatalf("без известного окна «нужно» не считаем, got %d", needed)
	}

	// Пустое/битое тело — нули, без паники.
	if e, l, n := p.contextGrowthNumbers("bk-1", "gemma", nil, "/api/chat"); e != 0 || l != 0 || n != 0 {
		t.Fatalf("пустое тело: (%d,%d,%d), want нули", e, l, n)
	}
	if e, l, n := p.contextGrowthNumbers("", "gemma", body, "/api/chat"); e != 0 || l != 0 || n != 0 {
		t.Fatalf("пустой backend: (%d,%d,%d), want нули", e, l, n)
	}
}
