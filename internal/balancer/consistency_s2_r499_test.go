//go:build llama_stub

// consistency_s2_r499_test.go — R499 (2026-11-19), шаг S2 аудита
// docs/plans/2026-11-19-consistency-audit.md: «либо потребитель, либо удаление».
//
// Каждая проверка закрывает конкретный пункт §1.3 аудита и обязана краснеть от
// возврата прежнего поведения:
//
//	S-2  пороги GPU/CPU/RAM были зашиты (90/95/98), а настройки ресурсов
//	     использовались только как выключатель — оператор менял число и не видел
//	     эффекта;
//	S-3  у LB_GPU_MAX_TEMP потребителя не было ВООБЩЕ;
//	S-4  balancing.scoring.modelLoadingCost не читался (штраф 5/10/20 зашит);
//	S-1  тюнер весов удалён — сторож не даёт вернуть его молча.
package balancer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// proxyWithLimitsR499 — прокси с метриками бэкенда и заданными лимитами.
func proxyWithLimitsR499(t *testing.T, gpu types.GPUMetrics, sys types.SystemMetrics, res types.ResourceLimits) *Proxy {
	t.Helper()
	p := &Proxy{
		metricsMgr: NewMetricsManager(),
		config: &types.LoadBalancerConfig{
			Resources: res,
		},
		backends: map[string]*BackendState{},
	}
	p.metricsMgr.metrics["b1"] = &types.BackendMetrics{GPU: gpu, System: sys}
	return p
}

// TestR499_ResourceLimitsUseConfiguredThreshold — S-2: порог берётся из конфига.
func TestR499_ResourceLimitsUseConfiguredThreshold(t *testing.T) {
	t.Setenv("LB_RESOURCE_LIMITS_HARD_BLOCK", "1") // жёсткий режим, чтобы видеть решение

	// GPU занят на 60 %. При пороге 50 — отказ, при пороге 90 — допуск.
	busy := proxyWithLimitsR499(t,
		types.GPUMetrics{UsagePercent: 60},
		types.SystemMetrics{},
		types.ResourceLimits{GPU: types.GPULimits{MaxUsagePercent: 50}})
	if busy.checkResourceLimits("b1") {
		t.Error("порог GPU=50, занятость 60 — бэкенд обязан быть отклонён: " +
			"настройка ресурсов не читается (аудит S-2)")
	}

	lenient := proxyWithLimitsR499(t,
		types.GPUMetrics{UsagePercent: 60},
		types.SystemMetrics{},
		types.ResourceLimits{GPU: types.GPULimits{MaxUsagePercent: 90}})
	if !lenient.checkResourceLimits("b1") {
		t.Error("порог GPU=90, занятость 60 — бэкенд обязан быть допущен")
	}

	// CPU: настройка 40 против живых 50.
	cpu := proxyWithLimitsR499(t,
		types.GPUMetrics{},
		types.SystemMetrics{CPUUsagePercent: 50},
		types.ResourceLimits{CPU: types.CPULimits{MaxUsagePercent: 40}})
	if cpu.checkResourceLimits("b1") {
		t.Error("порог CPU=40, занятость 50 — бэкенд обязан быть отклонён")
	}

	// RAM: жёсткий отказ по памяти — тоже по настройке, а не по зашитым 98.
	ram := proxyWithLimitsR499(t,
		types.GPUMetrics{},
		types.SystemMetrics{MemoryTotal: 1000, MemoryUsed: 900},
		types.ResourceLimits{Memory: types.MemoryLimits{MaxUsagePercent: 80}})
	if ram.checkResourceLimits("b1") {
		t.Error("порог RAM=80 %, занятость 90 % — бэкенд обязан быть отклонён: " +
			"зашитые 98 не давали настройке влиять (аудит S-2)")
	}
}

