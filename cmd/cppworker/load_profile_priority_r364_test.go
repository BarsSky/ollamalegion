// load_profile_priority_r364_test.go — R364 (2026-10-31).
//
// Контракт, который проверяется: при загрузке модели
//
//	явное поле запроса  >  профиль модели  >  конфиг/env воркера
//
// и профиль модели применяется ВСЕГДА (R83/v62: ignoreDefaults гасит только
// применение дефолтного профиля, а не профиля модели).
//
// Живой дефект, из которого тест вырос: прямой POST /api/models/load
// {"name":"gemma-4-E4B-it-Q4_K_M"} поднял модель с ctx 65536 вместо 32768,
// kv q8_0 вместо q4_0 и reasoning=off — и выдал 2.9 tok/s против 25.9 у
// инстанса, загруженного балансером. Профиль на legacy-пути не применялся
// вовсе.
package main

import (
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func intPtrR364(v int) *int       { return &v }
func strPtrR364(v string) *string { return &v }
func boolPtrR364(v bool) *bool    { return &v }

// TestR364_PrecedenceRequestOverProfileOverDefault — три слоя приоритета.
func TestR364_PrecedenceRequestOverProfileOverDefault(t *testing.T) {
	defs := loadDefaultsR364{GPULayers: 20, ContextSize: 65536, BatchSize: 256, FlashAttnType: -1}
	seed := loadProfileSeedR364{
		ContextSize: 32768, BatchSize: 512, GPULayers: -1,
		FlashAttn: boolPtrR364(true), KVCacheType: "q4_0", Parallel: 2,
	}

	t.Run("явный запрос побеждает профиль и конфиг", func(t *testing.T) {
		req := loadModelRequest{
			ContextSize: intPtrR364(8192),
			BatchSize:   intPtrR364(128),
			GPULayers:   intPtrR364(10),
			Parallel:    intPtrR364(4),
			KVCacheType: strPtrR364("f16"),
		}
		got := resolveLoadParamsR364(req, seed, defs)
		if got.ContextSize != 8192 {
			t.Errorf("contextSize = %d, want 8192 (явный запрос обязан побеждать профиль)", got.ContextSize)
		}
		if got.BatchSize != 128 {
			t.Errorf("batchSize = %d, want 128", got.BatchSize)
		}
		if got.GPULayers != 10 {
			t.Errorf("gpuLayers = %d, want 10", got.GPULayers)
		}
		if got.Parallel != 4 {
			t.Errorf("parallel = %d, want 4", got.Parallel)
		}
		if got.KVCacheType != "f16" {
			t.Errorf("kvCacheType = %q, want f16", got.KVCacheType)
		}
	})

	t.Run("профиль побеждает конфиг, когда запрос молчит", func(t *testing.T) {
		// Это ровно живой случай: запрос — только name, конфиг контейнера задаёт
		// ctx 65536 / gpu_layers 20, профиль — 32768 / -1 / q4_0 / parallel 2.
		got := resolveLoadParamsR364(loadModelRequest{}, seed, defs)
		if got.ContextSize != 32768 {
			t.Errorf("contextSize = %d, want 32768 из профиля (дефект R364: было 65536 из конфига)", got.ContextSize)
		}
		if got.GPULayers != -1 {
			t.Errorf("gpuLayers = %d, want -1 (все слои) из профиля", got.GPULayers)
		}
		if got.BatchSize != 512 {
			t.Errorf("batchSize = %d, want 512 из профиля", got.BatchSize)
		}
		if got.KVCacheType != "q4_0" {
			t.Errorf("kvCacheType = %q, want q4_0 из профиля", got.KVCacheType)
		}
		if got.Parallel != 2 {
			t.Errorf("parallel = %d, want 2 из профиля", got.Parallel)
		}
		if got.FlashAttnType != 1 {
			t.Errorf("flashAttnType = %d, want 1 из профиля", got.FlashAttnType)
		}
	})

	t.Run("без профиля действует конфиг", func(t *testing.T) {
		got := resolveLoadParamsR364(loadModelRequest{}, loadProfileSeedR364{}, defs)
		if got.ContextSize != 65536 || got.GPULayers != 20 || got.BatchSize != 256 {
			t.Errorf("без профиля обязаны действовать конфиг/env: %+v", got)
		}
		if got.KVCacheType != "" {
			t.Errorf("kvCacheType = %q, want пусто (дефолт llama.cpp)", got.KVCacheType)
		}
		if got.Parallel != 0 {
			t.Errorf("parallel = %d, want 0 (наследовать CPPWORKER_N_PARALLEL)", got.Parallel)
		}
	})
}

// TestR364_ProfileParallelAndKvReachLoad — именно эти два поля терялись в
// исторических дефектах (R135: parallel; R83/v52: kvCacheType). Держим их
// отдельным тестом, чтобы регресс был виден по имени.
func TestR364_ProfileParallelAndKvReachLoad(t *testing.T) {
	seed := loadProfileSeedR364{Parallel: 2, KVCacheType: "q4_0"}
	got := resolveLoadParamsR364(loadModelRequest{}, seed, loadDefaultsR364{})
	if got.Parallel != 2 {
		t.Errorf("parallel из профиля не доехал до загрузки: %+v", got)
	}
	if got.KVCacheType != "q4_0" {
		t.Errorf("kvCacheType из профиля не доехал до загрузки: %+v", got)
	}
}

// TestR364_InvalidKvIsDroppedNotSubstituted — неподдержанный тип KV не
// подменяется значением из профиля: иначе API соврал бы о параметрах загрузки.
func TestR364_InvalidKvIsDroppedNotSubstituted(t *testing.T) {
	seed := loadProfileSeedR364{KVCacheType: "q4_0"}
	req := loadModelRequest{KVCacheType: strPtrR364("q2_xxl")}

	got := resolveLoadParamsR364(req, seed, loadDefaultsR364{})
	if !got.KVCacheInvalid {
		t.Fatal("неподдержанный kvCacheType обязан помечаться как invalid")
	}
	if got.KVCacheRaw != "q2_xxl" {
		t.Errorf("KVCacheRaw = %q, want q2_xxl (нужен для предупреждения)", got.KVCacheRaw)
	}
	if got.KVCacheType != "" {
		t.Errorf("kvCacheType = %q, want пусто (дефолт llama.cpp), а не подмена профилем", got.KVCacheType)
	}
}

// TestR364_ParallelNonPositiveMeansInherit — явные 0 и отрицательные значения
// parallel означают «наследовать CPPWORKER_N_PARALLEL», как и до R364: туда
// применялось только *req.Parallel > 0. Заодно проверяем, что явный 0 НЕ
// подменяется профилем (запрос всё-таки главнее).
func TestR364_ParallelNonPositiveMeansInherit(t *testing.T) {
	seed := loadProfileSeedR364{Parallel: 2}
	for _, explicit := range []int{0, -1} {
		got := resolveLoadParamsR364(loadModelRequest{Parallel: intPtrR364(explicit)}, seed, loadDefaultsR364{})
		if got.Parallel != 0 {
			t.Errorf("явный parallel=%d дал %d, want 0 (наследовать)", explicit, got.Parallel)
		}
	}
}

// flash attention на legacy-пути проверяется в profile_flash_attn_r366_test.go:
// с R366 правило одно на все пути (profileFlashAttnToLoad), и дублировать его
// здесь означало бы держать две копии контракта.

// TestR364_ProfileGPULayersZeroMeansInherit — 0 в профиле это «наследовать»,
// поэтому -1 (все слои) и -2 (авто) обязаны проходить как есть.
func TestR364_ProfileGPULayersZeroMeansInherit(t *testing.T) {
	for _, profileVal := range []int{-1, -2, 12} {
		got := resolveLoadParamsR364(loadModelRequest{}, loadProfileSeedR364{GPULayers: profileVal}, loadDefaultsR364{GPULayers: 20})
		if got.GPULayers != profileVal {
			t.Errorf("профильный gpuLayers=%d не применён: got %d", profileVal, got.GPULayers)
		}
	}
	got := resolveLoadParamsR364(loadModelRequest{}, loadProfileSeedR364{}, loadDefaultsR364{GPULayers: 20})
	if got.GPULayers != 20 {
		t.Errorf("gpuLayers = %d, want 20 (0 в профиле = наследовать конфиг)", got.GPULayers)
	}
}

// TestR364_ReadProfileSeedsAllFields — профиль из syncer-кэша обязан дать seed
// по всем полям, которые участвуют в загрузке, включая reasoning (его терял
// legacy-путь: профиль «включить», факт reasoning=false).
func TestR364_ReadProfileSeedsAllFields(t *testing.T) {
	oldSyncer := profileSyncer
	defer func() { profileSyncer = oldSyncer }()
	profileSyncer = &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{
		"gemma-4-E4B-it-Q4_K_M": {
			ContextLength:   32768,
			BatchSize:       512,
			NumGPULayers:    -1,
			KVCacheType:     "q4_0",
			Parallel:        2,
			FlashAttn:       boolPtrR364(true),
			EnableReasoning: boolPtrR364(true),
		},
	}}

	got := readProfileForLoadR364("gemma-4-E4B-it-Q4_K_M", "")
	if got == nil {
		t.Fatal("профиль модели не найден, хотя он есть в кэше syncer-а")
	}
	if got.Key != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("ключ профиля = %q", got.Key)
	}
	if got.Seed.ContextSize != 32768 || got.Seed.BatchSize != 512 || got.Seed.GPULayers != -1 ||
		got.Seed.KVCacheType != "q4_0" || got.Seed.Parallel != 2 {
		t.Fatalf("seed профиля неполный: %+v", got.Seed)
	}
	if got.Seed.FlashAttn == nil || !*got.Seed.FlashAttn {
		t.Errorf("flashAttn из профиля = %v, want true (seed хранит сырое значение, перевод — R366)", got.Seed.FlashAttn)
	}
	if got.Seed.Reasoning == nil || !*got.Seed.Reasoning {
		t.Error("reasoning из профиля потерян — legacy-путь грузил модель с размышлениями выключенными")
	}
}

