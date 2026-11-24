// space_test.go — проверка свободного места: арифметика и настоящий том.
//
// ЗАЧЕМ ТЕСТЫ НА ЦИФРЫ. Отказ по месту — единственная ошибка ре-экспорта, которую
// оператор читает ЧИСЛАМИ: «нужно 33.0 ГиБ, свободно 12.0 ГиБ». Если слагаемые
// перепутаны (вход посчитан дважды или запас потерян), отказ становится ложным:
// задача «не влезает» там, где влезает. Поэтому проверяются и слагаемые, и то,
// что свободное место на настоящем томе действительно ИЗМЕРЯЕТСЯ (Statfs на Unix,
// GetDiskFreeSpaceEx на Windows), а не возвращается нулём.
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNeedForTaskCountsInputOutputAndReserve(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "inkling.gguf")
	const size = 4 << 20 // 4 МиБ: размер файла в тесте, а не 16 ГиБ
	if err := os.WriteFile(src, make([]byte, size), 0o644); err != nil {
		t.Fatalf("вход: %v", err)
	}

	// normalize: результат — копия данных входа, отдельного выхода нет.
	need, err := needForTask("inkling2_loop", src, filepath.Join(dir, "out.gguf"), jobOptions{})
	if err != nil {
		t.Fatalf("needForTask(normalize): %v", err)
	}
	if need.Input != size {
		t.Fatalf("вход %d, ожидался %d", need.Input, size)
	}
	if need.Output != 0 {
		t.Fatalf("normalize не пишет отдельный выход, а насчитал %d", need.Output)
	}
	if want := int64(size) + spaceReserveBytes; need.Need != want {
		t.Fatalf("нужно %d, ожидалось «вход + запас» = %d", need.Need, want)
	}

	// convert: вход + выход + запас (выход ≈ вход: конвертер пишет те же веса).
	need, err = needForTask("hf_to_gguf", src, filepath.Join(dir, "out.gguf"), jobOptions{})
	if err != nil {
		t.Fatalf("needForTask(convert): %v", err)
	}
	if need.Output != need.Input {
		t.Fatalf("convert: выход %d, ожидалась оценка «выход ≈ вход» %d", need.Output, need.Input)
	}
	if want := 2*int64(size) + spaceReserveBytes; need.Need != want {
		t.Fatalf("нужно %d, ожидалось «вход + выход + запас» = %d", need.Need, want)
	}

	// dry-run ничего не пишет — и места не требует.
	need, err = needForTask("inkling2_loop", src, filepath.Join(dir, "out.gguf"), jobOptions{DryRun: true})
	if err != nil {
		t.Fatalf("needForTask(dry-run): %v", err)
	}
	if need.Need != 0 {
		t.Fatalf("dry-run требует %d байт, а не пишет ничего", need.Need)
	}
}

func TestInputBytesSumsCheckpointDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct {
		name string
		size int
	}{{"model-00001.safetensors", 1000}, {"model-00002.safetensors", 2000}, {"config.json", 40}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), make([]byte, f.size), 0o644); err != nil {
			t.Fatalf("файл %s: %v", f.name, err)
		}
	}
	// Подкаталог тоже входит: у чекпойнтов бывают вложенные части.
	sub := filepath.Join(dir, "extra")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("подкаталог: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "part.bin"), make([]byte, 500), 0o644); err != nil {
		t.Fatalf("часть: %v", err)
	}
	got, err := inputBytes(dir)
	if err != nil {
		t.Fatalf("inputBytes: %v", err)
	}
	if want := int64(1000 + 2000 + 40 + 500); got != want {
		t.Fatalf("объём каталога %d, ожидался %d", got, want)
	}
}

func TestFreeSpaceBytesIsMeasuredOnRealVolume(t *testing.T) {
	dir := t.TempDir()
	free, err := freeSpaceBytes(dir)
	if err != nil {
		t.Fatalf("freeSpaceBytes(%s): %v", dir, err)
	}
	if free <= 0 {
		t.Fatalf("свободное место %d байт: метрика тома не измерена", free)
	}
	// Тот же том и для ещё не созданного каталога: путь результата может не
	// существовать до старта задачи, и это не должно ронять проверку.
	missing := filepath.Join(dir, "no-such-dir", "deeper")
	free2, err := freeSpaceBytes(missing)
	if err != nil {
		t.Fatalf("freeSpaceBytes(%s): %v", missing, err)
	}
	if free2 != free {
		t.Fatalf("место в несуществующем каталоге %d != место тома %d", free2, free)
	}
}

func TestNearestExistingDirWalksUp(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "a", "b", "c")
	if got := nearestExistingDir(missing); got != filepath.Clean(dir) {
		t.Fatalf("ближайший существующий каталог %q, ожидался %q", got, filepath.Clean(dir))
	}
}

func TestFormatGiB(t *testing.T) {
	cases := map[int64]string{
		1 << 30:        "1.0 ГиБ",
		3 << 29:        "1.5 ГиБ",
		15 * (1 << 30): "15.0 ГиБ",
		33 << 30:       "33.0 ГиБ",
		100 << 20:      "0.1 ГиБ",
	}
	for bytes, want := range cases {
		if got := formatGiB(bytes); got != want {
			t.Fatalf("formatGiB(%d) = %q, ожидалось %q", bytes, got, want)
		}
	}
}
