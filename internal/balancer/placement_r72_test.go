package balancer

// R72 (P0): placement policy — resolution и наблюдаемость.
// План: plans/2026-09-23-multi-backend-placement-policy.md
//
// Ключевое требование P0: поведение маршрутизации НЕ меняется. Поэтому
// проверяем чистое решение (ResolvePlacementDecision) и счётчики, а не прокси.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func placementCfgR72() types.PlacementSettings {
	return types.PlacementSettings{
		Enabled:              true,
		AllowRequestOverride: true,
		Fallback:             types.PlacementFallbackError,
		Models: []types.PlacementModelRule{
			{Model: "gemma-4-E4B-it-Q4_K_M", Strategy: "single", Reason: "3.3B Q4 влезает в 8 ГБ VRAM"},
			{Model: "Qwen3*", Strategy: "auto", Reason: "крупная модель — решает авто-правило"},
			{Model: "virtual:prod-chat", Strategy: "pool", Pool: []string{"a:18092"}},
		},
		Classes: []types.PlacementClassRule{
			{Match: types.PlacementMatch{SizeGB: ">12"}, Strategy: "sharded", Reason: "не влезает в один бэкенд"},
			{Match: types.PlacementMatch{SizeGB: "<4"}, Strategy: "single", Reason: "мелкая модель"},
		},
	}
}

func TestResolvePlacement_Priority_R72(t *testing.T) {
	cfg := placementCfgR72()

	cases := []struct {
		name     string
		model    string
		override string
		strategy types.PlacementStrategy
		source   PlacementSource
		sizeGB   float64
	}{
		{
			name: "запрос > модель > класс > дефолт (override выигрывает)",
			// Модель описана правилом single и по размеру попадает в класс <4,
			// но X-LB-Placement=replicated важнее.
			model: "gemma-4-E4B-it-Q4_K_M", sizeGB: 3.3, override: "replicated",
			strategy: types.PlacementReplicated, source: PlacementSourceRequest,
		},
		{
			name:  "модель > класс (точное правило важнее класса по размеру)",
			model: "gemma-4-E4B-it-Q4_K_M", sizeGB: 13,
			strategy: types.PlacementSingle, source: PlacementSourceModel,
		},
		{
			name:  "маска модели",
			model: "Qwen3.8-27B-UD-Q4_K_M", sizeGB: 16,
			strategy: types.PlacementAuto, source: PlacementSourceModel,
		},
		{
			name:  "класс по размеру",
			model: "unknown-13b", sizeGB: 13,
			strategy: types.PlacementSharded, source: PlacementSourceClass,
		},
		{
			name:  "класс мелких моделей",
			model: "unknown-tiny", sizeGB: 3,
			strategy: types.PlacementSingle, source: PlacementSourceClass,
		},
		{
			name:  "не описана и размер неизвестен → глобальный дефолт",
			model: "unlisted-model", sizeGB: 0,
			strategy: types.PlacementSingle, source: PlacementSourceGlobal,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := ResolvePlacementDecision(cfg, "standard", tc.model, tc.sizeGB, tc.override)
			if d.Strategy != tc.strategy {
				t.Errorf("strategy = %q, ожидалось %q (reason=%q)", d.Strategy, tc.strategy, d.Reason)
			}
			if d.Source != tc.source {
				t.Errorf("source = %q, ожидалось %q", d.Source, tc.source)
			}
			if d.Reason == "" {
				t.Error("reason не должен быть пустым — он идёт в метрики и логи")
			}
		})
	}
}

func TestResolvePlacement_DisabledKeepsOperatingMode_R72(t *testing.T) {
	// Обратная совместимость: placement.enabled=false → стратегия из
	// operatingMode, правила игнорируются.
	for _, tc := range []struct {
		mode     string
		strategy types.PlacementStrategy
	}{
		{"standard", types.PlacementSingle},
		{"virtual_router", types.PlacementPool},
		{"replication", types.PlacementReplicated},
		{"rpc_coordinator", types.PlacementRPC},
		{"distributed_inference", types.PlacementSharded},
	} {
		cfg := placementCfgR72()
		cfg.Enabled = false
		d := ResolvePlacementDecision(cfg, tc.mode, "Qwen3.8-27B", 16, "")
		if d.Strategy != tc.strategy {
			t.Errorf("mode=%s: strategy = %q, ожидалось %q", tc.mode, d.Strategy, tc.strategy)
		}
		if d.Source != PlacementSourceDisabled {
			t.Errorf("mode=%s: source = %q, ожидалось %q", tc.mode, d.Source, PlacementSourceDisabled)
		}
		if d.RuleIndex != -1 {
			t.Errorf("mode=%s: ruleIndex = %d, ожидалось -1 (правила не применялись)", tc.mode, d.RuleIndex)
		}
	}
}

func TestResolvePlacement_OverrideGated_R72(t *testing.T) {
	cfg := placementCfgR72()
	cfg.AllowRequestOverride = false

	d := ResolvePlacementDecision(cfg, "standard", "gemma-4-E4B-it-Q4_K_M", 3.3, "replicated")
	if d.Source == PlacementSourceRequest || d.Strategy == types.PlacementReplicated {
		t.Errorf("override должен игнорироваться при allowRequestOverride=false: %+v", d)
	}

	// Неизвестная стратегия в заголовке тоже игнорируется (не ломаем запрос).
	cfg.AllowRequestOverride = true
	d = ResolvePlacementDecision(cfg, "standard", "gemma-4-E4B-it-Q4_K_M", 3.3, "spread")
	if d.Source == PlacementSourceRequest {
		t.Errorf("неизвестная стратегия в override не должна применяться: %+v", d)
	}
}

