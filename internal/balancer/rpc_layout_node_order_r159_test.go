//go:build llama_stub

// rpc_layout_node_order_r159_test.go — R159 (2026-10-17): повтор приказа узлу
// раскладки не каждый тик.
//
// ЖИВОЙ ДЕФЕКТ. Троттлинг повтора работал только для случая «воркер вообще не
// отчитывается» (`!known`). Самый частый случай — другой: воркер ОТЧИТАЛСЯ, но
// порт ещё не слушает (узел стартует, или порт не опубликован). В этой ветке
// приказ уходил каждый проход (~15 с), а каждый приказ — запись файла
// операторских переопределений на донорской машине. Наблюдалось живьём: после
// пересоздания контейнера донора балансер молотил приказом до перезапуска.
//
// КОНТРФАКТУАЛЬНОСТЬ: тест «не повторять» на старом коде ПАДАЕТ (старый код
// отправлял приказ на каждом проходе), «повторить после интервала» и «ответить
// сразу на падение узла» держат обратную сторону — фикс не должен превратиться
// в «приказ один раз и навсегда».
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// reportNodeStateAt — воркер сообщил состояние СВОЕГО узла раскладки.
func reportNodeStateAt(p *Proxy, backendID string, running, listening bool, port int) {
	p.metricsMgr.mu.Lock()
	defer p.metricsMgr.mu.Unlock()
	p.metricsMgr.llamaMetrics[backendID] = &types.LlamaCppMetrics{
		RPCNode: &types.RPCNodeStatus{
			Enabled: running || listening, Running: running, Listening: listening, Port: port,
		},
	}
}

// backdateOrder — сделать вид, что последний приказ отправлен давно.
func backdateOrder(p *Proxy, backendID string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rpcNodeOrderAt == nil {
		p.rpcNodeOrderAt = map[string]time.Time{}
	}
	p.rpcNodeOrderAt[backendID] = time.Now().Add(-d)
}

// TestR159OrderNotRepeatedWhileNodeIsStarting — главный контрпример: узел
// стартует (процесс есть, порт ещё нет), состояние не меняется — приказ один.
func TestR159OrderNotRepeatedWhileNodeIsStarting(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	reportNodeStateAt(p, "node-1", true, false, 50052) // «starting»

	changed, reason := p.EnsureRPCNodeServing("node-1")
	if !changed {
		t.Fatalf("первый приказ не отправлен (reason=%q)", reason)
	}
	// Дальше — три прохода подряд при том же состоянии воркера.
	for i := 2; i <= 4; i++ {
		if changed, _ := p.EnsureRPCNodeServing("node-1"); changed {
			t.Fatalf("проход %d: приказ ПОВТОРЁН при неизменном состоянии узла "+
				"(на доноре — запись файла каждый тик)", i)
		}
	}
	if n := len(nodePushes(rec)); n != 1 {
		t.Errorf("приказов узлу = %d, want 1 (старый код дал бы 4)", n)
	}
}

// TestR159OrderRepeatedAfterRetryInterval — обратная сторона: если состояние
// залипло (воркер не двигается), приказ обязан повторяться — иначе узел никогда
// не поднимется после пересоздания воркера.
func TestR159OrderRepeatedAfterRetryInterval(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	reportNodeStateAt(p, "node-1", true, false, 50052)

	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatal("подготовка: приказ не отправлен")
	}
	backdateOrder(p, "node-1", rpcNodeOrderRetryInterval+time.Minute)

	if changed, reason := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatalf("после интервала приказ не повторён (reason=%q): самовосстановление сломано", reason)
	}
	if n := len(nodePushes(rec)); n != 2 {
		t.Errorf("приказов узлу = %d, want 2 (первый + повтор после интервала)", n)
	}
}

// TestR159ImmediateOrderWhenNodeFell — состояние ИЗМЕНИЛОСЬ (узел упал): ждать
// интервал нельзя, упавший узел чинят сразу.
func TestR159ImmediateOrderWhenNodeFell(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor)
	reportNodeStateAt(p, "node-1", true, false, 50052) // starting
	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatal("подготовка: приказ не отправлен")
	}

	reportNodeStateAt(p, "node-1", false, false, 50052) // упал
	if changed, reason := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatalf("упавший узел НЕ получил приказ (reason=%q): ждёт интервал вместо немедленной реакции", reason)
	}
	if n := len(nodePushes(rec)); n != 2 {
		t.Errorf("приказов узлу = %d, want 2 (подъём + реакция на падение)", n)
	}
}

// TestR159UnknownStateStillThrottled — поведение старого случая не сломано:
// воркер не отчитывается вовсе (старый образ) — повтор не чаще интервала.
func TestR159UnknownStateStillThrottled(t *testing.T) {
	p, rec := newRPCNodeTestProxy(t, types.BackendRoleDonor) // метрик нет → known=false

	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Fatal("подготовка: приказ не отправлен")
	}
	if changed, _ := p.EnsureRPCNodeServing("node-1"); changed {
		t.Error("воркер не отчитывается, а приказ повторён сразу")
	}
	if n := len(nodePushes(rec)); n != 1 {
		t.Errorf("приказов узлу = %d, want 1", n)
	}

	backdateOrder(p, "node-1", rpcNodeOrderRetryInterval+time.Minute)
	if changed, _ := p.EnsureRPCNodeServing("node-1"); !changed {
		t.Error("после интервала приказ не повторён: донор со старым образом не получит узел")
	}
}

// TestR159StateKey — грубая классификация состояния (по ней решается «ждём» или
// «действуем сейчас»). Контрпримеры подобраны по живым наблюдениям.
func TestR159StateKey(t *testing.T) {
	cases := []struct {
		known, running, listening bool
		want                      string
	}{
		{false, false, false, "unknown"}, // поллер ещё не собрал rpc_server
		{true, true, true, "up"},         // узел работает
		{true, true, false, "starting"},  // процесс есть, порт не слушается
		{true, false, true, "down"},      // порт остался, процесса нет — тоже down
		{true, false, false, "down"},     // узел упал
	}
	for _, c := range cases {
		if got := rpcNodeStateKey(c.known, c.running, c.listening); got != c.want {
			t.Errorf("rpcNodeStateKey(known=%v running=%v listening=%v) = %q, want %q",
				c.known, c.running, c.listening, got, c.want)
		}
	}
}
