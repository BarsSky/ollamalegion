//go:build llama_stub

// internal/cppbackend/feasible_key_length_r704_test.go — R704 (2026-11-24).
//
// ПРЕДМЕТ. ComputeFeasible считал ширину головы K/V как NEmbd/NHeads и игнорировал
// точные <arch>.attention.key_length / value_length из заголовка GGUF. Для
// моделей, где эти числа не совпадают, KV на токен занижался, а рекомендованное
// окно — завышалось. Живые факты стенда (проверено по заголовкам и тензорам):
//
//	Inkling-2-Mini: 2304/24 = 96  при key_length = 128 → KV на 25 % меньше,
//	                             окно завышено на 33 % (582 542 вместо 436 906);
//	gemma-4-E4B:    2560/8  = 320 при key_length = 512 → KV на 37.5 % меньше;
//	Qwen3-4B:       2560/32 = 80  при key_length = 128 → KV на 60 % меньше;
//	Qwen3.8-9B:     4096/16 = 256 = key_length = 256 → числа НЕ меняются.
//
// Тесты ниже закрепляют три свойства:
//  1. key_length из GGUF учитывается (числа считаются от него, а не от формулы);
//  2. без key_length — прежняя формула (обратная совместимость);
//  3. для модели, у которой head_dim совпадает с формулой, числа не изменились.
//
// НЕ-ВАКУУМНОСТЬ. Ожидаемые числа записаны по формуле ВРУЧНУЮ (по key_length), а не
// взяты из кода; отдельные проверки требуют, чтобы значение ОТЛИЧАЛОСЬ от старой
// формулы — поэтому тест краснеет на состоянии до правки (см. RED в отчёте).
//

package cppbackend

import (
	"path/filepath"
	"testing"
)

// injectLoadedInfoR704 — положить в backend «загруженную» модель с заданным
// ModelInfo, без C-моста (handle == nil). Тест живёт в этом же пакете, поэтому
// карта инстансов доступна напрямую; отдельный хелпер, чтобы не тащить
// InjectLoadedModelForTest (он не умеет GGUFContextLength/HeadDimV).
func injectLoadedInfoR704(t *testing.T, b *Backend, info ModelInfo) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.models[info.Name] = &modelInstance{info: info}
}

// keyLengthTestConfig — backend над каталогом с одним fake GGUF.
func keyLengthTestConfig(dir, kvType string) Config {
	return Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: kvType,
	}
}

// feasibleKVFor — ComputeFeasible над моделью с заданным KV-типом.
func feasibleKVFor(t *testing.T, dir, modelName, kvType string) *FeasibleInfo {
	t.Helper()
	b := NewBackend(keyLengthTestConfig(dir, kvType))
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	info, err := b.ComputeFeasible(modelName)
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	return info
}

