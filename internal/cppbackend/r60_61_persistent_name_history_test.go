package cppbackend

// ============================================================
// R60.61 (2026-09-14): tests for persistent nameHistory + SingleGGUFPath fallback
// ============================================================
//
// Background: OpenWebUI кэширует имена моделей в своей локальной БД. Если
// на диске файл переименован (например "qwen3-instruct.gguf" →
// "Qwen3-Instruct-2507-q4km.gguf"), OpenWebUI продолжает слать старое имя
// до ручной очистки. cppworker должен терпеть это:
//   1. nameHistory переживает рестарт (persisted в файл)
//   2. SingleGGUFPath() помогает когда modelsDir содержит только один .gguf
//   3. auto-pick fallback в lazyload.go использует SingleGGUFPath

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestR60_61_SingleGGUFPath_OneFile — если в modelsDir ровно один .gguf,
// SingleGGUFPath возвращает его путь + true.
func TestR60_61_SingleGGUFPath_OneFile(t *testing.T) {
	dir := t.TempDir()
	mustWriteGGUFDummy(t, dir, "Qwen3-Instruct-2507-q4km.gguf", 100)

	m := NewModelManager(dir, Config{})
	if _, err := m.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	path, ok := m.SingleGGUFPath()
	if !ok {
		t.Fatalf("SingleGGUFPath: expected ok=true with one gguf file")
	}
	want := filepath.Join(dir, "Qwen3-Instruct-2507-q4km.gguf")
	if path != want {
		t.Errorf("SingleGGUFPath: got %q, want %q", path, want)
	}
}

// TestR60_61_SingleGGUFPath_TwoFiles — два файла → неоднозначно → false.
func TestR60_61_SingleGGUFPath_TwoFiles(t *testing.T) {
	dir := t.TempDir()
	mustWriteGGUFDummy(t, dir, "model-a.gguf", 100)
	mustWriteGGUFDummy(t, dir, "model-b.gguf", 100)

	m := NewModelManager(dir, Config{})
	if _, err := m.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	if _, ok := m.SingleGGUFPath(); ok {
		t.Errorf("SingleGGUFPath: expected ok=false with two gguf files")
	}
}

// TestR60_61_SingleGGUFPath_Empty — пустая директория → false.
func TestR60_61_SingleGGUFPath_Empty(t *testing.T) {
	dir := t.TempDir()

	m := NewModelManager(dir, Config{})
	if _, err := m.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	if _, ok := m.SingleGGUFPath(); ok {
		t.Errorf("SingleGGUFPath: expected ok=false on empty dir")
	}
}

// TestR60_61_PersistentNameHistory — RecordModelLoad + перезапуск NewModelManager
// восстанавливает nameHistory из файла.
func TestR60_61_PersistentNameHistory(t *testing.T) {
	dir := t.TempDir()
	mustWriteGGUFDummy(t, dir, "Qwen3-Instruct-2507-q4km.gguf", 100)

	// First instance: RecordModelLoad записывает в файл.
	m1 := NewModelManager(dir, Config{})
	if _, err := m1.ScanModels(); err != nil {
		t.Fatalf("ScanModels m1: %v", err)
	}
	m1.RecordModelLoad("qwen3-instruct", filepath.Join(dir, "Qwen3-Instruct-2507-q4km.gguf"))

	// Wait for async save goroutine.
	waitForFile(t, filepath.Join(dir, ".name_history.json"))

	// Second instance: NewModelManager должен восстановить.
	m2 := NewModelManager(dir, Config{})
	if _, err := m2.ScanModels(); err != nil {
		t.Fatalf("ScanModels m2: %v", err)
	}

	got, ok := m2.LookupNameHistory("qwen3-instruct")
	if !ok {
		t.Fatalf("LookupNameHistory qwen3-instruct: not found after restart")
	}
	want := filepath.Join(dir, "Qwen3-Instruct-2507-q4km.gguf")
	if got != want {
		t.Errorf("LookupNameHistory: got %q, want %q", got, want)
	}
}

// TestR60_61_PersistentNameHistory_MissingFile — если файла нет
// (первый запуск), NewModelManager не падает, LookupNameHistory возвращает false.
func TestR60_61_PersistentNameHistory_MissingFile(t *testing.T) {
	dir := t.TempDir()
	mustWriteGGUFDummy(t, dir, "model.gguf", 100)

	m := NewModelManager(dir, Config{})
	if _, err := m.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	if _, ok := m.LookupNameHistory("anything"); ok {
		t.Errorf("LookupNameHistory: expected false on fresh start (no persisted file)")
	}
}

// --- helpers ---

func mustWriteGGUFDummy(t *testing.T, dir, name string, sizeBytes int) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if sizeBytes > 0 {
		if _, err := f.Write(make([]byte, sizeBytes)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		// poll every 10ms, max 500ms
		<-time.After(10 * 1_000_000) // 10ms
	}
	t.Fatalf("waitForFile: %s not created in 500ms", path)
}
