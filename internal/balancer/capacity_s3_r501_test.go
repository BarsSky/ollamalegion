//go:build llama_stub

// capacity_s3_r501_test.go — R501 (2026-11-19), аудит S3, пункты C-2, C-3, C-5, C-6.
//
// Каждый тест держит ОДИН пункт аудита и падает, если правило откатили:
//
//	C-2 — маршрутизация читает эффективную вместимость, а не статику конфига;
//	C-3 — ноль и минус один читаются одинаково (types.CapacityFromValueR501):
//	      «не задано» и «без лимита» соответственно;
//	C-5 — снапшот кластера не отдаёт статику рядом с эффективной;
//	C-6 — операторский лимит не объявляется «значением ноды».
package balancer

import (
	"path/filepath"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// bareProxyR501 — прокси без фоновых циклов: тесты здесь не ходят в сеть.
func bareProxyR501() *Proxy {
	return &Proxy{
		config:     &types.LoadBalancerConfig{},
		backends:   map[string]*BackendState{},
		metricsMgr: NewMetricsManager(),
		sessionMgr: NewSessionManager(),
	}
}

// TestR501_LoadStateStampsNodeClaimAge — C-1: заявка узла получает отметку
// времени при загрузке состояния (момент записи state.json = момент последнего
// подтверждения). Без неё возраст считался бы «неизвестным» навсегда, и
// просроченная заявка жила бы вечно свежей.
func TestR501_LoadStateStampsNodeClaimAge(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                  "b1",
		Host:                "localhost",
		CppWorkerPort:       18092,
		Type:                types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:   2,
		NodeClaimedCapacity: 2, // заявка есть, отметки времени нет (старая сборка)
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 2,
	}})

	got := p.GetBackend("b1")
	if got.NodeClaimedAt == nil {
		t.Fatal("NodeClaimedAt не проставлен при LoadState: возраст заявки узла " +
			"останется «неизвестным» навсегда (аудит C-1)")
	}
	if age := time.Since(*got.NodeClaimedAt); age > time.Minute {
		t.Fatalf("отметка заявки узла = %v назад, ожидалось «только что»", age)
	}
}

// TestR501_MarkCapacityFromNodeStampsAge — C-1: путь саморегистрации обновляет
// отметку подтверждения вместе с самой заявкой.
func TestR501_MarkCapacityFromNodeStampsAge(t *testing.T) {
	b := &types.Backend{
		ID:                "fresh",
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 2,
		Labels:            []string{"auto-registered"},
	}
	markCapacityFromNode(b)
	if b.NodeClaimedAt == nil {
		t.Fatal("markCapacityFromNode не поставил отметку времени заявки узла")
	}
}

// TestR501_FreeSlotsUseEffectiveCapacity — C-2: свободные слоты считаются по
// ЭФФЕКТИВНОЙ вместимости. До правки здесь стояла статика конфига, и
// операторский лимит (runtime 2 при статике 4) на этом пути не соблюдался:
// least_loaded-селектор видел 4 свободных слота на узле, куда пускают 2.
func TestR501_FreeSlotsUseEffectiveCapacity(t *testing.T) {
	p := bareProxyR501()
	p.backends["bk"] = &BackendState{Backend: &types.Backend{
		ID:                           "bk",
		Type:                         types.BackendTypeLlamaCpp,
		Status:                       types.StatusHealthy,
		MaxConcurrentReqs:            4, // статика конфига
		RuntimeMaxConcurrentRequests: 2, // лимит оператора
	}}

	free, ok := p.GetBackendFreeSlots("bk")
	if !ok {
		t.Fatal("GetBackendFreeSlots: ok=false для существующего бэкенда")
	}
	if free != 2 {
		t.Fatalf("свободных слотов = %d, ожидалось 2: считает по статике (4), "+
			"операторский лимит не соблюдается (аудит C-2)", free)
	}

	p.backends["bk"].ActiveReqs = 1
	if free, _ := p.GetBackendFreeSlots("bk"); free != 1 {
		t.Fatalf("при одном занятом слоте свободно = %d, ожидалось 1", free)
	}
}

