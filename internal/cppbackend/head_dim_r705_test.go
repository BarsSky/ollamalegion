//go:build llama_stub

// internal/cppbackend/head_dim_r705_test.go — R705 (2026-11-24).
//
// ПРЕДМЕТ. R704 научил ComputeFeasible брать <arch>.attention.key_length из
// GGUF-ЗАГОЛОВКА, но путь «модель загружена» (loaded_info) по-прежнему брал
// ширину головы у C-моста, а мост считал её как n_embd/n_heads
// (c/bridge/bridge.c, bridge_get_model_metadata). Для реальных моделей стенда это
// ЗАНИЖАЛО KV на токен:
//
//	Qwen3-4B:      2560/32 = 80  при key_length = 128 → KV на 60 % меньше;
//	gemma-4-E4B:   2560/8  = 320 при key_length = 512 → KV на 60 % меньше;
//	Inkling-2-Mini: 2304/24 = 96  при key_length = 128 → KV на 33.3 % меньше;
//	Qwen3.8-9B:    4096/16 = 256 = key_length = 256 → числа НЕ меняются.
//
// ТЕСТЫ НИЖЕ проверяют:
//  1. loaded_info берёт ширину из модели (128/512) — KV считается от неё;
//  2. вместе с числом публикуется ИСТОЧНИК (head_dim_source): gguf или computed;
//  3. старый мост (источник не сообщён) читается как computed, число остаётся
//     рабочим — обратная совместимость;
//  4. для модели, где key_length == n_embd/n_heads (Qwen3.8-9B), числа прежние;
//  5. путь заголовка файла тоже маркирует источник.
//
// НЕ-ВАКУУМНОСТЬ. Ожидаемые KV/токен посчитаны ВРУЧНУЮ по формуле
// (head_dim_k + head_dim_v) × n_layers × n_kv_heads × 2, а рядом проверяется, что
// значение ОТЛИЧАЕТСЯ от прежней формулы n_embd/n_heads — поэтому тест краснеет на
// состоянии до правки (Qwen3-4B: 92 160 вместо 147 456).

package cppbackend

import (
	"testing"

	"ollama-loadbalancer/c/bridge"
)

// injectLoadedInfoR705 — положить «загруженную» модель с заданным ModelInfo
// (без C-моста: handle == nil).
func injectLoadedInfoR705(t *testing.T, b *Backend, info ModelInfo) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.models[info.Name] = &modelInstance{info: info}
}

// loadedInfoBackendR705 — backend с одним «загруженным» инстансом.
func loadedInfoBackendR705(t *testing.T, info ModelInfo, kvType string) *Backend {
	t.Helper()
	b := NewBackend(Config{
		ModelsDir:          t.TempDir(),
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: kvType,
	})
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	injectLoadedInfoR705(t, b, info)
	return b
}