// TestR499_GPUTemperatureHasConsumer — S-3: у LB_GPU_MAX_TEMP появился потребитель.
func TestR499_GPUTemperatureHasConsumer(t *testing.T) {
	t.Setenv("LB_RESOURCE_LIMITS_HARD_BLOCK", "1")

	hot := proxyWithLimitsR499(t,
		types.GPUMetrics{Temperature: 90},
		types.SystemMetrics{},
		types.ResourceLimits{GPU: types.GPULimits{MaxUsagePercent: 100, MaxTemperature: 85}})
	if hot.checkResourceLimits("b1") {
		t.Error("карта 90 °C при пределе 85 — бэкенд обязан быть отклонён: " +
			"до S-3 настройка температуры не читалась никем (аудит S-3)")
	}

	// Без явного предела температура не ограничивает: карта бывает горячей по
	// конструкции, выдумывать ей предел нельзя.
	noLimit := proxyWithLimitsR499(t,
		types.GPUMetrics{Temperature: 95},
		types.SystemMetrics{},
		types.ResourceLimits{GPU: types.GPULimits{MaxUsagePercent: 100}})
	if !noLimit.checkResourceLimits("b1") {
		t.Error("без MaxTemperature карта при 95 °C отклонена — предела по умолчанию быть не должно")
	}
}

// TestR499_ModelLoadingCostIsUsed — S-4: штраф за загрузку берётся из конфига.
func TestR499_ModelLoadingCostIsUsed(t *testing.T) {
	mk := func(cost float64, vramUsedPercent uint64) float64 {
		p := &Proxy{
			metricsMgr: NewMetricsManager(),
			config: &types.LoadBalancerConfig{
				Balancing: types.BalancingSettings{Scoring: types.ScoringWeights{ModelLoadingCost: cost}},
			},
		}
		p.metricsMgr.metrics["b1"] = &types.BackendMetrics{
			GPU: types.GPUMetrics{MemoryTotal: 100, MemoryUsed: vramUsedPercent},
		}
		return p.calculateModelLoadPenalty("b1", "не-загруженная-модель")
	}

	// Базовая семантика не изменилась: 5/10/20 при 30/65/85 % VRAM.
	if got := mk(0, 30); got != 5 {
		t.Errorf("без настройки при 30 %% VRAM штраф = %v, ожидалось 5", got)
	}
	if got := mk(0, 65); got != 10 {
		t.Errorf("без настройки при 65 %% VRAM штраф = %v, ожидалось 10", got)
	}
	if got := mk(0, 85); got != 20 {
		t.Errorf("без настройки при 85 %% VRAM штраф = %v, ожидалось 20", got)
	}

	// Настройка масштабирует: cost=10 → 10/20/40.
	if got := mk(10, 30); got != 10 {
		t.Errorf("cost=10 при 30 %% → %v, ожидалось 10 (настройка не читается, аудит S-4)", got)
	}
	if got := mk(10, 65); got != 20 {
		t.Errorf("cost=10 при 65 %% → %v, ожидалось 20 (×2)", got)
	}
	if got := mk(10, 85); got != 40 {
		t.Errorf("cost=10 при 85 %% → %v, ожидалось 40 (×4)", got)
	}
}

// TestR499_WeightTunerIsGone — S-1: удалённый тюнер весов не вернулся молча.
func TestR499_WeightTunerIsGone(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("корень: %v", err)
	}
	for _, rel := range []string{filepath.Join("internal", "balancer", "weight_tuner.go"),
		filepath.Join("internal", "balancer", "weight_tuner_test.go")} {
		if _, err := os.Stat(filepath.Join(root, rel)); err == nil {
			t.Errorf("%s снова существует: тюнер весов удалён по решению владельца "+
				"(аудит S-1) — его веса не читались скорингом, а RecordOutcome недостижим "+
				"на llama.cpp-пути. Если он нужен — сначала потребитель, потом код", rel)
		}
	}
	// И символа не должно быть в боевом коде.
	files, err := filepath.Glob(filepath.Join(root, "internal", "*", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "AdaptiveWeightTuner") {
			t.Errorf("%s ссылается на AdaptiveWeightTuner — тюнер удалён (аудит S-1)",
				filepath.Base(path))
		}
	}
}
