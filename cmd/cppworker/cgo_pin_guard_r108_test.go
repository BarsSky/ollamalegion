//go:build llama_stub

// cgo_pin_guard_r108_test.go — S1 (2026-10-13): статическая защита от паники
// «cgo argument has Go pointer to unpinned Go pointer».
//
// ЖИВОЙ ДЕФЕКТ (первая же реальная раскладка на два устройства):
//
//	panic: runtime error: cgo argument has Go pointer to unpinned Go pointer
//	ollama-loadbalancer/c/bridge.LoadModelWithEarlyHandle.func4(...)
//		c/bridge/bridge.go:769
//
// В c/bridge/bridge.go массив пропорций tensor_split копировался в Go-слайс
// `ts` и передавался в C как `cCfg.tensor_split = &ts[0]` БЕЗ pinning. Пока
// раскладка не включалась (одна карта → GPUCount=1), tensor_split не заполнялся,
// и паника не воспроизводилась; с подключением RPC-узла она стала обязательной.
//
// Проверка статическая: у cgo-файла нет сборки под llama_stub, поэтому смотрим
// исходник. Правило простое — сколько раз берём адрес слайса, столько раз должны
// его закрепить.
package main

import (
	"os"
	"strings"
	"testing"
)

// TestR108_TensorSplitArrayIsPinned — каждый `cCfg.tensor_split = &ts[0]` обязан
// сопровождаться `tsPinner.Pin(&ts[0])`.
func TestR108_TensorSplitArrayIsPinned(t *testing.T) {
	const path = "../../c/bridge/bridge.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не удалось прочитать %s: %v", path, err)
	}
	text := stripLineComments(string(src))

	assigned := strings.Count(text, "cCfg.tensor_split = &ts[0]")
	pinned := strings.Count(text, "tsPinner.Pin(&ts[0])")
	if assigned == 0 {
		t.Fatal("не найден ни один `cCfg.tensor_split = &ts[0]` — тест устарел, поправьте его")
	}
	if assigned != pinned {
		t.Errorf("tensor_split передаётся в C %d раз, а pinning сделан %d раз: "+
			"незакреплённый Go-указатель в аргументе cgo даёт panic "+
			"«cgo argument has Go pointer to unpinned Go pointer» на первой же "+
			"раскладке на несколько устройств (живой дефект S1)", assigned, pinned)
	}

	// Тот же класс ошибки у массивов RPC-узлов и override-tensors: адрес слайса
	// берётся только после pinner.Pin.
	for _, frag := range []string{"pinner.Pin(&cs[0])", "pinner.Pin(&pinnedPats[0])"} {
		if !strings.Contains(text, frag) {
			t.Errorf("не найден %s: массив указателей для C снова передаётся без pinning", frag)
		}
	}
}
