// internal/cppbackend/feasible_test.go — Round 37 (2026-08-18) test.
//
// Покрывает ComputeFeasible — single source of truth для "что n_ctx достижим
// на текущем hardware". Без этого теста легко вернуться к старому поведению
// "use profile.contextLength as-is" и снова ловить 413 preflight.
//
// Эти тесты требуют build tag llama_stub (как и остальные в этом пакете).
//go:build llama_stub

package cppbackend

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writeFakeGGUF создаёт минимальный валидный GGUF v3 файл с заданными metadata.
//
// GGUF v3 format:
//   [4]byte  magic = "GGUF"
//   uint32   version = 3
//   uint64   tensorCount
//   uint64   metadataKvCount
//   [metadata]   — N пар key/value
//
// Metadata KV:
//   uint64 keyLen
//   []byte key
//   uint32 valType  (8=string, 4=uint32)
//   [value] зависит от типа
//
// Возвращает путь к файлу. Caller должен удалить (t.TempDir() авто-удалит).
func writeFakeGGUF(t *testing.T, dir, filename string, kvPairs map[string]interface{}) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fake gguf: %v", err)
	}
	defer f.Close()

	// Magic
	if _, err := f.Write([]byte("GGUF")); err != nil {
		t.Fatal(err)
	}
	// Version
	if err := binary.Write(f, binary.LittleEndian, uint32(3)); err != nil {
		t.Fatal(err)
	}
	// Tensor count
	if err := binary.Write(f, binary.LittleEndian, uint64(0)); err != nil {
		t.Fatal(err)
	}
	// Metadata KV count
	if err := binary.Write(f, binary.LittleEndian, uint64(len(kvPairs))); err != nil {
		t.Fatal(err)
	}

	for k, v := range kvPairs {
		// Key
		if err := binary.Write(f, binary.LittleEndian, uint64(len(k))); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(k)); err != nil {
			t.Fatal(err)
		}
		// Value type + value
		switch val := v.(type) {
		case string:
			if err := binary.Write(f, binary.LittleEndian, uint32(8)); err != nil { // GGUF type 8 = string
				t.Fatal(err)
			}
			if err := binary.Write(f, binary.LittleEndian, uint64(len(val))); err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte(val)); err != nil {
				t.Fatal(err)
			}
		case uint32:
			if err := binary.Write(f, binary.LittleEndian, uint32(4)); err != nil { // GGUF type 4 = uint32
				t.Fatal(err)
			}
			if err := binary.Write(f, binary.LittleEndian, val); err != nil {
				t.Fatal(err)
			}
		case int:
			if err := binary.Write(f, binary.LittleEndian, uint32(4)); err != nil {
				t.Fatal(err)
			}
			if err := binary.Write(f, binary.LittleEndian, uint32(val)); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unsupported kv value type: %T for key %q", v, k)
		}
	}
	return path
}

// TestComputeFeasible_Qwen36_35B — главный сценарий из production bug 2026-08-18.
//
// Модель Qwen3.6-35B-A3B-UD-Q4_K_M, GGUF context_length = 262144.
// ComputeFeasible должен вернуть:
//   - GGUFMax = 262144
//   - KVPerToken = 2 (q4_0) * 2 (K+V) * 40 * 2 * 128 = 40960 bytes
//   - MaxRAMCtx = min(20GB / 40960, 262144) = min(524288, 262144) = 262144
//   - Source = "gguf" (модель не загружена, читаем из файла)
func TestComputeFeasible_Qwen36_35B(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "Qwen3.6-35B-A3B-UD-Q4_K_M.gguf", map[string]interface{}{
		"general.architecture":            "qwen3",
		"qwen3.block_count":               uint32(40),
		"qwen3.attention.head_count":      uint32(16),
		"qwen3.attention.head_count_kv":   uint32(2),
		"qwen3.embedding_length":          uint32(2048),
		"qwen3.context_length":            uint32(262144), // 262K
	})

	cfg := Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: "q4_0", // matches Qwen3.6 profile
	}
	b := NewBackend(cfg)
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	info, err := b.ComputeFeasible("Qwen3.6-35B-A3B-UD-Q4_K_M")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}

	if info.GGUFMax != 262144 {
		t.Errorf("GGUFMax = %d, want 262144", info.GGUFMax)
	}

	// kvPerToken with headDimK estimation = NEmbd/NHeads = 2048/16 = 128:
	//   fp16: 2 * 40 * 2 * 128 * 2 = 40960
	//   q4_0: 40960 / 4 = 10240
	expectedKVPerToken := 2 * 40 * 2 * 128 * 2 / 4
	if info.KVPerToken != expectedKVPerToken {
		t.Errorf("KVPerToken = %d, want %d (q4_0, headDim=128 estimated from NEmbd/NHeads)",
			info.KVPerToken, expectedKVPerToken)
	}

	if info.KVCacheType != "q4_0" {
		t.Errorf("KVCacheType = %q, want q4_0", info.KVCacheType)
	}

	if info.Source != "gguf" {
		t.Errorf("Source = %q, want gguf (model not loaded)", info.Source)
	}

	// MaxRAMCtx должен быть > 0 (у нас есть free RAM)
	// и capped by GGUFMax
	if info.MaxRAMCtx <= 0 {
		t.Errorf("MaxRAMCtx = %d, want > 0 (have free RAM)", info.MaxRAMCtx)
	}
	if info.MaxRAMCtx > info.GGUFMax {
		t.Errorf("MaxRAMCtx (%d) > GGUFMax (%d) — should be capped", info.MaxRAMCtx, info.GGUFMax)
	}
}

