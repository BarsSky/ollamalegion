//go:build llama_stub

// load_error_tail_test.go — R697 (2026-11-24).
//
// ПРЕДМЕТ. Причина провала загрузки живёт в логе llama.cpp, а не в ответе моста:
// мост на null из llama_model_load_from_file печатает только
// «failed to load model from <путь>» (c/bridge/bridge.c:1905), а «unknown model
// architecture: 'inkling2'» остаётся строкой LLAMA_LOG_ERROR, которую R521 уже
// перехватывает (c/bridge/layers_plan.go, recordLogLine), но приклеивал только к
// ошибкам декода.
//
// Тесты проверяют чистые функции выбора вердикта: живой мост в тестах не поднять
// (llama_stub), поэтому вход подменяется списком строк.
package cppbackend

import (
	"errors"
	"strings"
	"testing"
)

// logTailInklingR697 — дословный хвост лога c/llama.cpp b1b770d90 на файле
// Inkling-2-Mini-UD-Q4_K_M.gguf (general.architecture="inkling2", 734 тензора):
// метаданные прочитаны, llm_arch_from_string вернул LLM_ARCH_UNKNOWN, до тензоров
// дело не дошло.
var logTailInklingR697 = []string{
	"print_info: arch                  = inkling2",
	"llama_model_load: error loading model: unknown model architecture: 'inkling2'",
	"llama_model_load_from_file_impl: failed to load model",
}

func TestR697_LoadEngineVerdictLinesKeepsReason(t *testing.T) {
	lines := append([]string{"llama_model_loader: loaded meta data with 58 key-value pairs"}, logTailInklingR697...)
	got := loadEngineVerdictLines(lines)
	if len(got) != 2 {
		t.Fatalf("вердикт должен быть из двух строк (причина + итог), получено %d: %v", len(got), got)
	}
	if !strings.Contains(got[0], "unknown model architecture: 'inkling2'") {
		t.Errorf("первая строка вердикта = %q, want причина от llama.cpp", got[0])
	}
	// Порядок как в логе: причина ПЕРЕД итоговой строкой.
	if !strings.Contains(got[1], "failed to load model") {
		t.Errorf("вторая строка вердикта = %q, want итоговая строка движка", got[1])
	}
}

// TestR697_LoadEngineVerdictLinesIgnoresNoise — обычная раскладка/метаданные не
// должны попадать в текст ошибки: клиент показывает её в диалоге.
func TestR697_LoadEngineVerdictLinesIgnoresNoise(t *testing.T) {
	lines := []string{
		"load_tensors: layer 0 assigned to device CUDA0",
		"print_info: n_ctx = 4096",
		"",
		"   ",
		"ggml_backend_cuda_init: found 1 CUDA devices",
	}
	if got := loadEngineVerdictLines(lines); len(got) != 0 {
		t.Fatalf("в вердикт попал шум раскладки: %v", got)
	}
	// Дубли одной и той же строки не размножаются.
	dup := []string{
		"llama_model_load: error loading model: unknown model architecture: 'inkling2'",
		"llama_model_load: error loading model: unknown model architecture: 'inkling2'",
	}
	if got := loadEngineVerdictLines(dup); len(got) != 1 {
		t.Fatalf("дубликаты строк вердикта не схлопнуты: %v", got)
	}
}

func TestR697_LoadErrorWithTailText(t *testing.T) {
	const msg = "load model x: load model failed: failed to load model from /app/models/x.gguf"
	got := loadErrorWithTailText(msg, logTailInklingR697)
	if !strings.Contains(got, "unknown model architecture: 'inkling2'") {
		t.Fatalf("причина не приклеена: %q", got)
	}
	if !strings.HasPrefix(got, msg) {
		t.Errorf("исходный текст ошибки изменён: %q", got)
	}
	if !strings.Contains(got, "[llama.cpp] ") {
		t.Errorf("нет маркера источника строк (как у R521): %q", got)
	}

	// Пустой хвост — текст не меняется вовсе (никаких «лог недоступен» на загрузке:
	// это не диагностика декода).
	if got := loadErrorWithTailText(msg, nil); got != msg {
		t.Errorf("без строк лога текст изменён: %q", got)
	}
	// Шум без вердикта — тоже без изменений.
	if got := loadErrorWithTailText(msg, []string{"load_tensors: layer 1 assigned to device CPU"}); got != msg {
		t.Errorf("шум раскладки приклеен к ошибке загрузки: %q", got)
	}
}

// TestR697_LoadErrorWithTailPreservesIdentity — обёртка не должна ломать errors.Is:
// на этом признаке держится отличие «отменено пользователем» от «сломалось».
func TestR697_LoadErrorWithTailPreservesIdentity(t *testing.T) {
	if LoadErrorWithTail(nil) != nil {
		t.Fatal("LoadErrorWithTail(nil) обязан вернуть nil")
	}
	sentinel := errors.New("aborted by user")
	wrapped := LoadErrorWithTail(sentinel)
	if !errors.Is(wrapped, sentinel) {
		t.Fatalf("errors.Is потерян после приклейки хвоста: %v", wrapped)
	}
	// В lama_stub перехвата строк нет — ошибка возвращается как есть.
	if wrapped.Error() != sentinel.Error() {
		t.Errorf("без лога текст ошибки изменён: %q", wrapped.Error())
	}
}