// TestR705_LoadedInfoUsesRealHeadDim — Qwen3-4B: мост отдаёт 128 (key_length),
// раньше отдавал 80 (n_embd/n_heads).
//
// Ожидание f16: (128 + 128) × 36 × 8 × 2 = 147 456 Б/токен.
// Прежняя формула дала бы 2 × 36 × 8 × 80 × 2 = 92 160 — на 60 % меньше.
func TestR705_LoadedInfoUsesRealHeadDim(t *testing.T) {
	const modelName = "Qwen3-4B-Instruct-2507-Q4_K_M"
	b := loadedInfoBackendR705(t, ModelInfo{
		Name:              modelName,
		State:             StateLoaded,
		Architecture:      "qwen3",
		NLayers:           36,
		NHeads:            32,
		NKvHeads:          8,
		HeadDimK:          128, // llama_hparams.n_embd_head_k_full (attention.key_length)
		HeadDimV:          128,
		HeadDimSource:     bridge.HeadDimSourceGGUF,
		NEmbd:             2560,
		GGUFContextLength: 262144,
	}, "f16")

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}

	const wantKV = (128 + 128) * 36 * 8 * 2 // 147456
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (head_dim из модели 128, а не n_embd/n_heads=80)",
			info.KVPerToken, wantKV)
	}
	// НЕ-ВАКУУМНОСТЬ: значение обязано отличаться от прежней формулы.
	const oldKV = 2 * 36 * 8 * (2560 / 32) * 2 // 92160
	if info.KVPerToken == oldKV {
		t.Fatalf("KVPerToken = %d — ширина всё ещё считается как n_embd/n_heads", oldKV)
	}
	if info.HeadDimK != 128 || info.HeadDimV != 128 {
		t.Errorf("опубликованная ширина = %d/%d, want 128/128 (это то, что видит /api/models и балансер)",
			info.HeadDimK, info.HeadDimV)
	}
	if info.HeadDimSource != bridge.HeadDimSourceGGUF {
		t.Errorf("HeadDimSource = %q, want %q — число из модели обязано быть помечено как факт",
			info.HeadDimSource, bridge.HeadDimSourceGGUF)
	}

	// q8_0/q4_0 — ТОЧНЫЕ коэффициенты (34/32, 18/32 от memfit.KVBytesPerElement,
	// R707), но от НОВОЙ ширины: прежние «/2» и «/4» занижали KV на 6.25 %/12.5 %.
	const elems = 2 * 36 * 8 * 128
	for _, tc := range []struct {
		kvType string
		want   int
	}{{"q8_0", elems * 34 / 32}, {"q4_0", elems * 18 / 32}} {
		got := loadedInfoBackendR705(t, ModelInfo{
			Name: modelName, State: StateLoaded, Architecture: "qwen3",
			NLayers: 36, NHeads: 32, NKvHeads: 8,
			HeadDimK: 128, HeadDimV: 128, HeadDimSource: bridge.HeadDimSourceGGUF,
			NEmbd: 2560, GGUFContextLength: 262144,
		}, tc.kvType)
		fi, err := got.ComputeFeasible(modelName)
		if err != nil {
			t.Fatalf("%s: ComputeFeasible: %v", tc.kvType, err)
		}
		if fi.KVPerToken != tc.want {
			t.Errorf("%s: KVPerToken = %d, want %d", tc.kvType, fi.KVPerToken, tc.want)
		}
	}

	// Рекомендованное окно считается от НОВОГО KV: оно обязано быть строго меньше,
	// чем было бы по заниженной ширине при том же свободном месте.
	if info.MaxRAMCtx > 0 {
		oldRamCtx := int(info.FreeRAMMB*1024*1024) / oldKV
		if info.MaxRAMCtx >= oldRamCtx && oldRamCtx > 0 {
			t.Errorf("MaxRAMCtx = %d не меньше прежнего %d — окно всё ещё считается по ширине 80",
				info.MaxRAMCtx, oldRamCtx)
		}
	}
}

// TestR705_Gemma4HeadDimFromModel — gemma-4-E4B: key_length = 512 при
// n_embd/n_heads = 320.
//
// Ожидание q4_0 (R707: точный коэффициент 18/32 от memfit.KVBytesPerElement):
// элементов (2 × 42 × 2 × 512 = 86 016) × 18/32 = 48 384 Б/токен.
// Прежняя формула по ширине 320 дала бы 2 × 42 × 2 × 320 × 18/32 = 30 240.
//
// Здесь метаданных файла нет (каталог пуст), поэтому классы слоёв неизвестны:
// план вырождается в плоский («все 42 слоя шириной key_length»), и тест
// проверяет именно ШИРИНУ — послойный расчёт gemma-4 проверяет R83/R707-тест с
// реальными ключами shared_kv_layers/sliding_window_pattern.
func TestR705_Gemma4HeadDimFromModel(t *testing.T) {
	const modelName = "gemma-4-E4B-it-Q4_K_M"
	b := loadedInfoBackendR705(t, ModelInfo{
		Name:              modelName,
		State:             StateLoaded,
		Architecture:      "gemma4",
		NLayers:           42,
		NHeads:            8,
		NKvHeads:          2,
		HeadDimK:          512,
		HeadDimV:          512,
		HeadDimSource:     bridge.HeadDimSourceGGUF,
		NEmbd:             2560,
		GGUFContextLength: 131072,
	}, "q4_0")

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	const elems = 2 * 42 * 2 * 512
	const wantKVQ4 = elems * 18 / 32 // 48384
	if info.KVPerToken != wantKVQ4 {
		t.Fatalf("KVPerToken(q4_0) = %d, want %d (key_length=512, а не 320)",
			info.KVPerToken, wantKVQ4)
	}
	const oldKVQ4 = 2 * 42 * 2 * (2560 / 8) * 18 / 32 // 30240
	if info.KVPerToken == oldKVQ4 {
		t.Fatalf("KVPerToken = %d — gemma-4 всё ещё считается по n_embd/n_heads=320", oldKVQ4)
	}
	if info.HeadDimK != 512 {
		t.Errorf("HeadDimK = %d, want 512", info.HeadDimK)
	}
	if info.KVPlanSource != KVPlanSourceFlat {
		t.Errorf("KVPlanSource = %q, want %q — без метаданных файла классами слоёв не владеем",
			info.KVPlanSource, KVPlanSourceFlat)
	}
}