// TestComputeFeasible_KVCache_q4_0_vs_f16 — проверяет, что q4_0 даёт 4x больше
// контекста, чем f16 (KV cache compression ratio).
//
// Модель с фиксированными параметрами: 32 layers, 8 KV heads, head_dim=80 (2560/32).
//   f16: kv = 2 * 32 * 8 * 80 * 2 = 81920 bytes/token → 1GB / 81920 = ~13107 ctx
//   q4_0: kv = 81920 / 4 = 20480 bytes/token → 1GB / 20480 = ~52428 ctx
//
// Отношение q4_0 / f16 ≈ 4x.
//
// GGUFMax = 1M — большой, чтобы не cap MaxCtx (иначе ratio = 1:1).
func TestComputeFeasible_KVCache_q4_0_vs_f16(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "test_7b.gguf", map[string]interface{}{
		"general.architecture":            "llama",
		"llama.block_count":               uint32(32),
		"llama.attention.head_count":      uint32(32),
		"llama.attention.head_count_kv":   uint32(8),
		"llama.embedding_length":          uint32(2560),
		"llama.context_length":            uint32(1048576), // 1M — большой, чтобы не cap
	})

	// Test f16
	cfgF16 := Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: "f16",
	}
	bF16 := NewBackend(cfgF16)
	if _, err := bF16.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	infoF16, err := bF16.ComputeFeasible("test_7b")
	if err != nil {
		t.Fatalf("ComputeFeasible f16: %v", err)
	}

	// Test q4_0 (same dir, different backend instance)
	cfgQ4 := cfgF16
	cfgQ4.DefaultKVCacheType = "q4_0"
	bQ4 := NewBackend(cfgQ4)
	if _, err := bQ4.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}
	infoQ4, err := bQ4.ComputeFeasible("test_7b")
	if err != nil {
		t.Fatalf("ComputeFeasible q4_0: %v", err)
	}

	// Verify q4_0 gives 4x more ctx
	if infoF16.KVPerToken == 0 {
		t.Fatal("infoF16.KVPerToken = 0")
	}
	if infoQ4.KVPerToken == 0 {
		t.Fatal("infoQ4.KVPerToken = 0")
	}
	ratio := infoF16.KVPerToken / infoQ4.KVPerToken
	if ratio != 4 {
		t.Errorf("KV ratio f16/q4_0 = %d, want 4", ratio)
	}

	// MaxRAMCtx (or MaxVRAMCtx, whichever is non-zero) should be ~4x larger for q4_0
	var f16, q4 int
	if infoF16.MaxRAMCtx > 0 && infoQ4.MaxRAMCtx > 0 {
		f16 = infoF16.MaxRAMCtx
		q4 = infoQ4.MaxRAMCtx
	} else if infoF16.MaxVRAMCtx > 0 && infoQ4.MaxVRAMCtx > 0 {
		f16 = infoF16.MaxVRAMCtx
		q4 = infoQ4.MaxVRAMCtx
	}
	if f16 > 0 && q4 > 0 {
		// Allow some fuzz (free RAM is sampled dynamically)
		if q4 < f16*3 {
			t.Errorf("q4_0 MaxCtx (%d) should be ~4x f16 MaxCtx (%d), got ratio=%.2f",
				q4, f16, float64(q4)/float64(f16))
		}
	}
}

// TestComputeFeasible_ModelNotFound — error path.
func TestComputeFeasible_ModelNotFound(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: "f16",
	}
	b := NewBackend(cfg)

	_, err := b.ComputeFeasible("nonexistent-model")
	if err == nil {
		t.Error("expected error for nonexistent model, got nil")
	}
}

// TestComputeFeasible_ContextLength_ParsedFromGGUF — критическая проверка:
// после фикса Round 37 .context_length должен парситься из GGUF header
// (раньше парсились только .block_count, .head_count, .head_count_kv,
// .embedding_length — context_length был пропущен).
func TestComputeFeasible_ContextLength_ParsedFromGGUF(t *testing.T) {
	dir := t.TempDir()
	writeFakeGGUF(t, dir, "with_ctx.gguf", map[string]interface{}{
		"general.architecture":            "qwen2",
		"qwen2.block_count":               uint32(32),
		"qwen2.attention.head_count":      uint32(32),
		"qwen2.attention.head_count_kv":   uint32(8),
		"qwen2.embedding_length":          uint32(2560),
		"qwen2.context_length":            uint32(32768), // 32K
	})

	cfg := Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
		DefaultKVCacheType: "f16",
	}
	b := NewBackend(cfg)
	if b == nil {
		t.Fatal("NewBackend returned nil")
	}
	if _, err := b.ModelManager().ScanModels(); err != nil {
		t.Fatalf("ScanModels: %v", err)
	}

	info, err := b.ComputeFeasible("with_ctx")
	if err != nil {
		t.Fatalf("ComputeFeasible: %v", err)
	}

	if info.GGUFMax != 32768 {
		t.Errorf("GGUFMax = %d, want 32768 (Round 37 must parse *.context_length)",
			info.GGUFMax)
	}
}

// TestGetSetGlobalBackend — singleton accessor для CLI tools.
func TestGetSetGlobalBackend(t *testing.T) {
	// Initially nil
	if got := GetBackend(); got != nil {
		t.Errorf("GetBackend() before SetGlobalBackend = %v, want nil", got)
	}

	dir := t.TempDir()
	cfg := Config{
		ModelsDir:          dir,
		DefaultCtxSize:     4096,
		DefaultBatchSize:   512,
	}
	b := NewBackend(cfg)
	SetGlobalBackend(b)
	defer SetGlobalBackend(nil) // reset for next tests

	if got := GetBackend(); got != b {
		t.Errorf("GetBackend() = %v, want %v", got, b)
	}
}
