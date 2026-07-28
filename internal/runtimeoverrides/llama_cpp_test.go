// runtimeoverrides_test.go — тесты для sidecar runtime-overrides (Session 17).
package runtimeoverrides

import (
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// testStore создаёт Store с временной директорией, удаляет её при t.Cleanup.
func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return New(dir)
}

func TestStore_New_IsEnabled(t *testing.T) {
	// dataDir = "" → отключен
	if New("").IsEnabled() {
		t.Error("empty dataDir should be disabled")
	}
	// dataDir = "/tmp" → включен
	if !New(t.TempDir()).IsEnabled() {
		t.Error("non-empty dataDir should be enabled")
	}
}

func TestStore_LoadLlamaCpp_NotFound(t *testing.T) {
	s := testStore(t)
	ll, exists, err := s.LoadLlamaCpp()
	if err != nil {
		t.Errorf("expected no error on not-found, got %v", err)
	}
	if exists {
		t.Error("expected exists=false when no file")
	}
	if ll != nil {
		t.Errorf("expected nil config on not-found, got %+v", ll)
	}
}

func TestStore_SaveAndLoad(t *testing.T) {
	s := testStore(t)
	original := &types.LlamaCppConfig{
		NumGPULayers:        20,
		ContextLength:       131072,
		BatchSize:           1024,
		FlashAttention:      false,
		NUMA:                false,
		UseMMap:             true,
		UseMLock:            false,
		Strategy:            "vram-ratio",
		AutoGpuDistribution: true,
		KVCacheType:         "q8_0",
		RopeScalingType:     "yarn",
		RopeScalingFactor:   4.0,
		YarnExtFactor:       2.0,
		MainGPU:             0,
		RPCBackend:          "cuda",
	}

	// Save
	if err := s.SaveLlamaCpp(original); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// HasLlamaCppOverride должен стать true.
	if !s.HasLlamaCppOverride() {
		t.Error("HasLlamaCppOverride should be true after Save")
	}

	// Load
	loaded, exists, err := s.LoadLlamaCpp()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true after Save")
	}
	if loaded == nil {
		t.Fatal("Load returned nil config")
	}

	// Проверяем ключевые поля (Float сравниваем точно).
	if loaded.ContextLength != 131072 {
		t.Errorf("ContextLength: got %d, want 131072", loaded.ContextLength)
	}
	if loaded.KVCacheType != "q8_0" {
		t.Errorf("KVCacheType: got %q, want q8_0", loaded.KVCacheType)
	}
	if loaded.RopeScalingType != "yarn" {
		t.Errorf("RopeScalingType: got %q, want yarn", loaded.RopeScalingType)
	}
	if loaded.RopeScalingFactor != 4.0 {
		t.Errorf("RopeScalingFactor: got %v, want 4.0", loaded.RopeScalingFactor)
	}
	if loaded.NumGPULayers != 20 {
		t.Errorf("NumGPULayers: got %d, want 20", loaded.NumGPULayers)
	}
}

func TestStore_Save_CreatesDir(t *testing.T) {
	s := testStore(t)
	// Директории runtime-overrides/ ещё нет — Save должен её создать.
	dir := filepath.Join(s.dataDir, DirName)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir already exists before Save (cleanup issue?)")
	}
	if err := s.SaveLlamaCpp(&types.LlamaCppConfig{ContextLength: 4096}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	// Теперь директория должна быть.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("dir not created: %v", err)
	}
}

func TestStore_Save_OverwritesPrevious(t *testing.T) {
	s := testStore(t)
	// Первое сохранение.
	if err := s.SaveLlamaCpp(&types.LlamaCppConfig{ContextLength: 4096}); err != nil {
		t.Fatalf("first Save failed: %v", err)
	}
	// Второе сохранение с другим значением.
	if err := s.SaveLlamaCpp(&types.LlamaCppConfig{ContextLength: 131072}); err != nil {
		t.Fatalf("second Save failed: %v", err)
	}
	loaded, exists, err := s.LoadLlamaCpp()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !exists {
		t.Fatal("expected exists=true")
	}
	if loaded.ContextLength != 131072 {
		t.Errorf("overwrite didn't take effect: got %d, want 131072", loaded.ContextLength)
	}
}

