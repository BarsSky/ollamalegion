// queue_gate_r480_test.go — R480 (P1-6, P1-7): гейт R459 на обходных путях.
//
// P1-6: переезд «исключая бэкенд» (занятый слот, 503, failover) выбирал копию
// только по загрузке — теперь отбор идёт с предикатом «нужного окна».
//
// P1-7: единая очередь не выдаёт слот мимо того же предиката живой сессии:
// лучше подождать слот своей копии (R139), чем молча уехать на холодную/узкую.
package balancer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR480_GatedSelectorSkipsNarrowCandidate — P1-6: отбор «исключая бэкенд»
// с предикатом действительно пропускает кандидатов через предикат, а не берёт
// первый свободный.
func TestR480_GatedSelectorSkipsNarrowCandidate(t *testing.T) {
	t.Setenv("LB_R459_SESSION_TTL_SEC", "900")
	p := r480SelectorProxy(t)

	// Предикат, который допускает только b1: при исключении b1 кандидатов нет.
	onlyB1 := func(id string) bool { return id == "b1" }
	if got := p.selectBackendExcludingGatedR480(r459Model, map[string]bool{"b1": true}, types.BackendTypeLlamaCpp, onlyB1); got != "" {
		t.Fatalf("выбран %q, хотя предикат допускает только b1", got)
	}

	// Предикат допускает всё — работает прежний отбор (b2 единственный).
	allowAll := func(string) bool { return true }
	if got := p.selectBackendExcludingGatedR480(r459Model, map[string]bool{"b1": true}, types.BackendTypeLlamaCpp, allowAll); got != "b2" {
		t.Fatalf("получено %q, ожидался b2", got)
	}

	// Без предиката поведение прежнее — путь не сломан для новых сессий.
	if got := p.selectBackendExcluding(r459Model, map[string]bool{"b1": true}, types.BackendTypeLlamaCpp); got != "b2" {
		t.Fatalf("без предиката получено %q, ожидался b2", got)
	}

	// Гейт R459 с need 65536 (окно b1) отклоняет узкий b2 — на этом пути тоже.
	gate := p.sessionMoveGateR480("b1", r459Model, "", 0, 12000)
	if got := p.selectBackendExcludingGatedR480(r459Model, map[string]bool{"b1": true}, types.BackendTypeLlamaCpp, gate); got != "" {
		t.Fatalf("выбран %q: 32768 < need 65536, обходной путь обязан отклонить", got)
	}
}

// r480SelectorProxy — НАСТОЯЩИЙ Proxy (нужен queueMgr для selectByResources):
// b1 с окном 65536, b2 с окном 32768, по 10 слотов.
//
// ВАЖНО: состояние гоняет только через блокирующие API (UpdateLlamaCppMetrics,
// UpdateMetrics, MarkLlamaCppPolled). Прямая запись в p.backends[...].ActiveReqs
// в тестах ловилась -race: health-check/poller читают те же поля из горутин.
func r480SelectorProxy(t *testing.T) *Proxy {
	t.Helper()
	return newR480SelectorProxy(t, 10)
}

