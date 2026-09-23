// alias_scan_r66d_test.go — R66d (2026-09-23).
//
// РЕГРЕСС. POST /api/create (Ollama-совместимое создание модели) пишет
// <name>.gguf.json с полем source, но ModelManager эти файлы не сканировал:
//   - ListModels()/ListAliases() алиас не видели → /api/tags клиенту его не
//     показывал («создали модель — её нет в выборе»);
//   - resolveModelPath() не умел резолвить алиас в .gguf → загрузка падала
//     с «failed to load model from models/<alias>.gguf» (живой кейс
//     gemma-4-E4B-it-Q4_K_M).
//
// Тест проверяет скан алиасов, цепочки alias → alias → .gguf, циклы и битые
// алиасы (файл-источник удалён).

package cppbackend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeAlias(t *testing.T, dir, name, source string) {
	t.Helper()
	rec := map[string]interface{}{
		"name":       name,
		"source":     source,
		"created_at": "2026-09-23T06:00:00Z",
		"modelfile":  "FROM " + source,
	}
	raw, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatalf("marshal alias: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".gguf.json"), raw, 0o600); err != nil {
		t.Fatalf("write alias: %v", err)
	}
}

func writeGGUFFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600); err != nil {
		t.Fatalf("write gguf: %v", err)
	}
}

func TestScanModels_Aliases_R66d(t *testing.T) {
	dir := t.TempDir()
	writeGGUFFile(t, dir, "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf", 4096)
	// Обычный алиас на файл.
	writeAlias(t, dir, "gemma-4-E4B-it-Q4_K_M", "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf")
	// Цепочка: short → gemma-4-E4B-it-Q4_K_M → .gguf.
	writeAlias(t, dir, "short", "gemma-4-E4B-it-Q4_K_M.gguf")
	// Цикл: a → b → a (не должен вешать резолв).
	writeAlias(t, dir, "cycle-a", "cycle-b.gguf")
	writeAlias(t, dir, "cycle-b", "cycle-a.gguf")
	// Битый алиас: файла-источника нет.
	writeAlias(t, dir, "broken", "does-not-exist.gguf")

	mm := NewModelManager(dir, Config{ModelsDir: dir})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	aliases := mm.ListAliases()
	if len(aliases) != 5 {
		t.Fatalf("ListAliases = %d записей (%v), want 5", len(aliases), aliases)
	}
	byName := make(map[string]GGUFAlias, len(aliases))
	for _, a := range aliases {
		byName[a.Name] = a
	}

	// Прямой алиас резолвится в файл и знает размер.
	src, ok := mm.AliasSourcePath("gemma-4-E4B-it-Q4_K_M")
	if !ok {
		t.Fatalf("прямой алиас не резолвится: %+v", byName["gemma-4-E4B-it-Q4_K_M"])
	}
	if filepath.Base(src) != "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf" {
		t.Errorf("прямой алиас резолвится в %q, want ...qat-UD-Q4_K_XL.gguf", src)
	}
	if a := byName["gemma-4-E4B-it-Q4_K_M"]; !a.SourceExists || a.SourceSizeBytes != 4096 {
		t.Errorf("у прямого алиаса SourceExists=%v size=%d, want true/4096", a.SourceExists, a.SourceSizeBytes)
	}

	// Цепочка резолвится до реального .gguf (а не до промежуточного алиаса).
	chainSrc, chainOK := mm.AliasSourcePath("short")
	if !chainOK || filepath.Base(chainSrc) != "gemma-4-E4B-it-qat-UD-Q4_K_XL.gguf" {
		t.Errorf("цепочка алиасов резолвится в %q ok=%v, want ...qat-UD-Q4_K_XL.gguf/true", chainSrc, chainOK)
	}

	// Цикл: без зависания, резолв не удался.
	if p, ok := mm.AliasSourcePath("cycle-a"); ok {
		t.Errorf("цикл не должен резолвиться, получили %q ok=%v", p, ok)
	}

	// Битый алиас: путь есть (для логов), ok=false.
	p, ok := mm.AliasSourcePath("broken")
	if ok {
		t.Errorf("битый алиас не должен резолвиться, получили %q", p)
	}
	if !filepath.IsAbs(p) {
		t.Errorf("для битого алиаса ожидался абсолютный путь-источник, получили %q", p)
	}
	if byName["broken"].SourceExists {
		t.Error("у битого алиаса SourceExists должен быть false (иначе клиент увидит незагружаемую модель)")
	}

	// Не-алиас имя: false.
	if _, ok := mm.AliasSourcePath("Qwen3-Instruct-2507-q4km"); ok {
		t.Error("обычная модель не должна считаться алиасом")
	}

	// AliasByName работает с .gguf и без.
	if a, ok := mm.AliasByName("short.gguf"); !ok || a.Name != "short" {
		t.Errorf("AliasByName(short.gguf) = %+v ok=%v, want запись short", a, ok)
	}
	// Имена алиасов не попадают в список файлов (это не .gguf).
	for _, f := range mm.ListModels() {
		if f.Filename == "short.gguf" {
			t.Error("алиас не должен попадать в ListModels")
		}
	}
}