// TestR501_ClusterStateNoAgentReportsEffective — C-5: ветка БЕЗ агента отдаёт
// эффективную вместимость и в maxConcurrentRequests. До правки `/api/v1/cluster`
// отдавал статику (4) рядом с эффективной (1): панель рисовала узел с
// n_parallel=1 загруженным на 25% при полной занятости.
func TestR501_ClusterStateNoAgentReportsEffective(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())
	if err := p.AddBackend(types.Backend{
		ID:                           "no-agent",
		Name:                         "no-agent",
		Host:                         "cppworker-no-agent",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		Status:                       types.StatusHealthy,
		MaxConcurrentReqs:            4,
		RuntimeMaxConcurrentRequests: 1,
	}); err != nil {
		t.Fatalf("AddBackend: %v", err)
	}

	st := p.GetClusterState()
	var got *types.BackendMetrics
	for i := range st.Backends {
		if st.Backends[i].ID == "no-agent" {
			got = &st.Backends[i]
		}
	}
	if got == nil {
		t.Fatal("бэкенд no-agent отсутствует в снапшоте кластера")
	}
	if got.HasAgent {
		t.Fatal("у бэкенда нет агента, а снапшот считает, что есть")
	}
	if got.EffectiveMaxConcurrentRequests != 1 {
		t.Fatalf("effectiveMaxConcurrentRequests = %d, ожидалось 1",
			got.EffectiveMaxConcurrentRequests)
	}
	if got.MaxConcurrentRequests != 1 {
		t.Fatalf("maxConcurrentRequests = %d, ожидалось 1 (эффективная): "+
			"статика рядом с эффективной и есть дефект C-5", got.MaxConcurrentRequests)
	}
}

// TestR501_ReconcileDoesNotClaimOperatorLimitAsNode — C-6: операторский лимит,
// СОВПАВШИЙ со статикой, не объявляется «значением ноды». До правки
// reconcileNodeCapacity ставил признак по одному равенству
// runtimeMax == MaxConcurrentReqs, после чего заявка узла перетирала лимит.
func TestR501_ReconcileDoesNotClaimOperatorLimitAsNode(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	stateWithBackend(t, statePath, types.Backend{
		ID:                           "b1",
		Host:                         "localhost",
		CppWorkerPort:                18092,
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            4,
		RuntimeMaxConcurrentRequests: 4, // оператор выставил 4 через PUT /limits
		RuntimeCapacityFromNode:      false,
	})

	p := loadStateProxy(t, statePath, []types.Backend{{
		ID:                "b1",
		Host:              "localhost",
		CppWorkerPort:     18092,
		Type:              types.BackendTypeLlamaCpp,
		MaxConcurrentReqs: 4,
	}})

	got := p.GetBackend("b1")
	if got.RuntimeCapacityFromNode {
		t.Fatal("RuntimeCapacityFromNode = true после LoadState: операторское решение " +
			"объявлено значением ноды (аудит C-6)")
	}

	// Перерегистрация узла с заявкой 8 не имеет права поднять лимит оператора.
	p.AdoptCapacityFromNode("b1", 8)
	after := p.GetBackend("b1")
	eff, source := after.ResolveCapacity()
	if eff != 4 {
		t.Fatalf("после заявки узла 8 вместимость = %d, ожидалось 4 (лимит оператора)", eff)
	}
	if source != types.CapacitySourceOperator {
		t.Fatalf("источник = %q, ожидалось %q: снижение сделал оператор",
			source, types.CapacitySourceOperator)
	}
}

// TestR501_ReconcileMarksRealNodeClaim — C-6, обратная сторона: настоящее
// значение ноды (совпадает с сохранённой заявкой узла) признак получает, иначе
// после перезапуска балансера узел снова жил бы с чужим лимитом.
func TestR501_ReconcileMarksRealNodeClaim(t *testing.T) {
	b := types.Backend{
		ID:                           "node",
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            4,
		RuntimeMaxConcurrentRequests: 2,
		NodeClaimedCapacity:          2,
	}
	reconcileNodeCapacity(&b)
	if !b.RuntimeCapacityFromNode {
		t.Fatal("значение, совпадающее с заявкой узла, не помечено как значение ноды")
	}
}

