package config

// R97 (S7 плана 2026-10-07): миграция operatingMode → balancing.placement.
//
// Проверяем таблицу §9, безопасность конверта (fallback=single, а не «503 на
// апгрейде»), идемпотентность и то, что хук реально срабатывает на пути
// загрузки конфига (а не только при прямом вызове).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"ollama-loadbalancer/pkg/types"
)

func migrationConfigR97(mode string) *types.LoadBalancerConfig {
	return &types.LoadBalancerConfig{
		Balancing: types.BalancingSettings{OperatingMode: mode},
	}
}

func TestMigrateMode_StandardNeedsNoPolicy_R97(t *testing.T) {
	cfg := migrationConfigR97("standard")
	notes := MigrateOperatingModeToPlacement(cfg)

	assert.False(t, cfg.Balancing.Placement.Enabled,
		"standard — это single по умолчанию, включать политику нечего")
	assert.Empty(t, cfg.Balancing.Placement.Models)
	assert.Empty(t, cfg.Balancing.Placement.Classes)
	require.Len(t, notes, 1)
	// Оператор должен получить готовый сниппет для адаптивности.
	assert.Contains(t, notes[0], `strategy:"auto"`)
	assert.Contains(t, notes[0], "не просил")
	assert.Equal(t, "standard", cfg.Balancing.OperatingMode,
		"operatingMode не удаляем: обратная совместимость чтения (§9)")
}

func TestMigrateMode_VirtualRouterBuildsPoolRules_R97(t *testing.T) {
	cfg := migrationConfigR97("virtual_router")
	cfg.Balancing.VirtualModels = types.VirtualModelsConfig{
		Enabled: true,
		Models: []types.VirtualModelConfig{
			{
				Name: "prod-chat", ModelName: "llama-70b",
				BackendPool: []string{"a:18092", "b:18092"},
				Selection:   types.SelectionLeastLoaded,
			},
			// pipeline-режим (slices) политикой не выражается — пропускаем.
			{Name: "pipeline-vm", Slices: []types.ModelSliceConfig{{}}},
		},
	}

	notes := MigrateOperatingModeToPlacement(cfg)
	placement := cfg.Balancing.Placement
	assert.True(t, placement.Enabled)
	assert.Equal(t, types.PlacementFallbackSingle, placement.Fallback,
		"fallback=single: миграция не должна превращать обслуживание в 503")
	require.Len(t, placement.Models, 1, "только alias-on-pool модели")
	rule := placement.Models[0]
	assert.Equal(t, "prod-chat", rule.Model)
	assert.Equal(t, string(types.PlacementPool), rule.Strategy)
	assert.Equal(t, []string{"a:18092", "b:18092"}, rule.Pool)
	assert.Equal(t, string(types.SelectionLeastLoaded), rule.Selection)
	assert.Empty(t, placement.Classes)
	assert.Contains(t, strings.Join(notes, " | "), "strategy=pool")
	assert.Empty(t, placement.Validate(), "конверт обязан давать валидную политику")
}

func TestMigrateMode_VirtualRouterWithoutAliases_R97(t *testing.T) {
	cfg := migrationConfigR97("virtual_router")
	cfg.Balancing.VirtualModels = types.VirtualModelsConfig{
		Enabled: true,
		Models:  []types.VirtualModelConfig{{Name: "sliced", Slices: []types.ModelSliceConfig{{}}}},
	}

	notes := MigrateOperatingModeToPlacement(cfg)
	assert.False(t, cfg.Balancing.Placement.Enabled, "конвертировать нечего — политику не включаем")
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "конвертировать нечего")
}

func TestMigrateMode_ReplicationBuildsRules_R97(t *testing.T) {
	cfg := migrationConfigR97("replication")
	cfg.Balancing.ModelReplication = types.ModelReplicationConfig{
		Groups: []types.ModelGroupConfig{
			{ModelName: "llama3", MinInstances: 2, MaxInstances: 3},
			{ModelName: ""}, // мусорная группа — пропускаем
		},
	}

	notes := MigrateOperatingModeToPlacement(cfg)
	placement := cfg.Balancing.Placement
	assert.True(t, placement.Enabled)
	require.Len(t, placement.Models, 1)
	rule := placement.Models[0]
	assert.Equal(t, "llama3", rule.Model)
	assert.Equal(t, string(types.PlacementReplicated), rule.Strategy)
	require.NotNil(t, rule.Auto)
	assert.Equal(t, 2, rule.Auto.MinBackends, "minInstances группы → auto.minBackends")
	assert.Empty(t, placement.Validate())
	assert.Contains(t, strings.Join(notes, " | "), "strategy=replicated")
}

func TestMigrateMode_RpcCoordinatorToSharded_R97(t *testing.T) {
	cfg := migrationConfigR97("rpc_coordinator")

	notes := MigrateOperatingModeToPlacement(cfg)
	placement := cfg.Balancing.Placement
	assert.True(t, placement.Enabled)
	assert.Equal(t, types.PlacementFallbackSingle, placement.Fallback)
	require.Len(t, placement.Classes, 1, "глобальный режим выражается правилом класса")
	rule := placement.Classes[0]
	assert.Equal(t, "*", rule.Match.Model, "catch-all: правило на все модели")
	assert.Equal(t, string(types.PlacementSharded), rule.Strategy,
		"§9: rpc_coordinator → sharded (исполнитель раскладки)")
	assert.Empty(t, placement.Models)
	assert.Empty(t, placement.Validate(), "sharded — исполнимая стратегия, ошибок валидации быть не должно")

	joined := strings.Join(notes, " | ")
	assert.Contains(t, joined, "sharded")
	assert.Contains(t, joined, "backendGroups", "оператору нужно знать, что для раскладки нужны группы")
	assert.Contains(t, joined, "S1", "и что транспорт RPC ещё не собран")
	assert.Contains(t, joined, "fallback=single")
}

