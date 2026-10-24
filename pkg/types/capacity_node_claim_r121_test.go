//go:build llama_stub

// capacity_node_claim_r121_test.go — R121 (2026-10-15), пункт 1.
//
// ЖИВОЙ ДЕФЕКТ (воспроизведён на стенде). Узел честно заявляет вместимость 1
// (cppworker: defaultNParallel=2 при enableBatchedParallel=false → legacy-путь
// сериализует llama_decode), но после перезапуска БАЛАНСЕРА запись снова
// показывает 2 с источником «static», и heartbeat шлёт узлу 2.
//
// Замер до/после (node _diag/probe-capacity.js):
//
//	после рестарта cppworker:  effective=1 source=static   ← заявка узла, но БЕЗ владельца
//	после рестарта балансера:  effective=2 source=static   ← статика конфига победила
//
// Две причины, и обе здесь закрываются тестом:
//  1. заявка узла записывалась в СТАТИЧЕСКОЕ поле MaxConcurrentReqs, а признак
//     «значение от ноды» не выставлялся → при загрузке состояния статика из
//     конфига (2) затирала заявку;
//  2. даже с выставленным признаком резолвер возвращал staticMax (значение из
//     конфига), а не заявку узла.
//
// Поэтому у заявки узла появляется СОБСТВЕННОЕ поле NodeClaimedCapacity: оно
// однозначно (не путается ни со статикой конфига, ни с «эхом» агента) и
// переживает перезапуск балансера.
package types

import "testing"

// nodeClaimBackend — бэкенд с заявкой узла и (опционально) троттлингом оператора.
func nodeClaimBackend(nodeClaim, runtimeMax, staticMax, modelSlots int, fromNode bool) *Backend {
	return &Backend{
		ID:                           "b",
		Type:                         BackendTypeLlamaCpp,
		MaxConcurrentReqs:            staticMax,
		RuntimeMaxConcurrentRequests: runtimeMax,
		RuntimeModelSlots:            modelSlots,
		RuntimeCapacityFromNode:      fromNode,
		NodeClaimedCapacity:          nodeClaim,
	}
}

// TestR121_NodeClaim_WinsOverConfigStatic — после перезапуска балансера конфиг
// даёт статику 2, а узел заявил 1: победить обязана заявка узла.
func TestR121_NodeClaim_WinsOverConfigStatic(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := nodeClaimBackend(1, 0, 2, 0, true)

	got, source := b.ResolveCapacity()
	if got != 1 {
		t.Errorf("ResolveCapacity() = %d, want 1: статика конфига (2) затирает заявку узла — "+
			"это и есть живой дефект «после рестарта балансера вместимость откатилась»", got)
	}
	if source != CapacitySourceNode {
		t.Errorf("source = %q, want %q: значение пришло от узла, и отчёт обязан это показывать "+
			"(сейчас на стенде врёт «static»)", source, CapacitySourceNode)
	}
}

// TestR121_NodeClaim_OperatorThrottleLowers — троттлинг оператора НИЖЕ заявки
// узла уважается.
//
// ВАЖНО про комбинацию полей: операторский троттлинг выставляется через
// PUT /limits, а этот путь СНИМАЕТ RuntimeCapacityFromNode
// (internal/balancer/backend_registry.go: UpdateBackendLimits). Поэтому
// «от ноды + троттлинг одновременно» — состояние, которого на стенде не бывает;
// тест проверяет реальную комбинацию: владелец — оператор.
func TestR121_NodeClaim_OperatorThrottleLowers(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := nodeClaimBackend(2, 1, 4, 0, false)

	got, source := b.ResolveCapacity()
	if got != 1 {
		t.Errorf("ResolveCapacity() = %d, want 1 (оператор ограничил до 1)", got)
	}
	if source != CapacitySourceOperator {
		t.Errorf("source = %q, want %q", source, CapacitySourceOperator)
	}
}

// TestR121_NodeClaim_OperatorCannotRaiseAboveNode — троттлинг оператора ВЫШЕ
// заявки узла не открывает ложные слоты: узел физически обслуживает один запрос.
func TestR121_NodeClaim_OperatorCannotRaiseAboveNode(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := nodeClaimBackend(1, 4, 4, 0, false)

	got, _ := b.ResolveCapacity()
	if got != 1 {
		t.Errorf("ResolveCapacity() = %d, want 1: заявка узла — это ФАКТИЧЕСКАЯ способность, "+
			"и лимит оператора её не поднимает", got)
	}
}

// TestR121_NodeClaim_NoClaimKeepsLegacyBehaviour — без заявки узла правила
// прежние (регресс-защита для записей старых сборок).
func TestR121_NodeClaim_NoClaimKeepsLegacyBehaviour(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := nodeClaimBackend(0, 0, 2, 0, false)

	got, source := b.ResolveCapacity()
	if got != 2 || source != CapacitySourceStatic {
		t.Errorf("ResolveCapacity() = (%d, %q), want (2, %q): легаси-запись без заявки узла "+
			"обязана считаться как раньше", got, source, CapacitySourceStatic)
	}
}
