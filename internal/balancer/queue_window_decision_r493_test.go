// queue_window_decision_r493_test.go — решение оператора по архиву стенда
// (2026-11-12, A10 ×2, Qwen3.8-27B).
//
// ПРАВИЛО (дословно): «если копия не занята сессией, то принимать решение по
// выравниванию; если же занята, а есть бэкенд с нужным окном — ставить в очередь
// к нему на обработку».
//
// ЖИВАЯ РАСКЛАДКА ИЗ АРХИВА:
//
//	мастер        — 27B, окно 198656, активных 1 (слота нет), сессия привязана к нему;
//	A10-126       — 27B, окно 131072, активных 0 (свободен);
//	WIN-I9/ASTRA  — 8 ГБ, модели нет (27B туда не влезет).
//
// ЧЕМ ЭТО КОНЧАЛОСЬ ДО ПРАВКИ: гейт R459 не строился вовсе (в proxy.go «from»
// обнулялся, если у копии сессии нет слота), очередь брала свободный A10-126 с
// МЕНЬШИМ окном, дальше шли загрузки на 8-гигабайтные узлы, и заявка получала
// 503 на стадии select_backend.
//
// ЧТО ПРОВЕРЯЕМ СЕЙЧАС:
//  1. свободная копия с МЕНЬШИМ окном не получает заявку живой сессии (её слот не
//     захватывается) — вместо этого заявка ждёт слот подходящей копии;
//  2. свободная копия с НЕ МЕНЬШИМ окном забирает заявку сразу (занятость
//     мастера не должна тормозить обработку);
//  3. источник окна (stickyBackendForGateR493) отдаёт бэкенд сессии даже когда у
//     него нет свободного слота — именно из него строится гейт.
package balancer

import (
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

const (
	queueModelR493  = "m27b"
	queueMasterR493 = "master"
	queueNarrowR493 = "A10-126"
)

// queueProxyR493 — мастер (занят, окно wide) + свободная копия (окно cand).
func queueProxyR493(t *testing.T, wide, cand int) *Proxy {
	t.Helper()
	conf := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{Host: "localhost", Port: 8080, APIPort: 8081},
		Backends: []types.Backend{
			{ID: queueMasterR493, Name: "master", Host: "127.0.0.1", OllamaPort: 1,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
			{ID: queueNarrowR493, Name: "a10-126", Host: "127.0.0.1", OllamaPort: 2,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: 1, Weight: 1},
		},
		Balancing: types.BalancingSettings{Algorithm: types.AlgorithmResourceAware, MetricsInterval: 60},
	}
	p := newProxyWithCleanup(t, conf)

	setWin := func(id string, perSeq int) {
		lm := &types.LlamaCppMetrics{
			LoadedModels:    []types.LlamaCppModel{{Name: queueModelR493, State: "loaded", ContextLength: perSeq, MaxSlots: 1}},
			VramKnown:       true,
			AvailableVRAMMB: 3000,
		}
		p.metricsMgr.mu.Lock()
		p.metricsMgr.llamaMetrics[id] = lm
		p.metricsMgr.metrics[id] = &types.BackendMetrics{ID: id, LlamaCpp: *lm}
		p.metricsMgr.mu.Unlock()
		p.metricsMgr.MarkLlamaCppPolled(id, time.Now())
	}
	setWin(queueMasterR493, wide)
	setWin(queueNarrowR493, cand)

	// Мастер занят: единственный слот держит активный запрос (как в архиве).
	p.backends[queueMasterR493].ActiveReqs = 1
	return p
}

// TestR493_SaturatedWideCopyIsWaitedOn — правило 1: заявка не уезжает на
// свободную копию с меньшим окном, а ждёт слот подходящей.
func TestR493_SaturatedWideCopyIsWaitedOn(t *testing.T) {
	p := queueProxyR493(t, 198656, 131072)
	p.sessionMgr.Set("sess-a10", queueMasterR493, queueModelR493, "client", "127.0.0.1", "ua")

	from := p.stickyBackendForGateR493("sess-a10")
	if from != queueMasterR493 {
		t.Fatalf("источник окна = %q, ожидался мастер: без него гейт не строится и заявка "+
			"уезжает на копию с меньшим окном (живой 503 на select_backend)", from)
	}

	gate, fromWindow := p.sessionQueueGateR480(from, queueModelR493)
	if gate == nil {
		t.Fatalf("гейт очереди не построен (окно копии сессии = %d)", fromWindow)
	}
	if gate(queueNarrowR493) {
		t.Fatalf("гейт пропустил свободную копию с окном 131072 при нужном 198656 — " +
			"именно так заявка и уехала на A10-126 в архиве")
	}
	if !gate(queueMasterR493) {
		t.Fatal("гейт обязан пропускать собственную копию сессии: её слот и ждём")
	}

	// Очередь не должна захватить слот узкой копии.
	if id, _ := p.tryAcquireAnyBackendSlotDetailedGatedR480(queueModelR493, types.BackendTypeLlamaCpp, gate); id != "" {
		t.Fatalf("очередь захватила слот на %q, хотя окно не покрывает нужное — "+
			"заявка должна ждать слот своей копии", id)
	}
	if p.backends[queueNarrowR493].ActiveReqs != 0 {
		t.Fatal("слот узкой копии занят: очередь снова обошла гейт окна")
	}
}

// TestR493_FreeAdequateCopyTakesRequest — правило 2: если у свободной копии окно
// не меньше нужного, заявка обслуживается сразу (мастер занят — не повод ждать).
func TestR493_FreeAdequateCopyTakesRequest(t *testing.T) {
	p := queueProxyR493(t, 198656, 198656) // окно свободной копии не меньше
	p.sessionMgr.Set("sess-eq", queueMasterR493, queueModelR493, "client", "127.0.0.1", "ua")

	from := p.stickyBackendForGateR493("sess-eq")
	gate, _ := p.sessionQueueGateR480(from, queueModelR493)
	if gate == nil {
		t.Fatal("гейт не построен")
	}
	if !gate(queueNarrowR493) {
		t.Fatal("копия с равным окном обязана проходить гейт")
	}
	id, _ := p.tryAcquireAnyBackendSlotDetailedGatedR480(queueModelR493, types.BackendTypeLlamaCpp, gate)
	if id != queueNarrowR493 {
		t.Fatalf("заявку должен забрать свободный узел с достаточным окном, получено %q", id)
	}
}

// TestR493_GateSourceIgnoresSlotState — правило 3: источник окна не зависит от
// свободного слота; «предпочтение» — зависит. Это два разных вопроса, и склеивать
// их нельзя (склейка и дала дефект из архива).
func TestR493_GateSourceIgnoresSlotState(t *testing.T) {
	p := queueProxyR493(t, 198656, 131072)
	p.sessionMgr.Set("sess-2", queueMasterR493, queueModelR493, "client", "127.0.0.1", "ua")

	// Слот занят: предпочтения нет, источник окна есть.
	if got := p.stickyBackendIfAvailableR493("sess-2"); got != "" {
		t.Fatalf("предпочтение при занятом слоте = %q, ожидалось пусто", got)
	}
	if got := p.stickyBackendForGateR493("sess-2"); got != queueMasterR493 {
		t.Fatalf("источник окна при занятом слоте = %q, ожидался мастер", got)
	}

	// Слот освободился: оба отдают мастера.
	p.backends[queueMasterR493].ActiveReqs = 0
	if got := p.stickyBackendIfAvailableR493("sess-2"); got != queueMasterR493 {
		t.Fatalf("предпочтение при свободном слоте = %q, ожидался мастер", got)
	}
}
