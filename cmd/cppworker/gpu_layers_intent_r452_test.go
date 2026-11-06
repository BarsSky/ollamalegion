// gpu_layers_intent_r452_test.go — R452 (2026-11-06): gpuLayers в запросе —
// это НАМЕРЕНИЕ, а у загруженной модели хранится ФАКТ.
//
// ЖИВОЙ ЛОГ после R451 (проверка логирования причин перезагрузки):
//
//	"changed":["contextSize 32768→2048","gpuLayers 36→-1"]
//
// То есть при запросе «все слои» (-1) загруженная модель с 36 слоями на GPU
// считалась ИЗМЕНЁННОЙ — и любой повторный load тянул unload+load без причины.
// Балансер по умолчанию шлёт -2 (AUTO), что давало ту же картину.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

// loaded36 — модель с 36 слоями, все на GPU (факт 36).
func loaded36() cppbackend.ModelInfo {
	info := infoFromOptsR451(baseLoadOptsR451())
	info.NLayers = 36
	info.GPULayers = 36
	return info
}

// TestR452_AllLayersIsNotAChange — «-1» при полностью выгруженной модели.
func TestR452_AllLayersIsNotAChange(t *testing.T) {
	info := loaded36()
	opts := baseLoadOptsR451()
	opts.GPULayers = -1

	if diffs := loadOptionDiffs(info, opts); len(diffs) != 0 {
		t.Fatalf("«все слои» при фактических 36 — не изменение, получено %v", diffs)
	}
	if !sameLoadOptions(info, opts) {
		t.Fatal("sameLoadOptions=false: повторный load выгрузит и загрузит модель без причины")
	}
}

// TestR452_AutoIsNotAChange — «-2» (AUTO): систему уже спросили при загрузке.
func TestR452_AutoIsNotAChange(t *testing.T) {
	info := loaded36()
	opts := baseLoadOptsR451() // GPULayers: -2
	opts.GPULayers = -2

	if diffs := loadOptionDiffs(info, opts); len(diffs) != 0 {
		t.Fatalf("AUTO-слои не должны считаться изменением, получено %v", diffs)
	}
}

// TestR452_PartialOffloadIsAChange — «все слои» запрошены, а на GPU только часть.
func TestR452_PartialOffloadIsAChange(t *testing.T) {
	info := loaded36()
	info.GPULayers = 20 // часть слоёв в RAM
	opts := baseLoadOptsR451()
	opts.GPULayers = -1

	diffs := loadOptionDiffs(info, opts)
	if len(diffs) != 1 || !hasDiffName(diffs, "gpuLayers") {
		t.Fatalf("частичная выгрузка при запросе «все слои» — это изменение: %v", diffs)
	}
}

// TestR452_CPULayersSemantics — 0 = только CPU, N = ровно N.
func TestR452_CPULayersSemantics(t *testing.T) {
	gpu := loaded36()
	cpu := loaded36()
	cpu.GPULayers = 0

	opts := baseLoadOptsR451()

	opts.GPULayers = 0
	if diffs := loadOptionDiffs(gpu, opts); len(diffs) == 0 || !hasDiffName(diffs, "gpuLayers") {
		t.Fatalf("запрос CPU-only при модели на GPU — изменение: %v", diffs)
	}
	if diffs := loadOptionDiffs(cpu, opts); hasDiffName(diffs, "gpuLayers") {
		t.Fatalf("запрос CPU-only при CPU-only модели — не изменение: %v", diffs)
	}

	opts.GPULayers = 36
	if diffs := loadOptionDiffs(gpu, opts); hasDiffName(diffs, "gpuLayers") {
		t.Fatalf("запрос 36 слоёв при фактических 36 — не изменение: %v", diffs)
	}

	opts.GPULayers = 30
	if diffs := loadOptionDiffs(gpu, opts); !hasDiffName(diffs, "gpuLayers") {
		t.Fatalf("запрос 30 слоёв при 36 — изменение: %v", diffs)
	}
}

// TestR452_OtherParamsStillDetected — ядро сравнения не ослаблено: другие поля
// по-прежнему дают разницу (иначе «не изменение» превратилось бы в «не замечать
// ничего»).
func TestR452_OtherParamsStillDetected(t *testing.T) {
	info := loaded36()
	opts := baseLoadOptsR451()
	opts.GPULayers = -1 // намерение совпадает с фактом
	opts.ContextSize = 40134

	diffs := loadOptionDiffs(info, opts)
	if len(diffs) != 1 || !hasDiffName(diffs, "contextSize") {
		t.Fatalf("смена окна обязана остаться изменением: %v", diffs)
	}
}

func hasDiffName(diffs []string, name string) bool {
	for _, d := range diffs {
		if len(d) >= len(name) && d[:len(name)] == name {
			return true
		}
	}
	return false
}
