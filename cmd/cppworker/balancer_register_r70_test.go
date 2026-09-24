// balancer_register_r70_test.go — R70 (2026-09-24): cppworker сообщает балансеру
// РЕАЛЬНУЮ параллельную вместимость, а не константу.
//
// ПРОБЛЕМА: в регистрации стояло `MaxConcurrentReqs: 4`, поэтому балансер пускал
// до 4 одновременных inference-запросов туда, где cppworker обслуживает 1
// (slot manager: maxSlots=1 при n_parallel=0): остальные три блокировались внутри
// cppworker (SlotManager.Acquire), клиент ждал молча — без позиции в
// admission-очереди, без keepalive и без отражения в метриках балансера.
//
// ФИКС: effectiveNParallel() берёт currentConfig.DefaultNParallel (Round 12),
// безопасный минимум — 1.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// TestEffectiveNParallel_R70 — конфиг задаёт параллелизм, 0/отсутствие → 1.
func TestEffectiveNParallel_R70(t *testing.T) {
	orig := currentConfig
	defer func() { currentConfig = orig }()

	cases := []struct {
		name string
		cfg  *cppbackend.Config
		want int
	}{
		{"nil config → 1", nil, 1},
		{"default (0) → 1", &cppbackend.Config{DefaultNParallel: 0}, 1},
		{"negative → 1", &cppbackend.Config{DefaultNParallel: -3}, 1},
		{"explicit 4 → 4", &cppbackend.Config{DefaultNParallel: 4}, 4},
		{"explicit 1 → 1", &cppbackend.Config{DefaultNParallel: 1}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			currentConfig = tc.cfg
			if got := effectiveNParallel(); got != tc.want {
				t.Errorf("effectiveNParallel() = %d, ожидалось %d", got, tc.want)
			}
		})
	}
}
