// admission_pending_r329_test.go — R329 (2026-10-30).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «запрос крутится, а на мониторе его нет — будто
// потерялся». Причина: строки ожидания в панели очереди не писались с R78
// (qm.pending оставался мёртвым срезом), и оператор видел только агрегат
// CurrentSize — то есть «сколько-то ждут», без модели, цели и времени ожидания.
//
// Тесты фиксируют: строки появляются на время ожидания слота, снимаются по
// выходу (в том числе параллельно для разных запросов) и уходят в DTO-ответ.
package balancer

import (
	"testing"
	"time"
)

// TestR329_PendingRowVisibleWhileWaiting — строка есть, пока ждём, и уходит при выходе.
func TestR329_PendingRowVisibleWhileWaiting(t *testing.T) {
	qm := NewQueueManager(nil, 1, 1, time.Minute)
	p := &Proxy{queueMgr: qm}

	row := p.noteAdmissionPendingR329("gemma-4-E4B-it-Q4_K_M", "cppworker-gpu")
	if row == nil {
		t.Fatal("строка ожидания не создана — монитор снова покажет только агрегат")
	}

	dtos := qm.getPendingDTOs()
	if len(dtos) != 1 {
		t.Fatalf("pending DTO: %d строк, want 1", len(dtos))
	}
	if dtos[0]["model"] != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("модель в строке = %v, want gemma-4-E4B-it-Q4_K_M", dtos[0]["model"])
	}
	if dtos[0]["target"] != "cppworker-gpu" {
		t.Errorf("цель в строке = %v, want cppworker-gpu", dtos[0]["target"])
	}
	if dtos[0]["status"] != "pending" {
		t.Errorf("статус = %v, want pending", dtos[0]["status"])
	}

	p.clearAdmissionPendingR329(row)
	if got := len(qm.getPendingDTOs()); got != 0 {
		t.Errorf("после выдачи слота осталось %d строк — монитор будет показывать фантомы", got)
	}
}

// TestR329_ParallelRequestsKeepTheirOwnRows — удаление по указателю: два запроса
// с одной моделью не снимают строку друг друга.
func TestR329_ParallelRequestsKeepTheirOwnRows(t *testing.T) {
	qm := NewQueueManager(nil, 1, 1, time.Minute)
	p := &Proxy{queueMgr: qm}

	first := p.noteAdmissionPendingR329("m", "b1")
	second := p.noteAdmissionPendingR329("m", "b1")
	if len(qm.getPendingDTOs()) != 2 {
		t.Fatalf("ожидались две строки для двух запросов, got %d", len(qm.getPendingDTOs()))
	}

	p.clearAdmissionPendingR329(first)
	if got := len(qm.getPendingDTOs()); got != 1 {
		t.Fatalf("после снятия первой строки осталось %d, want 1", got)
	}

	p.clearAdmissionPendingR329(second)
	if got := len(qm.getPendingDTOs()); got != 0 {
		t.Errorf("после снятия обеих строк осталось %d", got)
	}
}

// TestR329_NilSafety — без менеджера очереди (юнит-пути) ничего не падает.
func TestR329_NilSafety(t *testing.T) {
	var p *Proxy
	if row := p.noteAdmissionPendingR329("m", "b"); row != nil {
		t.Error("nil-Proxy обязан вернуть nil")
	}
	p.clearAdmissionPendingR329(nil) // не должно паниковать

	qm := NewQueueManager(nil, 1, 1, time.Minute)
	if row := qm.noteAdmissionPendingR329("m", "b"); row == nil {
		t.Fatal("менеджер очереди обязан создать строку")
	}
	qm.clearAdmissionPendingR329(nil) // nil-строка — no-op
	if got := len(qm.getPendingDTOs()); got != 1 {
		t.Errorf("nil-строка не должна снимать чужие записи, осталось %d", got)
	}
}

// TestR329_PendingDTOCarriesWaitTime — waitTimeMs растёт: оператор видит, сколько ждём.
func TestR329_PendingDTOCarriesWaitTime(t *testing.T) {
	qm := NewQueueManager(nil, 1, 1, time.Minute)
	p := &Proxy{queueMgr: qm}
	row := p.noteAdmissionPendingR329("m", "b")
	defer p.clearAdmissionPendingR329(row)

	time.Sleep(15 * time.Millisecond)
	dtos := qm.getPendingDTOs()
	if len(dtos) != 1 {
		t.Fatalf("pending DTO: %d строк, want 1", len(dtos))
	}
	ms, ok := dtos[0]["waitTimeMs"].(int64)
	if !ok || ms < 10 {
		t.Errorf("waitTimeMs = %v (%T), want >= 10ms — иначе непонятно, сколько запрос ждёт", dtos[0]["waitTimeMs"], dtos[0]["waitTimeMs"])
	}
}
