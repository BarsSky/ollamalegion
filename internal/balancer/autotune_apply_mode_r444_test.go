package balancer

import "testing"

// TestR444_ApplyModeDefaultIsRecommend — по умолчанию AutoTune НЕ применяет план.
//
// Живой дефект: агентский цикл платил 43–67 с за unload+load перед каждым ходом
// (слои/KV «оптимизировались» по сиюминутной свободной VRAM).
func TestR444_ApplyModeDefaultIsRecommend(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_APPLY", "")
	if autoTuneApplyAutomaticR444() {
		t.Fatal("по умолчанию AutoTune применяет план автоматически — агентский цикл будет платить reload")
	}
	for _, v := range []string{"recommend", "0", "false", "off", "no", "RECOMMEND", "что-то"} {
		t.Setenv("LB_AUTOTUNE_APPLY", v)
		if autoTuneApplyAutomaticR444() {
			t.Errorf("%q: автоматическое применение включено", v)
		}
	}
	for _, v := range []string{"auto", "AUTO", " auto "} {
		t.Setenv("LB_AUTOTUNE_APPLY", v)
		if !autoTuneApplyAutomaticR444() {
			t.Errorf("%q: автоматическое применение выключено, хотя оператор его включил", v)
		}
	}
}

// TestR444_KVModeIndependentFromApplyMode — политики независимы: KV-гард R419
// остаётся в силе и при LB_AUTOTUNE_APPLY=auto.
func TestR444_KVModeIndependentFromApplyMode(t *testing.T) {
	t.Setenv("LB_AUTOTUNE_APPLY", "auto")
	t.Setenv("LB_AUTOTUNE_KV_MODE", "recommend")
	if !autoTuneApplyAutomaticR444() {
		t.Fatal("apply=auto должен разрешать применение")
	}
	if autoTuneKVAutomaticR419() {
		t.Fatal("KV-смена не должна становиться автоматической из-за apply=auto")
	}
}