// TestR704_KeyLengthFromGGUFIsUsed — геометрия Inkling-2-Mini: 48 блоков, 8 KV-голов,
// key_length/value_length = 128 при n_embd/n_heads = 2304/24 = 96.
//
// Ожидание f16: (128 + 128) * 48 * 8 * 2 = 196 608 Б/токен.
// Старая формула дала бы 2 * 48 * 8 * 96 * 2 = 147 456 — на 25 % меньше.
func TestR704_KeyLengthFromGGUFIsUsed(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "Inkling-2-Mini-UD-Q4_K_M.gguf", map[string]interface{}{
		"general.architecture":            "inkling2",
		"inkling2.block_count":            uint32(48),
		"inkling2.attention.head_count":   uint32(24),
		"inkling2.attention.head_count_kv": uint32(8),
		"inkling2.embedding_length":       uint32(2304),
		"inkling2.context_length":         uint32(1048576),
		"inkling2.attention.key_length":   uint32(128),
		"inkling2.attention.value_length": uint32(128),
	})

	info := feasibleKVFor(t, dir, "Inkling-2-Mini-UD-Q4_K_M", "f16")

	const wantKV = (128 + 128) * 48 * 8 * 2 // 196608
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (key_length=128, а не n_embd/n_heads=96)",
			info.KVPerToken, wantKV)
	}
	// НЕ-ВАКУУМНОСТЬ: значение обязано отличаться от старой формулы.
	const oldKV = 2 * 48 * 8 * (2304 / 24) * 2 // 147456
	if info.KVPerToken == oldKV {
		t.Fatalf("KVPerToken = %d — считается по n_embd/n_heads, key_length проигнорирован", oldKV)
	}

	// q8_0/q4_0 — ТОЧНЫЕ коэффициенты llama.cpp (34/32 и 18/32, memfit.KVBytesPerElement),
	// но от НОВОЙ ширины. R707: прежние «/2» и «/4» расходились с memfit на
	// +6.25 %/+12.5 % в сторону занижения KV — то есть рекомендовали окно больше
	// физического; теперь feasible и решения считают одной константой.
	const elems = 2 * 48 * 8 * 128
	for _, tc := range []struct {
		kvType string
		want   int
	}{
		{"q8_0", elems * 34 / 32},
		{"q4_0", elems * 18 / 32},
	} {
		got := feasibleKVFor(t, dir, "Inkling-2-Mini-UD-Q4_K_M", tc.kvType)
		if got.KVPerToken != tc.want {
			t.Errorf("%s: KVPerToken = %d, want %d", tc.kvType, got.KVPerToken, tc.want)
		}
	}

	// Рекомендованное окно — min(VRAM, RAM, GGUF) — обязано считаться от НОВОГО KV.
	// GGUF-потолок (1048576) выше любого окна по памяти, поэтому рекомендуемое =
	// max_ram_n_ctx = freeRAM / kvPerToken, и оно на 25 % меньше прежнего.
	ggufMax := info.GGUFMax
	if ggufMax != 1048576 {
		t.Fatalf("GGUFMax = %d, want 1048576", ggufMax)
	}
	if info.MaxRAMCtx <= 0 {
		t.Fatalf("MaxRAMCtx = %d, want > 0", info.MaxRAMCtx)
	}
	wantRAM := int(info.FreeRAMMB*1024*1024) / wantKV
	if info.MaxRAMCtx != wantRAM {
		t.Errorf("MaxRAMCtx = %d, want %d (свободная RAM / новый KV)", info.MaxRAMCtx, wantRAM)
	}
	oldRAM := int(info.FreeRAMMB*1024*1024) / oldKV
	if info.MaxRAMCtx >= oldRAM {
		t.Errorf("MaxRAMCtx = %d не уменьшился относительно старой оценки %d — окно так и завышено",
			info.MaxRAMCtx, oldRAM)
	}
}

// TestR704_ValueLengthCountedSeparately — K и V могут быть разной ширины
// (gemma-4: глобальные слои 512, SWA-слои 256): байты считаются как K + V.
func TestR704_ValueLengthCountedSeparately(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "asym.gguf", map[string]interface{}{
		"general.architecture":           "llama",
		"llama.block_count":              uint32(4),
		"llama.attention.head_count":     uint32(8),
		"llama.attention.head_count_kv":  uint32(2),
		"llama.embedding_length":         uint32(512),
		"llama.context_length":           uint32(8192),
		"llama.attention.key_length":     uint32(128),
		"llama.attention.value_length":   uint32(64),
	})

	info := feasibleKVFor(t, dir, "asym", "f16")
	// (128 + 64) * 4 * 2 * 2 = 3072
	const wantKV = (128 + 64) * 4 * 2 * 2
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (K=128, V=64 — разная ширина)", info.KVPerToken, wantKV)
	}
}

// TestR704_NoKeyLengthFallsBackToFormula — метаданных key_length/value_length нет
// (старые файлы, конвертеры без этих ключей) — остаётся прежняя формула
// NEmbd/NHeads, числа не должны измениться.
func TestR704_NoKeyLengthFallsBackToFormula(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "nokey.gguf", map[string]interface{}{
		"general.architecture":           "llama",
		"llama.block_count":              uint32(32),
		"llama.attention.head_count":     uint32(32),
		"llama.attention.head_count_kv":  uint32(8),
		"llama.embedding_length":         uint32(2560),
		"llama.context_length":           uint32(32768),
	})

	info := feasibleKVFor(t, dir, "nokey", "f16")
	// 2560/32 = 80 → 2 * 32 * 8 * 80 * 2 = 81920
	const wantKV = 2 * 32 * 8 * (2560 / 32) * 2
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (прежняя формула без key_length)", info.KVPerToken, wantKV)
	}
}