// TestR364_ReadProfileAliasUsesFileOwner — профиль берётся у владельца файла
// (R154): загрузка под коротким алиасом не должна поднимать модель на чужом
// профиле.
func TestR364_ReadProfileAliasUsesFileOwner(t *testing.T) {
	oldSyncer := profileSyncer
	defer func() { profileSyncer = oldSyncer }()
	profileSyncer = &profileSyncerT{cache: map[string]types.LlamaCppModelProfile{
		"gemma-4-E4B-it-Q4_K_M": {ContextLength: 32768, KVCacheType: "q4_0"},
		"gemma-4":               {ContextLength: 8192, KVCacheType: "f16"},
	}}

	got := readProfileForLoadR364("gemma-4", "/app/models/gemma-4-E4B-it-Q4_K_M.gguf")
	if got == nil {
		t.Fatal("профиль владельца файла не найден")
	}
	if got.Key != "gemma-4-E4B-it-Q4_K_M" {
		t.Errorf("ключ профиля = %q, want владельца файла gemma-4-E4B-it-Q4_K_M", got.Key)
	}
	if got.Seed.ContextSize != 32768 {
		t.Errorf("ctx = %d, want 32768 (профиль владельца файла, а не алиаса)", got.Seed.ContextSize)
	}
}

// TestR364_NoSyncerKeepsLegacyBehaviour — без syncer-а (тесты, одиночный
// воркер) поведение обязано остаться прежним: чистая функция возвращает nil.
func TestR364_NoSyncerKeepsLegacyBehaviour(t *testing.T) {
	oldSyncer := profileSyncer
	defer func() { profileSyncer = oldSyncer }()
	profileSyncer = nil

	if got := readProfileForLoadR364("gemma-4-E4B-it-Q4_K_M", ""); got != nil {
		t.Fatalf("без syncer-а профиля быть не может, got %+v", got)
	}
}
