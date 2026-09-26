//go:build llama_stub

// auto_offload_memfit_r83_test.go — R83 §9.4 шаг 1 (2026-09-26).
//
// Замена оценки числа GPU-слоёв: решение принимает memfit, legacy-формула
// (веса ×0.7, KV «256 Б/токен») остаётся только fallback'ом на случай, когда
// memfit судить не о чем. Тест фиксирует ГРАНИЦУ между этими случаями:
// без backend'а/метаданных обязано вернуться прежнее поведение (а не выдуманные
// нули), иначе на стендах без каталога моделей загрузка деградировала бы до
// CPU-only.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// withConfig подменяет пакетные autoOffload/currentConfig на время теста.
func withConfig(t *testing.T, cfg *cppbackend.Config, autoOffloadVal bool) {
	t.Helper()
	prevCfg := currentConfig
	prevAuto := autoOffload
	currentConfig = cfg
	v := autoOffloadVal
	autoOffload = &v
	t.Cleanup(func() {
		currentConfig = prevCfg
		autoOffload = prevAuto
	})
}

// TestR83_AutoOffload_NoBackendFallsBackToLegacy — без глобального backend'а
// memfit не может собрать ModelSpec → обязан сработать legacy-путь.
func TestR83_AutoOffload_NoBackendFallsBackToLegacy(t *testing.T) {
	oldBackend := backend
	defer func() { backend = oldBackend }()
	backend = nil

	cfg := &cppbackend.Config{DefaultGPULayers: 30, DefaultCtxSize: 32768}
	withConfig(t, cfg, true)

	m := cppbackend.ModelInfo{Name: "no-meta-model"}
	got := calculateOptimalGPULayersForModel(m, "q8_0")
	if got != cfg.DefaultGPULayers {
		t.Errorf("без backend'а и без метаданных: %d, want DefaultGPULayers=%d "+
			"(legacy-fallback, а не выдуманный ноль)", got, cfg.DefaultGPULayers)
	}

	// Сам memfit-путь обязан честно сообщить «не могу».
	if _, ok := memfitGPULayersForModel(m, "q8_0"); ok {
		t.Error("memfitGPULayersForModel вернул ok=true без backend'а — " +
			"вызывающий решил бы, что решение принято")
	}
}

// TestR83_AutoOffload_DisabledReturnsDefault — CPPWORKER_AUTO_OFFLOAD=false
// сохраняет прежнее поведение: расчёта нет.
func TestR83_AutoOffload_DisabledReturnsDefault(t *testing.T) {
	cfg := &cppbackend.Config{DefaultGPULayers: 30, DefaultCtxSize: 32768}
	withConfig(t, cfg, false)

	got := calculateOptimalGPULayersForModel(cppbackend.ModelInfo{
		Name: "m", SizeBytes: 16 * 1024 * 1024 * 1024, NLayers: 64,
	}, "q8_0")
	if got != cfg.DefaultGPULayers {
		t.Errorf("auto_offload выключен: %d, want DefaultGPULayers=%d",
			got, cfg.DefaultGPULayers)
	}
}

// TestR83_AutoOffload_NoConfigReturnsAuto — без currentConfig судить не о чем:
// возвращаем -1 (=auto), чтобы решение принял checkVRAMForModel через memfit,
// а не упали в nil-pointer (это и воспроизвёл первый прогон теста).
func TestR83_AutoOffload_NoConfigReturnsAuto(t *testing.T) {
	prev := currentConfig
	currentConfig = nil
	defer func() { currentConfig = prev }()

	got := calculateOptimalGPULayersForModel(cppbackend.ModelInfo{Name: "m"}, "q8_0")
	if got != -1 {
		t.Errorf("без currentConfig: %d, want -1 (auto)", got)
	}
}