// TestR705_Qwen38Unchanged — Qwen3.8-9B: key_length = 256 = n_embd/n_heads.
//
// ШИРИНА обязана остаться прежней (256), и это здесь проверяется явно:
// per-layer вклад = (256 + 256) × 4 × 2 = 4 096 Б/токен (f16).
//
// R707: ЧИСЛО KV на токен при этом уменьшилось — и это правильно. qwen35 —
// гибридная архитектура: KV держат только attention-слои (каждый 4-й), то есть
// 8 из 32 слоёв, а не все 32. Поэтому 32 768, а не 131 072; на ширину это не
// влияет, и «совпадение ширины с формулой» проверяется отдельным полем.
func TestR705_Qwen38Unchanged(t *testing.T) {
	const modelName = "Qwen3.8-9B-Distill-uncensored-heretic.Q4_K_M"
	b := loadedInfoBackendR705(t, ModelInfo{
		Name:              modelName,
		State:             StateLoaded,
		Architecture:      "qwen35",
		NLayers:           32,
		NHeads:            16,
		NKvHeads:          4,
		HeadDimK:          256,
		HeadDimV:          256,
		HeadDimSource:     bridge.HeadDimSourceGGUF,
		NEmbd:             4096,
		GGUFContextLength: 262144,
	}, "f16")

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	// Гибридное правило: attention-слоёв 8 из 32 (full_attention_interval по умолчанию 4).
	const kvLayers = 8
	const wantKV = (256 + 256) * kvLayers * 4 * 2 // 32768
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (%d attention-слоёв × ширина 256)",
			info.KVPerToken, wantKV, kvLayers)
	}
	// Ширина — ровно та, что даёт и формула (4096/16 = 256): правка не сдвинула её.
	const widthFromFormula = 4096 / 16
	if info.HeadDimK != widthFromFormula || info.KVGlobalHeadDim != widthFromFormula {
		t.Errorf("ширина = %d/%d, want %d — key_length совпадает с n_embd/n_heads",
			info.HeadDimK, info.KVGlobalHeadDim, widthFromFormula)
	}
	if info.KVGlobalLayers != kvLayers {
		t.Errorf("глобальных KV-слоёв = %d, want %d", info.KVGlobalLayers, kvLayers)
	}
	if info.KVPlanSource != KVPlanSourcePlan {
		t.Errorf("KVPlanSource = %q, want %q", info.KVPlanSource, KVPlanSourcePlan)
	}
}

