// load_option_diffs_r451_test.go — R451 (2026-11-06): строка «reloading model
// because parameters changed» обязана называть, ЧТО разошлось.
//
// ЖИВОЙ ФАКТ. На стенде второй ход Cline вызвал перезагрузку 9B на 243 с:
// cppworker написал `reloading model because parameters changed` с oldPath ==
// newPath (тот же файл) и без единого слова о причине. Реальная причина —
// contextSize 32768 → 40134 — нашлась только по параметрам запроса на загрузку.
package main

import (
	"strings"
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// baseLoadOptsR451 — параметры, с которыми модель уже загружена в живом случае.
func baseLoadOptsR451() cppbackend.LoadModelOpts {
	return cppbackend.LoadModelOpts{
		ContextSize:   32768,
		BatchSize:     256,
		GPULayers:     -2,
		FlashAttnType: 1,
		UseMmap:       true,
		Parallel:      1,
		KVCacheType:   "q8_0",
	}
}

func infoFromOptsR451(opts cppbackend.LoadModelOpts) cppbackend.ModelInfo {
	return cppbackend.ModelInfo{
		Name:          "m",
		ContextSize:   opts.ContextSize,
		BatchSize:     opts.BatchSize,
		GPULayers:     opts.GPULayers,
		FlashAttnType: opts.FlashAttnType,
		NUMA:          opts.NUMA,
		UseMmap:       opts.UseMmap,
		Parallel:      opts.Parallel,
		KVCacheType:   opts.KVCacheType,
		TensorSplit:   opts.TensorSplit,
	}
}

// TestR451_SameOptionsNoDiffs — одинаковые параметры: ни «разниц», ни перезагрузки.
func TestR451_SameOptionsNoDiffs(t *testing.T) {
	opts := baseLoadOptsR451()
	info := infoFromOptsR451(opts)

	if diffs := loadOptionDiffs(info, opts); len(diffs) != 0 {
		t.Fatalf("различий нет, а получено %v", diffs)
	}
	if !sameLoadOptions(info, opts) {
		t.Fatal("sameLoadOptions=false при одинаковых параметрах")
	}
}

// TestR451_ContextChangeIsNamed — живой случай: только окно.
func TestR451_ContextChangeIsNamed(t *testing.T) {
	opts := baseLoadOptsR451()
	info := infoFromOptsR451(opts)
	opts.ContextSize = 40134 // ровно то, что пришло в живом дефекте

	diffs := loadOptionDiffs(info, opts)
	if len(diffs) != 1 {
		t.Fatalf("ожидалась ровно одна разница, получено %v", diffs)
	}
	got := diffs[0]
	if !strings.Contains(got, "contextSize") || !strings.Contains(got, "32768") || !strings.Contains(got, "40134") {
		t.Fatalf("причина перезагрузки названа непонятно: %q (нужно contextSize 32768→40134)", got)
	}
	if sameLoadOptions(info, opts) {
		t.Fatal("sameLoadOptions=true при другом contextSize")
	}
}

// TestR451_EveryParamIsNames — каждая сравниваемая пара попадает в лог.
//
// Если sameLoadOptions начнёт считать разницей то, чего нет в loadOptionDiffs,
// строка «parameters changed» снова станет немой — этот тест и ловит расхождение.
func TestR451_EveryParamIsNamed(t *testing.T) {
	base := baseLoadOptsR451()
	info := infoFromOptsR451(base)

	mutations := map[string]func(*cppbackend.LoadModelOpts){
		"contextSize":   func(o *cppbackend.LoadModelOpts) { o.ContextSize = 40134 },
		"batchSize":     func(o *cppbackend.LoadModelOpts) { o.BatchSize = 512 },
		"gpuLayers":     func(o *cppbackend.LoadModelOpts) { o.GPULayers = -1 },
		"flashAttnType": func(o *cppbackend.LoadModelOpts) { o.FlashAttnType = 0 },
		"numa":          func(o *cppbackend.LoadModelOpts) { o.NUMA = true },
		"useMmap":       func(o *cppbackend.LoadModelOpts) { o.UseMmap = false },
		"tensorSplit":   func(o *cppbackend.LoadModelOpts) { o.TensorSplit = []float32{1, 2} },
		"parallel":      func(o *cppbackend.LoadModelOpts) { o.Parallel = 2 },
		"kvCacheType":   func(o *cppbackend.LoadModelOpts) { o.KVCacheType = "q4_0" },
	}
	for name, mutate := range mutations {
		opts := base
		mutate(&opts)
		if sameLoadOptions(info, opts) {
			t.Fatalf("%s: изменение не замечено — модель не перезагрузится", name)
		}
		diffs := loadOptionDiffs(info, opts)
		joined := strings.Join(diffs, "; ")
		if !strings.Contains(joined, name) {
			t.Fatalf("%s: изменение замечено, но в лог не попало (diffs=%q)", name, joined)
		}
	}
}
