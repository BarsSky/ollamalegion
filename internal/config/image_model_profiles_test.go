package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

func validProfile(name string) types.ImageModelProfile {
	return types.ImageModelProfile{
		Name:   name,
		Family: "sd15",
		Files: []types.ImageModelFile{
			{
				Role:      types.ImageFileRoleDiffusion,
				Repo:      "second-state/stable-diffusion-v1-5-GGUF",
				Filename:  "stable-diffusion-v1-5-pruned-emaonly-Q8_0.gguf",
				SizeBytes: 1763578176,
			},
		},
		Defaults:       types.DefaultImageGenDefaults("sd15"),
		VramEstimateMB: 2100,
	}
}

// TestImageModelProfileStore_StripsUTF8BOM — R-Image (2026-10-02): файл профилей,
// сохранённый Notepad'ом (UTF-8 с BOM), обязан читаться. До фикса json.Unmarshal
// падал на BOM («invalid character 'ï'»), и оператор видел «профилей нет» при
// внешне корректном файле.
func TestImageModelProfileStore_StripsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "image-model-profiles.json")

	payload := `{"profiles":{"sd15-bom":{"name":"sd15-bom","family":"sd15",` +
		`"files":[{"role":"diffusion","repo":"r","filename":"m.gguf"}],` +
		`"defaults":{"steps":20,"cfgScale":7,"sampler":"euler_a","scheduler":"discrete","width":512,"height":512,"batchCount":1,"seed":-1}}}}`

	// BOM + валидный JSON — ровно то, что пишет Notepad.
	if err := os.WriteFile(path, append([]byte{0xEF, 0xBB, 0xBF}, []byte(payload)...), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store := NewImageModelProfileStore(path)
	if err := store.Load(); err != nil {
		t.Fatalf("Load with UTF-8 BOM must succeed, got %v", err)
	}
	if _, ok := store.Get("sd15-bom"); !ok {
		t.Fatal("profile from BOM-prefixed file must be loaded")
	}
}

// TestImageModelProfileStore_CRUDAndPersistence — Set/Get/Delete + переживание
// рестарта (новый store читает тот же файл).
func TestImageModelProfileStore_CRUDAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "image-model-profiles.json")
	store := NewImageModelProfileStore(path)

	if err := store.EnsureLoaded(); err != nil {
		t.Fatalf("EnsureLoaded on missing file must be OK (empty store), got %v", err)
	}
	if len(store.List()) != 0 {
		t.Fatal("expected empty store")
	}

	p := validProfile("sd15-q8-0")
	if err := store.Set("sd15-q8-0", p); err != nil {
		t.Fatalf("Set failed: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("profile file must be written: %v", err)
	}

	got, ok := store.Get("sd15-q8-0")
	if !ok || got.Name != "sd15-q8-0" || got.Family != "sd15" {
		t.Fatalf("Get returned %+v, ok=%v", got, ok)
	}
	if names := store.Names(); len(names) != 1 || names[0] != "sd15-q8-0" {
		t.Errorf("Names = %v", names)
	}

	// Перезагрузка (симуляция рестарта балансера).
	reloaded := NewImageModelProfileStore(path)
	if err := reloaded.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if _, ok := reloaded.Get("sd15-q8-0"); !ok {
		t.Error("profile must survive reload")
	}

	existed, err := store.Delete("sd15-q8-0")
	if err != nil || !existed {
		t.Fatalf("Delete = %v, %v", existed, err)
	}
	if _, ok := store.Get("sd15-q8-0"); ok {
		t.Error("profile must be gone after Delete")
	}
	existed, err = store.Delete("sd15-q8-0")
	if err != nil || existed {
		t.Errorf("second Delete must report not-existed, got %v, %v", existed, err)
	}

	afterDelete := NewImageModelProfileStore(path)
	if err := afterDelete.Load(); err != nil {
		t.Fatalf("Load after delete failed: %v", err)
	}
	if len(afterDelete.List()) != 0 {
		t.Error("deleted profile must not come back after reload")
	}
}

// TestImageModelProfileStore_RejectsInvalid — валидация делегируется
// types.ValidateImageModelProfile: невалидный профиль НЕ сохраняется на диск.
func TestImageModelProfileStore_RejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	store := NewImageModelProfileStore(path)

	cases := []struct {
		name    string
		profile types.ImageModelProfile
	}{
		{
			name: "no diffusion file",
			profile: types.ImageModelProfile{
				Name: "x", Family: "sd15",
				Files:    []types.ImageModelFile{{Role: types.ImageFileRoleVae, Repo: "a", Filename: "v.safetensors"}},
				Defaults: types.DefaultImageGenDefaults("sd15"),
			},
		},
		{
			name: "unknown family",
			profile: func() types.ImageModelProfile {
				p := validProfile("x")
				p.Family = "not-a-family"
				return p
			}(),
		},
		{
			name: "width not multiple of 64",
			profile: func() types.ImageModelProfile {
				p := validProfile("x")
				p.Defaults.Width = 100
				return p
			}(),
		},
		{
			name: "dit family without vae",
			profile: func() types.ImageModelProfile {
				p := validProfile("x")
				p.Family = "flux"
				p.Defaults = types.DefaultImageGenDefaults("flux")
				return p
			}(),
		},
		{
			name: "offload and paramsBackend together",
			profile: func() types.ImageModelProfile {
				p := validProfile("x")
				p.Runtime.OffloadToCPU = true
				p.Runtime.ParamsBackend = "cpu"
				return p
			}(),
		},
		{
			name: "unsafe name",
			profile: func() types.ImageModelProfile {
				p := validProfile("x")
				p.Name = "../escape"
				return p
			}(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := store.Set(c.profile.Name, c.profile)
			if err == nil {
				t.Fatal("expected validation error")
			}
			if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
				t.Errorf("invalid profile must not create the file (stat err=%v)", statErr)
			}
			if len(store.List()) != 0 {
				t.Error("invalid profile must not land in the store")
			}
		})
	}
}

// TestImageModelProfileStore_SaveIsAtomic — в каталоге не остаётся .tmp, а файл
// читается как валидный JSON (иначе после рестарта «пропали все профили»).
func TestImageModelProfileStore_SaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	store := NewImageModelProfileStore(path)

	if err := store.Set("a", validProfile("a")); err != nil {
		t.Fatal(err)
	}
	if err := store.Set("b", validProfile("b")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"profiles"`) {
		t.Errorf("unexpected file content: %s", string(data))
	}
}

// TestResolveImageModelProfilesPath — env override уважается.
func TestResolveImageModelProfilesPath(t *testing.T) {
	t.Setenv(EnvImageModelProfilesPath, `C:\tmp\custom-profiles.json`)
	if got := ResolveImageModelProfilesPath(); got != `C:\tmp\custom-profiles.json` {
		t.Errorf("ResolveImageModelProfilesPath = %q", got)
	}
	t.Setenv(EnvImageModelProfilesPath, "")
	if got := ResolveImageModelProfilesPath(); got != DefaultImageModelProfilesPath {
		t.Errorf("default path = %q, want %q", got, DefaultImageModelProfilesPath)
	}
}

// TestResolveImageModelCatalogPath — env override каталога.
func TestResolveImageModelCatalogPath(t *testing.T) {
	t.Setenv(EnvImageModelCatalogPath, "/tmp/catalog.json")
	if got := ResolveImageModelCatalogPath(); got != "/tmp/catalog.json" {
		t.Errorf("ResolveImageModelCatalogPath = %q", got)
	}
}
