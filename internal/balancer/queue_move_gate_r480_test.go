// queue_move_gate_r480_test.go — R480 (P1-7): порог гейта очереди — окно
// ТЕКУЩЕЙ КОПИИ сессии, а не потребность запроса.
//
// Первый вариант сравнивал кандидата с потребностью запроса. Если она больше
// ВСЕХ окон в кластере (большой промпт на маленьких копиях), гейт отклонял
// всех, и запрос висел в очереди до 900 с вместо того, чтобы уйти на свою
// копию и дать preflight поднять окно.
//
// Тесты проверяют сам предикат (детерминированно, без «занятия слотов»:
// прямая подмена ActiveReqs ловится -race, а EffectiveMaxConcurrentRequests
// может брать ёмкость из метрик модели) и то, что очередь без известного окна
// ведёт себя по-прежнему.
package balancer

import (
	"context"
	"testing"
	"time"

	"ollama-loadbalancer/pkg/types"
)

// TestR480_QueueMoveGateUsesSessionWindow — гейт очереди строится по окну своей
// копии: узкая копия отклоняется, равная/широкая — допускается, своя — всегда.
func TestR480_QueueMoveGateUsesSessionWindow(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r480SelectorProxy(t) // b1: 65536, b2: 32768

	gate, need := p.sessionQueueGateR480("b1", r459Model)
	if gate == nil || need != 65536 {
		t.Fatalf("гейт от b1: gate=%v need=%d, ожидались непустой и 65536", gate != nil, need)
	}
	if !gate("b1") {
		t.Fatal("своя копия обязана быть допустима")
	}
	if gate("b2") {
		t.Fatal("b2 (32768) уже b1 (65536) — переезд обязан быть отклонён")
	}

	gate2, need2 := p.sessionQueueGateR480("b2", r459Model)
	if gate2 == nil || need2 != 32768 {
		t.Fatalf("гейт от b2: gate=%v need=%d, ожидались непустой и 32768", gate2 != nil, need2)
	}
	if !gate2("b1") {
		t.Fatal("b1 (65536) не хуже b2 (32768) — переезд разрешён")
	}
}

// TestR480_QueueMoveGateColdCandidateRejected — свежая метрика без модели:
// «холодный» кандидат очередью не берётся.
func TestR480_QueueMoveGateColdCandidateRejected(t *testing.T) {
	t.Setenv("LB_R459_COLD_TTL_SEC", "90")
	p := r480SelectorProxy(t)
	// b3 — бэкенд с известной пустой метрикой (свежий опрос, модели нет).
	p.metricsMgr.UpdateLlamaCppMetrics("b3", &types.LlamaCppMetrics{})
	p.metricsMgr.MarkLlamaCppPolled("b3", time.Now())

	gate, _ := p.sessionQueueGateR480("b1", r459Model)
	if gate == nil {
		t.Fatal("гейт не построен")
	}
	if gate("b3") {
		t.Fatal("холодный кандидат (модели нет по свежей метрике) обязан быть отклонён")
	}
}

// TestR480_QueueMoveGateUnknownWindowNoGate — окно своей копии неизвестно:
// сравнивать не с чем, гейт не ставится (поведение прежнее).
func TestR480_QueueMoveGateUnknownWindowNoGate(t *testing.T) {
	p := r480SelectorProxy(t)
	if gate, need := p.sessionQueueGateR480("b9", r459Model); gate != nil || need != 0 {
		t.Fatalf("для неизвестного окна ожидался nil-гейт, получено gate=%v need=%d", gate != nil, need)
	}
}

// TestR480_QueueMoveWithoutKnownWindowServes — очередь с неизвестным окном
// обслуживает запрос как раньше (не блокируется).
func TestR480_QueueMoveWithoutKnownWindowServes(t *testing.T) {
	p := r480SelectorProxy(t)
	p.setAdmissionWait(2 * time.Second)
	id, release, _, _, err := p.waitForInferenceBackendWithMoveR480(
		context.Background(), r459Model, types.BackendTypeLlamaCpp, "s1", "b9")
	if err != nil {
		t.Fatalf("без известного окна очередь обязана обслужить запрос, получено %v", err)
	}
	if id == "" {
		t.Fatal("пустой бэкенд при неизвестном окне")
	}
	if release != nil {
		release()
	}
}
