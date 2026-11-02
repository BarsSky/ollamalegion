// backend_wait_queue_r403_test.go — R403 (2026-11-05).
//
// Очередь ожидания СВОБОДНОГО БЭКЕНДА (не слота): когда healthy llama.cpp
// бэкендов нет вовсе, запрос ждёт их появления до LB_BACKEND_WAIT_SEC, а не
// получает немедленный 503. FIFO по модели, чтобы быстрый retry одного клиента
// не перехватывал бэкенд у остальных.
package balancer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestR403_QueueFIFO — первый ожидающий держит очередь модели; после его ухода
// первым становится следующий.
func TestR403_QueueFIFO(t *testing.T) {
	q := &backendWaitQueue{waiters: map[string][]uint64{}}
	a, posA := q.enqueue("m")
	b, posB := q.enqueue("m")
	if posA != 1 || posB != 2 {
		t.Fatalf("позиции = %d,%d, want 1,2", posA, posB)
	}
	if !q.isFirst("m", a) || q.isFirst("m", b) {
		t.Fatal("первым обязан быть A, B не должен перехватывать бэкенд")
	}
	// Другая модель — своя очередь: ожидание одной не блокирует другую.
	otherID, _ := q.enqueue("other")
	if !q.isFirst("other", otherID) {
		t.Fatal("очередь по модели 'other' должна быть независимой")
	}
	q.dequeue("m", a)
	if !q.isFirst("m", b) {
		t.Fatal("после ухода A первым становится B")
	}
	q.dequeue("m", b)
	if q.position("m", b) != 0 {
		t.Fatal("B обязан покинуть очередь")
	}
}

// TestR403_WaitGetsBackend — ожидание завершается успехом, когда probe
// возвращает бэкенд (не дольше таймаута).
func TestR403_WaitGetsBackend(t *testing.T) {
	q := &backendWaitQueue{waiters: map[string][]uint64{}}
	calls := 0
	probe := func() string {
		calls++
		if calls >= 3 {
			return "backend-1"
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id, waited, pos, ok := waitBackendR403(ctx, q, "m", 2*time.Second, probe)
	if !ok || id != "backend-1" {
		t.Fatalf("ожидание не вернуло бэкенд: id=%q ok=%v", id, ok)
	}
	if waited <= 0 {
		t.Fatalf("waited=%v, want >0 (запрос реально ждал)", waited)
	}
	if pos != 1 {
		t.Fatalf("position=%d, want 1", pos)
	}
	if q.position("m", 1) != 0 {
		t.Fatal("после успеха ожидающий обязан покинуть очередь")
	}
}

// TestR403_WaitTimeout — таймаут: false, очередь очищена (position 0).
func TestR403_WaitTimeout(t *testing.T) {
	q := &backendWaitQueue{waiters: map[string][]uint64{}}
	id, waited, pos, ok := waitBackendR403(context.Background(), q, "m",
		300*time.Millisecond, func() string { return "" })
	if ok || id != "" {
		t.Fatalf("ожидался таймаут, получено id=%q ok=%v", id, ok)
	}
	if waited < 250*time.Millisecond {
		t.Fatalf("waited=%v, want >=250ms", waited)
	}
	// position — позиция на момент выхода (полезна в 503), но очередь после
	// возврата обязана быть очищена.
	if pos < 1 {
		t.Fatalf("pos=%d, want >=1 (позиция на момент таймаута)", pos)
	}
	q.mu.Lock()
	left := len(q.waiters["m"])
	q.mu.Unlock()
	if left != 0 {
		t.Fatalf("в очереди осталось %d ожидающих, want 0", left)
	}
}

// TestR403_WaitDisabledIsImmediate — LB_BACKEND_WAIT_SEC=0 сохраняет прежнее
// поведение: один быстрый probe, без очереди.
func TestR403_WaitDisabledIsImmediate(t *testing.T) {
	q := &backendWaitQueue{waiters: map[string][]uint64{}}
	start := time.Now()
	id, _, _, ok := waitBackendR403(context.Background(), q, "m", 0, func() string { return "b" })
	if !ok || id != "b" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("timeout=0: id=%q ok=%v elapsed=%v", id, ok, time.Since(start))
	}
	if id2, waited, _, ok2 := waitBackendR403(context.Background(), q, "m", 0, func() string { return "" }); ok2 || id2 != "" || waited != 0 {
		t.Fatalf("timeout=0 без бэкенда: id=%q ok=%v waited=%v", id2, ok2, waited)
	}
}

// TestR403_TimeoutFromEnv — LB_BACKEND_WAIT_SEC читается (горячая настройка
// перекрывает env, поэтому проверяем, что env даёт ожидаемое значение при
// отсутствии override).
func TestR403_TimeoutFromEnv(t *testing.T) {
	t.Setenv("LB_BACKEND_WAIT_SEC", "7")
	if got := backendWaitTimeoutR403(); got != 7*time.Second {
		t.Fatalf("timeout = %v, want 7s", got)
	}
	t.Setenv("LB_BACKEND_WAIT_SEC", "0")
	if got := backendWaitTimeoutR403(); got != 0 {
		t.Fatalf("timeout = %v, want 0 (выключено)", got)
	}
}

// TestR403_UnavailableResponse — 503 обязан нести числа и позицию, а не только
// текст: иначе клиент не отличает «нет бэкенда» от «подожди и повтори».
func TestR403_UnavailableResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	writeBackendUnavailableR403(rec, "gemma-4", 1500*time.Millisecond, 2, true)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Error("нет Retry-After")
	}
	if got := rec.Header().Get("X-Backend-Queue-Position"); got != "2" {
		t.Errorf("X-Backend-Queue-Position = %q, want 2", got)
	}
	if got := rec.Header().Get("X-Backend-Queue-Wait-Ms"); got != "1500" {
		t.Errorf("X-Backend-Queue-Wait-Ms = %q, want 1500", got)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("тело не JSON: %v", err)
	}
	if body["code"] != "no_backend" || body["model"] != "gemma-4" {
		t.Fatalf("тело без кода/модели: %v", body)
	}
	if msg, _ := body["error"].(string); msg != "no llama.cpp backend available" {
		t.Errorf("error = %q, want прежний текст контракта", msg)
	}
	if pos, _ := body["position"].(float64); int(pos) != 2 {
		t.Errorf("position = %v, want 2", body["position"])
	}
}
