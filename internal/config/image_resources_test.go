// image_resources_test.go — хранилище переопределения политики сосуществования.
//
// ЧТО ФИКСИРУЕМ:
//  1. отсутствие файла = «переопределения нет» (действуют значения config.json),
//     а не ошибка;
//  2. сохранение атомарно и переживает повторное чтение (рестарт балансера);
//  3. невалидные значения ОТКЛОНЯЮТСЯ на входе — иначе опечатка в политике молча
//     превращалась бы в дефолт exclusive и оператор не понял бы, почему не
//     работает;
//  4. удаление возвращает значения config.json и не падает, если файла нет.
package config

import (
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestImageResourcesStore_AbsentFileMeansNoOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-resources.json")
	store := NewImageResourcesStore(path)
	if err := store.EnsureLoaded(); err != nil {
		t.Fatalf("отсутствие файла не должно быть ошибкой: %v", err)
	}
	if store.Present() {
		t.Fatal("Present() должен быть false, когда файла нет")
	}

	cfg := &types.LoadBalancerConfig{}
	cfg.Balancing.Image.Coexistence = types.ImageCoexistenceExclusive
	if ApplyImageResourcesOverride(cfg, store) {
		t.Fatal("без файла переопределять нечего")
	}
	if cfg.Balancing.Image.EffectiveCoexistencePolicy() != types.ImageCoexistenceExclusive {
		t.Fatalf("значение из config.json изменено: %v", cfg.Balancing.Image)
	}
}

func TestImageResourcesStore_SaveLoadRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-resources.json")
	store := NewImageResourcesStore(path)

	settings := types.ImageResourceSettings{
		Coexistence:                types.ImageCoexistenceOffload,
		VramHeadroomMB:             1024,
		BlockOnUnknownVRAMEstimate: true,
		QueueWaitTimeoutSec:        120,
		ExclusiveLockTimeoutSec:    900,
		GateDisabled:               false,
	}
	if err := store.Save(settings); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("файл не создан: %v", err)
	}

	// Повторное чтение = рестарт балансера.
	fresh := NewImageResourcesStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !fresh.Present() {
		t.Fatal("Present() должен быть true после сохранения")
	}
	got := fresh.Settings()
	if got.EffectiveCoexistencePolicy() != types.ImageCoexistenceOffload ||
		got.VramHeadroomMB != 1024 || !got.BlockOnUnknownVRAMEstimate ||
		got.QueueWaitTimeoutSec != 120 || got.ExclusiveLockTimeoutSec != 900 {
		t.Fatalf("значения не пережили запись/чтение: %+v", got)
	}

	// Применение к конфигу перекрывает config.json.
	cfg := &types.LoadBalancerConfig{}
	cfg.Balancing.Image.Coexistence = types.ImageCoexistenceExclusive
	if !ApplyImageResourcesOverride(cfg, fresh) {
		t.Fatal("переопределение должно примениться")
	}
	if cfg.Balancing.Image.Coexistence != types.ImageCoexistenceOffload {
		t.Fatalf("политика не применена: %v", cfg.Balancing.Image.Coexistence)
	}

	// Удаление возвращает «нет переопределения».
	if err := store.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if store.Present() {
		t.Fatal("после Remove переопределения быть не должно")
	}
	if err := store.Remove(); err != nil {
		t.Fatalf("повторный Remove не должен падать: %v", err)
	}
}

func TestValidateImageResourceSettings(t *testing.T) {
	ok := []types.ImageResourceSettings{
		{}, // пусто = дефолт exclusive
		{Coexistence: types.ImageCoexistenceExclusive},
		{Coexistence: types.ImageCoexistenceOffload, VramHeadroomMB: 512, QueueWaitTimeoutSec: 30, ExclusiveLockTimeoutSec: 600},
		{Coexistence: types.ImageCoexistenceDedicated, GateDisabled: true},
	}
	for _, s := range ok {
		if err := ValidateImageResourceSettings(s); err != nil {
			t.Errorf("валидные значения отклонены (%+v): %v", s, err)
		}
	}
	bad := []types.ImageResourceSettings{
		{Coexistence: "exlusive"}, // опечатка — не должна молча стать exclusive
		{Coexistence: "EXCLUSIVE"},
		{VramHeadroomMB: -1},
		{VramHeadroomMB: ImageResourceMaxHeadroomMB + 1},
		{QueueWaitTimeoutSec: -5},
		{QueueWaitTimeoutSec: ImageResourceMaxQueueWaitS + 1},
		{ExclusiveLockTimeoutSec: -1},
		{ExclusiveLockTimeoutSec: ImageResourceMaxLockFuseS + 1},
	}
	for _, s := range bad {
		if err := ValidateImageResourceSettings(s); err == nil {
			t.Errorf("невалидные значения приняты: %+v", s)
		}
	}
}

func TestImageResourcesStore_RejectsInvalidAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-resources.json")
	store := NewImageResourcesStore(path)
	if err := store.Save(types.ImageResourceSettings{Coexistence: types.ImageCoexistenceOffload}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Save(types.ImageResourceSettings{Coexistence: "nonsense"}); err == nil {
		t.Fatal("невалидная политика должна быть отклонена")
	}
	// Отклонённая запись не должна портить уже сохранённое значение.
	fresh := NewImageResourcesStore(path)
	if err := fresh.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if fresh.Settings().Coexistence != types.ImageCoexistenceOffload {
		t.Fatalf("сохранённое значение испорчено отклонённой записью: %+v", fresh.Settings())
	}
}

func TestResolveImageResourcesPath_EnvOverride(t *testing.T) {
	t.Setenv(EnvImageResourcesPath, "/tmp/custom-image-resources.json")
	if got := ResolveImageResourcesPath(); got != "/tmp/custom-image-resources.json" {
		t.Fatalf("env не учтён: %q", got)
	}
	t.Setenv(EnvImageResourcesPath, "")
	if got := ResolveImageResourcesPath(); got != DefaultImageResourcesPath {
		t.Fatalf("дефолт не применён: %q", got)
	}
}
