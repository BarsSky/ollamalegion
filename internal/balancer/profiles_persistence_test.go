// profiles_persistence_test.go — tests for /app/data/profiles.json persistence.
package balancer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// boolPtr — helper to get *bool from bool literal (FlashAttn is *bool).
func boolPtr(b bool) *bool { return &b }

// TestProfilesPersistence_SaveLoadRoundtrip —
// Сохраняем профили → создаём новый Proxy → загружаем → проверяем что профили на месте.
func TestProfilesPersistence_SaveLoadRoundtrip(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")
	profilesPath := filepath.Join(tmpDir, "profiles.json")

	p := &Proxy{
		statePath: statePath,
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4-E4B-it-Q4_K_M": {
					ContextLength: 65536,
					BatchSize:     512,
					NumGPULayers:  20,
					FlashAttn:     boolPtr(true),
					KVCacheType:   "f16",
					Notes:         "gemma-4 64K",
				},
				"qwen3.6-35b": {
					ContextLength: 32768,
					BatchSize:     256,
					NumGPULayers:  30,
				},
			},
		},
	}

	// Save
	if err := p.SaveProfilesToFile(); err != nil {
		t.Fatalf("SaveProfilesToFile: %v", err)
	}
	// Verify file exists
	if _, err := os.Stat(profilesPath); err != nil {
		t.Fatalf("profiles file not created: %v", err)
	}

	// Load in fresh proxy
	p2 := &Proxy{
		statePath: statePath,
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: make(map[string]types.LlamaCppModelProfile),
		},
	}
	loaded, err := p2.LoadProfilesFromFile()
	if err != nil {
		t.Fatalf("LoadProfilesFromFile: %v", err)
	}
	if loaded != 2 {
		t.Errorf("expected 2 profiles loaded, got %d", loaded)
	}

	// Verify content
	for name, want := range p.config.LlamaCppModelProfiles {
		got, ok := p2.config.LlamaCppModelProfiles[name]
		if !ok {
			t.Errorf("profile %q not loaded", name)
			continue
		}
		if got.ContextLength != want.ContextLength {
			t.Errorf("profile %q: ContextLength got %d, want %d",
				name, got.ContextLength, want.ContextLength)
		}
		if got.Notes != want.Notes {
			t.Errorf("profile %q: Notes got %q, want %q",
				name, got.Notes, want.Notes)
		}
	}
}

// TestProfilesPersistence_LoadConfigPriority —
// config.json (bundled defaults) должен иметь приоритет над profiles.json.
func TestProfilesPersistence_LoadConfigPriority(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	// Step 1: Write profiles.json with model "gemma-4" having contextLength=4096
	disk := &Proxy{
		statePath: statePath,
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 4096, Notes: "from-disk"},
			},
		},
	}
	if err := disk.SaveProfilesToFile(); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Step 2: Load in proxy where config.json has DIFFERENT profile for same model
	loaded := &Proxy{
		statePath: statePath,
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"gemma-4": {ContextLength: 32768, Notes: "from-config"},
			},
		},
	}
	count, err := loaded.LoadProfilesFromFile()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 profiles loaded (config has priority), got %d", count)
	}
	// Config value should be preserved
	if got := loaded.config.LlamaCppModelProfiles["gemma-4"]; got.ContextLength != 32768 {
		t.Errorf("config value not preserved: got %d, want 32768", got.ContextLength)
	}
	if got := loaded.config.LlamaCppModelProfiles["gemma-4"]; got.Notes != "from-config" {
		t.Errorf("config notes not preserved: got %q", got.Notes)
	}
}

// TestProfilesPersistence_AtomicWrite —
// Concurrent writes не повреждают файл.
func TestProfilesPersistence_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	statePath := filepath.Join(tmpDir, "state.json")

	p := &Proxy{
		statePath: statePath,
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: map[string]types.LlamaCppModelProfile{
				"test": {ContextLength: 2048},
			},
		},
	}

	// Write 100 times rapidly
	for i := 0; i < 100; i++ {
		p.config.LlamaCppModelProfiles["test"] = types.LlamaCppModelProfile{
			ContextLength: 1024 + i,
		}
		if err := p.SaveProfilesToFile(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	// Verify file is valid JSON
	data, err := os.ReadFile(filepath.Join(tmpDir, "profiles.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var f profilesFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal: %v (file corrupt)", err)
	}
	if f.Profiles["test"].ContextLength < 1024 || f.Profiles["test"].ContextLength >= 1024+100 {
		t.Errorf("contextLength out of range: %d", f.Profiles["test"].ContextLength)
	}
}

// TestProfilesPersistence_MissingFile —
// Если файла нет — LoadProfilesFromFile не падает, возвращает (0, nil).
func TestProfilesPersistence_MissingFile(t *testing.T) {
	tmpDir := t.TempDir()
	p := &Proxy{
		statePath: filepath.Join(tmpDir, "state.json"),
		config: &types.LoadBalancerConfig{
			LlamaCppModelProfiles: make(map[string]types.LlamaCppModelProfile),
		},
	}
	loaded, err := p.LoadProfilesFromFile()
	if err != nil {
		t.Errorf("expected no error for missing file, got: %v", err)
	}
	if loaded != 0 {
		t.Errorf("expected 0 loaded, got %d", loaded)
	}
}

// TestProfilesPersistence_ProfilesFilePath —
// Проверяем что profilesFilePath правильно выводится из statePath.
func TestProfilesPersistence_ProfilesFilePath(t *testing.T) {
	tests := []struct {
		statePath string
		expected  string
	}{
		{"data/state.json", "data/profiles.json"},
		{"/app/data/state.json", "/app/data/profiles.json"},
		{"/var/lib/balancer/state.json", "/var/lib/balancer/profiles.json"},
		{"", "data/profiles.json"}, // empty → default
	}
	for _, tt := range tests {
		got := profilesFilePath(tt.statePath)
		// Normalize to forward slashes for cross-platform comparison
		gotNorm := filepath.ToSlash(got)
		expectedNorm := filepath.ToSlash(tt.expected)
		if gotNorm != expectedNorm {
			t.Errorf("statePath=%q: got %q, want %q", tt.statePath, gotNorm, expectedNorm)
		}
	}
}
