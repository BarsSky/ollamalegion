//go:build llama_stub

// capacity_single_source_r501_test.go — R501 (2026-11-19), аудит S3 (§1.1).
//
// ПРОВЕРЯЕТСЯ три вещи, каждая — конкретный пункт аудита:
//
//	C-1/C-6  операторский лимит выше заявки узла, и перерегистрация узла его не
//	         перетирает (заявка узла считается ОДИН раз при старте агента);
//	C-3      ноль и `-1` трактуются ОДНОЗНАЧНО (не задано / без лимита), а не
//	         пятью способами;
//	флаг     LB_CAPACITY_SINGLE_SOURCE: off — прежнее поведение, shadow —
//	         считать и не менять решения (дефолт), on — новые решения.
package types

import "testing"

// TestR501_CapacityFromValue — одна трактовка чисел (C-3).
func TestR501_CapacityFromValue(t *testing.T) {
	// Порядок полей — под fieldalignment (govet в CI): сначала числа, затем
	// булевы, иначе структура занимает 32 байта вместо 24 и линтер краснеет.
	cases := []struct {
		n         int
		wantValue int
		wantKnown bool
		wantUnlim bool
	}{
		{4, 4, true, false},
		{1, 1, true, false},
		{0, 0, false, false}, // «не задано», а не «ноль слотов» и не «10»
		{-1, 0, true, true},  // «без лимита»
		{-7, 0, true, true},  // любое отрицательное — тот же смысл
	}
	for _, c := range cases {
		got := CapacityFromValueR501(c.n)
		if got.Known != c.wantKnown || got.Unlimited != c.wantUnlim || got.Value != c.wantValue {
			t.Errorf("CapacityFromValueR501(%d) = %+v, ожидалось known=%v value=%d unlimited=%v",
				c.n, got, c.wantKnown, c.wantValue, c.wantUnlim)
		}
	}
}

// TestR501_OperatorBeatsNodeClaim — главное правило S3 (C-1).
func TestR501_OperatorBeatsNodeClaim(t *testing.T) {
	// Живой случай §4.5 аудита: узел заявил 4, оператор поставил 1.
	if got := ResolveEffectiveCapacityV2R501(0, 1, 4, 4); got != 1 {
		t.Errorf("оператор=1, заявка узла=4 → %d, ожидалось 1: "+
			"заявка узла не может перебивать решение оператора (аудит C-1)", got)
	}
	// Оператор не задавал — работает заявка узла.
	if got := ResolveEffectiveCapacityV2R501(0, 0, 4, 10); got != 4 {
		t.Errorf("без оператора, заявка узла=4 → %d, ожидалось 4", got)
	}
	// Ни оператора, ни заявки — статика.
	if got := ResolveEffectiveCapacityV2R501(0, 0, 0, 10); got != 10 {
		t.Errorf("только статика=10 → %d, ожидалось 10", got)
	}
	// Фактические слоты модели выше всего: оператор может понизить, но не превысить.
	if got := ResolveEffectiveCapacityV2R501(2, 8, 8, 8); got != 2 {
		t.Errorf("слоты модели=2, оператор=8 → %d, ожидалось 2 (R83: физику не превышаем)", got)
	}
	if got := ResolveEffectiveCapacityV2R501(8, 2, 8, 8); got != 2 {
		t.Errorf("слоты модели=8, оператор=2 → %d, ожидалось 2 (оператор понижает)", got)
	}
	// «Без лимита» у оператора: узел всё равно физическая граница.
	if got := ResolveEffectiveCapacityV2R501(0, -1, 3, 10); got != 3 {
		t.Errorf("оператор=-1 (без лимита), заявка узла=3 → %d, ожидалось 3", got)
	}
}

// TestR501_ModeDefaultShadow — дефолт: диагноз считается, решения прежние.
func TestR501_ModeDefaultShadow(t *testing.T) {
	cases := []struct {
		env  string
		want CapacityModeR501
	}{
		{"", CapacityModeShadowR501},
		{"off", CapacityModeOffR501},
		{"shadow", CapacityModeShadowR501},
		{"on", CapacityModeOnR501},
		{"banana", CapacityModeShadowR501},
	}
	for _, c := range cases {
		ResetCapacitySingleSourceModeR501R501()
		t.Setenv(capacityModeEnvR501, c.env)
		if got := CapacitySingleSourceModeR501(); got != c.want {
			t.Errorf("LB_CAPACITY_SINGLE_SOURCE=%q → %v, ожидалось %v", c.env, got, c.want)
		}
	}
	ResetCapacitySingleSourceModeR501R501()
}

// TestR501_ResolveCapacityModes — флаг управляет решениями, а не только логом.
func TestR501_ResolveCapacityModes(t *testing.T) {
	mk := func() *Backend {
		return &Backend{
			ID: "b1", Type: BackendTypeLlamaCpp,
			MaxConcurrentReqs:            4,
			RuntimeMaxConcurrentRequests: 1, // оператор поставил 1
			NodeClaimedCapacity:          4, // узел заявил 4
			RuntimeCapacityFromNode:      true,
		}
	}

	// off: прежнее поведение — заявка узла (4).
	ResetCapacitySingleSourceModeR501R501()
	t.Setenv(capacityModeEnvR501, "off")
	if eff, _ := mk().ResolveCapacity(); eff != 4 {
		t.Errorf("off: effective=%d, ожидалось 4 (прежнее поведение)", eff)
	}

	// shadow: решения прежние.
	ResetCapacitySingleSourceModeR501R501()
	t.Setenv(capacityModeEnvR501, "shadow")
	if eff, _ := mk().ResolveCapacity(); eff != 4 {
		t.Errorf("shadow: effective=%d, ожидалось 4 (решения не меняются)", eff)
	}

	// on: оператор (1) выше заявки узла (4).
	ResetCapacitySingleSourceModeR501R501()
	t.Setenv(capacityModeEnvR501, "on")
	b := mk()
	eff, src := b.ResolveCapacity()
	if eff != 1 {
		t.Errorf("on: effective=%d, ожидалось 1 (оператор выше узла, аудит C-1)", eff)
	}
	if src != CapacitySourceOperator {
		t.Errorf("on: источник=%q, ожидалось %q", src, CapacitySourceOperator)
	}

	// §4.5 аудита: PUT /limits=1 при MaxConcurrentReqs=4 → ВСЕ пути видят 1.
	if got := b.EffectiveMaxConcurrentRequests(); got != 1 {
		t.Errorf("EffectiveMaxConcurrentRequests() = %d, ожидалось 1: "+
			"именно этот геттер читают admission/слоты/выбор", got)
	}
	ResetCapacitySingleSourceModeR501R501()
}
