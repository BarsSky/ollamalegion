package types

// R72 (P0): placement policy — типы, валидация, разбор выражений размера.
// План: plans/2026-09-23-multi-backend-placement-policy.md

import (
	"strings"
	"testing"
)

func TestPlacementStrategyForOperatingMode_R72(t *testing.T) {
	cases := map[string]PlacementStrategy{
		"":                      PlacementSingle,
		"standard":              PlacementSingle,
		"virtual_router":        PlacementPool,
		"replication":           PlacementReplicated,
		"rpc_coordinator":       PlacementRPC,
		"distributed_inference": PlacementSharded,
		"unknown-mode":          PlacementSingle,
	}
	for mode, want := range cases {
		if got := PlacementStrategyForOperatingMode(mode); got != want {
			t.Errorf("PlacementStrategyForOperatingMode(%q) = %q, ожидалось %q", mode, got, want)
		}
	}
}

func TestIsValidPlacementStrategy_R72(t *testing.T) {
	for _, valid := range PlacementStrategies() {
		if !IsValidPlacementStrategy(string(valid)) {
			t.Errorf("стратегия %q должна быть валидной", valid)
		}
	}
	if !IsValidPlacementStrategy("  SINGLE ") {
		t.Error("стратегия должна нормализоваться (регистр/пробелы)")
	}
	if IsValidPlacementStrategy("spread") {
		t.Error("неизвестная стратегия не должна быть валидной")
	}
	if IsValidPlacementStrategy("") {
		t.Error("пустая стратегия не валидна (это «не задано»)")
	}
}

func TestIsExecutablePlacementStrategy_R72(t *testing.T) {
	executable := map[PlacementStrategy]bool{
		PlacementSingle:     true,
		PlacementPool:       true,
		PlacementReplicated: true,
		PlacementAuto:       true,
		PlacementSharded:    false, // этап P2 (нужен транспорт раскладки)
		PlacementRPC:        false, // этап P2
	}
	for st, want := range executable {
		if got := IsExecutablePlacementStrategy(st); got != want {
			t.Errorf("IsExecutablePlacementStrategy(%q) = %v, ожидалось %v", st, got, want)
		}
	}
}

func TestParseSizeGBExpression_R72(t *testing.T) {
	cases := []struct {
		expr  string
		size  float64
		match bool
	}{
		{">12", 13, true},
		{">12", 12, false},
		{">=8", 8, true},
		{">=8", 7.9, false},
		{"<4", 3.9, true},
		{"<4", 4, false},
		{"<=8", 8, true},
		{"<=8", 8.1, false},
		{"8", 8, true},
		{"8", 8.5, false},
		{"=8", 8, true},
		{"4-8", 4, true},
		{"4-8", 8, true},
		{"4-8", 8.1, false},
		{"4-8", 3.9, false},
		{"  > 12 ", 13, true},
	}
	for _, tc := range cases {
		matcher, err := ParseSizeGBExpression(tc.expr)
		if err != nil {
			t.Errorf("ParseSizeGBExpression(%q): неожиданная ошибка: %v", tc.expr, err)
			continue
		}
		if got := matcher(tc.size); got != tc.match {
			t.Errorf("ParseSizeGBExpression(%q)(%v) = %v, ожидалось %v", tc.expr, tc.size, got, tc.match)
		}
	}
}

func TestParseSizeGBExpression_Errors_R72(t *testing.T) {
	for _, expr := range []string{"", "abc", ">abc", "4-", "8-4", ">12<8"} {
		if _, err := ParseSizeGBExpression(expr); err == nil {
			t.Errorf("ParseSizeGBExpression(%q): ожидалась ошибка", expr)
		}
	}
}

func TestPlacementSettingsValidate_R72(t *testing.T) {
	valid := PlacementSettings{
		Enabled:  true,
		Fallback: PlacementFallbackSingle,
		Models: []PlacementModelRule{
			{Model: "gemma-4*", Strategy: "single", Reason: "влезает в 8 ГБ"},
			{Model: "virtual:prod", Strategy: "pool", Pool: []string{"a:18092"}, Selection: "least_loaded"},
			{Model: "Qwen3*", Strategy: "auto", Auto: &PlacementAutoRule{Prefer: []string{"replicated", "single"}, MinBackends: 2}},
		},
		Classes: []PlacementClassRule{
			{Match: PlacementMatch{SizeGB: ">12"}, Strategy: "auto"},
			{Match: PlacementMatch{Model: "tiny*", SizeGB: "<4"}, Strategy: "single"},
		},
	}
	if errs := valid.Validate(); len(errs) != 0 {
		t.Errorf("корректный конфиг дал ошибки: %v", errs)
	}

	broken := PlacementSettings{
		Enabled:  true,
		Fallback: "maybe",
		Models: []PlacementModelRule{
			{Model: "", Strategy: "single"},                     // пустое имя
			{Model: "a", Strategy: "spread"},                    // неизвестная стратегия
			{Model: "b", Strategy: "pool"},                      // pool без списка
			{Model: "c", Strategy: "sharded"},                   // не исполняется (P2)
			{Model: "d", Strategy: "single", Selection: "best"}, // неизвестный selection
			{Model: "e", Strategy: "auto", Auto: &PlacementAutoRule{Prefer: []string{"nope"}}},
		},
		Classes: []PlacementClassRule{
			{Match: PlacementMatch{}, Strategy: "single"},               // пустой match
			{Match: PlacementMatch{SizeGB: ">abc"}, Strategy: "single"}, // битое выражение
			{Match: PlacementMatch{Model: "x"}, Strategy: "nope"},       // неизвестная стратегия
		},
	}
	errs := broken.Validate()
	if len(errs) < 9 {
		t.Errorf("ожидалось много ошибок валидации, получено %d: %v", len(errs), errs)
	}
	for _, want := range []string{"model", "spread", "pool", "P2", "selection", "prefer", "match", "sizeGB"} {
		found := false
		for _, e := range errs {
			if containsFold(e, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("в ошибках нет упоминания %q: %v", want, errs)
		}
	}

	// enabled=true без правил — предупреждение (политика ни на что не влияет).
	empty := PlacementSettings{Enabled: true}
	if len(empty.Validate()) == 0 {
		t.Error("enabled=true без правил должен давать предупреждение")
	}

	// Выключенная политика без правил — ошибок нет (обратная совместимость).
	off := PlacementSettings{}
	if errs := off.Validate(); len(errs) != 0 {
		t.Errorf("пустая выключенная политика не должна давать ошибок: %v", errs)
	}
}

func TestValidatePlacementStrategyName_R72(t *testing.T) {
	if err := ValidatePlacementStrategyName(""); err != nil {
		t.Errorf("пустая стратегия (нет override) — не ошибка: %v", err)
	}
	if err := ValidatePlacementStrategyName("replicated"); err != nil {
		t.Errorf("валидная стратегия: %v", err)
	}
	if err := ValidatePlacementStrategyName("spread"); err == nil {
		t.Error("неизвестная стратегия должна давать ошибку")
	}
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}
