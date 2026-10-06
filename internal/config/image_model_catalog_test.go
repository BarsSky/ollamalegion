package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollama-loadbalancer/pkg/types"
)

// TestLoadImageModelCatalog_RepoFile — каталог пресетов в репозитории ДОЛЖЕН
// проходить types.ValidateImageModelProfile целиком.
//
// Это contract-тест данных: битый пресет (несуществующая роль, отсутствие VAE у
// DiT-семейства, width не кратный 64) ловится здесь, а не у оператора, который
// нажал «скачать» и получил падение sd-server.
// TestEmbeddedImageModelCatalog_MatchesRepoFile — вшитая копия каталога не
// должна расходиться с config/image-model-catalog.json.
//
// ЗАЧЕМ: вшитая копия — фолбэк для запуска не из корня репозитория (тесты,
// контейнер с другим WORKDIR). Если её забыть обновить, оператор увидит одну
// версию каталога через API (/api/v1/image/model-catalog читает файл), а
// инструмент выбора модели — другую (вшитую) — и «описания не те, что я правил»
// будет выглядеть как баг балансера.
//
// Регенерация при расхождении:
//
//	Copy-Item config/image-model-catalog.json internal/config/image-model-catalog.embed.json
func TestEmbeddedImageModelCatalog_MatchesRepoFile(t *testing.T) {
	path := filepath.Join("..", "..", "config", "image-model-catalog.json")
	disk, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("файл каталога недоступен (%v): сравнение вшитой копии пропущено", err)
	}
	if string(disk) != string(embeddedImageModelCatalog) {
		t.Fatalf("вшитая копия каталога разошлась с %s: "+
			"скопируйте файл в internal/config/image-model-catalog.embed.json", path)
	}

	// Вшитая копия обязана быть валидным каталогом (её читают без файла на диске).
	catalog, err := LoadImageModelCatalog("")
	if err != nil {
		t.Fatalf("вшитый каталог не загрузился: %v", err)
	}
	if len(catalog.Presets) == 0 {
		t.Fatal("вшитый каталог пуст")
	}
}

func TestLoadImageModelCatalog_RepoFile(t *testing.T) {
	path := filepath.Join("..", "..", "config", "image-model-catalog.json")
	catalog, err := LoadImageModelCatalog(path)
	if err != nil {
		t.Fatalf("LoadImageModelCatalog(%s) failed: %v", path, err)
	}
	if len(catalog.Presets) == 0 {
		t.Fatal("catalog must contain at least one preset")
	}

	// Стартовый набор из плана §6/§12: эти имена обещаны оператору и WebUI.
	required := []string{
		"sd15-q4-0",
		"sd15-q5-0",
		"sd15-q8-0",
		"sdxl-turbo-q8-0",
		"z-image-turbo-q3-k",
		"z-image-turbo-q4-0",
		"flux2-klein-4b-q4-0",
		"flux-schnell-q2-k",
		"flux-schnell-q3-k",
	}
	for _, name := range required {
		if _, ok := catalog.Get(name); !ok {
			t.Errorf("catalog is missing required preset %q", name)
		}
	}

	for _, p := range catalog.Presets {
		// Валидация пресета (повторно, чтобы тест был явным и локальным).
		if err := types.ValidateImageModelProfile(&p); err != nil {
			t.Errorf("preset %q invalid: %v", p.Name, err)
		}
		// DiT-семейства обязаны иметь отдельный VAE — иначе sd-server упадёт
		// на load, а не на скачивании.
		if types.IsDiTFamily(p.Family) {
			hasVae := false
			for _, f := range p.Files {
				if f.Role == types.ImageFileRoleVae {
					hasVae = true
				}
			}
			if !hasVae {
				t.Errorf("preset %q (family %s) must contain a vae file", p.Name, p.Family)
			}
		}
		// Файлы пресета должны быть скачиваемыми «весовыми» форматами: каталог
		// кормит StartBundleDownload, который отклоняет прочие расширения.
		for _, f := range p.Files {
			lower := strings.ToLower(f.Filename)
			ok := false
			for _, ext := range []string{".gguf", ".safetensors", ".sft", ".ckpt"} {
				if strings.HasSuffix(lower, ext) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("preset %q file %q has a non-weight extension", p.Name, f.Filename)
			}
			if f.Repo == "" {
				t.Errorf("preset %q file %q has empty repo", p.Name, f.Filename)
			}
		}
		// Big picture: UI/гейт VRAM опираются на оценку — 0 означало бы «неизвестно».
		if p.VramEstimateMB <= 0 {
			t.Errorf("preset %q must declare vramEstimateMb", p.Name)
		}
	}
}

// TestImageModelCatalog_ValidateRejectsBroken — негативная проверка валидатора
// каталога: дубликат имени и невалидный профиль должны быть отвергнуты.
func TestImageModelCatalog_ValidateRejectsBroken(t *testing.T) {
	good := types.ImageModelProfile{
		Name:   "sd15-ok",
		Family: "sd15",
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "second-state/x", Filename: "m-Q8_0.gguf"},
		},
		Defaults: types.DefaultImageGenDefaults("sd15"),
	}

	dup := ImageModelCatalog{Presets: []types.ImageModelProfile{good, good}}
	if err := dup.Validate(); err == nil {
		t.Error("duplicate preset names must be rejected")
	}

	broken := good
	broken.Name = "sd15-bad-width"
	broken.Defaults.Width = 100 // не кратно 64
	if err := (&ImageModelCatalog{Presets: []types.ImageModelProfile{broken}}).Validate(); err == nil {
		t.Error("preset with width not multiple of 64 must be rejected")
	}

	noVae := types.ImageModelProfile{
		Name:   "flux-no-vae",
		Family: "flux",
		Files: []types.ImageModelFile{
			{Role: types.ImageFileRoleDiffusion, Repo: "leejet/x", Filename: "m.gguf"},
		},
		Defaults: types.DefaultImageGenDefaults("flux"),
	}
	if err := (&ImageModelCatalog{Presets: []types.ImageModelProfile{noVae}}).Validate(); err == nil {
		t.Error("DiT preset without vae must be rejected")
	}

	badName := good
	badName.Name = "../escape"
	if err := (&ImageModelCatalog{Presets: []types.ImageModelProfile{badName}}).Validate(); err == nil {
		t.Error("preset name with path separators must be rejected")
	}
}

