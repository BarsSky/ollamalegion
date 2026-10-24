// memfit_breakdown_r115_test.go — R115: отказ по памяти обязан показывать, чего
// именно не хватило. Тесты проверяют две вещи: раскладка берётся ИЗ вердикта
// (не пересчитывается) и попадает в тело 422 вместе с текстом.
package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

func TestR115_BreakdownMirrorsVerdict(t *testing.T) {
	budget := memfit.Budget{
		VRAMFree:      memfit.MiBOf(7017),
		VRAMTotal:     memfit.MiBOf(24564),
		VRAMKnown:     true,
		RAMAvail:      memfit.MiBOf(23429),
		RAMTotal:      memfit.MiBOf(25044),
		RAMLimit:      memfit.MiBOf(20000),
		RAMLimitKnown: true,
		Source:        "cgroup+procfs",
	}
	v := memfit.Verdict{
		Stage:       memfit.StageDoesNotFit,
		GPULayers:   0,
		TotalLayers: 64,
		KVPerToken:  memfit.Bytes(32768),
		Weights:     memfit.MiBOf(15300),
		GPUWeights:  0,
		CPUWeights:  memfit.MiBOf(15300),
		GPUKV:       0,
		CPUKV:       memfit.MiBOf(1024),
		UsableVRAM:  memfit.MiBOf(6900),
		UsableRAM:   memfit.MiBOf(19900),
	}

	m := buildMemoryBreakdown(budget, v, 1)
	f := m.Fields()

	// Раскладка обязана повторять вердикт до мебибайта: любое расхождение здесь
	// означало бы вторую формулу — то, чего быть не должно.
	checks := map[string]int64{
		"vram_free_mb":   7017,
		"vram_total_mb":  24564,
		"ram_avail_mb":   23429,
		"ram_limit_mb":   20000,
		"weights_mb":     15300,
		"cpu_weights_mb": 15300,
		"gpu_weights_mb": 0,
		"cpu_kv_mb":      1024,
		"usable_vram_mb": 6900,
		"usable_ram_mb":  19900,
	}
	for key, want := range checks {
		got, ok := f[key].(int64)
		if !ok {
			t.Fatalf("R115: поле %s отсутствует или не int64: %#v", key, f[key])
		}
		if got != want {
			t.Errorf("R115: %s = %d, want %d", key, got, want)
		}
	}
	// Счётные поля — int, не int64: это слои и байты на токен, а не мебибайты.
	counts := map[string]int{
		"gpu_layers":   0,
		"total_layers": 64,
		"kv_per_token": 32768,
	}
	for key, want := range counts {
		got, ok := f[key].(int)
		if !ok {
			t.Fatalf("R115: поле %s отсутствует или не int: %#v", key, f[key])
		}
		if got != want {
			t.Errorf("R115: %s = %d, want %d", key, got, want)
		}
	}
	if f["vram_known"] != true || f["ram_limit_known"] != true {
		t.Errorf("R115: признак известности потерян: vram_known=%v ram_limit_known=%v",
			f["vram_known"], f["ram_limit_known"])
	}
	if f["rpc_devices"] != 1 {
		t.Errorf("R115: rpc_devices = %v, want 1", f["rpc_devices"])
	}
	if f["stage"] != string(memfit.StageDoesNotFit) {
		t.Errorf("R115: stage = %v, want %s", f["stage"], memfit.StageDoesNotFit)
	}

	// Текст обязан называть обе стороны памяти: отказ «по VRAM» и отказ «по RAM»
	// требуют разных действий оператора.
	text := m.Text()
	for _, want := range []string{"VRAM free 7017 MB", "RAM available 23429 MB",
		"cgroup headroom 20000 MB", "weights 15300 MB", "KV 1024 MB",
		"stage does_not_fit"} {
		if !contains(text, want) {
			t.Errorf("R115: в тексте раскладки нет %q:\n%s", want, text)
		}
	}
}

func TestR115_UnknownMemoryIsNotZeroMemory(t *testing.T) {
	// Ноль без признака известности не должен читаться как «памяти нет»:
	// виртуальная машина без cgroup-лимита обязана получить явное «unknown».
	m := buildMemoryBreakdown(memfit.Budget{Source: "procfs"}, memfit.Verdict{Stage: memfit.StageUnknown}, 0)
	f := m.Fields()
	if f["ram_limit_known"] != false {
		t.Errorf("R115: ram_limit_known = %v, want false", f["ram_limit_known"])
	}
	if f["vram_known"] != false {
		t.Errorf("R115: vram_known = %v, want false", f["vram_known"])
	}
	if !contains(m.Text(), "cgroup limit unknown") {
		t.Errorf("R115: неизвестный лимит cgroup не проговорён: %s", m.Text())
	}
	if breakdownFieldsOrNil(nil) != nil {
		t.Errorf("R115: раскладка без бюджета обязана быть nil, а не нулями")
	}
}

func TestR115_InfeasibleResponseCarriesBreakdown(t *testing.T) {
	bd := buildMemoryBreakdown(
		memfit.Budget{
			VRAMFree: memfit.MiBOf(7017), VRAMTotal: memfit.MiBOf(24564), VRAMKnown: true,
			RAMAvail: memfit.MiBOf(23429), RAMTotal: memfit.MiBOf(25044), Source: "procfs",
		},
		memfit.Verdict{Stage: memfit.StageDoesNotFit, TotalLayers: 64, Weights: memfit.MiBOf(15300)},
		0)
	f := NCtxFeasibility{
		Model:          "Qwen3.8-27B-UD-Q4_K_M",
		RequestedNCtx:  32768,
		MaxVRAMNCtx:    0,
		MaxRAMNCtx:     0,
		GGUFMaxContext: 262144,
		HardMaxNCtx:    0,
		Stage:          nctxStageInfeasible,
		Known:          true,
		Breakdown:      &bd,
	}

	w := httptest.NewRecorder()
	writeNCtxInfeasibleResponse(w, f)

	if w.Code != 422 {
		t.Fatalf("R115: код ответа = %d, want 422", w.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("R115: тело 422 не разбирается: %v", err)
	}
	if body["code"] != "n_ctx_infeasible" {
		t.Errorf("R115: code = %v, want n_ctx_infeasible", body["code"])
	}
	got, ok := body["memory_breakdown"].(map[string]interface{})
	if !ok {
		t.Fatalf("R115: в 422 нет memory_breakdown: %#v", body)
	}
	if got["vram_free_mb"] != float64(7017) || got["weights_mb"] != float64(15300) {
		t.Errorf("R115: раскладка в 422 не совпадает с вердиктом: %#v", got)
	}
	errText, _ := body["error"].(string)
	if !contains(errText, "memory breakdown") || !contains(errText, "VRAM free 7017 MB") {
		t.Errorf("R115: текст отказа не содержит раскладку:\n%s", errText)
	}
}
