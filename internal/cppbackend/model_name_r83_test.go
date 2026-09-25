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
	"testing"
)

// Чистая логика нормализации (StripTag/Variants/Matches) тестируется в самом
// пакете pkg/modelname — здесь только интеграция с каталогом моделей, чтобы не
// дублировать те же таблицы в двух местах.

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
