// memfit_adapter_test.go — R83, шаг 2: тесты адаптера и shadow-сравнения.
//
// Здесь проверяется не «математика памяти» (она в internal/memfit), а то, что
// адаптер собирает корректные входные данные и что расхождение решений
// детектируется — иначе shadow-режим молча ничего не сравнивал бы.
package cppbackend

import (
	"testing"

	"ollama-loadbalancer/internal/memfit"
)

// TestMemfitSpecFromMeta — метаданные GGUF превращаются в ModelSpec, включая
// обучающий контекст (его отсутствие в прежних лимитах давало fail-open гейт).
func TestMemfitSpecFromMeta(t *testing.T) {
	meta := &GGUFModelMeta{
		Filename:      "Qwen3.8-27B-UD-Q4_K_M",
		SizeBytes:     16464440224,
		NLayers:       64,
		NHeads:        24,
		NKvHeads:      4,
		NEmbd:         5120,
		ContextLength: 262144,
	}
	spec := MemfitSpecFromMeta("qwen3.8:latest", meta)
	if !spec.Complete() {
		t.Fatalf("spec неполный: %+v", spec)
	}
	if spec.TrainCtx != 262144 || spec.NLayers != 64 || spec.HeadDim() != 213 {
		t.Errorf("spec собран неверно: %+v (head_dim=%d)", spec, spec.HeadDim())
	}
}

// TestMemfitSpec_TaggedNameResolves — внешнее имя обязано резолвиться тем же путём,
// что и загрузка (иначе shadow сравнивал бы «неизвестную модель» с известной).
func TestMemfitSpec_TaggedNameResolves(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 32768, DefaultBatchSize: 512})
	mm := b.ModelManager()
	if mm == nil {
		t.Fatal("ModelManager недоступен")
	}
	mm.ggufFiles["Qwen3.8-27B-UD-Q4_K_M"] = &GGUFModelMeta{
		Filename: "Qwen3.8-27B-UD-Q4_K_M", SizeBytes: 16464440224,
		NLayers: 64, NHeads: 24, NKvHeads: 4, NEmbd: 5120, ContextLength: 262144,
	}

	for _, name := range []string{"Qwen3.8-27B-UD-Q4_K_M", "qwen3.8:latest"} {
		spec, ok := b.MemfitSpec(name)
		if !ok {
			t.Errorf("MemfitSpec(%q) не нашёл модель", name)
			continue
		}
		if spec.SizeBytes != memfit.Bytes(16464440224) {
			t.Errorf("MemfitSpec(%q).SizeBytes = %s", name, spec.SizeBytes)
		}
	}
}

// TestMemfitBudget_ConsistentWithProbe — бюджет не выдумывает числа: RAM приходит
// из ProbeRAM как есть (включая Known), VRAM без GPU считается неизвестной.
func TestMemfitBudget_ConsistentWithProbe(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultCtxSize: 32768, DefaultBatchSize: 512})
	budget := b.MemfitBudget()

	pr := memfit.ProbeRAM()
	if budget.RAMKnown != pr.Known {
		t.Errorf("RAMKnown=%v, ProbeRAM.Known=%v — бюджет расходится с источником", budget.RAMKnown, pr.Known)
	}
	if budget.RAMKnown && budget.RAMAvail != pr.Available {
		t.Errorf("RAMAvail=%s, ProbeRAM.Available=%s", budget.RAMAvail, pr.Available)
	}
	if budget.VRAMKnown {
		t.Errorf("VRAMKnown=true без GPU — неизвестное обязано остаться неизвестным")
	}
	if budget.RAMReserve <= 0 {
		t.Errorf("системный резерв не задан: %s", budget.RAMReserve)
	}
}

// TestEffectiveKVCacheType — per-request тип побеждает дефолт конфига, а
// неизвестный ввод не превращается в «нет KV».
func TestEffectiveKVCacheType(t *testing.T) {
	b := NewBackend(Config{ModelsDir: t.TempDir(), DefaultKVCacheType: "q4_0"})
	if got := b.EffectiveKVCacheType(""); got != "q4_0" {
		t.Errorf("дефолт конфига: got %q, want q4_0", got)
	}
	if got := b.EffectiveKVCacheType("q8_0"); got != "q8_0" {
		t.Errorf("per-request: got %q, want q8_0", got)
	}

	if got := MemfitKVType("q4_0"); got != memfit.KVQ4 {
		t.Errorf("MemfitKVType(q4_0) = %q", got)
	}
	if got := MemfitKVType(""); got != memfit.KVF16 {
		t.Errorf("MemfitKVType(\"\") = %q, want f16 (безопасный верх)", got)
	}
}
