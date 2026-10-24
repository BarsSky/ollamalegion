//go:build llama_stub

// memfit_units_r219_test.go — R219 (2026-10-21).
//
// Живой дефект: в отказе загрузки (Qwen3.8, ctx 262144, kv f16) писалось
// «KV ... at 65536 MB/token» — kv_per_token приходит в БАЙТАХ (64 KB/токен), а
// подпись врала на три порядка. Оператор, считающий бюджет по этой строке,
// получает абсурдные числа. Тест держит правильные единицы и сами цифры отказа.
package main

import (
	"strings"
	"testing"
)

func TestR219_MemfitBreakdownUnitsAndNumbers(t *testing.T) {
	m := memoryBreakdown{
		WeightsMB:    15701,
		GPUWeightsMB: 2415,
		CPUWeightsMB: 13286,
		GPUKVMB:      2520,
		CPUKVMB:      13863,
		KVPerToken:   65536, // БАЙТ на токен (64 KB)
		UsableVRAMMB: 5049,
		UsableRAMMB:  18681,
		GPULayers:    10,
		TotalLayers:  65,
		Stage:        "does_not_fit",
		VRAMFreeMB:   7097,
		VRAMTotalMB:  8191,
		RAMAvailMB:   22777,
		RAMTotalMB:   25044,
		RAMSource:    "meminfo:MemAvailable",
	}

	text := m.Text()

	if strings.Contains(text, "MB/token") {
		t.Fatalf("единица kv_per_token снова врёт: %q", text)
	}
	if !strings.Contains(text, "65536 bytes/token") {
		t.Errorf("нет байтов на токен в строке: %q", text)
	}
	if !strings.Contains(text, "64 KB/token") {
		t.Errorf("нет KB на токен (человекочитаемая единица): %q", text)
	}
	if !strings.Contains(text, "KV 16383 MB") {
		t.Errorf("неверный суммарный KV (16383 MB): %q", text)
	}
	if !strings.Contains(text, "stage does_not_fit") {
		t.Errorf("не назван stage отказа: %q", text)
	}
}
