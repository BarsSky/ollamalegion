//go:build llama_stub

// reload_splitmode_r179_test.go — R179 (2026-10-19).
//
// ЗАКРЫВАЕТ дефект «Применить профиль теряет раскладку», который R174-R178
// локализовывали: при reload в мост уходил SplitMode=0 (LLAMA_SPLIT_MODE_NONE),
// и llama.cpp оставлял из списка устройств только main_gpu — RPC-узел
// отбрасывался. Снаружи это выглядело так: планировщик видел ДВА устройства,
// серверы передавались в мост (`RPC-раскладка передана в мост`), а llama.cpp
// печатал только `using device CUDA0`. Путь /api/models/load-with-params режим
// передавал всегда, поэтому свежая загрузка раскладывала слои, а reload нет.
package main

import (
	"os"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// setSplitConfigR179 — изолировать глобальный конфиг на время теста.
func setSplitConfigR179(t *testing.T, defaultSplitMode int) {
	t.Helper()
	prevCfg := currentConfig
	cfg := cppbackend.DefaultConfig()
	cfg.DefaultSplitMode = defaultSplitMode
	currentConfig = &cfg
	t.Cleanup(func() { currentConfig = prevCfg })
}

// TestR179_ReloadSplitMode_DefaultFromConfig — без явного значения режим берётся
// из конфига (у стенда это 1 = LAYER), а НЕ 0 = NONE.
func TestR179_ReloadSplitMode_DefaultFromConfig(t *testing.T) {
	setSplitConfigR179(t, 1)
	os.Unsetenv("CPPWORKER_SPLIT_MODE")

	if got := reloadSplitMode(nil); got != 1 {
		t.Fatalf("reloadSplitMode(nil) = %d, want 1 (LAYER): при 0 llama.cpp отбрасывает "+
			"RPC-узел и раскладка исчезает (авария R179)", got)
	}
}

// TestR179_ReloadSplitMode_ExplicitWins — явный режим запроса уважается, включая 0
// (0 = NONE — законное значение, подменять его нельзя).
func TestR179_ReloadSplitMode_ExplicitWins(t *testing.T) {
	setSplitConfigR179(t, 1)
	zero, layer, tensor, bad := 0, 1, 3, 99
	if got := reloadSplitMode(&zero); got != 0 {
		t.Errorf("явный splitMode=0 (NONE) переписан на %d — оператор не может выключить раскладку", got)
	}
	if got := reloadSplitMode(&layer); got != 1 {
		t.Errorf("явный splitMode=1 -> %d", got)
	}
	if got := reloadSplitMode(&tensor); got != 3 {
		t.Errorf("явный splitMode=3 -> %d", got)
	}
	if got := reloadSplitMode(&bad); got != 1 {
		t.Errorf("недопустимый splitMode=99 -> %d, want 1 (конфиг)", got)
	}
}

// TestR179_ReloadSplitMode_EnvWinsOverConfig — CPPWORKER_SPLIT_MODE перекрывает конфиг.
func TestR179_ReloadSplitMode_EnvWinsOverConfig(t *testing.T) {
	setSplitConfigR179(t, 1)
	t.Setenv("CPPWORKER_SPLIT_MODE", "2")
	if got := reloadSplitMode(nil); got != 2 {
		t.Errorf("reloadSplitMode(nil) = %d, want 2 (env CPPWORKER_SPLIT_MODE)", got)
	}
}
