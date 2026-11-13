//go:build llama_stub

// context_size_compare_r493_test.go — R493-12h: сравнение окна с выравниванием.
//
// ЖИВОЙ ФАКТ из архива 09:53 (второй стенд, Qwen3.8-27B):
//
//	changed=["contextSize 150016→149809"]   ← перезагрузка ВНИЗ на 207 токенов
//	changed=["contextSize 160768→160674"]   ← и ещё одна, на 94
//
// Балансер присылает «нужное» окно из оценки промпта (невыровненное), воркер
// держит окно выровненным по 256. Прямое сравнение видело разницу и выгружало
// 27B (avgDurationMs 215875 мс) ради ничего.
package main

import (
	"testing"

	"ollama-loadbalancer/internal/cppbackend"
)

func TestR493_SameContextSizeAligned(t *testing.T) {
	cases := []struct {
		name      string
		loaded    int
		requested int
		want      bool
	}{
		{"архив: 150016 загружено, просят 149809", 150016, 149809, true},
		{"архив: 160768 загружено, просят 160674", 160768, 160674, true},
		{"128k внутри одного шага выравнивания", 131072, 130944, true},
		{"ровно совпадает", 131072, 131072, true},
		{"существенно меньше — это перезагрузка вниз по просьбе", 131072, 65536, false},
		{"больше загруженного — нужен рост", 131072, 137109, false},
		{"модели нет", 0, 8192, false},
		{"окно не задано", 8192, 0, false},
		{"окно не задано и у модели нет", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sameContextSizeR493(tc.loaded, tc.requested); got != tc.want {
				t.Fatalf("sameContextSizeR493(loaded=%d, requested=%d) = %v, ожидалось %v",
					tc.loaded, tc.requested, got, tc.want)
			}
		})
	}
}

// TestR493_LoadOptionDiffsNoDownReload — то же через публичную для пакета
// функцию сравнения параметров: разница только в невыровненном окне не должна
// попадать в «параметры изменились».
func TestR493_LoadOptionDiffsNoDownReload(t *testing.T) {
	info := cppbackend.ModelInfo{
		Name:        "Qwen3.8-27B-UD-Q4_K_M",
		State:       cppbackend.StateLoaded,
		ContextSize: 150016,
		GPULayers:   36,
	}
	opts := cppbackend.LoadModelOpts{
		ContextSize: 149809,
		GPULayers:   36,
	}
	if diffs := loadOptionDiffs(info, opts); len(diffs) != 0 {
		t.Fatalf("разница найдена там, где окно уже покрывает запрос: %v (это и есть "+
			"перезагрузка вниз на 207 токенов из архива)", diffs)
	}
	opts.ContextSize = 160000
	if diffs := loadOptionDiffs(info, opts); len(diffs) == 0 {
		t.Fatal("существенный рост (150016 → 160000) обязан считаться различием")
	}
}
