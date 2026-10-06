package sdbackend

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRegistry_RefreshIfChanged_DetectsNewBundle — ЖИВОЙ ДЕФЕКТ (2026-10-06):
// модель, появившаяся на диске ПОСЛЕ старта воркера (одиночная загрузка файла
// через HF), не показывалась в GET /api/image/models до перезапуска контейнера:
// реестр строился один раз, а перечитывал его только bundle-путь.
//
// RefreshIfChanged вызывается перед отдачей списка моделей и обязан:
//   - не сканировать диск, если ничего не изменилось (дешёвая проверка);
//   - перечитать реестр, когда каталог моделей изменился.
func TestRegistry_RefreshIfChanged_DetectsNewBundle(t *testing.T) {
	dir := t.TempDir()
	modelsDir := filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	reg := NewRegistry(modelsDir)
	if err := reg.Load(); err != nil {
		t.Fatalf("первый Load: %v", err)
	}
	if names := reg.Names(); len(names) != 0 {
		t.Fatalf("пустой каталог: names=%v", names)
	}

	// Ничего не менялось — повторная проверка не должна ничего перечитывать.
	if reg.RefreshIfChanged() {
		t.Fatal("без изменений на диске реестр перечитывать не нужно")
	}

	// Появился новый bundle (так его кладёт загрузка файла в воркер).
	bundle := filepath.Join(modelsDir, "new-model")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	file := filepath.Join(bundle, "new-model.gguf")
	if err := os.WriteFile(file, []byte("GGUF"), 0o644); err != nil {
		t.Fatalf("write gguf: %v", err)
	}

	if !reg.RefreshIfChanged() {
		t.Fatal("появившийся bundle обязан вызывать перечитывание реестра")
	}
	names := reg.Names()
	if len(names) != 1 || names[0] != "new-model" {
		t.Fatalf("новая модель не появилась в реестре без рестарта: names=%v", names)
	}
	// Профиль синтезируется по содержимому каталога (family=other) — модель
	// видна и пригодна к дальнейшей настройке.
	if p, ok := reg.Profile("new-model"); !ok || p.Name != "new-model" {
		t.Fatalf("профиль новой модели не найден: %+v ok=%v", p, ok)
	}

	// И снова: без изменений — без перечитывания.
	if reg.RefreshIfChanged() {
		t.Fatal("повторный вызов без изменений не должен перечитывать реестр")
	}
}
