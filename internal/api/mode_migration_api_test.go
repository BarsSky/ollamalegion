package api

// R97 (S7 плана 2026-10-07): заметки миграции режима видны в /api/v1/placement.
//
// §9 требует автоконверт с записью в лог, но по логу старта оператор не поймёт,
// откуда в его конфиге правила и почему fallback=single. Поэтому заметки отдаются
// там же, где он смотрит решения о размещении.

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func TestPlacementEndpoint_ModeMigrationNotes_R97(t *testing.T) {
	s, cleanup := setupTestServerWithBackends(t, []types.Backend{{
		ID: "b1", Host: "localhost", CppWorkerPort: 18092,
		Type: types.BackendTypeLlamaCpp, Status: types.StatusHealthy,
	}})
	defer cleanup()

	// s.config — тот же указатель, что и у прокси (NewServer(proxy, config, ...)),
	// поэтому заметки, выставленные загрузчиком конфига, видны через API.
	s.config.ModeMigrationNotes = []string{
		"operatingMode=virtual_router → placement: 1 правил strategy=pool по виртуальным алиасам",
		"placement.fallback=single выставлен автоматически",
	}

	resp := getPlacementJSON(t, s, "")
	notes, ok := resp["modeMigration"].([]interface{})
	if !ok {
		t.Fatalf("modeMigration отсутствует: %T (%v)", resp["modeMigration"], resp["modeMigration"])
	}
	if len(notes) != 2 {
		t.Fatalf("заметок = %d, ожидалось 2: %v", len(notes), notes)
	}
	first, _ := notes[0].(string)
	if first == "" {
		t.Errorf("заметка должна быть строкой: %v", notes[0])
	}

	// Без миграции ключа нет: ответ не засоряем.
	s.config.ModeMigrationNotes = nil
	resp = getPlacementJSON(t, s, "")
	if _, ok := resp["modeMigration"]; ok {
		t.Errorf("без миграции ключ появляться не должен: %v", resp["modeMigration"])
	}
}