// TestR705_LegacyBridgeFallsBackToComputed — старый мост не сообщает источник
// (HeadDimSource == "") и присылает прежнее число n_embd/n_heads.
//
// Требование обратной совместимости: число остаётся рабочим (KV считается как
// раньше), но в API оно обязано быть помечено computed, а не gguf.
func TestR705_LegacyBridgeFallsBackToComputed(t *testing.T) {
	const modelName = "legacy-bridge-model"
	b := loadedInfoBackendR705(t, ModelInfo{
		Name:              modelName,
		State:             StateLoaded,
		Architecture:      "qwen3",
		NLayers:           36,
		NHeads:            32,
		NKvHeads:          8,
		HeadDimK:          80, // ровно n_embd/n_heads — как присылал прежний мост
		HeadDimV:          80,
		HeadDimSource:     "", // старый мост источника не сообщает
		NEmbd:             2560,
		GGUFContextLength: 262144,
	}, "f16")

	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	const wantKV = 2 * 36 * 8 * 80 * 2 // 92160 — прежняя формула
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (fallback обязан дать прежнюю формулу)",
			info.KVPerToken, wantKV)
	}
	if info.HeadDimSource != bridge.HeadDimSourceComputed {
		t.Fatalf("HeadDimSource = %q, want %q — без источника моста число обязано считаться оценкой",
			info.HeadDimSource, bridge.HeadDimSourceComputed)
	}
	if info.HeadDimK != 80 {
		t.Errorf("HeadDimK = %d, want 80 (значение старого моста не подменяется)", info.HeadDimK)
	}
}

// TestR705_HeaderPathMarksSource — путь «читаем GGUF-заголовок» тоже маркирует
// источник: с <arch>.attention.key_length → gguf, без него → computed.
func TestR705_HeaderPathMarksSource(t *testing.T) {
	withKey := t.TempDir()
	writeFakeGGUF(t, withKey, "qwen3-4b.gguf", map[string]interface{}{
		"general.architecture":          "qwen3",
		"qwen3.block_count":             uint32(36),
		"qwen3.attention.head_count":    uint32(32),
		"qwen3.attention.head_count_kv": uint32(8),
		"qwen3.embedding_length":        uint32(2560),
		"qwen3.context_length":          uint32(262144),
		"qwen3.attention.key_length":    uint32(128),
		"qwen3.attention.value_length":  uint32(128),
	})
	info := feasibleKVFor(t, withKey, "qwen3-4b", "f16")
	if info.HeadDimK != 128 || info.HeadDimSource != bridge.HeadDimSourceGGUF {
		t.Fatalf("header path: head_dim = %d source = %q, want 128/gguf",
			info.HeadDimK, info.HeadDimSource)
	}

	withoutKey := t.TempDir()
	writeFakeGGUF(t, withoutKey, "qwen3-4b.gguf", map[string]interface{}{
		"general.architecture":          "qwen3",
		"qwen3.block_count":             uint32(36),
		"qwen3.attention.head_count":    uint32(32),
		"qwen3.attention.head_count_kv": uint32(8),
		"qwen3.embedding_length":        uint32(2560),
		"qwen3.context_length":          uint32(262144),
	})
	info = feasibleKVFor(t, withoutKey, "qwen3-4b", "f16")
	if info.HeadDimK != 80 || info.HeadDimSource != bridge.HeadDimSourceComputed {
		t.Fatalf("header path без key_length: head_dim = %d source = %q, want 80/computed",
			info.HeadDimK, info.HeadDimSource)
	}
}

// TestR705_ResolveHeadDimSource — правила нормализации источника.
func TestR705_ResolveHeadDimSource(t *testing.T) {
	cases := []struct {
		reported string
		headDim  int
		want     string
	}{
		{bridge.HeadDimSourceGGUF, 128, bridge.HeadDimSourceGGUF},
		{bridge.HeadDimSourceComputed, 80, bridge.HeadDimSourceComputed},
		{"", 80, bridge.HeadDimSourceComputed}, // старый мост: число есть → формула
		{"", 0, ""},                            // числа нет → неизвестно
		{bridge.HeadDimSourceGGUF, 0, ""},      // ширины нет — и «факта» нет
		{"мусор", 128, bridge.HeadDimSourceComputed},
	}
	for _, tc := range cases {
		if got := resolveHeadDimSourceR705(tc.reported, tc.headDim); got != tc.want {
			t.Errorf("resolveHeadDimSourceR705(%q, %d) = %q, want %q",
				tc.reported, tc.headDim, got, tc.want)
		}
	}
}
