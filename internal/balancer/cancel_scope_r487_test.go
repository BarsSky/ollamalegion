// cancel_scope_r487_test.go — R487 (2026-11-11): отмена должна знать, ЧТО отменяет.
//
// ЖИВОЙ ДЕФЕКТ (два архива второго стенда подряд): снятие запроса, стоявшего в
// ОЧЕРЕДИ, оставляло его висеть, а POST /api/cancel по модели убивал генерацию,
// которая в этот момент РАБОТАЛА на занятом бэкенде (и доезжал до cppworker).
// Тесты фиксируют новый резолв:
//   - request_id известен и заявка в очереди → снять локально, воркер не звать;
//   - request_id известен и заявка на бэкенде → приказ ровно на ЕГО бэкенд;
//   - request_id неизвестен → НЕ рассылать (именно это убивало чужое);
//   - явная личность клиента (X-Client-ID) → отменить только её заявки;
//   - ничего из перечисленного → прежняя широковещательная семантика, но с
//     явной пометкой scope:"model_broadcast" в ответе.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cancelBodyR487 разбирает ответ балансера в карту.
func cancelBodyR487(t *testing.T, rec *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var out map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ответ не JSON: %v; body=%s", err, rec.Body.String())
	}
	return out
}

func TestR487_IndexTrackQueuedThenRunning(t *testing.T) {
	idx := newRequestIndexR487()
	cancelled := false
	idx.TrackQueued("req-1", "sess-a", "client:cline-1", "backend-1", func() { cancelled = true })

	task, ok := idx.Lookup("req-1")
	if !ok || !task.Queued || task.Backend != "backend-1" || task.Identity != "client:cline-1" {
		t.Fatalf("queued-запись не сохранилась: %+v ok=%v", task, ok)
	}
	idx.MarkRunning("req-1", "backend-2")
	task, _ = idx.Lookup("req-1")
	if task.Queued || task.Backend != "backend-2" || task.CancelQue != nil {
		t.Fatalf("MarkRunning не перевёл заявку в running: %+v", task)
	}
	if task.Identity != "client:cline-1" {
		t.Fatalf("личность потеряна при MarkRunning: %+v", task)
	}
	if got := len(idx.ByIdentity("client:cline-1")); got != 1 {
		t.Fatalf("ByIdentity вернул %d заявок, want 1", got)
	}
	_ = cancelled
	idx.Drop("req-1")
	if idx.Len() != 0 {
		t.Fatalf("после Drop индекс не пуст: %d", idx.Len())
	}
	if _, ok := idx.Lookup("req-1"); ok {
		t.Fatal("Lookup после Drop вернул запись")
	}
}

// TestR487_QueuedCancelIsLocal — заявка снимается с очереди, воркер не зовётся.
func TestR487_QueuedCancelIsLocal(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)
	queueCancelCalls := 0
	p.requestIndexR487().TrackQueued("req-queued", "sess-a", "", "backend-1", func() {
		queueCancelCalls++
	})

	rec := doCancel(t, p, http.MethodPost, `{"request_id":"req-queued"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := cancelBodyR487(t, rec)
	if body["status"] != "cancelled_queued" {
		t.Errorf("status = %v, want cancelled_queued (%s)", body["status"], rec.Body.String())
	}
	if body["scope"] != "request_id" {
		t.Errorf("scope = %v, want request_id", body["scope"])
	}
	if queueCancelCalls != 1 {
		t.Errorf("очередь снята %d раз, want 1", queueCancelCalls)
	}
	for i, r := range recs {
		if r.count() != 0 {
			t.Errorf("backend-%d получил %d отмен, want 0: отмена очереди не должна доезжать до воркера", i+1, r.count())
		}
	}
	if _, ok := p.requestIndexR487().Lookup("req-queued"); ok {
		t.Error("заявка осталась в индексе после снятия с очереди")
	}
}

// TestR487_RunningCancelGoesOnlyToOwningBackend — ровно один бэкенд и его id.
func TestR487_RunningCancelGoesOnlyToOwningBackend(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)
	p.requestIndexR487().MarkRunning("req-running", "backend-2")

	rec := doCancel(t, p, http.MethodPost, `{"request_id":"req-running","user_id":"alice"}`)
	body := cancelBodyR487(t, rec)
	if body["status"] != "cancelled" {
		t.Errorf("status = %v, want cancelled (%s)", body["status"], rec.Body.String())
	}
	if recs[0].count() != 0 {
		t.Errorf("backend-1 получил %d отмен — отмена ушла не туда", recs[0].count())
	}
	if recs[1].count() != 1 {
		t.Fatalf("backend-2 получил %d отмен, want 1", recs[1].count())
	}
	got := recs[1].last()
	if got["request_id"] != "req-running" {
		t.Errorf("в теле отмены request_id = %v, want req-running", got["request_id"])
	}
	if got["user_id"] != "alice" {
		t.Errorf("в теле отмены user_id = %v, want alice", got["user_id"])
	}
}

// TestR487_UnknownRequestIDIsNotBroadcast — неизвестный id НЕ рассылается.
//
// Это и есть дефект: раньше приказ по модели уходил во все бэкенды с загруженной
// моделью, и Stop одной заявки убивал чужую генерацию.
func TestR487_UnknownRequestIDIsNotBroadcast(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)

	rec := doCancel(t, p, http.MethodPost, `{"request_id":"req-unknown","model":"m1"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (неизвестный id — не ошибка транспорта)", rec.Code)
	}
	body := cancelBodyR487(t, rec)
	if body["status"] != "not_cancelled" {
		t.Errorf("status = %v, want not_cancelled (%s)", body["status"], rec.Body.String())
	}
	if body["scope"] != "request_id" {
		t.Errorf("scope = %v, want request_id", body["scope"])
	}
	hint, _ := body["hint"].(string)
	if !strings.Contains(hint, "R487") {
		t.Errorf("в ответе нет причины: %q", hint)
	}
	for i, r := range recs {
		if r.count() != 0 {
			t.Errorf("backend-%d получил %d отмен по неизвестному id, want 0", i+1, r.count())
		}
	}
}

