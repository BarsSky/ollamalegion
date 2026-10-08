// admission_stats_detail_r91_test.go — R91 (2026-10-08): поимённый список
// ожидающих в admission-статистике.
//
// ЖАЛОБА ОПЕРАТОРА. «При большом количестве сессий на два бэкенда совершенно не
// отображается правильно состояние очереди, от чего возникает вопрос — работает
// ли она». Разбор: страница «Очередь» рисует ЛЕГАСИ-очередь
// (/api/v1/queue/details → all/pending/processing), а в этом развёртывании она
// всегда пуста: реальное ожидание слота живёт в admission-очереди. Живой замер:
// queue/details → {"all":[],"total":0}, queue/stats → admission{served_total:42,
// waited_total:42, avg_wait_ms:13926}. То есть 42 запроса прождали в среднем
// 13.9 с, а страница показывала «Очередь пуста».
//
// Чтобы в таблице очереди было что показать (кто ждёт, на каком бэкенде и
// сколько уже ждёт), в queueAdmissionStats добавлен WaitingDetail. Тест
// фиксирует его содержимое и порядок.
package balancer

import (
	"context"
	"testing"
	"time"
)

// TestAdmissionStats_WaitingDetail — детальный снимок очереди.
func TestAdmissionStats_WaitingDetail(t *testing.T) {
	t.Setenv("LB_ADMISSION_KEEPALIVE_SEC", "0")
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(5 * time.Second)

	// Слот занят «другим клиентом», поэтому следующие два запроса встанут в очередь.
	if !p.tryAcquireSlot("llama_adm") {
		t.Fatal("не удалось занять единственный слот")
	}
	defer p.releaseSlot("llama_adm")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, session := range []string{"X-User-Id:alice", "X-User-Id:bob"} {
		session := session
		go func() {
			lease, _, err := p.acquireInferenceSlot(ctx, "llama_adm", session)
			if lease != nil {
				lease.release()
			}
			_ = err
		}()
	}
	waitForAdmissionWaiters(t, p, 2)

	st := p.admission.stats(p.admissionWaitTimeout())

	if st.Waiting != 2 {
		t.Fatalf("Waiting = %d, want 2", st.Waiting)
	}
	if len(st.WaitingDetail) != 2 {
		t.Fatalf("WaitingDetail = %d записей, want 2: %+v", len(st.WaitingDetail), st.WaitingDetail)
	}
	// Порядок в очереди определяется моментом постановки (seq), а не порядком
	// запуска горутин, поэтому проверяем состав, а не конкретную перестановку.
	seen := map[string]bool{}
	for _, w := range st.WaitingDetail {
		seen[w.Session] = true
		if w.Backend != "llama_adm" {
			t.Errorf("Backend = %q, want llama_adm", w.Backend)
		}
		if w.WaitedMs < 0 {
			t.Errorf("WaitedMs = %d, не может быть отрицательным", w.WaitedMs)
		}
	}
	for _, want := range []string{"X-User-Id:alice", "X-User-Id:bob"} {
		if !seen[want] {
			t.Errorf("в WaitingDetail нет %q: %+v", want, st.WaitingDetail)
		}
	}
	// Список и «плоский» Sessions должны идти в одном порядке: их читает одна и
	// та же таблица UI, расхождение выглядело бы как перестановка очереди.
	for i := range st.WaitingDetail {
		if st.WaitingDetail[i].Session != st.Sessions[i] {
			t.Errorf("порядок Sessions (%v) не совпадает с WaitingDetail (%+v)",
				st.Sessions, st.WaitingDetail)
			break
		}
	}
	// Порядок стабилен между опросами: UI перерисовывает очередь ежесекундно, и
	// «прыгающие» строки читались бы как работающая очередь с другим составом.
	again := p.admission.stats(p.admissionWaitTimeout())
	for i := range again.WaitingDetail {
		if again.WaitingDetail[i].Session != st.WaitingDetail[i].Session {
			t.Errorf("порядок очереди изменился между опросами: %+v → %+v",
				st.WaitingDetail, again.WaitingDetail)
			break
		}
	}
	// Карта по бэкендам и поимённый список должны сходиться: иначе в UI
	// «счётчик 2, список пуст».
	if st.ByBackend["llama_adm"] != len(st.WaitingDetail) {
		t.Errorf("waiting_by_backend=%d, waiting_detail=%d — расхождение",
			st.ByBackend["llama_adm"], len(st.WaitingDetail))
	}
	if len(st.Sessions) != len(st.WaitingDetail) {
		t.Errorf("sessions=%d, waiting_detail=%d — расхождение", len(st.Sessions), len(st.WaitingDetail))
	}
}

// TestAdmissionStats_WaitingDetailEmptyWhenIdle — пустая очередь даёт пустой
// список (и он не сериализуется: omitempty), иначе UI рисовал бы «ожидающих»
// без ожидания.
func TestAdmissionStats_WaitingDetailEmptyWhenIdle(t *testing.T) {
	p := admissionTestProxy(t, 1)
	p.setAdmissionWait(time.Second)

	st := p.admission.stats(p.admissionWaitTimeout())
	if st.Waiting != 0 || len(st.WaitingDetail) != 0 {
		t.Fatalf("простаивающая очередь не пуста: waiting=%d detail=%+v", st.Waiting, st.WaitingDetail)
	}
	if st.Enabled != true {
		t.Error("Enabled должен отражать ненулевой предел ожидания")
	}
}