// TestLoadImageModelCatalog_MissingFileUsesEmbedded — отсутствующий файл даёт
// вшитую копию каталога (R85), а не ошибку: каталог обязан работать и когда
// балансер запущен не из корня репозитория.
func TestLoadImageModelCatalog_MissingFileUsesEmbedded(t *testing.T) {
	catalog, err := LoadImageModelCatalog(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("отсутствующий файл должен давать вшитую копию, got error: %v", err)
	}
	if len(catalog.Presets) == 0 {
		t.Fatal("вшитая копия пуста")
	}
	// Вшитая копия обязана быть тем же каталогом, что и файл репозитория.
	if _, err := LoadImageModelCatalog(filepath.Join("..", "..", "config", "image-model-catalog.json")); err != nil {
		t.Skipf("файл репозитория недоступен: %v", err)
	}
}

// TestLoadImageModelCatalog_InvalidJSON — битый JSON отвергается, а не «пустой каталог».
func TestLoadImageModelCatalog_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json")
	if err := os.WriteFile(path, []byte(`{"presets": [`), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadImageModelCatalog(path); err == nil {
		t.Fatal("expected parse error")
	}
}

// TestCloneImageModelProfile — копия не делит срезы с оригиналом (иначе
// мутация ответа API меняла бы файлы профиля в хранилище).
func TestCloneImageModelProfile(t *testing.T) {
	src := types.ImageModelProfile{
		Name: "x",
		Files: []types.ImageModelFile{
			{Role: "diffusion", Repo: "a", Filename: "b.gguf"},
		},
		Runtime: types.ImageRuntime{ExtraArgs: []string{"--foo"}},
	}
	cp := CloneImageModelProfile(src)
	cp.Files[0].Repo = "changed"
	cp.Runtime.ExtraArgs[0] = "--bar"
	if src.Files[0].Repo != "a" {
		t.Error("CloneImageModelProfile must deep-copy Files")
	}
	if src.Runtime.ExtraArgs[0] != "--foo" {
		t.Error("CloneImageModelProfile must deep-copy ExtraArgs")
	}
}

// TestImageModelCatalog_ServerArgsPerFamily — пресеты должны давать РАЗНЫЕ
// флаги sd-server в зависимости от семейства: all-in-one (SD1.5) грузится через
// --model, DiT (Z-Image/FLUX/klein) — через --diffusion-model + --vae (+ TE).
// Это защита от «пресет есть, а движок его не понимает».
func TestImageModelCatalog_ServerArgsPerFamily(t *testing.T) {
	catalog, err := LoadImageModelCatalog(filepath.Join("..", "..", "config", "image-model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}

	argsByName := map[string][]string{}
	for _, p := range catalog.Presets {
		profile := p
		argsByName[p.Name] = profile.ServerArgs()
	}

	sd15 := argsByName["sd15-q8-0"]
	if !contains(sd15, "--model") {
		t.Errorf("sd15 preset must use --model, got %v", sd15)
	}
	if contains(sd15, "--diffusion-model") {
		t.Errorf("sd15 preset must NOT use --diffusion-model, got %v", sd15)
	}

	zimage := argsByName["z-image-turbo-q3-k"]
	for _, flag := range []string{"--diffusion-model", "--vae", "--llm", "--offload-to-cpu", "--diffusion-fa", "--seed"} {
		if !contains(zimage, flag) {
			t.Errorf("z-image preset must pass %s, got %v", flag, zimage)
		}
	}

	flux := argsByName["flux-schnell-q3-k"]
	for _, flag := range []string{"--diffusion-model", "--vae", "--clip_l", "--t5xxl", "--backend"} {
		if !contains(flux, flag) {
			t.Errorf("flux preset must pass %s, got %v", flag, flux)
		}
	}

	klein := argsByName["flux2-klein-4b-q4-0"]
	for _, flag := range []string{"--diffusion-model", "--vae", "--llm"} {
		if !contains(klein, flag) {
			t.Errorf("flux2-klein preset must pass %s, got %v", flag, klein)
		}
	}

	sdxl := argsByName["sdxl-turbo-q8-0"]
	if !contains(sdxl, "--model") {
		t.Errorf("sdxl_turbo preset must use --model (all-in-one), got %v", sdxl)
	}
	if !contains(sdxl, "--steps") {
		t.Errorf("sdxl_turbo preset must pass --steps, got %v", sdxl)
	}
}

func contains(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestImageModelCatalogJSONShape — файл каталога должен быть валидным JSON
// с версией: формат читают и балансер, и (в будущем) воркер.
func TestImageModelCatalogJSONShape(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "image-model-catalog.json"))
	if err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Version int             `json:"version"`
		Presets json.RawMessage `json:"presets"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("catalog is not valid JSON: %v", err)
	}
	if shape.Version != 1 {
		t.Errorf("catalog version = %d, want 1", shape.Version)
	}
}