// TestR487_IdentityCancelTargetsOnlyThatClient — X-Client-ID сужает адресатов.
func TestR487_IdentityCancelTargetsOnlyThatClient(t *testing.T) {
	p, recs := newCancelTestProxy(t, 2)
	idx := p.requestIndexR487()
	idx.MarkRunning("req-a", "backend-1")
	idx.MarkIdentity("req-a", "client:cline-1", "")
	idx.MarkRunning("req-b", "backend-2")
	idx.MarkIdentity("req-b", "client:cline-2", "")

	req := httptest.NewRequest(http.MethodPost, "/api/cancel",
		strings.NewReader(`{"model":"m1"}`))
	req.Header.Set("X-Client-ID", "cline-1")
	rec := httptest.NewRecorder()
	p.handleClientCancel(rec, req)

	body := cancelBodyR487(t, rec)
	if body["scope"] != "identity" {
		t.Fatalf("scope = %v, want identity (%s)", body["scope"], rec.Body.String())
	}
	if recs[0].count() != 1 {
		t.Errorf("backend-1 (та личность) получил %d отмен, want 1", recs[0].count())
	}
	if recs[1].count() != 0 {
		t.Errorf("backend-2 (другая личность) получил %d отмен, want 0", recs[1].count())
	}
	if got := recs[0].last(); got["request_id"] != "req-a" {
		t.Errorf("request_id в приказе = %v, want req-a", got["request_id"])
	}
}

// TestR487_ModelOnlyCancelIsMarkedBroadcast — совместимость с OpenWebUI, но честно.
func TestR487_ModelOnlyCancelIsMarkedBroadcast(t *testing.T) {
	p, _ := newCancelTestProxy(t, 1)
	rec := doCancel(t, p, http.MethodPost, `{"model":"m1"}`)
	body := cancelBodyR487(t, rec)
	if body["scope"] != "model_broadcast" {
		t.Fatalf("scope = %v, want model_broadcast (%s)", body["scope"], rec.Body.String())
	}
	if _, ok := body["hint_scope"]; !ok {
		t.Fatalf("нет hint_scope: широковещательная отмена должна объяснять себя (%s)", rec.Body.String())
	}
}

// TestR487_CancelIdentityFromHeadersAndBody — личность берётся только из явного.
func TestR487_CancelIdentityFromHeadersAndBody(t *testing.T) {
	h := http.Header{}
	if got := cancelIdentityR487(h, ""); got != "" {
		t.Fatalf("без заголовков и тела identity = %q, want пусто", got)
	}
	h.Set("X-Client-ID", "cline-9")
	if got := cancelIdentityR487(h, ""); got != "client:cline-9" {
		t.Fatalf("identity = %q, want client:cline-9", got)
	}
	h2 := http.Header{}
	if got := cancelIdentityR487(h2, "alice"); got != "user:alice" {
		t.Fatalf("identity из тела = %q, want user:alice", got)
	}
	h3 := http.Header{}
	h3.Set("X-User-Id", "u-1")
	h3.Set("X-Client-ID", "cline-9")
	if got := cancelIdentityR487(h3, ""); got != "user:u-1" {
		t.Fatalf("X-User-Id должен быть приоритетнее: %q", got)
	}
}

// TestR487_QueueWaitContextIsNotTheRequestContext — снятие заявки не рушит ctx
// соседней сессии: cancel выставляется только на СВОЙ дочерний контекст.
func TestR487_QueueWaitContextIsNotTheRequestContext(t *testing.T) {
	p, _ := newCancelTestProxy(t, 1)
	idx := p.requestIndexR487()
	parent := context.Background()
	child, cancel := context.WithCancel(parent)
	idx.TrackQueued("req-x", "sess-x", "", "backend-1", cancel)

	task, ok := idx.Lookup("req-x")
	if !ok || !task.Queued {
		t.Fatal("заявка не зарегистрирована")
	}
	task.CancelQue()
	if child.Err() == nil {
		t.Fatal("дочерний контекст не отменён")
	}
	if parent.Err() != nil {
		t.Fatal("отмена заявки не должна отменять родительский контекст")
	}
}