// TestR704_EqualHeadDimNumbersUnchanged — модель, у которой key_length СОВПАДАЕТ с
// NEmbd/NHeads (Qwen3.8-9B: 4096/16 = 256 = key_length), не должна «поехать» по
// ШИРИНЕ: per-layer вклад считается от 256, а не от какой-то другой оценки.
//
// R707: само ЧИСЛО KV на токен меняется — и это правильно: у qwen35 KV держат
// только attention-слои (каждый 4-й), то есть 8 из 33 блоков, а не все 33. Ширина
// при этом остаётся 256 (= n_embd/n_heads), что здесь и проверяется.
func TestR704_EqualHeadDimNumbersUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "qwen38-9b.gguf", map[string]interface{}{
		"general.architecture":            "qwen35",
		"qwen35.block_count":              uint32(33),
		"qwen35.attention.head_count":     uint32(16),
		"qwen35.attention.head_count_kv":  uint32(4),
		"qwen35.embedding_length":         uint32(4096),
		"qwen35.context_length":           uint32(262144),
		"qwen35.attention.key_length":     uint32(256),
		"qwen35.attention.value_length":   uint32(256),
	})

	// Гибридное правило qwen35: 32 слоя (block_count − MTP), attention каждый 4-й.
	const kvLayers = 8
	const elems = 2 * kvLayers * 4 * 256
	for _, tc := range []struct {
		kvType string
		want   int
	}{
		{"f16", elems * 2},
		{"q8_0", elems * 34 / 32},
		{"q4_0", elems * 18 / 32},
	} {
		info := feasibleKVFor(t, dir, "qwen38-9b", tc.kvType)
		if info.KVPerToken != tc.want {
			t.Errorf("%s: KVPerToken = %d, want %d (8 attention-слоёв × ширина 256)",
				tc.kvType, info.KVPerToken, tc.want)
		}
		// Ширина — из key_length, а не какая-то другая: 4096/16 = 256 совпадает,
		// поэтому проверяем именно её, а не «совпало с формулой» целиком.
		if info.KVGlobalHeadDim != 256 || info.HeadDimK != 256 {
			t.Errorf("%s: ширина = %d/%d, want 256/256 (key_length совпадает с n_embd/n_heads)",
				tc.kvType, info.KVGlobalHeadDim, info.HeadDimK)
		}
		if info.KVGlobalLayers != kvLayers {
			t.Errorf("%s: глобальных KV-слоёв = %d, want %d", tc.kvType, info.KVGlobalLayers, kvLayers)
		}
	}

	// Плоская формула по всем 33 блокам дала бы 135 168 — тест обязан быть
	// красным на состоянии до R707, иначе он не проверяет послойный расчёт.
	flat := feasibleKVFor(t, dir, "qwen38-9b", "f16")
	if flat.KVPerToken == (256+256)*33*4*2 {
		t.Fatalf("KVPerToken = %d — считается по всем блокам, а не по attention-слоям",
			flat.KVPerToken)
	}
}

// TestR704_LoadedInfoUsesMeasuredGeom — путь "loaded_info" (модель уже загружена)
// берёт ширину головы у C-моста и обязан давать те же числа, что и до правки:
// изменение касается GGUF-пути, а не измеренной геометрии.
func TestR704_LoadedInfoUsesMeasuredGeom(t *testing.T) {
	dir := t.TempDir()
	// Файл нужен, чтобы ModelManager его видел (путь к модели), но числа берутся
	// из инстанса, поэтому в заголовке держим ДРУГИЕ значения — они не должны
	// победить измеренные.
	writeFakeGGUF(t, dir, "loaded.gguf", map[string]interface{}{
		"general.architecture":           "llama",
		"llama.block_count":              uint32(48),
		"llama.attention.head_count":     uint32(24),
		"llama.attention.head_count_kv":  uint32(8),
		"llama.embedding_length":         uint32(2304),
		"llama.context_length":           uint32(1048576),
		"llama.attention.key_length":     uint32(512),
	})

	b := NewBackend(keyLengthTestConfig(dir, "f16"))
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	// Инжектируем «загруженную» модель с ИЗМЕРЕННОЙ геометрией (как её кладёт
	// C-мост после llama.cpp): key_length в заголовке (512) — намеренно другой,
	// он не должен победить измеренные 96.
	injectLoadedInfoR704(t, b, ModelInfo{
		Name:              "loaded",
		Path:              filepath.Join(dir, "loaded.gguf"),
		State:             StateLoaded,
		Architecture:      "llama",
		NLayers:           48,
		NHeads:            24,
		NKvHeads:          8,
		NEmbd:             2304,
		HeadDimK:          96,
		HeadDimV:          96,
		GGUFContextLength: 1048576,
	})

	info, err := b.ComputeFeasible("loaded")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}
	if info.Source != "loaded_info" {
		t.Fatalf("Source = %q, want loaded_info", info.Source)
	}
	// Измеренная ширина головы 96 → 2 * 48 * 8 * 96 * 2 = 147456 (как до правки).
	const wantKV = 2 * 48 * 8 * 96 * 2
	if info.KVPerToken != wantKV {
		t.Fatalf("KVPerToken = %d, want %d (измеренная геометрия загруженной модели)", info.KVPerToken, wantKV)
	}
}
