//go:build llama_stub

// capacity_zero_source_r501_test.go — R501 (2026-11-19), аудит S3: C-3 и C-7.
//
// C-3: одно число вместимости читалось пятью способами (0 = «без лимита» /
// «слотов нет» / «лимит 1» / «10 в UI»). Здесь держится единственная трактовка
// (CapacityFromValueR501) и то, что отрицательное значение — «без лимита», а не
// «слотов нет».
//
// C-7: отчёт врал об источнике — заявка узла подписывалась CapacitySourceOperator,
// хотя оператор такого значения не ставил.
package types

import "testing"

// TestR501_CapacityFromValueOneInterpretation — единственная трактовка числа (C-3).
func TestR501_CapacityFromValueOneInterpretation(t *testing.T) {
	cases := []struct {
		name      string
		in        int
		wantValue int
		wantKnown bool
		wantUnlim bool
	}{
		{name: "положительное — значение лимита", in: 3, wantValue: 3, wantKnown: true},
		{name: "ноль — не задано", in: 0},
		{name: "минус один — без лимита", in: -1, wantKnown: true, wantUnlim: true},
		{name: "минус пять — тоже без лимита", in: -5, wantKnown: true, wantUnlim: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CapacityFromValueR501(tc.in)
			if got.Value != tc.wantValue || got.Known != tc.wantKnown || got.Unlimited != tc.wantUnlim {
				t.Fatalf("CapacityFromValueR501(%d) = {value:%d known:%v unlimited:%v}, "+
					"ожидалось {value:%d known:%v unlimited:%v}",
					tc.in, got.Value, got.Known, got.Unlimited,
					tc.wantValue, tc.wantKnown, tc.wantUnlim)
			}
			if got.Unlimited && !got.Known {
				t.Fatal("«без лимита» обязано быть Known: это ОСОЗНАННОЕ значение, " +
					"а не отсутствие настройки")
			}
		})
	}
}

// TestR501_NodeClaimReportedAsNode — C-7: когда заявка узла перебивает
// операторский лимит (узел физически слабее), источник — «node», а не «operator».
func TestR501_NodeClaimReportedAsNode(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := &Backend{
		ID:                           "b",
		Type:                         BackendTypeLlamaCpp,
		MaxConcurrentReqs:            4,
		RuntimeMaxConcurrentRequests: 5, // оператор разрешил 5
		NodeClaimedCapacity:          3, // но узел обслуживает 3
	}

	got, source := b.ResolveCapacity()
	if got != 3 {
		t.Fatalf("вместимость = %d, ожидалось 3 (заявка узла)", got)
	}
	if source != CapacitySourceNode {
		t.Fatalf("источник = %q, ожидалось %q: значение пришло от узла, а отчёт "+
			"приписывал его оператору (аудит C-7)", source, CapacitySourceNode)
	}
}

// TestR501_OperatorBelowNodeClaimReportedAsOperator — обратная сторона C-7:
// когда лимит оператора НИЖЕ заявки узла, источник обязан быть «operator»
// (снижение сделал человек), и значение — его.
func TestR501_OperatorBelowNodeClaimReportedAsOperator(t *testing.T) {
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")
	b := &Backend{
		ID:                           "b",
		Type:                         BackendTypeLlamaCpp,
		MaxConcurrentReqs:            8,
		RuntimeMaxConcurrentRequests: 2,
		NodeClaimedCapacity:          6,
	}

	got, source := b.ResolveCapacity()
	if got != 2 {
		t.Fatalf("вместимость = %d, ожидалось 2 (лимит оператора)", got)
	}
	if source != CapacitySourceOperator {
		t.Fatalf("источник = %q, ожидалось %q", source, CapacitySourceOperator)
	}
}

// TestR501_OnModeRespectsSlotsFlag — включение LB_CAPACITY_SINGLE_SOURCE=on не
// включает вторую настройку: пока LB_CAPACITY_FROM_MODEL_SLOTS выключен, слоты
// модели в решении не участвуют (иначе один флаг молча менял бы два правила).
func TestR501_OnModeRespectsSlotsFlag(t *testing.T) {
	resetCapacityCachesR501()
	t.Cleanup(resetCapacityCachesR501)
	t.Setenv(capacityModeEnvR501, "on")
	t.Setenv("LB_CAPACITY_FROM_MODEL_SLOTS", "false")

	b := &Backend{
		ID:                "b",
		Type:              BackendTypeLlamaCpp,
		MaxConcurrentReqs: 2,
		RuntimeModelSlots: 4, // факт модели, но флаг слотов выключен
	}
	eff, src := b.ResolveCapacity()
	if eff != 2 {
		t.Errorf("вместимость = %d, ожидалось 2: при выключенном "+
			"LB_CAPACITY_FROM_MODEL_SLOTS слоты модели не учитываются", eff)
	}
	if src != CapacitySourceStatic {
		t.Errorf("источник = %q, ожидалось %q", src, CapacitySourceStatic)
	}
}