func TestMigrateMode_DistributedInferenceToSharded_R97(t *testing.T) {
	cfg := migrationConfigR97("distributed_inference")

	notes := MigrateOperatingModeToPlacement(cfg)
	require.Len(t, cfg.Balancing.Placement.Classes, 1)
	assert.Equal(t, string(types.PlacementSharded), cfg.Balancing.Placement.Classes[0].Strategy)
	joined := strings.Join(notes, " | ")
	assert.Contains(t, joined, "backendGroups")
	assert.Contains(t, joined, "/api/v1/placement/validate", "нужна подсказка, где проверить группу")
}

func TestMigrateMode_ExplicitPolicyWins_R97(t *testing.T) {
	cfg := migrationConfigR97("virtual_router")
	explicit := types.PlacementSettings{
		Enabled:  true,
		Fallback: types.PlacementFallbackError,
		Models: []types.PlacementModelRule{
			{Model: "only-this", Strategy: string(types.PlacementSingle)},
		},
	}
	cfg.Balancing.Placement = explicit
	cfg.Balancing.VirtualModels = types.VirtualModelsConfig{
		Enabled: true,
		Models:  []types.VirtualModelConfig{{Name: "vm", ModelName: "m", BackendPool: []string{"a:1"}}},
	}

	notes := MigrateOperatingModeToPlacement(cfg)
	assert.Equal(t, explicit, cfg.Balancing.Placement,
		"политика оператора не перезаписывается конвертом")
	assert.Contains(t, strings.Join(notes, " "), "уже настроена")
}

func TestMigrateMode_Idempotent_R97(t *testing.T) {
	cfg := migrationConfigR97("replication")
	cfg.Balancing.ModelReplication = types.ModelReplicationConfig{
		Groups: []types.ModelGroupConfig{{ModelName: "llama3", MinInstances: 2}},
	}

	first := MigrateOperatingModeToPlacement(cfg)
	require.NotEmpty(t, first)
	snapshot := cfg.Balancing.Placement

	second := MigrateOperatingModeToPlacement(cfg)
	assert.Equal(t, snapshot, cfg.Balancing.Placement, "повторный вызов ничего не меняет")
	require.Len(t, second, 1)
	assert.Contains(t, second[0], "уже настроена")
}

func TestMigrateMode_UnknownMode_R97(t *testing.T) {
	cfg := migrationConfigR97("spread")
	notes := MigrateOperatingModeToPlacement(cfg)
	assert.False(t, cfg.Balancing.Placement.Enabled)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "неизвестный режим")
}

func TestMigrateMode_EmptyModeIsStandard_R97(t *testing.T) {
	cfg := migrationConfigR97("")
	notes := MigrateOperatingModeToPlacement(cfg)
	assert.False(t, cfg.Balancing.Placement.Enabled)
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "standard")
}

func TestMigrateMode_NilConfig_R97(t *testing.T) {
	assert.Nil(t, MigrateOperatingModeToPlacement(nil))
}

// TestLoad_RunsModeMigration_R97 — ГЛАВНАЯ проверка хука: миграция должна
// срабатывать на реальном пути загрузки конфига, а не только при прямом вызове.
// (ValidateConfigOnLoad, куда она напрашивалась, в продакшене не вызывается.)
func TestLoad_RunsModeMigration_R97(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	data := []byte(`{
		"backends": [{"id": "b1", "host": "192.0.2.1", "ollamaPort": 11434}],
		"balancing": {
			"operatingMode": "virtual_router",
			"virtualModels": {
				"enabled": true,
				"models": [{"name": "vm1", "modelName": "m", "backendPool": ["a:1", "b:1"]}]
			}
		}
	}`)
	require.NoError(t, os.WriteFile(configPath, data, 0o644))

	loaded, err := Load(configPath)
	require.NoError(t, err)

	cfg := loaded.Get()
	assert.True(t, cfg.Balancing.Placement.Enabled,
		"миграция обязана выполняться в config.Load, иначе конверта не будет никогда")
	require.Len(t, cfg.Balancing.Placement.Models, 1)
	assert.Equal(t, "vm1", cfg.Balancing.Placement.Models[0].Model)
	assert.Equal(t, "virtual_router", cfg.Balancing.OperatingMode, "режим остаётся для чтения")
}

// TestLoad_MigrationDoesNotBreakStandardStand_R97 — самый частый конфиг (стандартный
// режим) после апгрейда не должен получить ни политики, ни новых отказов.
func TestLoad_MigrationDoesNotBreakStandardStand_R97(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"backends": [{"id": "b1", "host": "192.0.2.1", "ollamaPort": 11434}],
		"balancing": {"operatingMode": "standard"}
	}`), 0o644))

	loaded, err := Load(configPath)
	require.NoError(t, err)

	placement := loaded.Get().Balancing.Placement
	assert.False(t, placement.Enabled)
	assert.Empty(t, placement.Models)
	assert.Empty(t, placement.Classes)
	assert.Empty(t, placement.Validate())
}
