//go:build llama_stub

// c_bridge_sources_parity_test.go — CI-гейт C-моста (2026-11-24).
//
// ЖИВОЙ ДЕФЕКТ, ЗАКРЫТЫЙ ЭТИМ ТЕСТОМ. В R705 добавили `c/bridge/csrc/model_hparams.cpp`
// (реальная ширина головы K/V из llama_hparams) и перечислили его в
// `c/bridge/CMakeLists.txt`, но НЕ в `c/bridge/tests/CMakeLists.txt`. Тесты
// компилируют `../bridge.c` напрямую, поэтому CI-джоба «C-bridge regression test
// (libllama-common.a CPU-only)» падала на линковке:
//
//	bridge.c:(.text+0x7cc5): undefined reference to `ollamalegion_model_head_dim_v_full'
//	bridge.c:(.text+0x7cd9): undefined reference to `ollamalegion_model_head_dim_k_full'
//	make[2]: *** [CMakeFiles/test_batch_n_tokens.dir/build.make:191: test_batch_n_tokens] Error 1
//
// То есть ЛОКАЛЬНАЯ сборка образа была зелёной (там свои CMake-цели), а CI —
// красной, и причину видел только тот, кто открыл лог линковки. Правило, которое
// ловит это заранее: любой исходник моста, который собирает `c/bridge/CMakeLists.txt`,
// обязан быть назван и в тестовом CMake (напрямую или отдельной целью).
package tests

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// bridgeSourcesR734 — список BRIDGE_SOURCES из c/bridge/CMakeLists.txt.
func bridgeSourcesR734(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile("../c/bridge/CMakeLists.txt")
	if err != nil {
		t.Fatalf("не читается c/bridge/CMakeLists.txt: %v", err)
	}
	text := string(body)
	start := strings.Index(text, "set(BRIDGE_SOURCES")
	if start < 0 {
		t.Fatal("в c/bridge/CMakeLists.txt нет set(BRIDGE_SOURCES) — правило нечего проверять")
	}
	rest := text[start:]
	end := strings.Index(rest, ")")
	if end < 0 {
		t.Fatal("set(BRIDGE_SOURCES не закрыт")
	}
	var out []string
	for _, line := range strings.Split(rest[:end], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "set(") {
			continue
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		t.Fatal("BRIDGE_SOURCES пуст — это не «зелёный» случай")
	}
	return out
}

