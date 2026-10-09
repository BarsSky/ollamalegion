package cppbackend

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// R93 (2026-10-09): «почистить WebUI от моделей, что не лежат на диске, и
// отработать механизм, который проверяет наличие и фиксирует изменения без
// перезапуска контейнера».
//
// Живой случай на стенде: каталог моделей был bind-mount'ом
// (D:\ollama-legion-models -> /app/models), каталог удалили — а /api/models/files
// продолжал отдавать 3 файла, потому что список лежал в кэше ModelManager, а
// ScanModels вызывался только на старте и после import/share. Ошибка чтения
// каталога вообще оставляла старый кэш нетронутым.

// writeGGUF — создать файл-заглушку нужного размера.
func writeGGUF(t *testing.T, dir, name string, size int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestScanModels_DirGone_ClearsInventory — каталог исчез: инвентарь обязан стать
// пустым (WebUI не должен показывать модели, которых нет на диске).
func TestScanModels_DirGone_ClearsInventory(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "ghost-a.gguf", 32)
	writeGGUF(t, dir, "ghost-b.gguf", 64)

	mm := NewModelManager(dir, Config{})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("первый скан: %v", err)
	}
	if got := len(mm.ListModels()); got != 2 {
		t.Fatalf("после первого скана моделей %d, ожидалось 2", got)
	}

	// Каталог удалён (оператор почистил диск). ScanModels создаст его заново —
	// главное, что в инвентаре НЕ остаётся прежних записей.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("скан после удаления каталога: %v", err)
	}
	if got := len(mm.ListModels()); got != 0 {
		t.Fatalf("в инвентаре осталось %d моделей после удаления каталога — WebUI покажет призраков", got)
	}
	st := mm.InventoryStatus()
	if len(st.Removed) != 2 {
		t.Errorf("removed = %v, ожидались оба удалённых файла", st.Removed)
	}
	if st.ChangedAt.IsZero() {
		t.Error("changedAt не проставлен, хотя инвентарь изменился (2 файла исчезли)")
	}
	if st.Changes == 0 {
		t.Error("счётчик изменений не вырос")
	}
}

// TestScanModels_DirUnavailable_RecordsReason — каталог недоступен (удалённый
// bind-mount: mkdir отвечает «File exists», ReadDir — ENOENT). Инвентарь пуст, а
// причина видна в статусе, а не проглочена.
func TestScanModels_DirUnavailable_RecordsReason(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "models-as-file")
	writeGGUF(t, base, "models-as-file", 8) // путь существует, но это ФАЙЛ

	mm := NewModelManager(dir, Config{})
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels не должен возвращать ошибку на недоступном каталоге: %v", err)
	}
	if got := len(mm.ListModels()); got != 0 {
		t.Fatalf("моделей %d, ожидалось 0", got)
	}
	st := mm.InventoryStatus()
	if st.DirError == "" {
		t.Fatal("dirError пуст: оператор не узнает, почему список пуст")
	}
	if st.DirErrorAt.IsZero() {
		t.Error("dirErrorAt не проставлен")
	}
}

// TestDiffInventory — что именно изменилось: добавлено, удалено, изменился размер.
func TestDiffInventory(t *testing.T) {
	oldFiles := map[string]*GGUFModelMeta{
		"keep.gguf":  {Filename: "keep.gguf", SizeBytes: 100},
		"gone.gguf":  {Filename: "gone.gguf", SizeBytes: 200},
		"grown.gguf": {Filename: "grown.gguf", SizeBytes: 300},
	}
	newFiles := map[string]*GGUFModelMeta{
		"keep.gguf":  {Filename: "keep.gguf", SizeBytes: 100},
		"grown.gguf": {Filename: "grown.gguf", SizeBytes: 999},
		"new.gguf":   {Filename: "new.gguf", SizeBytes: 50},
	}
	added, removed, resized := diffInventory(oldFiles, newFiles)
	if len(added) != 1 || added[0] != "new.gguf" {
		t.Errorf("added = %v, ожидался [new.gguf]", added)
	}
	if len(removed) != 1 || removed[0] != "gone.gguf" {
		t.Errorf("removed = %v, ожидался [gone.gguf]", removed)
	}
	if len(resized) != 1 || resized[0] != "grown.gguf" {
		t.Errorf("resized = %v, ожидался [grown.gguf]", resized)
	}
}

// TestScanModelsIfStale — механизм «проверять наличие без перезапуска контейнера»:
// по TTL скан повторяется, но не чаще TTL; 0 = выключено.
func TestScanModelsIfStale(t *testing.T) {
	dir := t.TempDir()
	writeGGUF(t, dir, "first.gguf", 16)

	mm := NewModelManager(dir, Config{})
	mm.SetRescanTTL(60 * time.Millisecond)
	if _, err := mm.ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	// TTL ещё не истёк — повторного скана нет.
	if mm.ScanModelsIfStale() {
		t.Fatal("скан выполнен раньше TTL")
	}

	// Файл появился на диске (scp/docker cp/WebUI) — после TTL он обязан всплыть
	// БЕЗ перезапуска контейнера.
	writeGGUF(t, dir, "second.gguf", 32)
	time.Sleep(90 * time.Millisecond)
	if !mm.ScanModelsIfStale() {
		t.Fatal("скан не выполнен после истечения TTL")
	}
	if got := len(mm.ListModels()); got != 2 {
		t.Fatalf("моделей %d, ожидалось 2 — новый файл не подхватился", got)
	}
	st := mm.InventoryStatus()
	if len(st.Added) != 1 || st.Added[0] != "second.gguf" {
		t.Errorf("added = %v, ожидался [second.gguf]", st.Added)
	}
	if st.RescanTTLSec != 0 && st.RescanTTLSec < 1 {
		t.Errorf("rescanTtlSec = %d", st.RescanTTLSec)
	}

	// Выключенная фоновая проверка (0) не сканирует вообще.
	mm.SetRescanTTL(0)
	writeGGUF(t, dir, "third.gguf", 48)
	if mm.ScanModelsIfStale() {
		t.Fatal("при rescanTTL=0 скан выполняться не должен")
	}
}
