// model_name_r83_test.go — R83 (2026-09-25).
//
// Живой кейс: клиент (Cline, Ollama-провайдер) просит "qwen3.8:latest", файл на
// диске — "Qwen3.8-27B-UD-Q4_K_M.gguf". До этой правки cppworker шёл открывать
// "models/qwen3.8:latest.gguf", llama.cpp отвечал
// "failed to open GGUF file ... (No such file or directory)", наружу уходил 500.
package cppbackend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestR83_StripOllamaTag(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"тег latest", "qwen3.8:latest", "qwen3.8"},
		{"тег квантования", "qwen3.8:q4_k_m", "qwen3.8"},
		{"тег с версией", "gemma-4:q4_k_m", "gemma-4"},
		{"тега нет", "qwen3.8", "qwen3.8"},
		{"пробелы по краям", "  qwen3.8:latest  ", "qwen3.8"},
		{"точка в теге", "model:v1.2.3", "model"},
		// Пути НЕ трогаем: у Windows-пути своё двоеточие.
		{"windows-путь", `C:\models\Qwen3.8.gguf`, `C:\models\Qwen3.8.gguf`},
		{"unix-путь", "models/qwen3.8:latest.gguf", "models/qwen3.8:latest.gguf"},
		{"относительный путь", "./models/x.gguf", "./models/x.gguf"},
		// Неправдоподобный «тег» оставляем как есть — лучше честный 404,
		// чем молча обрезанное настоящее имя.
		{"тег с пробелом", "модель: последняя", "модель: последняя"},
		{"двоеточие в конце", "model:", "model:"},
		{"только двоеточие", ":", ":"},
		{"длинный хвост", "model:" + strings.Repeat("x", maxOllamaTagLen+1), "model:" + strings.Repeat("x", maxOllamaTagLen+1)},
		{"пустая строка", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripOllamaTag(tc.in); got != tc.want {
				t.Errorf("StripOllamaTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestR83_ModelNameVariants(t *testing.T) {
	got := ModelNameVariants("qwen3.8:latest")
	// Первым идёт ТОЧНЫЙ вариант: нормализация не должна перебивать точное имя.
	if len(got) == 0 || got[0] != "qwen3.8:latest" {
		t.Fatalf("первым должен идти точный вариант, got %v", got)
	}
	for _, want := range []string{"qwen3.8:latest", "qwen3.8", "qwen3.8.gguf"} {
		if !containsStr(got, want) {
			t.Errorf("нет варианта %q в %v", want, got)
		}
	}
	// Имя с расширением тоже должно давать форму без него — иначе
	// "qwen3.8.gguf" не найдёт "Qwen3.8-27B-UD-Q4_K_M.gguf" по вхождению.
	gotExt := ModelNameVariants("qwen3.8.gguf")
	for _, want := range []string{"qwen3.8.gguf", "qwen3.8"} {
		if !containsStr(gotExt, want) {
			t.Errorf("для имени с расширением нет варианта %q в %v", want, gotExt)
		}
	}
	// Дубликатов быть не должно: список идёт в цикл поиска по файлам.
	seen := map[string]bool{}
	for _, v := range got {
		if seen[v] {
			t.Errorf("дубликат варианта %q", v)
		}
		if strings.TrimSpace(v) == "" {
			t.Errorf("пустой вариант в %v", got)
		}
		seen[v] = true
	}
	if ModelNameVariants("   ") != nil {
		t.Error("пустое имя не должно давать вариантов")
	}
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestR83_FindModelByPath_OllamaTag — главный регресс: внешнее имя с тегом
// должно находить реальный файл, а не уходить в llama.cpp несуществующим путём.
func TestR83_FindModelByPath_OllamaTag(t *testing.T) {
	dir := t.TempDir()
	writeGGUFFile(t, dir, "Qwen3.8-27B-UD-Q4_K_M.gguf", 4096)

	mm := NewModelManager(dir, Config{ModelsDir: dir})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// Живой кейс из лога.
	path, err := mm.FindModelByPath("qwen3.8:latest")
	if err != nil {
		t.Fatalf("FindModelByPath(qwen3.8:latest) = ошибка %v, ожидался файл", err)
	}
	if filepath.Base(path) != "Qwen3.8-27B-UD-Q4_K_M.gguf" {
		t.Errorf("резолв в %q, want Qwen3.8-27B-UD-Q4_K_M.gguf", path)
	}

	// Варианты без тега и с расширением — тоже должны работать.
	for _, name := range []string{"qwen3.8", "qwen3.8.gguf", "QWEN3.8:LATEST", "Qwen3.8-27B-UD-Q4_K_M"} {
		if _, err := mm.FindModelByPath(name); err != nil {
			t.Errorf("FindModelByPath(%q) = %v, ожидалось попадание", name, err)
		}
	}

	// Каноническое имя нужно, чтобы показать клиенту правильный ID.
	found, canonical, ok := mm.FindModelByVariants("qwen3.8:latest")
	if !ok {
		t.Fatal("FindModelByVariants не нашёл модель")
	}
	if canonical != "Qwen3.8-27B-UD-Q4_K_M.gguf" {
		t.Errorf("canonical = %q, want Qwen3.8-27B-UD-Q4_K_M.gguf", canonical)
	}
	if filepath.Base(found) != canonical {
		t.Errorf("путь %q не соответствует canonical %q", found, canonical)
	}
}

// TestR83_FindModelByPath_StillFailsForUnknown — нормализация не должна
// «находить» что угодно: неизвестное имя обязано давать ошибку, чтобы
// вызывающий отдал 404 со списком, а не грузил случайную модель.
func TestR83_FindModelByPath_StillFailsForUnknown(t *testing.T) {
	dir := t.TempDir()
	writeGGUFFile(t, dir, "Qwen3.8-27B-UD-Q4_K_M.gguf", 4096)

	mm := NewModelManager(dir, Config{ModelsDir: dir})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	for _, name := range []string{"qwen3.5:latest", "nosuchmodel", "llama-3-8b:latest"} {
		if _, err := mm.FindModelByPath(name); err == nil {
			t.Errorf("FindModelByPath(%q) нашёл модель — ложное срабатывание", name)
		}
	}
}

// TestR83_AliasSourcePath_WithTag — алиас, созданный через POST /api/create под
// коротким именем, должен резолвиться и когда клиент присылает его с тегом.
func TestR83_AliasSourcePath_WithTag(t *testing.T) {
	dir := t.TempDir()
	writeGGUFFile(t, dir, "Qwen3.8-27B-UD-Q4_K_M.gguf", 4096)
	writeAlias(t, dir, "qwen3.8", "Qwen3.8-27B-UD-Q4_K_M.gguf")

	mm := NewModelManager(dir, Config{ModelsDir: dir})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	for _, name := range []string{"qwen3.8", "qwen3.8:latest"} {
		src, ok := mm.AliasSourcePath(name)
		if !ok {
			t.Errorf("AliasSourcePath(%q) не разрезолвил алиас", name)
			continue
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("AliasSourcePath(%q) вернул несуществующий путь %q: %v", name, src, err)
		}
	}
}