// stripCMakeCommentsR734 — убрать строки-комментарии из CMake.
//
// ЗАЧЕМ. Первая версия теста искала имя файла подстрокой во всём тексте — и
// проходила на ОДНОМ УПОМИНАНИИ В КОММЕНТАРИИ («bridge.c зовёт … из
// csrc/model_hparams.cpp»). Мутация «убрать строку из target» не краснила тест:
// проверка была вакуумной. Теперь комментарии не считаются.
func stripCMakeCommentsR734(text string) string {
	var b strings.Builder
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestR734_BridgeTestSourcesCoverBridgeSources — каждый исходник моста назван и в
// тестовом CMake. Красный тест означает, что образ и CI собирают РАЗНЫЕ наборы
// файлов: расхождение вылезет на линковке в CI (undefined reference), а не здесь.
func TestR734_BridgeTestSourcesCoverBridgeSources(t *testing.T) {
	sources := bridgeSourcesR734(t)
	body, err := os.ReadFile("../c/bridge/tests/CMakeLists.txt")
	if err != nil {
		t.Fatalf("не читается c/bridge/tests/CMakeLists.txt: %v", err)
	}
	testText := stripCMakeCommentsR734(string(body))
	if len(strings.TrimSpace(testText)) == 0 {
		t.Fatal("тестовый CMake пуст — проверять нечего")
	}
	for _, src := range sources {
		// Сравниваем по имени файла: в BRIDGE_SOURCES путь относительный
		// (`csrc/model_hparams.cpp`), в тестах — от каталога тестов
		// (`../csrc/model_hparams.cpp`). Смысл один: файл обязан компилироваться.
		name := src
		if i := strings.LastIndex(src, "/"); i >= 0 {
			name = src[i+1:]
		}
		if !strings.Contains(testText, name) {
			t.Errorf("c/bridge/tests/CMakeLists.txt не упоминает %q из BRIDGE_SOURCES: "+
				"CI-цели компилируют ../bridge.c без этого файла, и линковка падает "+
				"undefined reference (так было с csrc/model_hparams.cpp — R705, CI красный)", src)
		}
	}
}

// TestR734_BridgeIncludeDirsParity — тестовый CMake объявляет те же каталоги
// заголовков llama.cpp, что и боевой.
//
// ЖИВОЙ ДЕФЕКТ (прогон 45307667015). После того как `csrc/model_hparams.cpp`
// добавили в списки целей, CI упал уже на КОМПИЛЯЦИИ:
//
//	model_hparams.cpp:31:10: fatal error: llama-model.h: No such file or directory
//
// Файл включает ПРИВАТНЫЙ заголовок `llama.cpp/src/llama-model.h`, а боевой
// CMake каталог `src` объявляет, тестовый — нет (он знал только include/,
// ggml/include и tools/mtmd). Тест сверяет каталоги по хвосту после `llama.cpp/`:
// так сравнение не зависит от того, как каждая сторона строит абсолютный путь.
func TestR734_BridgeIncludeDirsParity(t *testing.T) {
	bridgeBody, err := os.ReadFile("../c/bridge/CMakeLists.txt")
	if err != nil {
		t.Fatalf("не читается c/bridge/CMakeLists.txt: %v", err)
	}
	testsBody, err := os.ReadFile("../c/bridge/tests/CMakeLists.txt")
	if err != nil {
		t.Fatalf("не читается c/bridge/tests/CMakeLists.txt: %v", err)
	}
	// Боевой CMake пишет пути через ${LLAMA_CPP_ROOT}, тестовый — через
	// ${CMAKE_CURRENT_SOURCE_DIR}/../../llama.cpp. Приводим к одному виду, иначе
	// сравнение хвостов не находит ничего и проверка вырождается.
	bridgeText := strings.ReplaceAll(stripCMakeCommentsR734(string(bridgeBody)), "${LLAMA_CPP_ROOT}", "llama.cpp")
	testsText := stripCMakeCommentsR734(string(testsBody))

	// Хвосты вида "llama.cpp/src" из боевого CMake.
	dirs := map[string]bool{}
	for _, line := range strings.Split(bridgeText, "\n") {
		i := strings.Index(line, "llama.cpp/")
		if i < 0 {
			continue
		}
		tail := strings.TrimSpace(line[i:])
		tail = strings.TrimRight(tail, `")`)
		if tail != "" {
			dirs[tail] = true
		}
	}
	if len(dirs) == 0 {
		t.Fatal("в боевом CMake не нашлось каталогов llama.cpp — проверка вырождена")
	}
	for dir := range dirs {
		if !strings.Contains(testsText, dir) {
			t.Errorf("c/bridge/tests/CMakeLists.txt не объявляет каталог %q: "+
				"тесты компилируют те же исходники, но без этого include-пути падают "+
				"на приватных заголовках llama.cpp (так было с llama.cpp/src)", dir)
		}
	}
}
// TestR734_BridgeTestsCompileEveryCsrc — каждый `.cpp` из csrc/ назван в тестовом
// CMake. Отдельно от списка выше: новый файл могут забыть добавить И в
// BRIDGE_SOURCES, а тесты всё равно обязаны его видеть (иначе bridge.c снова
// сошлётся на несуществующий символ).
func TestR734_BridgeTestsCompileEveryCsrc(t *testing.T) {
	entries, err := os.ReadDir("../c/bridge/csrc")
	if err != nil {
		t.Fatalf("не читается c/bridge/csrc: %v", err)
	}
	body, err := os.ReadFile("../c/bridge/tests/CMakeLists.txt")
	if err != nil {
		t.Fatalf("не читается c/bridge/tests/CMakeLists.txt: %v", err)
	}
	testText := stripCMakeCommentsR734(string(body))
	checked := 0
	re := regexp.MustCompile(`\.cpp$`)
	for _, e := range entries {
		if e.IsDir() || !re.MatchString(e.Name()) {
			continue
		}
		checked++
		if !strings.Contains(testText, e.Name()) {
			t.Errorf("c/bridge/tests/CMakeLists.txt не компилирует csrc/%s: "+
				"образ её собирает, а CI-тесты — нет", e.Name())
		}
	}
	if checked == 0 {
		t.Fatal("в c/bridge/csrc нет .cpp — проверка вырождена")
	}
}
