package balancer

// R97 (S7 плана 2026-10-07): наблюдаемость миграции operatingMode → политика.
//
// Сама миграция живёт в internal/config (чистая функция + хук в config.Load).
// Здесь проверяется только то, что балансер отдаёт её заметки наружу: без этого
// оператор видит «fallback=single» и правила, происхождение которых не объяснено.

import (
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestModeMigrationNotes_ExposedFromConfig_R97(t *testing.T) {
	cfg := createTestConfig()
	cfg.Balancing.OperatingMode = "virtual_router"
	cfg.Balancing.Placement = types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackSingle,
		Models:   []types.PlacementModelRule{{Model: "vm", Strategy: string(types.PlacementPool), Pool: []string{"a:1"}}},
	}
	cfg.ModeMigrationNotes = []string{
		"operatingMode=virtual_router → placement: 1 правил strategy=pool",
		"placement.fallback=single выставлен автоматически",
	}
	p := newProxyWithCleanup(t, cfg)

	notes := p.ModeMigrationNotes()
	if len(notes) != 2 {
		t.Fatalf("заметок = %d, ожидалось 2: %v", len(notes), notes)
	}
	if !strings.Contains(strings.Join(notes, " "), "fallback=single") {
		t.Errorf("оператор должен видеть причину fallback=single: %v", notes)
	}
	// Режим остаётся читаемым (обратная совместимость §9), а политика — рабочей.
	if p.OperatingMode() != "virtual_router" {
		t.Errorf("OperatingMode = %q", p.OperatingMode())
	}
	if !p.PlacementSettings().Enabled {
		t.Error("политика из миграции должна быть активна")
	}
}

func TestModeMigrationNotes_EmptyWhenNoMigration_R97(t *testing.T) {
	p := newProxyWithCleanup(t, createTestConfig())
	if notes := p.ModeMigrationNotes(); len(notes) != 0 {
		t.Errorf("без миграции заметок быть не должно: %v", notes)
	}
}

func TestModeMigrationNotes_NilProxy_R97(t *testing.T) {
	var p *Proxy
	if notes := p.ModeMigrationNotes(); notes != nil {
		t.Errorf("nil-прокси не паникует и не выдумывает заметки: %v", notes)
	}
}