// TestR501_ReconcileHealsLegacyEcho — C-6: устаревшее «эхо» агента (выше
// статики) по-прежнему лечится — иначе узел с n_parallel=1 жил бы с порогом 4.
func TestR501_ReconcileHealsLegacyEcho(t *testing.T) {
	b := types.Backend{
		ID:                           "legacy",
		Type:                         types.BackendTypeLlamaCpp,
		MaxConcurrentReqs:            1,
		RuntimeMaxConcurrentRequests: 4, // «эхо» старой сборки
	}
	reconcileNodeCapacity(&b)
	if b.RuntimeMaxConcurrentRequests != 1 {
		t.Fatalf("runtime-значение = %d, ожидалось 1 (эхо приведено к статике)",
			b.RuntimeMaxConcurrentRequests)
	}
	if !b.RuntimeCapacityFromNode {
		t.Fatal("после лечения эха признак «значение от ноды» обязан быть выставлен")
	}
}

// TestR501_NegativeCapacityMeansUnlimited — C-3: -1 документирован как «без
// лимита» (handlers_backends.go). До правки availabilityFirst превращал -1 в
// «лимит 1» и запрос уходил в очередь к узлу, который не ограничивает.
func TestR501_NegativeCapacityMeansUnlimited(t *testing.T) {
	p := bareProxyR501()
	p.backends["unl"] = &BackendState{Backend: &types.Backend{
		ID:                "unl",
		Type:              types.BackendTypeLlamaCpp,
		Status:            types.StatusHealthy,
		MaxConcurrentReqs: -1, // «без лимита»
	}}
	if eff := p.backends["unl"].Backend.EffectiveMaxConcurrentRequests(); eff != -1 {
		t.Fatalf("эффективная вместимость = %d, ожидалось -1 (без лимита)", eff)
	}
	p.backends["unl"].ActiveReqs = 9
	if !p.hasFreeSlotNowR493("unl") {
		t.Fatal("бэкенд с «без лимита» считается занятым при 9 активных запросах (аудит C-3)")
	}
	if !p.tryAcquireSlot("unl") {
		t.Fatal("admission не пускает запрос на бэкенд с «без лимита»")
	}
	if free, _ := p.GetBackendFreeSlots("unl"); free != 1000 {
		t.Fatalf("свободных слотов = %d, ожидалось 1000 (без лимита)", free)
	}
}

// TestR501_ZeroCapacityIsNotSet — C-3: ноль = «не задано», а не «слотов нет» и
// не «лимит 1». Предикат доступности берёт консервативный дефолт R117 (один
// слот), на бэкенд с известным номером это не влияет, но и «без лимита» из нуля
// не делается.
func TestR501_ZeroCapacityIsNotSet(t *testing.T) {
	p := bareProxyR501()
	p.backends["zero"] = &BackendState{Backend: &types.Backend{
		ID:     "zero",
		Type:   types.BackendTypeLlamaCpp,
		Status: types.StatusHealthy,
	}}
	if eff := p.backends["zero"].Backend.EffectiveMaxConcurrentRequests(); eff != 0 {
		t.Fatalf("эффективная вместимость = %d, ожидалось 0 (не задано)", eff)
	}
	if !p.hasFreeSlotNowR493("zero") {
		t.Fatal("пустой бэкенд без единого активного запроса обязан быть доступен")
	}
	p.backends["zero"].ActiveReqs = 1
	if p.hasFreeSlotNowR493("zero") {
		t.Fatal("ноль прочитан как «без лимита»: при неизвестной вместимости " +
			"предикат обязан держать один слот (аудит C-3)")
	}
	if free, _ := p.GetBackendFreeSlots("zero"); free != 1000 {
		t.Fatalf("свободных слотов = %d, ожидалось 1000 (лимит не ограничивает)", free)
	}
}
