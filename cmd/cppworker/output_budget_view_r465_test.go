// output_budget_view_r465_test.go — R465 (2026-11-06).
//
// ЖИВАЯ ЖАЛОБА ОПЕРАТОРА: «num_predict в env выставлен, но в WebUI его значения не
// видеть: поле в настройках бэкенда всегда 0… на страницах Загруженные среди
// списка выводимых параметров этого значения тоже нет».
//
// Тесты фиксируют, что политика бюджета вывода отдаётся API явно (число +
// источник), а не остаётся только в ENV/константах.
package main

import "testing"

// TestR465_ViewHasNumbersAndSource — раздел outputBudget содержит и числа, и
// откуда они взяты.
func TestR465_ViewHasNumbersAndSource(t *testing.T) {
	view := outputBudgetViewR465()

	if got := view["reasoningDefault"]; got != DefaultNPredictReasoning {
		t.Fatalf("reasoningDefault = %v, want %d", got, DefaultNPredictReasoning)
	}
	if got := view["ceiling"]; got != defaultMaxNPredictR463 {
		t.Fatalf("ceiling = %v, want %d", got, defaultMaxNPredictR463)
	}
	if got := view["upperBoundRaisesDefault"]; got != true {
		t.Fatalf("upperBoundRaisesDefault = %v, want true (R463)", got)
	}
	if view["reasoningDefaultSource"] != "default" {
		t.Fatalf("источник дефолта = %v, want default (env не задан)", view["reasoningDefaultSource"])
	}
	if view["note"] == "" {
		t.Fatal("нет пояснения — оператор не поймёт, откуда число")
	}
}

// TestR465_ViewReflectsEnv — значения из ENV видны и помечены источником.
func TestR465_ViewReflectsEnv(t *testing.T) {
	t.Setenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING", "12288")
	t.Setenv("CPPWORKER_MAX_N_PREDICT", "24576")

	view := outputBudgetViewR465()
	if got := view["reasoningDefault"].(int); got != DefaultNPredictReasoning {
		// Константа остаётся константой; ENV действует через ResolveNPredict и
		// показывается отдельными полями (число+источник), чтобы оператор видел
		// РАЗНИЦУ между «зашито» и «переопределено».
		t.Fatalf("reasoningDefault = %d", got)
	}
	if view["reasoningDefaultSource"] != "env" || view["reasoningDefaultEnv"] != "12288" {
		t.Fatalf("env-переопределение не показано: source=%v env=%v",
			view["reasoningDefaultSource"], view["reasoningDefaultEnv"])
	}
	if got := view["ceiling"].(int); got != 24576 {
		t.Fatalf("ceiling = %d, want 24576 из ENV", got)
	}
	if view["ceilingSource"] != "env" || view["ceilingEnv"] != "24576" {
		t.Fatalf("источник потолка не показан: %v/%v", view["ceilingSource"], view["ceilingEnv"])
	}
}

// TestR465_DefaultNumPredictPerModel — что получит запрос к этой модели без
// явного num_predict (для страницы «Загруженные»).
func TestR465_DefaultNumPredictPerModel(t *testing.T) {
	// reasoning-модель: дефолт политики, но не выше потолка.
	if got := defaultNumPredictForModelR465("Qwen3.8-9B-Distill"); got != DefaultNPredictReasoning {
		t.Fatalf("reasoning-модель: %d, want %d", got, DefaultNPredictReasoning)
	}
	// не-reasoning: ResolveNPredict возвращает 0 (мост подставит свой дефолт).
	if got := defaultNumPredictForModelR465("llama-3-8b"); got != 0 {
		t.Fatalf("не-reasoning: %d, want 0 (решает мост)", got)
	}
	t.Setenv("CPPWORKER_DEFAULT_N_PREDICT_REASONING", "32768")
	t.Setenv("CPPWORKER_MAX_N_PREDICT", "16384")
	if got := defaultNumPredictForModelR465("Qwen3.8-9B-Distill"); got != 16384 {
		t.Fatalf("env 32768 при потолке 16384 → %d, want 16384", got)
	}
}

// TestR465_ParsePositiveInt — мусор не превращается в число.
func TestR465_ParsePositiveInt(t *testing.T) {
	cases := map[string]int{"16384": 16384, "0": 0, "": 0, "-5": 0, "12x": 0, "999999999": 0}
	for raw, want := range cases {
		got, ok := parsePositiveIntR465(raw)
		if want == 0 {
			if ok {
				t.Errorf("%q: ожидался отказ, получено %d", raw, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q → (%d, %v), want (%d, true)", raw, got, ok, want)
		}
	}
}