func TestResolvePlacement_ExecutableFlag_R72(t *testing.T) {
	cfg := placementCfgR72()

	// sharded резолвится (правило сработало), но помечен как неисполнимый:
	// запрос пойдёт по fallback, а оператор увидит причину в метриках.
	d := ResolvePlacementDecision(cfg, "standard", "unknown-13b", 13, "")
	if d.Strategy != types.PlacementSharded {
		t.Fatalf("strategy = %q, ожидалось sharded", d.Strategy)
	}
	if d.Executable {
		t.Error("sharded пока не исполняется (P2) — Executable должен быть false")
	}
	if d.Fallback != types.PlacementFallbackError {
		t.Errorf("fallback = %q, ожидалось error (default)", d.Fallback)
	}
}

func TestMatchModelMask_R72(t *testing.T) {
	cases := []struct {
		pattern string
		model   string
		want    bool
	}{
		{"gemma-4*", "gemma-4-E4B-it-Q4_K_M", true},
		{"GEMMA-4*", "gemma-4-E4B-it-Q4_K_M", true}, // регистр не важен
		{"*Q4_K_M", "gemma-4-E4B-it-Q4_K_M", true},
		{"gemma-4-E4B-it-Q4_K_M", "gemma-4-E4B-it-Q4_K_M", true},
		{"gemma-4-E4B-it-Q4_K_M", "gemma-4-E4B-it-Q4_K_M.gguf", true}, // расширение
		{"qwen?", "qwen3", true},
		{"qwen?", "qwen33", false},
		{"hf.co/Qwen*", "hf.co/Qwen3.8-27B", true}, // `/` не спецсимвол
		{"Qwen3*", "gemma-4", false},
		{"", "gemma-4", false},
		{"   ", "gemma-4", false},
	}
	for _, tc := range cases {
		if got := matchModelMask(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchModelMask(%q, %q) = %v, ожидалось %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func TestClassMatches_SizeUnknownDoesNotMatch_R72(t *testing.T) {
	// Размер неизвестен → правила класса по размеру не совпадают (не угадываем).
	if classMatches(types.PlacementMatch{SizeGB: ">12"}, "big-model", 0) {
		t.Error("правило класса по размеру не должно совпадать при неизвестном размере")
	}
	if classMatches(types.PlacementMatch{}, "any", 5) {
		t.Error("пустое правило класса не должно совпадать ни с чем")
	}
	if !classMatches(types.PlacementMatch{Model: "big*", SizeGB: ">12"}, "big-model", 13) {
		t.Error("оба условия выполнены — правило должно совпасть")
	}
	if classMatches(types.PlacementMatch{Model: "big*", SizeGB: ">12"}, "big-model", 5) {
		t.Error("условие по размеру не выполнено — правило не должно совпадать")
	}
}

func TestProxyPlacementDecision_EndpointReport_R72(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.Placement = placementCfgR72()
	proxy := newProxyWithCleanup(t, cfg)

	if warnings := proxy.PlacementWarnings(); len(warnings) != 0 {
		t.Errorf("корректная политика дала предупреждения: %v", warnings)
	}

	d := proxy.ResolvePlacement("gemma-4-E4B-it-Q4_K_M", 3.3, "")
	if d.Source != PlacementSourceModel || d.Strategy != types.PlacementSingle {
		t.Fatalf("решение через Proxy не совпало с ожиданием: %+v", d)
	}
	proxy.recordPlacementDecision(d) // P0: заглушка, не должна паниковать

	// Смена политики на лету (reload конфига): SetPlacementSettings.
	proxy.SetPlacementSettings(types.PlacementSettings{Enabled: false})
	if got := proxy.ResolvePlacement("gemma-4-E4B-it-Q4_K_M", 3.3, ""); got.Source != PlacementSourceDisabled {
		t.Errorf("после SetPlacementSettings источник = %q, ожидалось %q", got.Source, PlacementSourceDisabled)
	}
	proxy.SetPlacementSettings(placementCfgR72())

	// Отчёт для /api/v1/placement: первая строка — глобальный дефолт "*".
	decisions := proxy.PlacementDecisions()
	if len(decisions) != 1+len(placementCfgR72().Models) {
		t.Fatalf("decisions = %d, ожидалось %d", len(decisions), 1+len(placementCfgR72().Models))
	}
	if decisions[0].Model != "*" {
		t.Errorf("первым должен идти глобальный дефолт, получено %q", decisions[0].Model)
	}
	if decisions[0].Source != PlacementSourceGlobal {
		t.Errorf("источник дефолта = %q, ожидалось %q", decisions[0].Source, PlacementSourceGlobal)
	}
	// Модели отсортированы (стабильный вывод для UI/тестов).
	for i := 2; i < len(decisions); i++ {
		if decisions[i-1].Model > decisions[i].Model {
			t.Errorf("decisions не отсортированы: %q > %q", decisions[i-1].Model, decisions[i].Model)
		}
	}
}

func TestProxyPlacementWarnings_InvalidConfig_R72(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled: true,
		Models:  []types.PlacementModelRule{{Model: "a", Strategy: "spread"}},
	}
	proxy := newProxyWithCleanup(t, cfg)

	warnings := proxy.PlacementWarnings()
	if len(warnings) == 0 {
		t.Fatal("ожидалось предупреждение о неизвестной стратегии")
	}
	if warnings[0] == "" {
		t.Error("предупреждение не должно быть пустым")
	}
}
