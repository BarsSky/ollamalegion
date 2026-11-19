//go:build llama_stub

// rpc_split_mode_wiring_r108_test.go — S1 (2026-10-13): статическая защита того,
// что режим раскладки слоёв доезжает от запроса до моста.
//
// ЖИВОЙ ДЕФЕКТ. На стенде стоял CPPWORKER_SPLIT_MODE=1, cppworker считал
// defSplitMode=1 и клал его в LoadModelOpts.SplitMode... а в bridge.ModelConfig
// это значение не присваивалось нигде. Мост видел 0 = LLAMA_SPLIT_MODE_NONE и
// llama.cpp (src/llama.cpp:284) оставлял из переданного списка устройств ТОЛЬКО
// main_gpu:
//
//	[bridge] RPC: устройств в раскладке 2 (локальные + удалённые)
//	llama_prepare_model_devices: using device CUDA0 (NVIDIA GeForce RTX 3070) - 7097 MiB free
//
// То есть транспорт работал, а раскладка была невозможна в принципе — и это
// выглядело как «RPC не работает» без единой ошибки в логе.
//
// Плюс вторая ловушка: -1 («дефолт llama.cpp» = LAYER по соглашению моста)
// переводился в 0 (= NONE), то есть «оставь как в llama.cpp» превращалось в
// «используй одно устройство».
package main

import (
	"os"
	"strings"
	"testing"
)

// TestR108_SplitModeReachesBridge — opts.SplitMode обязан попадать в конфиг моста.
func TestR108_SplitModeReachesBridge(t *testing.T) {
	// R509 (шаг R4): LoadModelWithOpts вынесен из backend.go в backend_load.go —
	// сторож читает файл, где путь загрузки живёт сейчас. Проверка та же.
	const path = "../../internal/cppbackend/backend_load.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("не удалось прочитать %s: %v", path, err)
	}
	text := stripLineComments(string(src))
	if !strings.Contains(text, "cfg.SplitMode = opts.SplitMode") {
		t.Error("в LoadModelWithOpts нет `cfg.SplitMode = opts.SplitMode`: режим раскладки " +
			"не доедет до моста, llama.cpp получит LLAMA_SPLIT_MODE_NONE (0) и оставит " +
			"из списка устройств только main_gpu — раскладка на RPC-узел станет невозможной")
	}
}

// TestR108_SplitModeDefaultIsLayer — «не задано» = -1 (дефолт llama.cpp = LAYER),
// а не 0 (NONE). Иначе раскладка выключена по умолчанию даже при заданных узлах.
func TestR108_SplitModeDefaultIsLayer(t *testing.T) {
	// R623 (2026-11-19): оба обработчика загрузки больше не в одном файле —
	// `handleLoadModel` остался в handlers_model.go, `handleLoadWithParams` переехал
	// в handlers_model_load_params.go. Читаем ОБА: сторож проверяет проводку в обоих
	// путях, и «файл не найден» не должно превращаться в ложное «дефект вернулся».
	var text string
	for _, file := range []string{"handlers_model.go", "handlers_model_load_params.go"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("не удалось прочитать %s: %v", file, err)
		}
		text += stripLineComments(string(src))
	}
	if !strings.Contains(text, "defSplitMode := -1") {
		t.Error("в handleLoadModel нет `defSplitMode := -1`: по умолчанию режим раскладки " +
			"снова станет NONE (одно устройство) вместо дефолта llama.cpp (LAYER)")
	}
	if !strings.Contains(text, "defSplitMode2 := -1") {
		t.Error("в handleLoadWithParams нет `defSplitMode2 := -1`: тот же дефект на пути загрузки, " +
			"которым ходит балансер")
	}
	if strings.Contains(text, "defSplitMode = 0 // bridge") {
		t.Error("вернулся перевод -1 → 0 в defSplitMode: мост трактует 0 как NONE, " +
			"а LAYER — это -1 (см. c/bridge/bridge.h)")
	}
}