func newR480SelectorProxy(t *testing.T, slots int) *Proxy {
	t.Helper()
	cfg := &types.LoadBalancerConfig{
		LoadBalancer: types.LoadBalancerSettings{
			Host: "localhost", Port: 8080, APIPort: 8081,
			StatePath: filepath.Join(t.TempDir(), "state.json"),
		},
		Backends: []types.Backend{
			{ID: "b1", Name: "b1", Host: "127.0.0.1", OllamaPort: 1, CppWorkerPort: 2,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: slots, Weight: 1},
			{ID: "b2", Name: "b2", Host: "127.0.0.1", OllamaPort: 3, CppWorkerPort: 4,
				Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy, MaxConcurrentReqs: slots, Weight: 1},
		},
		Balancing: types.BalancingSettings{
			Algorithm: types.AlgorithmResourceAware, QueueMaxSize: 100, QueueWorkers: 1,
			QueueTimeout: 30, HealthCheckInterval: 10, MetricsInterval: 5,
			SessionStickiness: true,
		},
	}
	p := newProxyWithCleanup(t, cfg)
	for id, perSeq := range map[string]int{"b1": 65536, "b2": 32768} {
		// R460-отказы читают SnapshotBackendMetrics — без этой записи бэкенд
		// выглядит как «метрик нет», и причина окна до него не доходит.
		// ВАЖНО: UpdateMetrics перезаписывает llamaMetrics из metrics.LlamaCpp,
		// поэтому окно ставим ПОСЛЕ него.
		p.UpdateMetrics(id, &types.BackendMetrics{
			ID:       id,
			LlamaCpp: types.LlamaCppMetrics{LoadedModels: []types.LlamaCppModel{{Name: r459Model}}},
		})
		p.metricsMgr.UpdateLlamaCppMetrics(id, &types.LlamaCppMetrics{
			LoadedModels: []types.LlamaCppModel{{Name: r459Model, ContextLength: perSeq}},
		})
		p.metricsMgr.MarkLlamaCppPolled(id, time.Now())
	}
	return p
}

// TestR480_QueueGateRejectsCandidate — P1-7: очередь с предикатом не берёт
// слот на отклонённом бэкенде (даже если он свободен).
func TestR480_QueueGateRejectsCandidate(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	reject := func(string) bool { return false }

	id, noCandidate := proxy.tryAcquireAnyBackendSlotDetailedGatedR480("m1", types.BackendTypeOllama, reject)
	if id != "" {
		t.Fatalf("очередь захватила слот на отклонённом бэкенде %q", id)
	}
	if noCandidate {
		t.Fatal("noCandidate=true, хотя кандидат был (его отклонил предикат, а не отсутствие)")
	}

	allow := func(string) bool { return true }
	id, _ = proxy.tryAcquireAnyBackendSlotDetailedGatedR480("m1", types.BackendTypeOllama, allow)
	if id != "ollama-1" {
		t.Fatalf("с разрешающим предикатом получено %q, ожидался ollama-1", id)
	}
	proxy.releaseSlot("ollama-1")
}

// TestR480_QueueGateWaitsInsteadOfMoving — P1-7: слот занят, предикат отклоняет
// единственный бэкенд — запрос ждёт (таймаут), а не уезжает.
func TestR480_QueueGateWaitsInsteadOfMoving(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	proxy.setAdmissionWait(200 * time.Millisecond)
	if !proxy.tryAcquireSlot("ollama-1") {
		t.Fatal("не удалось занять слот")
	}
	defer proxy.releaseSlot("ollama-1")

	reject := func(string) bool { return false }
	_, _, _, _, err := proxy.waitForInferenceBackendGatedR480(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u9", reject)
	if err != errAdmissionTimeout {
		t.Fatalf("ожидался errAdmissionTimeout (ждать свой слот), получено %v", err)
	}
	if waiting := proxy.admission.waitingCount(); waiting != 0 {
		t.Fatalf("после таймаута в очереди осталось %d ожидающих", waiting)
	}
}

// TestR480_QueueGateNilKeepsOldPath — gate == nil: очередь ведёт себя как до
// R480 (новые сессии не должны пострадать).
func TestR480_QueueGateNilKeepsOldPath(t *testing.T) {
	proxy := newUnifiedQueueProxyR73(t, 1)
	id, release, _, _, err := proxy.waitForInferenceBackendGatedR480(
		context.Background(), "m1", types.BackendTypeOllama, "X-User-Id:u10", nil)
	if err != nil {
		t.Fatalf("без предиката ожидался свободный слот, получена ошибка: %v", err)
	}
	if id != "ollama-1" {
		t.Fatalf("получено %q, ожидался ollama-1", id)
	}
	if release != nil {
		release()
	}
}