func TestStore_ClearLlamaCpp(t *testing.T) {
	s := testStore(t)
	// Сохраняем, потом удаляем.
	if err := s.SaveLlamaCpp(&types.LlamaCppConfig{ContextLength: 4096}); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	if !s.HasLlamaCppOverride() {
		t.Fatal("expected override after Save")
	}
	if err := s.ClearLlamaCpp(); err != nil {
		t.Fatalf("Clear failed: %v", err)
	}
	if s.HasLlamaCppOverride() {
		t.Error("override should be gone after Clear")
	}
	// Load после Clear → exists=false.
	_, exists, err := s.LoadLlamaCpp()
	if err != nil {
		t.Errorf("unexpected error after Clear: %v", err)
	}
	if exists {
		t.Error("expected exists=false after Clear")
	}
}

func TestStore_ClearLlamaCpp_Idempotent(t *testing.T) {
	s := testStore(t)
	// Clear без предварительного Save — не должно падать.
	if err := s.ClearLlamaCpp(); err != nil {
		t.Errorf("Clear on empty should be no-op, got error: %v", err)
	}
}

func TestStore_ApplyLlamaCppToConfig_NoOverride(t *testing.T) {
	s := testStore(t)
	target := &types.LoadBalancerConfig{
		LlamaCpp: types.LlamaCppConfig{
			ContextLength: 4096,
			Strategy:      "vram-ratio",
		},
	}
	// Без override — target.LlamaCpp не должен измениться.
	if err := s.ApplyLlamaCppToConfig(target); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if target.LlamaCpp.ContextLength != 4096 {
		t.Errorf("ContextLength changed without override: got %d, want 4096", target.LlamaCpp.ContextLength)
	}
	if target.LlamaCpp.Strategy != "vram-ratio" {
		t.Errorf("Strategy changed without override: got %q", target.LlamaCpp.Strategy)
	}
}

func TestStore_ApplyLlamaCppToConfig_WithOverride(t *testing.T) {
	s := testStore(t)
	// Override полностью заменяет target.LlamaCpp (не мердж).
	override := &types.LlamaCppConfig{
		ContextLength: 131072,
		KVCacheType:   "q8_0",
		Strategy:      "manual",
		NumGPULayers:  10, // main config может иметь другое значение
	}
	if err := s.SaveLlamaCpp(override); err != nil {
		t.Fatalf("Save failed: %v", err)
	}
	target := &types.LoadBalancerConfig{
		LlamaCpp: types.LlamaCppConfig{
			ContextLength: 4096,    // Будет перезаписано на 131072
			Strategy:      "vram-ratio", // Будет перезаписано на manual
			NumGPULayers:  20,      // Будет перезаписано на 10
			// Эти поля не в override — должны быть потеряны (replace, не merge).
			BatchSize: 512,
		},
	}
	if err := s.ApplyLlamaCppToConfig(target); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	// Проверяем, что override заменил target целиком.
	if target.LlamaCpp.ContextLength != 131072 {
		t.Errorf("ContextLength: got %d, want 131072", target.LlamaCpp.ContextLength)
	}
	if target.LlamaCpp.Strategy != "manual" {
		t.Errorf("Strategy: got %q, want manual", target.LlamaCpp.Strategy)
	}
	if target.LlamaCpp.NumGPULayers != 10 {
		t.Errorf("NumGPULayers: got %d, want 10", target.LlamaCpp.NumGPULayers)
	}
	// Поле, которого не было в override — должно быть zero value.
	if target.LlamaCpp.BatchSize != 0 {
		t.Errorf("BatchSize should be 0 (replace semantics), got %d", target.LlamaCpp.BatchSize)
	}
}

func TestStore_Disabled(t *testing.T) {
	s := New("") // disabled
	if err := s.SaveLlamaCpp(&types.LlamaCppConfig{}); err == nil {
		t.Error("Save should fail when disabled")
	}
	if _, _, err := s.LoadLlamaCpp(); err == nil {
		t.Error("Load should fail when disabled")
	}
	if err := s.ClearLlamaCpp(); err == nil {
		t.Error("Clear should fail when disabled")
	}
}
